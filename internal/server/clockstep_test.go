package server

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/abd-ulbasit/upgradescope/internal/server/notify"
)

// TestClockStepDoesNotReplayNotifications: after the server's clock steps
// backwards, rows written before the step are future-dated. Current state
// and notification baselines follow insertion order, so later pushes
// alert exactly once each; a future-dated evaluation counts as stale, so
// the next pass re-confirms it at the corrected time instead of serving
// it as fresh until the clock catches up.
func TestClockStepDoesNotReplayNotifications(t *testing.T) {
	h := newHarness(t, Config{KB: testKB()}, aug1)
	if code, out := h.push("prod", testInventoryWithPSP()); code != http.StatusAccepted {
		t.Fatalf("blocked push = %d %v", code, out)
	}
	h.clock.set(aug1.Add(time.Hour))
	if code, _ := h.push("prod", testInventory()); code != http.StatusAccepted {
		t.Fatalf("clean push = %d", code)
	}
	if evs := h.drain(); len(evs) != 1 || evs[0].Kind != notify.KindBecameReady {
		t.Fatalf("before the step: events %+v, want one became-ready", evs)
	}

	stepped := aug1.Add(-48 * time.Hour)
	h.clock.set(stepped)
	h.tick()
	if evs := h.drain(); len(evs) != 0 {
		t.Errorf("a pass after the clock step notified %+v, want nothing (the result is unchanged)", evs)
	}
	cur, err := h.st.CurrentEvaluation(context.Background(), h.clusterID("prod"), "1.35")
	if err != nil {
		t.Fatal(err)
	}
	if !cur.EvaluatedAt.Equal(stepped) {
		t.Errorf("current evaluation evaluatedAt = %v, want re-confirmed at %v (it was future-dated)", cur.EvaluatedAt, stepped)
	}

	h.clock.set(stepped.Add(time.Hour))
	if code, _ := h.push("prod", testInventoryWithPSP()); code != http.StatusAccepted {
		t.Fatalf("blocked again = %d", code)
	}
	if evs := h.drain(); len(evs) != 1 || evs[0].Kind != notify.KindNewBlocker {
		t.Errorf("blocked again: events %+v, want one new-blocker", evs)
	}
	h.clock.set(stepped.Add(2 * time.Hour))
	if code, _ := h.push("prod", testInventory()); code != http.StatusAccepted {
		t.Fatalf("clean again = %d", code)
	}
	if evs := h.drain(); len(evs) != 1 || evs[0].Kind != notify.KindBecameReady {
		t.Errorf("clean again: events %+v, want one became-ready (the baseline is the newest row, not the future-dated one)", evs)
	}
	if c := h.fleetCell("prod", "1.35"); c == nil || !c.Ready {
		t.Errorf("fleet cell = %+v, want the current (ready) evaluation", c)
	}
}
