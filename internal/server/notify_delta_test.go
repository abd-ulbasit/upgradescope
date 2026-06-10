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

// blocker has no Key on purpose: it exercises the Title fallback for
// reports stored before Finding.Key existed.
func blocker(title string) engine.Finding {
	return engine.Finding{Category: engine.CatRemovedAPI, Severity: engine.SevBlocker, Title: title, Detail: "detail: " + title}
}

func keyedBlocker(key, title string) engine.Finding {
	f := blocker(title)
	f.Key = key
	return f
}

func eolWarn(title string) engine.Finding {
	return engine.Finding{Category: engine.CatEOLApproaching, Severity: engine.SevWarning, Title: title, Detail: "detail: " + title}
}

func keyedEOLWarn(key, title string) engine.Finding {
	f := eolWarn(title)
	f.Key = key
	return f
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
			// THE volatile-count bug: titles embed object counts, so a
			// 3→2 improvement changes the Title but not the Key. Diffing
			// by Key must treat it as the same blocker → no event.
			name: "count change in title with same key is not a new blocker",
			prev: func() *engine.Report {
				r := rep(t, 40, keyedBlocker("removed-api/extensions/v1beta1/Ingress", "extensions/v1beta1 Ingress removed in 1.22 (3 objects)"))
				return &r
			}(),
			curr: rep(t, 40, keyedBlocker("removed-api/extensions/v1beta1/Ingress", "extensions/v1beta1 Ingress removed in 1.22 (2 objects)")),
			want: nil,
		},
		{
			name: "genuinely new key emits new-blocker even when prev had keyed blockers",
			prev: func() *engine.Report {
				r := rep(t, 40, keyedBlocker("removed-api/extensions/v1beta1/Ingress", "ingress removed (3 objects)"))
				return &r
			}(),
			curr: rep(t, 25,
				keyedBlocker("removed-api/extensions/v1beta1/Ingress", "ingress removed (2 objects)"),
				keyedBlocker("eol-addon/ingress-nginx", "ingress-nginx is end-of-life")),
			want: []notify.Event{{
				Cluster: "uid-1", Target: "1.36", Kind: notify.KindNewBlocker,
				Title: "ingress-nginx is end-of-life", Detail: "detail: ingress-nginx is end-of-life",
			}},
		},
		{
			name: "duplicate keys in one report emit one event",
			prev: func() *engine.Report {
				r := rep(t, 90)
				return &r
			}(),
			curr: rep(t, 25,
				keyedBlocker("eol-addon/dup", "dup blocker A"),
				keyedBlocker("eol-addon/dup", "dup blocker B")),
			want: []notify.Event{{
				Cluster: "uid-1", Target: "1.36", Kind: notify.KindNewBlocker,
				Title: "dup blocker A", Detail: "detail: dup blocker A",
			}},
		},
		{
			name: "eol-approaching diffs by key and dedups within one report",
			prev: func() *engine.Report {
				r := rep(t, 80, keyedEOLWarn("eol-approaching/legacy-mesh", "Legacy Mesh reaches end-of-life on 2026-08-15"))
				return &r
			}(),
			curr: rep(t, 70,
				keyedEOLWarn("eol-approaching/legacy-mesh", "Legacy Mesh reaches end-of-life on 2026-08-20"), // same key, new title
				keyedEOLWarn("eol-approaching/other", "Other reaches end-of-life on 2026-09-01"),
				keyedEOLWarn("eol-approaching/other", "Other reaches end-of-life on 2026-09-01 (dup)")),
			want: []notify.Event{{
				Cluster: "uid-1", Target: "1.36", Kind: notify.KindEOLApproaching,
				Title: "Other reaches end-of-life on 2026-09-01", Detail: "detail: Other reaches end-of-life on 2026-09-01",
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
