package server

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/abd-ulbasit/upgradescope/internal/kb"
)

func TestPercentileIsNearestRank(t *testing.T) {
	var xs []time.Duration
	for i := 1; i <= 100; i++ {
		xs = append(xs, time.Duration(i)*time.Millisecond)
	}
	for _, tc := range []struct {
		p    float64
		want time.Duration
	}{{50, 50 * time.Millisecond}, {99, 99 * time.Millisecond}, {100, 100 * time.Millisecond}, {1, time.Millisecond}} {
		if got := percentile(xs, tc.p); got != tc.want {
			t.Errorf("p%v = %v, want %v", tc.p, got, tc.want)
		}
	}
	if percentile(nil, 99) != 0 {
		t.Error("percentile of nothing should be 0")
	}
	if got := percentile(xs[:3], 99); got != 3*time.Millisecond {
		t.Errorf("p99 of 3 values = %v, want the largest", got)
	}
}

func TestFleetTierMix(t *testing.T) {
	counts := map[string]int{}
	for i := range 200 {
		counts[tierOf(i).name]++
	}
	if counts["small"] != 100 || counts["medium"] != 80 || counts["large"] != 20 {
		t.Errorf("tier mix over 200 clusters = %v, want 100 small, 80 medium, 20 large", counts)
	}
}

// Every tier's inventory must be one the server accepts; a changed round is
// a new snapshot, the same round again a duplicate; and the evaluations
// carry findings, so the stored reports are not empty shells.
func TestFleetInventoriesAreAcceptedChangeTheHashAndHaveFindings(t *testing.T) {
	k, err := kb.Load()
	if err != nil {
		t.Fatal(err)
	}
	plan := newFleetPlan(k)
	if len(plan.apis) == 0 || len(plan.addOns) == 0 {
		t.Fatalf("the shipped KB offers %d removed APIs and %d add-ons to build inventories from", len(plan.apis), len(plan.addOns))
	}
	h := newHarness(t, Config{KB: k, ExtraTargets: []string{"1.36", "1.37"}}, aug1)
	for _, i := range []int{0, 5, 9} { // small, medium, large
		name := fmt.Sprintf("tier-%d", i)
		for _, step := range []struct {
			gen  int
			want int
		}{{0, http.StatusAccepted}, {1, http.StatusAccepted}, {1, http.StatusOK}} {
			if code, out := h.push(name, fleetInventory(plan, i, step.gen)); code != step.want {
				t.Fatalf("cluster %d generation %d: status %d %v, want %d", i, step.gen, code, out, step.want)
			}
		}
	}
	heads, err := h.st.ListClusters(t.Context())
	if err != nil || len(heads) != 3 {
		t.Fatalf("clusters = %d, %v; want 3", len(heads), err)
	}
	found := 0
	for _, c := range heads {
		for _, target := range []string{"1.35", "1.36", "1.37"} { // the default (next minor of 1.34) and the two --targets
			ev, err := h.st.CurrentEvaluation(t.Context(), c.ID, target)
			if err != nil {
				t.Fatalf("cluster %s has no current evaluation for %s: %v", c.Name, target, err)
			}
			found++
			if ev.Blockers+ev.Warnings == 0 {
				t.Errorf("cluster %s target %s: no findings; the inventories should carry removed APIs and old add-ons", c.Name, target)
			}
		}
	}
	if found != 3*ingestTargets {
		t.Errorf("found %d evaluations, want %d", found, 3*ingestTargets)
	}
}

// The harness measures what it claims to: pushes land, the database grows
// by about a snapshot per new push and not for duplicates.
func TestRunIngestRoundMeasuresGrowthAndDuplicates(t *testing.T) {
	k, err := kb.Load()
	if err != nil {
		t.Fatal(err)
	}
	plan := newFleetPlan(k)
	db := newBenchDB(t, "sqlite")
	defer db.close()
	srv, err := New(Config{Store: db.store, KB: k, ExtraTargets: []string{"1.36", "1.37"}, IngestToken: "ingest-tok"})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	first := runIngestRound(t, ts, db, plan, "sqlite", "new", 0, 10, 4)
	if first.Accepted != 10 || first.Failed != 0 || first.Duplicates != 0 {
		t.Fatalf("first round: %+v, want 10 accepted", first)
	}
	if first.DBBytesPerSnap <= 0 || first.P99MS <= 0 || first.PushesPerSec <= 0 || first.GzipBytesAvg >= first.BodyBytesAvg {
		t.Errorf("first round measurements not filled in: %+v", first)
	}
	changed := runIngestRound(t, ts, db, plan, "sqlite", "changed", 1, 10, 1)
	if changed.Accepted != 10 || changed.DBBytesAfter <= changed.DBBytesBefore {
		t.Errorf("changed round: %+v, want 10 new snapshots and a larger database", changed)
	}
	dup := runIngestRound(t, ts, db, plan, "sqlite", "duplicate", 1, 10, 4)
	if dup.Duplicates != 10 || dup.Accepted != 0 {
		t.Errorf("duplicate round: %+v, want 10 duplicates", dup)
	}
	if dup.DBBytesPerSnap != 0 {
		t.Errorf("duplicate round reports %.0f bytes per snapshot, want 0 (there are no new snapshots)", dup.DBBytesPerSnap)
	}
}
