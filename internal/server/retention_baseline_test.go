package server

import (
	"context"
	"errors"
	"net/http/httptest"
	"slices"
	"testing"
	"time"

	"github.com/abd-ulbasit/upgradescope/internal/inventory"
	"github.com/abd-ulbasit/upgradescope/internal/kb"
	"github.com/abd-ulbasit/upgradescope/internal/server/notify"
	"github.com/abd-ulbasit/upgradescope/internal/server/store"
)

// widgetKB is testKB plus a second API removed in 1.35, so a later push
// can add a blocker the baseline does not have.
func widgetKB() kb.KB {
	k := testKB()
	removed := inventory.Version{Major: 1, Minor: 35}
	k.APILifecycle = append(k.APILifecycle, kb.APILifecycleEntry{
		Group: "example.dev", Version: "v1alpha1", Kind: "Widget",
		Introduced: inventory.Version{Major: 1, Minor: 10}, Removed: &removed,
	})
	return k
}

func withWidget(inv inventory.Inventory) inventory.Inventory {
	inv.APIUsage = append(append([]inventory.APIUsage(nil), inv.APIUsage...), inventory.APIUsage{
		Group: "example.dev", Version: "v1alpha1", Kind: "Widget", Count: 1, Namespaces: map[string]int{"apps": 1},
	})
	return inv
}

// TestRetentionKeepsTheNotificationBaseline (#241): a cluster blocked by
// PSP, then unknown (api-usage unavailable) for longer than --retention,
// then decided again. Pruning used to delete the last decided evaluation,
// so the next decided pass had no baseline and announced nothing: neither
// the new blocker nor, in the other ending, became-ready.
func TestRetentionKeepsTheNotificationBaseline(t *testing.T) {
	for _, c := range []struct {
		name string
		last inventory.Inventory
		want string
	}{
		{"new blocker", withWidget(testInventoryWithPSP()), notify.KindNewBlocker},
		{"became ready", testInventory(), notify.KindBecameReady},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := newHarness(t, Config{KB: widgetKB(), Retention: 90 * 24 * time.Hour}, aug1)
			h.push("prod", testInventoryWithPSP()) // blocked: the baseline
			h.clock.set(aug1.Add(24 * time.Hour))
			unknown := testInventory()
			unknown.Capabilities = collectedCaps()
			unknown.Capabilities[inventory.CapAPIUsage] = inventory.CapabilityStatus{Reason: "forbidden"}
			h.push("prod", unknown) // unknown: no baseline, and a new snapshot
			if evs := h.drain(); len(evs) != 0 {
				t.Fatalf("events before the stretch = %+v, want none", evs)
			}
			h.clock.set(aug1.Add(101 * 24 * time.Hour))
			h.srv.pruneOnce(context.Background())
			h.push("prod", c.last)
			evs := h.drain()
			if len(evs) != 1 || evs[0].Kind != c.want {
				t.Errorf("events after pruning = %+v, want one %s", evs, c.want)
			}
		})
	}
}

// TestRetentionKeepsBaselinesOfTargetsInUseOnly: retention spares a
// cluster's newest decided evaluation per target only for the targets the
// server evaluates for it (the default and --targets above its version)
// and the upgradeLookback minors below its default; the baselines of
// targets it no longer evaluates age out, so they do not pile up as the
// cluster upgrades or --targets changes.
func TestRetentionKeepsBaselinesOfTargetsInUseOnly(t *testing.T) {
	h := newHarness(t, Config{KB: testKB(), ExtraTargets: []string{"1.37", "1.45"}, Retention: 90 * 24 * time.Hour}, aug1)
	ctx := context.Background()
	baselineTargets := func(name string) []string {
		t.Helper()
		keep := h.srv.retainedBaselines(ctx)[h.clusterID(name)]
		slices.Sort(keep)
		return keep
	}

	// v1.34 pushes: default 1.35, the extras above it.
	h.push("prod", testInventoryWithPSP())
	if got, want := baselineTargets("prod"), []string{"1.32", "1.33", "1.34", "1.35", "1.37", "1.45"}; !slices.Equal(got, want) {
		t.Errorf("v1.34 keeps baselines of %v, want %v", got, want)
	}
	for _, target := range []string{"1.35", "1.37", "1.45"} {
		if e, err := h.st.LatestKnownEvaluation(ctx, h.clusterID("prod"), target); err != nil || e.Blockers == 0 {
			t.Fatalf("baseline at %s = (%+v, %v): the fixture must be decided there", target, e, err)
		}
	}

	// The cluster upgrades to v1.39: default 1.40, 1.37 is below it.
	h.clock.set(aug1.Add(24 * time.Hour))
	up := testInventoryWithPSP()
	up.ServerVersion = "v1.39.1"
	h.push("prod", up)
	if got, want := baselineTargets("prod"), []string{"1.37", "1.38", "1.39", "1.40", "1.45"}; !slices.Equal(got, want) {
		t.Errorf("v1.39 keeps baselines of %v, want %v: the 1.37 extra is below the default but within the lookback, 1.35 is out", got, want)
	}

	// Long after, 1.35's baseline (the old default) has aged out; those in
	// use have not.
	h.clock.set(aug1.Add(200 * 24 * time.Hour))
	h.srv.pruneOnce(ctx)
	id := h.clusterID("prod")
	if _, err := h.st.LatestKnownEvaluation(ctx, id, "1.35"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("baseline at 1.35 after pruning: %v, want ErrNotFound: not in use at v1.39", err)
	}
	for target, why := range map[string]string{
		"1.45": "an extra target above the version",
		// 1.37 was last evaluated on the first push's snapshot, long past
		// the window, so only the lookback keeps it: the previous default
		// an upgrade compares against.
		"1.37": "within the lookback below the default, on a snapshot past the window",
	} {
		if _, err := h.st.LatestKnownEvaluation(ctx, id, target); err != nil {
			t.Errorf("baseline at %s after pruning: %v, want kept: %s", target, err, why)
		}
	}
}

// TestRetentionKeepsEveryBaselineWhenTheVersionIsUnknown: a cluster whose
// server version cannot be told keeps every target's baseline; so does a
// pass whose heads cannot be read.
func TestRetentionKeepsEveryBaselineWhenTheVersionIsUnknown(t *testing.T) {
	st := newFakeStore()
	s, err := New(Config{Store: st, KB: testKB(), IngestToken: "ingest-tok", Retention: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()
	pushCluster(t, ts, "prod", testInventoryWithPSP())
	st.mu.Lock()
	st.snapshots[0].ServerVersion = "not-a-version"
	st.mu.Unlock()
	if keep := s.retainedBaselines(context.Background()); len(keep) != 0 {
		t.Errorf("a cluster at an unknown version is limited to %v, want left out (every baseline kept)", keep)
	}
	st.mu.Lock()
	st.errs = map[string]error{"LatestSnapshotHeads": errors.New("down")}
	st.mu.Unlock()
	if keep := s.retainedBaselines(context.Background()); keep != nil {
		t.Errorf("with the heads unreadable, baselines limited to %v, want nil (every baseline kept)", keep)
	}
}
