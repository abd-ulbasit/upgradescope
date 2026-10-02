package server

import (
	"net/http"
	"testing"
	"time"
)

// TestFleetReadAfterEOLCrossingIsFreshOrStale: Istio's end of life is
// 2026-11-30 and the cluster pushed on 08-01. Read on 12-15 with no push
// and no pass since, the fleet served ready/100 as if current: a verdict
// is only as fresh as its last evaluation, and reads never checked. Every
// read of a stored verdict now says when it is outdated (evaluated before
// today UTC, or under another KB or team map), and the next pass makes it
// fresh.
func TestFleetReadAfterEOLCrossingIsFreshOrStale(t *testing.T) {
	h := newHarness(t, Config{KB: istioKB()}, aug1)
	if code, out := h.push("prod", istioInventory()); code != http.StatusAccepted {
		t.Fatalf("push = %d %v", code, out)
	}
	var cell struct {
		Verdict  string `json:"verdict"`
		Outdated bool   `json:"outdated"`
	}
	fleetCell := func() {
		t.Helper()
		var f struct {
			Clusters []struct {
				Cells map[string]*struct {
					Verdict  string `json:"verdict"`
					Outdated bool   `json:"outdated"`
				} `json:"cells"`
			} `json:"clusters"`
		}
		if code := h.get("/api/v1/fleet?targets=1.35", &f); code != http.StatusOK || len(f.Clusters) != 1 || f.Clusters[0].Cells["1.35"] == nil {
			t.Fatalf("GET /fleet = %d %+v", code, f)
		}
		cell = *f.Clusters[0].Cells["1.35"]
	}
	type summary struct {
		Latest *struct {
			Verdict  string `json:"verdict"`
			Outdated bool   `json:"outdated"`
		} `json:"latest"`
	}
	type report struct {
		Verdict  string `json:"verdict"`
		Source   string `json:"source"`
		Outdated bool   `json:"outdated"`
	}
	id := itoa(h.clusterID("prod"))

	fleetCell()
	if cell.Verdict != "ready" || cell.Outdated {
		t.Fatalf("Aug 1 cell = %+v, want ready and current", cell)
	}

	h.clock.set(dec15)
	fleetCell()
	if cell.Verdict != "blocked" && !cell.Outdated {
		t.Errorf("Dec 15, before any pass: cell = %+v, want blocked or marked outdated", cell)
	}
	var list []summary
	if h.get("/api/v1/clusters", &list); len(list) != 1 || list[0].Latest == nil || (list[0].Latest.Verdict != "blocked" && !list[0].Latest.Outdated) {
		t.Errorf("Dec 15 cluster list = %+v, want blocked or marked outdated", list)
	}
	var rep report
	if h.get("/api/v1/clusters/"+id+"/report", &rep); rep.Verdict != "blocked" && !rep.Outdated {
		t.Errorf("Dec 15 report = %+v, want blocked or marked outdated", rep)
	}

	h.tick()
	fleetCell()
	if cell.Verdict != "blocked" || cell.Outdated {
		t.Errorf("after the pass: cell = %+v, want blocked and current", cell)
	}
	rep = report{}
	if h.get("/api/v1/clusters/"+id+"/report", &rep); rep.Verdict != "blocked" || rep.Outdated || rep.Source != "stored" {
		t.Errorf("after the pass: report = %+v, want stored, blocked and current", rep)
	}
}

// TestReevaluationWakesAtUTCMidnight: the background pass runs every
// interval and also at each UTC midnight, when EOL math moves a day, so
// a crossing is not served for up to an interval.
func TestReevaluationWakesAtUTCMidnight(t *testing.T) {
	at := func(s string) time.Time {
		v, err := time.Parse(time.RFC3339, s)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	for _, tc := range []struct {
		now  string
		want time.Duration
	}{
		{"2026-12-14T09:00:00Z", time.Hour},                         // midnight is far: the interval
		{"2026-12-14T23:30:00Z", 30*time.Minute + time.Second},      // midnight first (plus a second past it)
		{"2026-12-14T23:59:59.5Z", 1500 * time.Millisecond},         // just before
		{"2026-12-15T00:00:00Z", time.Hour},                         // at midnight: that pass is this one
		{"2026-12-14T18:30:00-05:00", 30*time.Minute + time.Second}, // UTC midnight, not local
	} {
		if got := nextPassIn(at(tc.now), time.Hour); got != tc.want {
			t.Errorf("nextPassIn(%s) = %v, want %v", tc.now, got, tc.want)
		}
	}
}
