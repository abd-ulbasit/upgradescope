package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync"
	"testing"

	"github.com/abd-ulbasit/upgradescope/internal/engine"
	"github.com/abd-ulbasit/upgradescope/internal/inventory"
	"github.com/abd-ulbasit/upgradescope/internal/kb"
	"github.com/abd-ulbasit/upgradescope/internal/server/notify"
	"github.com/abd-ulbasit/upgradescope/internal/server/store"
)

// TestHidesNoFindingHidesNothing: the gaps an unknown baseline may have
// (hidesNoFinding) leave no finding of any category unassessed
// (engine.HiddenBy), helm-release keys included, unavailable or partial.
// A capability added to hidesNoFinding that some finding comes from fails
// here.
func TestHidesNoFindingHidesNothing(t *testing.T) {
	cats := []engine.Category{engine.CatRemovedAPI, engine.CatDeprecatedAPI, engine.CatDeprecatedAPIInUse,
		engine.CatEOLAddon, engine.CatEOLApproaching, engine.CatVersionSkew, engine.CatChartIncompat,
		engine.CatKBStale, engine.CatAddOnNoData, engine.CatUnknownAPI, engine.CatCRDVersion,
		engine.CatSupportLifecycle, engine.CatVolumePlugin}
	for c := range hidesNoFinding {
		gaps := []engine.CapabilityGap{{Capability: c, Required: true}, {Capability: c, Required: true, Partial: true, Skipped: []string{"x"}}}
		for _, cat := range cats {
			for _, key := range []string{string(cat) + "/x", string(cat) + "/helm-release/ns/name"} {
				if hidden := engine.HiddenBy(gaps, cat, key); len(hidden) != 0 {
					t.Errorf("gap %s hides %s finding %q: a pass unknown for it alone did not see everything", c, cat, key)
				}
			}
		}
	}
}

// TestNotificationBaseline: decided verdicts are baselines; an unknown one
// is when every required gap hides no finding, optional gaps aside, and
// is not behind a required collector gap.
func TestNotificationBaseline(t *testing.T) {
	kbCoverage := engine.CapabilityGap{Capability: engine.GapKBCoverage, Required: true}
	for _, tc := range []struct {
		name string
		rep  engine.Report
		want bool
	}{
		{"ready", engine.Report{Verdict: engine.VerdictReady}, true},
		{"blocked behind a collector gap", engine.Report{Verdict: engine.VerdictBlocked,
			NotAssessed: []engine.CapabilityGap{{Capability: inventory.CapAPIUsage, Required: true}}}, true},
		{"unknown past the horizon", engine.Report{Verdict: engine.VerdictUnknown, NotAssessed: []engine.CapabilityGap{kbCoverage}}, true},
		{"unknown, not an upgrade", engine.Report{Verdict: engine.VerdictUnknown,
			NotAssessed: []engine.CapabilityGap{{Capability: engine.GapTarget, Required: true}}}, true},
		{"unknown past the horizon, helm optional", engine.Report{Verdict: engine.VerdictUnknown,
			NotAssessed: []engine.CapabilityGap{{Capability: inventory.CapHelm}, kbCoverage}}, true},
		{"unknown past the horizon, add-ons failed", engine.Report{Verdict: engine.VerdictUnknown,
			NotAssessed: []engine.CapabilityGap{{Capability: inventory.CapAddOns, Required: true}, kbCoverage}}, false},
		{"unknown, partial api-usage required", engine.Report{Verdict: engine.VerdictUnknown,
			NotAssessed: []engine.CapabilityGap{{Capability: inventory.CapAPIUsage, Required: true, Partial: true, Skipped: []string{"policy/v1beta1 PodSecurityPolicy"}}}}, false},
	} {
		if got := notificationBaseline(tc.rep); got != tc.want {
			t.Errorf("%s: notificationBaseline = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestUnknownPassKeepsBecameReadyPending pins computeDelta's rule for an
// unknown baseline pass: blockers it saw resolved are carried Resolved,
// are no blockers of the next pass's baseline (a return is new), and make
// the first ready pass a became-ready; a decided pass drops them.
func TestUnknownPassKeepsBecameReadyPending(t *testing.T) {
	nginx := keyedBlocker("eol-addon/ingress-nginx", "ingress-nginx is end-of-life")
	blocked := []findingHead{headOf(nginx)}
	unknown := engine.Report{Verdict: engine.VerdictUnknown, Target: inventory.Version{Major: 1, Minor: 38}}

	changes, carried := computeDelta(blocked, unknown, func(findingHead) bool { return false })
	if len(changes) != 0 {
		t.Fatalf("unknown pass resolving the blocker: changes %+v, want none", changes)
	}
	if len(carried) != 1 || !carried[0].Resolved || carried[0].blocker() || carried[0].key() != "eol-addon/ingress-nginx" {
		t.Fatalf("carried = %+v, want ingress-nginx, Resolved", carried)
	}
	// Another unknown pass keeps it, once.
	if _, again := computeDelta(carried, unknown, nil); !sameHeads(again, carried) || len(again) != 1 {
		t.Fatalf("second unknown pass carried %+v, want %+v", again, carried)
	}

	ready := engine.Report{Verdict: engine.VerdictReady, Target: unknown.Target}
	changes, kept := computeDelta(carried, ready, nil)
	if len(changes) != 1 || changes[0].Kind != notify.KindBecameReady || len(kept) != 0 {
		t.Fatalf("ready after it: changes %+v carried %+v, want one became-ready and nothing carried", changes, kept)
	}

	back := engine.Report{Verdict: engine.VerdictBlocked, Target: unknown.Target, Findings: []engine.Finding{nginx}}
	changes, kept = computeDelta(carried, back, nil)
	if len(changes) != 1 || changes[0].Kind != notify.KindNewBlocker || len(kept) != 0 {
		t.Fatalf("blocker back after it: changes %+v carried %+v, want one new-blocker and nothing carried", changes, kept)
	}

	if sameHeads(carried, blocked) {
		t.Error("sameHeads takes a Resolved head for the blocker it was")
	}
}

// TestNewestMinorWebhookReceiverGetsOneNewBlocker is #380's lab case end
// to end, through the real ingest endpoint, outbox and generic webhook: a
// cluster on the newest minor the KB knows (default target past its
// horizon) pushes a clean inventory, then one with an EOL add-on, and the
// receiver logs exactly one notification, one new-blocker for it.
func TestNewestMinorWebhookReceiverGetsOneNewBlocker(t *testing.T) {
	forEachStore(t, func(t *testing.T, st store.Store) {
		var mu sync.Mutex
		var got []notify.Notification
		receiver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var n notify.Notification
			if err := json.NewDecoder(r.Body).Decode(&n); err != nil {
				t.Errorf("receiver: %v", err)
			}
			mu.Lock()
			got = append(got, n)
			mu.Unlock()
		}))
		defer receiver.Close()
		k, err := kb.Load()
		if err != nil {
			t.Fatal(err)
		}
		srv, err := New(Config{Store: st, KB: k, Notifier: notify.NewGenericWebhook(receiver.URL), IngestToken: "tok"})
		if err != nil {
			t.Fatal(err)
		}
		ts := httptest.NewServer(srv.Handler())
		defer ts.Close()
		ctx := context.Background()
		for _, inv := range []inventory.Inventory{newestMinor(), newestMinor(ingressNginx)} {
			pushInventory(t, ts.URL, "tok", inv)
			srv.deliverOutbox(ctx)
		}

		mu.Lock()
		defer mu.Unlock()
		if len(got) != 1 || len(got[0].Changes) != 1 {
			t.Fatalf("receiver logged %+v, want one notification with one change", got)
		}
		c := got[0].Changes[0]
		if c.Kind != notify.KindNewBlocker || c.Key != "eol-addon/ingress-nginx" || !slices.Equal(c.Targets, []string{"1.38"}) {
			t.Fatalf("change = %+v, want new-blocker eol-addon/ingress-nginx for 1.38", c)
		}
	})
}
