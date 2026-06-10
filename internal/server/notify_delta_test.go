package server

import (
	"fmt"
	"reflect"
	"testing"

	"github.com/abd-ulbasit/upgradescope/internal/engine"
	"github.com/abd-ulbasit/upgradescope/internal/inventory"
	"github.com/abd-ulbasit/upgradescope/internal/server/notify"
)

// rep builds a minimal report for delta tests. ClusterID/Target feed the
// Event fields; Score feeds the became-ready detail line.
func rep(t *testing.T, score int, findings ...engine.Finding) engine.Report {
	t.Helper()
	target, err := inventory.ParseVersion("1.36")
	if err != nil {
		t.Fatal(err)
	}
	return engine.Report{ClusterID: "uid-1", Target: target, Score: score, Findings: findings}
}

func blocker(title string) engine.Finding {
	return engine.Finding{Category: engine.CatRemovedAPI, Severity: engine.SevBlocker, Title: title, Detail: "detail: " + title}
}

func eolWarn(title string) engine.Finding {
	return engine.Finding{Category: engine.CatEOLApproaching, Severity: engine.SevWarning, Title: title, Detail: "detail: " + title}
}

func TestComputeDelta(t *testing.T) {
	prevOneBlocker := rep(t, 40, blocker("psp removed"))

	manyNew := make([]engine.Finding, 7)
	for i := range manyNew {
		manyNew[i] = blocker(fmt.Sprintf("blocker-%d", i))
	}

	cases := []struct {
		name string
		prev *engine.Report
		curr engine.Report
		want []notify.Event
	}{
		{
			name: "first evaluation: nil prev emits nothing even with blockers",
			prev: nil,
			curr: rep(t, 40, blocker("psp removed")),
			want: nil,
		},
		{
			name: "no change emits nothing",
			prev: &prevOneBlocker,
			curr: rep(t, 40, blocker("psp removed")),
			want: nil,
		},
		{
			name: "new blocker emits new-blocker with title and detail",
			prev: &prevOneBlocker,
			curr: rep(t, 25, blocker("psp removed"), blocker("flowcontrol v1beta3 removed")),
			want: []notify.Event{{
				Cluster: "uid-1", Target: "1.36", Kind: notify.KindNewBlocker,
				Title: "flowcontrol v1beta3 removed", Detail: "detail: flowcontrol v1beta3 removed",
			}},
		},
		{
			name: "blocker resolved but others remain: no events",
			prev: func() *engine.Report {
				r := rep(t, 25, blocker("a"), blocker("b"))
				return &r
			}(),
			curr: rep(t, 40, blocker("a")),
			want: nil,
		},
		{
			name: "all blockers resolved emits became-ready",
			prev: &prevOneBlocker,
			curr: rep(t, 92),
			want: []notify.Event{{
				Cluster: "uid-1", Target: "1.36", Kind: notify.KindBecameReady,
				Title: "ready for 1.36: all blockers resolved", Detail: "score 92",
			}},
		},
		{
			name: "new eol-approaching warning emits eol-approaching",
			prev: func() *engine.Report {
				r := rep(t, 80, eolWarn("ingress-nginx EOL 2026-03"))
				return &r
			}(),
			curr: rep(t, 70, eolWarn("ingress-nginx EOL 2026-03"), eolWarn("chart foo EOL 2026-09")),
			want: []notify.Event{{
				Cluster: "uid-1", Target: "1.36", Kind: notify.KindEOLApproaching,
				Title: "chart foo EOL 2026-09", Detail: "detail: chart foo EOL 2026-09",
			}},
		},
		{
			name: "non-blocker severities never produce new-blocker events",
			prev: func() *engine.Report {
				r := rep(t, 90)
				return &r
			}(),
			curr: rep(t, 85, engine.Finding{Category: engine.CatVersionSkew, Severity: engine.SevWarning, Title: "skew"}),
			want: nil,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ComputeDelta(tc.prev, tc.curr)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("ComputeDelta:\n got  %+v\n want %+v", got, tc.want)
			}
		})
	}

	t.Run("seven new blockers capped at 5 plus and-N-more", func(t *testing.T) {
		prev := rep(t, 90)
		got := ComputeDelta(&prev, rep(t, 10, manyNew...))
		if len(got) != 6 {
			t.Fatalf("want 5 events + 1 summary = 6, got %d: %+v", len(got), got)
		}
		for i := 0; i < 5; i++ {
			if got[i].Kind != notify.KindNewBlocker || got[i].Title != fmt.Sprintf("blocker-%d", i) {
				t.Errorf("event %d = %+v, want new-blocker blocker-%d", i, got[i], i)
			}
		}
		last := got[5]
		if last.Kind != notify.KindNewBlocker || last.Title != "and 2 more new blockers" {
			t.Errorf("summary event = %+v, want \"and 2 more new blockers\"", last)
		}
	})
}
