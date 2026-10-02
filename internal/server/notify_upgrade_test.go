package server

import (
	"context"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/abd-ulbasit/upgradescope/internal/inventory"
	"github.com/abd-ulbasit/upgradescope/internal/kb"
	"github.com/abd-ulbasit/upgradescope/internal/server/notify"
	"github.com/abd-ulbasit/upgradescope/internal/server/store"
)

// upgradeServer is a server over a real SQLite store and the real KB, as
// in #34's reproduction, recording what it delivers.
func upgradeServer(t *testing.T, st store.Store, extraTargets ...string) (*Server, *recordingNotifier) {
	t.Helper()
	kbData, err := kb.Load()
	if err != nil {
		t.Fatal(err)
	}
	rec := &recordingNotifier{}
	srv, err := New(Config{Store: st, KB: kbData, Notifier: rec, IngestToken: "tok", ExtraTargets: extraTargets})
	if err != nil {
		t.Fatal(err)
	}
	return srv, rec
}

func openSQLite(t *testing.T) store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "db.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

// atVersion is an inventory of a cluster running serverVersion that calls
// the given APIs.
func atVersion(serverVersion string, usage ...inventory.APIUsage) inventory.Inventory {
	return inventory.Inventory{
		SchemaVersion: 1,
		ClusterID:     "uid-1",
		CollectedAt:   time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC),
		ServerVersion: serverVersion,
		Capabilities:  map[inventory.Capability]inventory.CapabilityStatus{},
		APIUsage:      usage,
	}
}

var (
	serviceCIDRv1beta1 = inventory.APIUsage{Group: "networking.k8s.io", Version: "v1beta1", Kind: "ServiceCIDR", Count: 1} // removed in 1.37
	podSecurityPolicy  = inventory.APIUsage{Group: "policy", Version: "v1beta1", Kind: "PodSecurityPolicy", Count: 1}      // removed in 1.25
)

// TestUpgradeNotifiesBlockerNewAtNextTarget is #34's ServiceCIDR case: a
// cluster on 1.35 calls networking.k8s.io/v1beta1 ServiceCIDR, which is
// fine for its default target 1.36. It upgrades to 1.36, so the default
// target becomes 1.37, where v1beta1 is removed. That blocker is new for
// the cluster, and the first evaluation of 1.37 must say so instead of
// being taken as a silent baseline.
func TestUpgradeNotifiesBlockerNewAtNextTarget(t *testing.T) {
	srv, rec := upgradeServer(t, openSQLite(t))
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	ctx := context.Background()

	pushInventory(t, ts.URL, "tok", atVersion("v1.35.3", serviceCIDRv1beta1))
	srv.deliverOutbox(ctx)
	if evs := rec.all(); len(evs) != 0 {
		t.Fatalf("first push must not notify, got %+v", evs)
	}

	pushInventory(t, ts.URL, "tok", atVersion("v1.36.1", serviceCIDRv1beta1))
	srv.deliverOutbox(ctx)

	got := rec.notifications()
	if len(got) != 1 || len(got[0].Changes) != 1 {
		t.Fatalf("want one notification with one change after the upgrade, got %+v", got)
	}
	c := got[0].Changes[0]
	if c.Kind != notify.KindNewBlocker || c.Key != "removed-api/networking.k8s.io/v1beta1/ServiceCIDR" ||
		len(c.Targets) != 1 || c.Targets[0] != "1.37" {
		t.Errorf("change = %+v, want new-blocker removed-api/networking.k8s.io/v1beta1/ServiceCIDR for 1.37", c)
	}
	if ts := got[0].Targets; len(ts) != 1 || ts[0].Target != "1.37" || ts[0].Verdict != "blocked" {
		t.Errorf("targets = %+v, want 1.37 blocked", ts)
	}
}

// TestUpgradeDoesNotReannounceKnownBlockers: a blocker the cluster already
// had at its old default target (PodSecurityPolicy, removed long ago, a
// blocker for every target) is not news after the upgrade. Only the
// blocker the new target adds is announced.
func TestUpgradeDoesNotReannounceKnownBlockers(t *testing.T) {
	srv, rec := upgradeServer(t, openSQLite(t))
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	ctx := context.Background()

	pushInventory(t, ts.URL, "tok", atVersion("v1.35.3", podSecurityPolicy))
	pushInventory(t, ts.URL, "tok", atVersion("v1.36.1", podSecurityPolicy))
	srv.deliverOutbox(ctx)
	if evs := rec.all(); len(evs) != 0 {
		t.Fatalf("a blocker known before the upgrade must not be re-announced, got %+v", evs)
	}

	pushInventory(t, ts.URL, "tok", atVersion("v1.36.1", podSecurityPolicy, serviceCIDRv1beta1))
	srv.deliverOutbox(ctx)
	evs := rec.all()
	if len(evs) != 1 || evs[0].Kind != notify.KindNewBlocker || evs[0].Target != "1.37" {
		t.Fatalf("want only the ServiceCIDR new-blocker for 1.37, got %+v", evs)
	}
}

// TestUpgradeAnnouncesOnlyNewBlockersInOnePass: the upgrade push itself
// carries a blocker known at the old target and one the new target adds;
// one notification lists only the new one.
func TestUpgradeAnnouncesOnlyNewBlockersInOnePass(t *testing.T) {
	srv, rec := upgradeServer(t, openSQLite(t))
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	pushInventory(t, ts.URL, "tok", atVersion("v1.35.3", podSecurityPolicy, serviceCIDRv1beta1))
	pushInventory(t, ts.URL, "tok", atVersion("v1.36.1", podSecurityPolicy, serviceCIDRv1beta1))
	srv.deliverOutbox(context.Background())

	got := rec.notifications()
	if len(got) != 1 || len(got[0].Changes) != 1 || got[0].Changes[0].Key != "removed-api/networking.k8s.io/v1beta1/ServiceCIDR" {
		t.Fatalf("want one notification with only the ServiceCIDR blocker, got %+v", got)
	}
}

// TestNewlyConfiguredTargetIsStillABaseline: the upgrade baseline is only
// for the default target. A target the operator adds to --targets is a
// new question, not a change in the cluster: its first evaluation stays
// silent, so restarting the server with one does not page every cluster
// in the fleet at once.
func TestNewlyConfiguredTargetIsStillABaseline(t *testing.T) {
	st := openSQLite(t)
	srv, rec := upgradeServer(t, st)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	ctx := context.Background()
	pushInventory(t, ts.URL, "tok", atVersion("v1.35.3", serviceCIDRv1beta1))

	restarted, rec2 := upgradeServer(t, st, "1.38")
	restarted.reevaluateAll(ctx)
	restarted.deliverOutbox(ctx)
	srv.deliverOutbox(ctx)
	if evs := append(rec.all(), rec2.all()...); len(evs) != 0 {
		t.Fatalf("first evaluation of a newly configured target must not notify, got %+v", evs)
	}
	if e, err := st.LatestKnownEvaluation(ctx, 1, "1.38"); err != nil || e.Blockers == 0 {
		t.Fatalf("1.38 evaluation = %+v, %v; want it stored blocked (ServiceCIDR v1beta1 removed in 1.37)", e, err)
	}
}

// TestClusterFirstSeenOnNewVersionIsStillBaseline: a cluster first seen
// already on 1.36 has no earlier target to compare with. Its first decided
// evaluation of 1.37 (after an unknown one: api-usage was forbidden) stays
// the baseline.
func TestClusterFirstSeenOnNewVersionIsStillBaseline(t *testing.T) {
	srv, rec := upgradeServer(t, openSQLite(t))
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	blind := atVersion("v1.36.1")
	blind.Capabilities = map[inventory.Capability]inventory.CapabilityStatus{
		inventory.CapAPIUsage: {Available: false, Reason: "forbidden"},
	}
	pushInventory(t, ts.URL, "tok", blind)
	pushInventory(t, ts.URL, "tok", atVersion("v1.36.1", serviceCIDRv1beta1))
	srv.deliverOutbox(context.Background())
	if evs := rec.all(); len(evs) != 0 {
		t.Fatalf("a new cluster's first decided evaluation must not notify, got %+v", evs)
	}
}
