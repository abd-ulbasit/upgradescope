package server

import (
	"context"
	"errors"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/abd-ulbasit/upgradescope/internal/inventory"
	"github.com/abd-ulbasit/upgradescope/internal/kb"
	"github.com/abd-ulbasit/upgradescope/internal/server/notify"
	"github.com/abd-ulbasit/upgradescope/internal/server/store"
)

// TestUpgradeBaselineSpansASkippedMinor: the agent missed the push at 1.35
// (1.34 → 1.36). The only decided evaluation below the new default target
// 1.37 is 1.35's, two minors down, and it is still the baseline: the
// ServiceCIDR v1beta1 call, fine at 1.35, is announced as a blocker for
// 1.37. With upgradeLookback = 1 the baseline is not found and the first
// evaluation of 1.37 would be silent.
func TestUpgradeBaselineSpansASkippedMinor(t *testing.T) {
	srv, rec := upgradeServer(t, openSQLite(t))
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	pushInventory(t, ts.URL, "tok", atVersion("v1.34.5", serviceCIDRv1beta1))
	pushInventory(t, ts.URL, "tok", atVersion("v1.36.1", serviceCIDRv1beta1))
	srv.deliverOutbox(context.Background())

	evs := rec.all()
	if len(evs) != 1 || evs[0].Kind != notify.KindNewBlocker || evs[0].Target != "1.37" ||
		!strings.Contains(evs[0].Title, "ServiceCIDR") {
		t.Fatalf("want the ServiceCIDR new-blocker for 1.37 across the skipped minor, got %+v", evs)
	}
}

// TestUpgradeBaselineSpansTwoSkippedMinors: 1.33 → 1.36 skips two minors.
// The only decided evaluation below the new default target 1.37 is 1.34's,
// three minors down, at the edge of upgradeLookback; the ServiceCIDR
// blocker new at 1.37 is announced against it.
func TestUpgradeBaselineSpansTwoSkippedMinors(t *testing.T) {
	srv, rec := upgradeServer(t, openSQLite(t))
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	pushInventory(t, ts.URL, "tok", atVersion("v1.33.5", serviceCIDRv1beta1))
	pushInventory(t, ts.URL, "tok", atVersion("v1.36.1", serviceCIDRv1beta1))
	srv.deliverOutbox(context.Background())

	evs := rec.all()
	if len(evs) != 1 || evs[0].Kind != notify.KindNewBlocker || evs[0].Target != "1.37" ||
		!strings.Contains(evs[0].Title, "ServiceCIDR") {
		t.Fatalf("want the ServiceCIDR new-blocker for 1.37 against 1.34, three minors down, got %+v", evs)
	}
}

// TestUpgradeBaselineIsTheNearestLowerTarget: 1.33 (clean) → 1.34 (PSP) →
// 1.36 (PSP). Below 1.37 the cluster has decided evaluations for 1.34
// (clean, from the 1.33 push) and 1.35 (PSP, from the 1.34 push). The
// nearest, 1.35, is the baseline, so the PSP blocker it already had is not
// announced again. Searching farthest-first would pick 1.34's clean
// evaluation and announce it.
func TestUpgradeBaselineIsTheNearestLowerTarget(t *testing.T) {
	srv, rec := upgradeServer(t, openSQLite(t))
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	pushInventory(t, ts.URL, "tok", atVersion("v1.33.5"))
	pushInventory(t, ts.URL, "tok", atVersion("v1.34.5", podSecurityPolicy))
	srv.deliverOutbox(context.Background())
	before := len(rec.all())

	pushInventory(t, ts.URL, "tok", atVersion("v1.36.1", podSecurityPolicy))
	srv.deliverOutbox(context.Background())
	if evs := rec.all(); len(evs) != before {
		t.Fatalf("PSP was already a blocker at the nearest lower target, got %+v", evs[before:])
	}
}

// lowHorizonServer is upgradeServer over the real KB cut off at minor,
// as a server whose knowledge base predates the newest releases.
func lowHorizonServer(t *testing.T, st store.Store, minor int) (*Server, *recordingNotifier) {
	t.Helper()
	k, err := kb.Load()
	if err != nil {
		t.Fatal(err)
	}
	k.MaxKnownK8s = inventory.Version{Major: 1, Minor: minor}
	k.Version += "; horizon cut to " + k.MaxKnownK8s.String()
	// What such a knowledge base cannot know yet: removals past its horizon.
	k.APILifecycle = slices.Clone(k.APILifecycle)
	for i, e := range k.APILifecycle {
		if e.Removed != nil && e.Removed.Minor > minor {
			e.Removed = nil
			k.APILifecycle[i] = e
		}
	}
	rec := &recordingNotifier{}
	srv, err := New(Config{Store: st, KB: k, Notifier: rec, IngestToken: "tok"})
	if err != nil {
		t.Fatal(err)
	}
	return srv, rec
}

// TestKBHorizonUnknownThenDecidedNotifies: a cluster upgraded 1.35 → 1.36
// while the knowledge base ends at 1.36, so its new default target 1.37 is
// unknown. The decided evaluation of 1.36 (from the 1.35 push) is stored.
// When a server with a newer knowledge base first decides 1.37, the
// cluster is told of the blocker 1.37 adds (ServiceCIDR v1beta1), compared
// with its own 1.36 evaluation.
func TestKBHorizonUnknownThenDecidedNotifies(t *testing.T) {
	st := openSQLite(t)
	old, oldRec := lowHorizonServer(t, st, 36)
	ts := httptest.NewServer(old.Handler())
	defer ts.Close()
	ctx := context.Background()

	pushInventory(t, ts.URL, "tok", atVersion("v1.35.3", serviceCIDRv1beta1))
	pushInventory(t, ts.URL, "tok", atVersion("v1.36.1", serviceCIDRv1beta1))
	old.deliverOutbox(ctx)
	if _, err := st.CurrentEvaluation(ctx, 1, "1.37"); err != nil {
		t.Fatalf("1.37 evaluation missing under a KB ending at 1.36: %v", err)
	}
	if e, err := st.LatestKnownEvaluation(ctx, 1, "1.37"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("1.37 under a KB ending at 1.36 has a decided evaluation %+v (%v); want it unknown", e, err)
	}
	if evs := oldRec.all(); len(evs) != 0 {
		t.Fatalf("nothing is decided yet, got %+v", evs)
	}

	updated, rec := upgradeServer(t, st)
	updated.reevaluateAll(ctx)
	updated.deliverOutbox(ctx)
	evs := rec.all()
	if len(evs) != 1 || evs[0].Kind != notify.KindNewBlocker || evs[0].Target != "1.37" ||
		!strings.Contains(evs[0].Title, "ServiceCIDR") {
		t.Fatalf("want the ServiceCIDR new-blocker for 1.37 once the KB decides it, got %+v", evs)
	}
}

// TestKBHorizonUnknownThenDecidedSilentWithoutLowerEvaluation: a cluster
// first seen on 1.36 under the same short knowledge base has no stored
// decided evaluation of a lower target, so when 1.37 is first decided it is
// a silent baseline (the docs qualify the claim above on this).
func TestKBHorizonUnknownThenDecidedSilentWithoutLowerEvaluation(t *testing.T) {
	st := openSQLite(t)
	old, _ := lowHorizonServer(t, st, 36)
	ts := httptest.NewServer(old.Handler())
	defer ts.Close()
	ctx := context.Background()

	pushInventory(t, ts.URL, "tok", atVersion("v1.36.1", serviceCIDRv1beta1))
	old.deliverOutbox(ctx)

	updated, rec := upgradeServer(t, st)
	updated.reevaluateAll(ctx)
	updated.deliverOutbox(ctx)
	if evs := rec.all(); len(evs) != 0 {
		t.Fatalf("no lower evaluation to compare with: want a silent baseline, got %+v", evs)
	}
	if e, err := st.LatestKnownEvaluation(ctx, 1, "1.37"); err != nil || e.Blockers == 0 {
		t.Fatalf("1.37 evaluation = %+v, %v; want it stored decided and blocked", e, err)
	}
}
