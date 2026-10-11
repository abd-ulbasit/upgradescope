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
// unknown. That unknown pass is the target's baseline (#380): only the
// knowledge-base horizon kept it from a verdict, and it was compared with
// the decided 1.36 evaluation (from the 1.35 push), which it matched.
// When a server with a newer knowledge base first decides 1.37, the
// cluster is told of the blocker 1.37 adds (ServiceCIDR v1beta1) exactly
// once: the next background pass does not announce it again.
func TestKBHorizonUnknownThenDecidedNotifies(t *testing.T) {
	forEachStore(t, func(t *testing.T, st store.Store) {
		old, oldRec := lowHorizonServer(t, st, 36)
		ts := httptest.NewServer(old.Handler())
		defer ts.Close()
		ctx := context.Background()

		pushInventory(t, ts.URL, "tok", atVersion("v1.35.3", serviceCIDRv1beta1))
		pushInventory(t, ts.URL, "tok", atVersion("v1.36.1", serviceCIDRv1beta1))
		old.deliverOutbox(ctx)
		cur, err := st.CurrentEvaluation(ctx, 1, "1.37")
		if err != nil {
			t.Fatalf("1.37 evaluation missing under a KB ending at 1.36: %v", err)
		}
		if cur.Ready || cur.Blockers != 0 || !cur.Baseline {
			t.Fatalf("1.37 under a KB ending at 1.36 = %+v; want unknown, and a baseline", cur)
		}
		if e, err := st.LatestKnownEvaluation(ctx, 1, "1.37"); err != nil || e.ID != cur.ID {
			t.Fatalf("1.37 baseline = (%+v, %v); want the unknown evaluation %d", e, err, cur.ID)
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

		updated.reevaluateAll(ctx)
		updated.deliverOutbox(ctx)
		if evs := rec.all(); len(evs) != 1 {
			t.Fatalf("the blocker was announced again: %+v", evs[1:])
		}
	})
}

// TestKBHorizonUnknownThenDecidedNotifiesWithoutLowerEvaluation: a cluster
// first seen on 1.36 under the same short knowledge base has no decided
// evaluation of a lower target. Its first, unknown evaluation of 1.37 is
// the baseline all the same (#380): only the horizon kept it from a
// verdict, and it saw everything a blocker of the KB it was judged with
// could come from. So when 1.37 is first decided, the blocker the update
// adds is announced once, as for a cluster with a lower evaluation.
func TestKBHorizonUnknownThenDecidedNotifiesWithoutLowerEvaluation(t *testing.T) {
	forEachStore(t, func(t *testing.T, st store.Store) {
		old, oldRec := lowHorizonServer(t, st, 36)
		ts := httptest.NewServer(old.Handler())
		defer ts.Close()
		ctx := context.Background()

		pushInventory(t, ts.URL, "tok", atVersion("v1.36.1", serviceCIDRv1beta1))
		old.deliverOutbox(ctx)
		if evs := oldRec.all(); len(evs) != 0 {
			t.Fatalf("first evaluation notified: %+v", evs)
		}

		updated, rec := upgradeServer(t, st)
		updated.reevaluateAll(ctx)
		updated.deliverOutbox(ctx)
		evs := rec.all()
		if len(evs) != 1 || evs[0].Kind != notify.KindNewBlocker || evs[0].Target != "1.37" ||
			!strings.Contains(evs[0].Title, "ServiceCIDR") {
			t.Fatalf("want the ServiceCIDR new-blocker for 1.37 once the KB decides it, got %+v", evs)
		}
		updated.reevaluateAll(ctx)
		updated.deliverOutbox(ctx)
		if evs := rec.all(); len(evs) != 1 {
			t.Fatalf("the blocker was announced again: %+v", evs[1:])
		}
	})
}

// TestKBHorizonUnknownThenDecidedSilentWithoutLowerEvaluation: the same
// cluster, first seen while its add-ons collector fails (a required gap
// that hides add-on findings), has no baseline at all: that unknown pass
// is none, since what it could not see may be a blocker. When 1.37 is
// first decided, that evaluation is the silent baseline, as before #380.
func TestKBHorizonUnknownThenDecidedSilentWithoutLowerEvaluation(t *testing.T) {
	forEachStore(t, func(t *testing.T, st store.Store) {
		old, _ := lowHorizonServer(t, st, 36)
		ts := httptest.NewServer(old.Handler())
		defer ts.Close()
		ctx := context.Background()

		pushInventory(t, ts.URL, "tok", withoutAddOnsCollector(atVersion("v1.36.1", serviceCIDRv1beta1)))
		old.deliverOutbox(ctx)
		if e, err := st.LatestKnownEvaluation(ctx, 1, "1.37"); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("1.37 baseline = (%+v, %v); want none behind the add-ons outage", e, err)
		}

		updated, rec := upgradeServer(t, st)
		updated.reevaluateAll(ctx)
		updated.deliverOutbox(ctx)
		if evs := rec.all(); len(evs) != 0 {
			t.Fatalf("no baseline to compare with: want a silent first one, got %+v", evs)
		}
		if e, err := st.LatestKnownEvaluation(ctx, 1, "1.37"); err != nil || e.Blockers == 0 {
			t.Fatalf("1.37 evaluation = %+v, %v; want it stored decided and blocked", e, err)
		}
	})
}

// newestMinor is a clean inventory of a cluster on the newest minor the
// shipped knowledge base knows (1.37): its default target, 1.38, is past
// the KB's horizon, so a clean pass is unknown (the kb-coverage gap) and
// never ready.
func newestMinor(addOns ...inventory.AddOnInstance) inventory.Inventory {
	inv := atVersion("v1.37.0")
	inv.AddOns = addOns
	return inv
}

var (
	// ingressNginx and promtail are add-ons past their end of life in the
	// shipped registry: a blocker for every target.
	ingressNginx = inventory.AddOnInstance{ID: "ingress-nginx", Version: "1.8.1", Namespaces: []string{"ingress-nginx"}, Source: "image"}
	promtail     = inventory.AddOnInstance{ID: "promtail", Version: "2.9.0", Namespaces: []string{"logging"}, Source: "image"}
)

// withoutAddOnsCollector is inv as pushed while the add-ons collector
// failed: a required gap that hides every add-on finding.
func withoutAddOnsCollector(inv inventory.Inventory) inventory.Inventory {
	inv.Capabilities = collectedCaps()
	inv.Capabilities[inventory.CapAddOns] = inventory.CapabilityStatus{Available: false, Reason: "forbidden"}
	inv.AddOns = nil
	return inv
}

// pushAndDeliver pushes inv and delivers what it queued, returning the
// events it added.
func pushAndDeliver(t *testing.T, srv *Server, url string, rec *recordingNotifier, inv inventory.Inventory) []event {
	t.Helper()
	before := len(rec.all())
	pushInventory(t, url, "tok", inv)
	srv.deliverOutbox(context.Background())
	return rec.all()[before:]
}

// announces reports whether evs is exactly one new-blocker per target
// whose title names name (lower-cased).
func announces(evs []event, name string, targets ...string) bool {
	if len(evs) != len(targets) {
		return false
	}
	for i, e := range evs {
		if e.Kind != notify.KindNewBlocker || e.Target != targets[i] || !strings.Contains(strings.ToLower(e.Title), name) {
			return false
		}
	}
	return true
}

// TestUnknownThenBlockedAnnouncesNewBlocker is #380's first case: a
// cluster first seen on the newest minor is unknown at its default target
// when clean, because the target is past the KB's horizon. That pass saw
// the add-ons, so it is the baseline, and the first EOL add-on after it is
// a new blocker, announced once; a second one is announced alone.
func TestUnknownThenBlockedAnnouncesNewBlocker(t *testing.T) {
	forEachStore(t, func(t *testing.T, st store.Store) {
		srv, rec := upgradeServer(t, st)
		ts := httptest.NewServer(srv.Handler())
		defer ts.Close()
		ctx := context.Background()

		if evs := pushAndDeliver(t, srv, ts.URL, rec, newestMinor()); len(evs) != 0 {
			t.Fatalf("first push notified: %+v", evs)
		}
		cur, err := st.CurrentEvaluation(ctx, 1, "1.38")
		if err != nil || cur.Ready || cur.Blockers != 0 || !cur.Baseline {
			t.Fatalf("clean 1.38 = (%+v, %v); want unknown, and a baseline", cur, err)
		}
		if evs := pushAndDeliver(t, srv, ts.URL, rec, newestMinor(ingressNginx)); !announces(evs, "ingress nginx", "1.38") {
			t.Fatalf("EOL ingress-nginx after a clean unknown pass: got %+v, want one new-blocker for 1.38", evs)
		}
		if evs := pushAndDeliver(t, srv, ts.URL, rec, newestMinor(ingressNginx, promtail)); !announces(evs, "promtail", "1.38") {
			t.Fatalf("EOL promtail added: got %+v, want one new-blocker for it alone", evs)
		}
		srv.reevaluateAll(ctx)
		srv.deliverOutbox(ctx)
		if evs := rec.all(); len(evs) != 2 {
			t.Fatalf("a background pass notified again: %+v", evs[2:])
		}
	})
}

// TestBlockerResolvedByUnknownPassIsAnnouncedOnReturn is #380's second
// case: blocked, then an unknown pass (past the horizon) that saw the
// add-on gone, then blocked by it again. The unknown pass resolved it, so
// its return is a new blocker.
func TestBlockerResolvedByUnknownPassIsAnnouncedOnReturn(t *testing.T) {
	forEachStore(t, func(t *testing.T, st store.Store) {
		srv, rec := upgradeServer(t, st)
		ts := httptest.NewServer(srv.Handler())
		defer ts.Close()

		pushAndDeliver(t, srv, ts.URL, rec, newestMinor())
		if evs := pushAndDeliver(t, srv, ts.URL, rec, newestMinor(ingressNginx, promtail)); len(evs) != 2 {
			t.Fatalf("two EOL add-ons: got %+v, want two new-blockers", evs)
		}
		if evs := pushAndDeliver(t, srv, ts.URL, rec, newestMinor()); len(evs) != 0 {
			t.Fatalf("both removed (unknown, never ready): got %+v, want nothing", evs)
		}
		if evs := pushAndDeliver(t, srv, ts.URL, rec, newestMinor(ingressNginx)); !announces(evs, "ingress nginx", "1.38") {
			t.Fatalf("ingress-nginx back: got %+v, want one new-blocker for it", evs)
		}
	})
}

// TestUnknownWithRequiredGapIsNotABaseline: an unknown pass behind a
// required gap that hides findings (the add-ons collector failed) is no
// baseline. A cluster first seen that way gets no alert for the EOL add-on
// its recovery shows (nothing says it is new), nor does one whose blocker
// never went away; and such a pass between a clean baseline and a blocker
// does not hide the blocker's news.
func TestUnknownWithRequiredGapIsNotABaseline(t *testing.T) {
	forEachStore(t, func(t *testing.T, st store.Store) {
		srv, rec := upgradeServer(t, st)
		ts := httptest.NewServer(srv.Handler())
		defer ts.Close()
		ctx := context.Background()

		pushAndDeliver(t, srv, ts.URL, rec, withoutAddOnsCollector(newestMinor()))
		if cur, err := st.CurrentEvaluation(ctx, 1, "1.38"); err != nil || cur.Ready || cur.Blockers != 0 || cur.Baseline {
			t.Fatalf("1.38 during the outage = (%+v, %v); want unknown, and no baseline", cur, err)
		}
		if e, err := st.LatestKnownEvaluation(ctx, 1, "1.38"); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("1.38 baseline = (%+v, %v); want none", e, err)
		}
		if evs := pushAndDeliver(t, srv, ts.URL, rec, newestMinor(ingressNginx)); len(evs) != 0 {
			t.Fatalf("recovery after a first-seen outage: got %+v, want a silent first baseline", evs)
		}
		if evs := pushAndDeliver(t, srv, ts.URL, rec, withoutAddOnsCollector(newestMinor())); len(evs) != 0 {
			t.Fatalf("outage: got %+v, want nothing", evs)
		}
		if evs := pushAndDeliver(t, srv, ts.URL, rec, newestMinor(ingressNginx)); len(evs) != 0 {
			t.Fatalf("recovery re-alerted a blocker that never went away: %+v", evs)
		}
		if evs := pushAndDeliver(t, srv, ts.URL, rec, newestMinor()); len(evs) != 0 {
			t.Fatalf("ingress-nginx removed: got %+v, want nothing", evs)
		}
		if evs := pushAndDeliver(t, srv, ts.URL, rec, withoutAddOnsCollector(newestMinor())); len(evs) != 0 {
			t.Fatalf("outage: got %+v, want nothing", evs)
		}
		if evs := pushAndDeliver(t, srv, ts.URL, rec, newestMinor(ingressNginx)); !announces(evs, "ingress nginx", "1.38") {
			t.Fatalf("ingress-nginx back after the clean baseline: got %+v, want one new-blocker", evs)
		}
	})
}

// TestNewTargetDoesNotReannounceStandingBlockers: a target added to
// --targets is a new question, and its first evaluation is a silent
// baseline whether it is blocked or unknown (#34): the EOL add-on the
// cluster already had is not announced for it. A blocker added later is
// announced for every target, once.
func TestNewTargetDoesNotReannounceStandingBlockers(t *testing.T) {
	forEachStore(t, func(t *testing.T, st store.Store) {
		srv, rec := upgradeServer(t, st)
		ts := httptest.NewServer(srv.Handler())
		defer ts.Close()
		ctx := context.Background()
		pushAndDeliver(t, srv, ts.URL, rec, newestMinor(ingressNginx))

		widened, wrec := upgradeServer(t, st, "1.39")
		widened.reevaluateAll(ctx)
		widened.deliverOutbox(ctx)
		if _, err := st.CurrentEvaluation(ctx, 1, "1.39"); err != nil {
			t.Fatalf("1.39 not evaluated: %v", err)
		}
		if evs := wrec.all(); len(evs) != 0 {
			t.Fatalf("the new target re-announced a standing blocker: %+v", evs)
		}
		wts := httptest.NewServer(widened.Handler())
		defer wts.Close()
		if evs := pushAndDeliver(t, widened, wts.URL, wrec, newestMinor(ingressNginx, promtail)); !announces(evs, "promtail", "1.38", "1.39") {
			t.Fatalf("promtail added: got %+v, want its new-blocker for 1.38 and 1.39", evs)
		}
	})
}

// TestKBUpdateDoesNotReannounceStandingBlockers: a knowledge-base update
// that first decides a target past the old horizon announces only what
// the update adds (ServiceCIDR v1beta1, removed in 1.37), never the EOL
// add-on the baseline already had; over a clean unknown baseline, an
// update that adds nothing announces nothing (no became-ready: the
// cluster was never blocked).
func TestKBUpdateDoesNotReannounceStandingBlockers(t *testing.T) {
	kbUpdate := func(t *testing.T, st store.Store, inv inventory.Inventory) []event {
		t.Helper()
		ctx := context.Background()
		old, _ := lowHorizonServer(t, st, 36)
		ts := httptest.NewServer(old.Handler())
		defer ts.Close()
		pushInventory(t, ts.URL, "tok", inv)
		old.deliverOutbox(ctx)
		updated, rec := upgradeServer(t, st)
		updated.reevaluateAll(ctx)
		updated.deliverOutbox(ctx)
		return rec.all()
	}
	t.Run("blocked", func(t *testing.T) {
		forEachStore(t, func(t *testing.T, st store.Store) {
			blocked := atVersion("v1.36.1", serviceCIDRv1beta1)
			blocked.AddOns = []inventory.AddOnInstance{ingressNginx}
			if evs := kbUpdate(t, st, blocked); !announces(evs, "servicecidr", "1.37") {
				t.Fatalf("KB update: got %+v, want only the ServiceCIDR new-blocker", evs)
			}
		})
	})
	t.Run("clean", func(t *testing.T) {
		forEachStore(t, func(t *testing.T, st store.Store) {
			if evs := kbUpdate(t, st, atVersion("v1.36.1")); len(evs) != 0 {
				t.Fatalf("KB update over a clean unknown baseline: got %+v, want nothing", evs)
			}
		})
	})
}

// TestBlockerResolvedByUnknownPassBecomesReadyWhenDecided: blocked, then
// an unknown pass past the horizon that saw the blocker gone: nothing is
// sent (unknown is never ready), and the pass is the baseline. When a
// knowledge-base update decides the target ready, the cluster gets its
// became-ready, once: the unknown pass kept the resolved blocker's
// readiness pending.
func TestBlockerResolvedByUnknownPassBecomesReadyWhenDecided(t *testing.T) {
	forEachStore(t, func(t *testing.T, st store.Store) {
		ctx := context.Background()
		old, oldRec := lowHorizonServer(t, st, 36)
		ts := httptest.NewServer(old.Handler())
		defer ts.Close()
		withNginx := atVersion("v1.36.1")
		withNginx.AddOns = []inventory.AddOnInstance{ingressNginx}
		pushAndDeliver(t, old, ts.URL, oldRec, withNginx)
		if evs := pushAndDeliver(t, old, ts.URL, oldRec, atVersion("v1.36.1")); len(evs) != 0 {
			t.Fatalf("resolved by an unknown pass: got %+v, want nothing", evs)
		}
		old.reevaluateAll(ctx)
		old.deliverOutbox(ctx)
		if evs := oldRec.all(); len(evs) != 0 {
			t.Fatalf("a background pass over the unknown baseline: got %+v, want nothing", evs)
		}

		updated, rec := upgradeServer(t, st)
		updated.reevaluateAll(ctx)
		updated.deliverOutbox(ctx)
		evs := rec.all()
		if len(evs) != 1 || evs[0].Kind != notify.KindBecameReady || evs[0].Target != "1.37" {
			t.Fatalf("KB update decides 1.37 ready: got %+v, want one became-ready", evs)
		}
		updated.reevaluateAll(ctx)
		updated.deliverOutbox(ctx)
		if evs := rec.all(); len(evs) != 1 {
			t.Fatalf("became-ready sent again: %+v", evs[1:])
		}
	})
}

// TestUnknownBaselineCarriesWhatItCouldNotSee: an unknown pass past the
// horizon that did not assess an optional capability (helm) is still a
// baseline, and carries forward what it could not see (#189): an add-on
// found through its chart alone, gone while helm fails, is neither
// resolved by it nor news when helm returns.
func TestUnknownBaselineCarriesWhatItCouldNotSee(t *testing.T) {
	forEachStore(t, func(t *testing.T, st store.Store) {
		srv, rec := upgradeServer(t, st)
		ts := httptest.NewServer(srv.Handler())
		defer ts.Close()
		ctx := context.Background()
		chartNginx := ingressNginx
		chartNginx.Source = "chart"

		pushAndDeliver(t, srv, ts.URL, rec, newestMinor())
		if evs := pushAndDeliver(t, srv, ts.URL, rec, newestMinor(chartNginx)); !announces(evs, "ingress nginx", "1.38") {
			t.Fatalf("EOL ingress-nginx: got %+v, want one new-blocker", evs)
		}
		helmDown := newestMinor()
		helmDown.Capabilities[inventory.CapHelm] = inventory.CapabilityStatus{Available: false, Reason: "forbidden"}
		if evs := pushAndDeliver(t, srv, ts.URL, rec, helmDown); len(evs) != 0 {
			t.Fatalf("helm outage: got %+v, want nothing", evs)
		}
		if cur, err := st.CurrentEvaluation(ctx, 1, "1.38"); err != nil || cur.Blockers != 0 || !cur.Baseline {
			t.Fatalf("1.38 during the helm outage = (%+v, %v); want unknown, and a baseline", cur, err)
		}
		if evs := pushAndDeliver(t, srv, ts.URL, rec, newestMinor(chartNginx)); len(evs) != 0 {
			t.Fatalf("helm back, ingress-nginx unchanged: got %+v, want nothing", evs)
		}
	})
}
