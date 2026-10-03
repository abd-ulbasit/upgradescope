package server

import (
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/abd-ulbasit/upgradescope/internal/engine"
	"github.com/abd-ulbasit/upgradescope/internal/inventory"
	"github.com/abd-ulbasit/upgradescope/internal/server/notify"
	"github.com/abd-ulbasit/upgradescope/internal/server/store"
)

// rep builds a minimal report for delta tests at target 1.36.
func rep(t *testing.T, score int, findings ...engine.Finding) engine.Report {
	t.Helper()
	target, err := inventory.ParseVersion("1.36")
	if err != nil {
		t.Fatal(err)
	}
	verdict := engine.VerdictReady
	for _, f := range findings {
		if f.Severity == engine.SevBlocker {
			verdict = engine.VerdictBlocked
		}
	}
	return engine.Report{ClusterID: "uid-1", Target: target, Score: score, Verdict: verdict,
		Ready: verdict == engine.VerdictReady, Findings: findings}
}

// unknownRep is a report with no blockers whose verdict is unknown: a
// required check (api-usage) could not run.
func unknownRep(t *testing.T, score int) engine.Report {
	r := rep(t, score)
	r.Verdict, r.Ready = engine.VerdictUnknown, false
	return r
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

// newBlockerChange is the change ComputeDelta emits for f at target 1.36.
func newBlockerChange(f engine.Finding) notify.Change {
	return notify.Change{Kind: notify.KindNewBlocker, Key: f.Key, Severity: "blocker", Title: f.Title, Detail: f.Detail, Targets: []string{"1.36"}}
}

func eolChange(f engine.Finding) notify.Change {
	return notify.Change{Kind: notify.KindEOLApproaching, Key: f.Key, Severity: "warning", Title: f.Title, Detail: f.Detail, Targets: []string{"1.36"}}
}

func TestComputeDelta(t *testing.T) {
	prevOneBlocker := rep(t, 40, blocker("psp removed"))
	ptr := func(r engine.Report) *engine.Report { return &r }

	cases := []struct {
		name string
		prev *engine.Report
		curr engine.Report
		want []notify.Change
	}{
		{
			name: "first evaluation: nil prev emits nothing even with blockers",
			prev: nil,
			curr: rep(t, 40, blocker("psp removed")),
		},
		{
			name: "no change emits nothing",
			prev: &prevOneBlocker,
			curr: rep(t, 40, blocker("psp removed")),
		},
		{
			name: "new blocker emits new-blocker with key, severity, title and detail",
			prev: &prevOneBlocker,
			curr: rep(t, 25, blocker("psp removed"), blocker("flowcontrol v1beta3 removed")),
			want: []notify.Change{newBlockerChange(blocker("flowcontrol v1beta3 removed"))},
		},
		{
			name: "blocker resolved but others remain: no changes",
			prev: ptr(rep(t, 25, blocker("a"), blocker("b"))),
			curr: rep(t, 40, blocker("a")),
		},
		{
			name: "all blockers resolved emits became-ready",
			prev: &prevOneBlocker,
			curr: rep(t, 92),
			want: []notify.Change{{Kind: notify.KindBecameReady, Title: becameReadyTitle, Targets: []string{"1.36"}}},
		},
		{
			name: "blockers gone but verdict unknown: not became-ready",
			prev: &prevOneBlocker,
			curr: unknownRep(t, 100),
		},
		{
			name: "new eol-approaching warning emits eol-approaching",
			prev: ptr(rep(t, 80, eolWarn("ingress-nginx EOL 2026-03"))),
			curr: rep(t, 70, eolWarn("ingress-nginx EOL 2026-03"), eolWarn("chart foo EOL 2026-09")),
			want: []notify.Change{eolChange(eolWarn("chart foo EOL 2026-09"))},
		},
		{
			// THE volatile-count bug: titles embed object counts, so a
			// 3→2 improvement changes the Title but not the Key. Diffing
			// by Key must treat it as the same blocker → no change.
			name: "count change in title with same key is not a new blocker",
			prev: ptr(rep(t, 40, keyedBlocker("removed-api/extensions/v1beta1/Ingress", "extensions/v1beta1 Ingress removed in 1.22 (3 objects)"))),
			curr: rep(t, 40, keyedBlocker("removed-api/extensions/v1beta1/Ingress", "extensions/v1beta1 Ingress removed in 1.22 (2 objects)")),
		},
		{
			name: "genuinely new key emits new-blocker even when prev had keyed blockers",
			prev: ptr(rep(t, 40, keyedBlocker("removed-api/extensions/v1beta1/Ingress", "ingress removed (3 objects)"))),
			curr: rep(t, 25,
				keyedBlocker("removed-api/extensions/v1beta1/Ingress", "ingress removed (2 objects)"),
				keyedBlocker("eol-addon/ingress-nginx", "ingress-nginx is end-of-life")),
			want: []notify.Change{newBlockerChange(keyedBlocker("eol-addon/ingress-nginx", "ingress-nginx is end-of-life"))},
		},
		{
			name: "duplicate keys in one report emit one change",
			prev: ptr(rep(t, 90)),
			curr: rep(t, 25,
				keyedBlocker("eol-addon/dup", "dup blocker A"),
				keyedBlocker("eol-addon/dup", "dup blocker B")),
			want: []notify.Change{newBlockerChange(keyedBlocker("eol-addon/dup", "dup blocker A"))},
		},
		{
			name: "eol-approaching diffs by key and dedups within one report",
			prev: ptr(rep(t, 80, keyedEOLWarn("eol-approaching/legacy-mesh", "Legacy Mesh reaches end-of-life on 2026-08-15"))),
			curr: rep(t, 70,
				keyedEOLWarn("eol-approaching/legacy-mesh", "Legacy Mesh reaches end-of-life on 2026-08-20"), // same key, new title
				keyedEOLWarn("eol-approaching/other", "Other reaches end-of-life on 2026-09-01"),
				keyedEOLWarn("eol-approaching/other", "Other reaches end-of-life on 2026-09-01 (dup)")),
			want: []notify.Change{eolChange(keyedEOLWarn("eol-approaching/other", "Other reaches end-of-life on 2026-09-01"))},
		},
		{
			name: "non-blocker severities never produce new-blocker changes",
			prev: ptr(rep(t, 90)),
			curr: rep(t, 85, engine.Finding{Category: engine.CatVersionSkew, Severity: engine.SevWarning, Title: "skew"}),
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ComputeDelta(tc.prev, tc.curr)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("ComputeDelta:\n got  %+v\n want %+v", got, tc.want)
			}
			if tc.prev == nil {
				return
			}
			// The server reads only the heads of a stored baseline's
			// findings: the same delta.
			stored, err := marshalJSON(*tc.prev)
			if err != nil {
				t.Fatal(err)
			}
			heads, err := storedFindingHeads(stored)
			if err != nil {
				t.Fatal(err)
			}
			if got := computeDelta(heads, tc.curr); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("computeDelta of the stored heads:\n got  %+v\n want %+v", got, tc.want)
			}
		})
	}
}

// delta builds a targetDelta for target with the given changes.
func delta(target string, changes ...notify.Change) targetDelta {
	for i := range changes {
		changes[i].Targets = []string{target}
	}
	return targetDelta{target: notify.Target{Target: target, Verdict: "blocked", Score: 50, Blockers: len(changes)}, changes: changes}
}

func TestBuildNotification(t *testing.T) {
	cluster := store.Cluster{ID: 3, Name: "prod"}
	now := time.Date(2026, 10, 2, 9, 0, 0, 0, time.UTC)
	nginx := notify.Change{Kind: notify.KindNewBlocker, Key: "eol-addon/ingress-nginx", Severity: "blocker", Title: "ingress-nginx is end-of-life"}
	skew := notify.Change{Kind: notify.KindNewBlocker, Key: "skew/kubelet", Severity: "blocker", Title: "kubelet too old"}
	istio := notify.Change{Kind: notify.KindEOLApproaching, Key: "eol-approaching/istio", Severity: "warning", Title: "Istio EOL soon"}

	t.Run("nothing changed", func(t *testing.T) {
		if _, ok := buildNotification(cluster, []targetDelta{delta("1.36"), delta("1.37")}, now, "id"); ok {
			t.Error("ok = true for a pass with no changes")
		}
	})

	t.Run("a change on several targets is one entry, kinds ordered", func(t *testing.T) {
		n, ok := buildNotification(cluster, []targetDelta{
			delta("1.35", istio, nginx),
			delta("1.36", nginx, skew),
			delta("1.37", nginx),
			delta("1.38"),
		}, now, "id-1")
		if !ok {
			t.Fatal("ok = false")
		}
		if n.SchemaVersion != 1 || n.DeliveryID != "id-1" || n.Type != notify.TypeReadinessChanged ||
			!n.Timestamp.Equal(now) || n.Cluster != (notify.Cluster{ID: 3, Name: "prod"}) {
			t.Errorf("envelope = %+v", n)
		}
		if len(n.Targets) != 3 || n.Targets[2].Target != "1.37" {
			t.Errorf("targets = %+v, want 1.35, 1.36, 1.37 (only targets with changes)", n.Targets)
		}
		var got []string
		for _, c := range n.Changes {
			got = append(got, fmt.Sprintf("%s %s %v", c.Kind, c.Key, c.Targets))
		}
		want := []string{
			"new-blocker eol-addon/ingress-nginx [1.35 1.36 1.37]",
			"new-blocker skew/kubelet [1.36]",
			"eol-approaching eol-approaching/istio [1.35]",
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("changes =\n %v\nwant\n %v", got, want)
		}
		if n.Omitted != nil {
			t.Errorf("omitted = %v, want none", n.Omitted)
		}
	})

	t.Run("new blockers and eol-approaching are capped per notification", func(t *testing.T) {
		var a, b []notify.Change
		for i := range 4 {
			a = append(a, notify.Change{Kind: notify.KindNewBlocker, Key: fmt.Sprintf("b-%d", i), Title: "x"})
			b = append(b, notify.Change{Kind: notify.KindNewBlocker, Key: fmt.Sprintf("b-%d", i+4), Title: "x"})
		}
		for i := range 8 {
			b = append(b, notify.Change{Kind: notify.KindEOLApproaching, Key: fmt.Sprintf("e-%d", i), Title: "x"})
		}
		n, _ := buildNotification(cluster, []targetDelta{delta("1.36", a...), delta("1.37", b...)}, now, "id")
		count := map[string]int{}
		for _, c := range n.Changes {
			count[c.Kind]++
		}
		if count[notify.KindNewBlocker] != maxBlockerChanges || count[notify.KindEOLApproaching] != maxEOLChanges {
			t.Errorf("kept %v, want %d new-blocker and %d eol-approaching", count, maxBlockerChanges, maxEOLChanges)
		}
		if !reflect.DeepEqual(n.Omitted, map[string]int{notify.KindNewBlocker: 3, notify.KindEOLApproaching: 3}) {
			t.Errorf("omitted = %v, want 3 of each", n.Omitted)
		}
		if n.Changes[0].Key != "b-0" || n.Changes[4].Key != "b-4" {
			t.Errorf("kept blockers start %s..%s, want the first five in pass order", n.Changes[0].Key, n.Changes[4].Key)
		}
	})

	t.Run("a change past the cap on several targets is omitted once", func(t *testing.T) {
		blockers := func(keys ...int) []notify.Change {
			var cs []notify.Change
			for _, k := range keys {
				cs = append(cs, notify.Change{Kind: notify.KindNewBlocker, Key: fmt.Sprintf("b-%d", k), Title: "x"})
			}
			return cs
		}
		n, _ := buildNotification(cluster, []targetDelta{
			delta("1.36", blockers(0, 1, 2, 3, 4, 5, 6)...),
			delta("1.37", blockers(6, 5, 0, 7)...),
		}, now, "id")
		if !reflect.DeepEqual(n.Omitted, map[string]int{notify.KindNewBlocker: 3}) {
			t.Errorf("omitted = %v, want b-5, b-6 and b-7 once each", n.Omitted)
		}
		if len(n.Changes) != maxBlockerChanges || !reflect.DeepEqual(n.Changes[0].Targets, []string{"1.36", "1.37"}) {
			t.Errorf("changes = %+v, want b-0..b-4, b-0 on both targets", n.Changes)
		}
	})
}
