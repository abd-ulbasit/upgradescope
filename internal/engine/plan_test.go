package engine

import (
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"os"
	"slices"
	"testing"
	"time"

	"github.com/abd-ulbasit/upgradescope/internal/inventory"
	"github.com/abd-ulbasit/upgradescope/internal/kb"
)

func mustMinor(s string) inventory.Version {
	ver, err := inventory.ParseVersion(s)
	if err != nil {
		panic(err)
	}
	return ver
}

func versions(vs []inventory.Version) []string {
	out := make([]string, len(vs))
	for i, ver := range vs {
		out[i] = ver.String()
	}
	return out
}

// Without upgrade steps in the KB, the control plane moves one minor per
// hop; a target that is not an upgrade has no hops.
func TestHopTargetsDefault(t *testing.T) {
	for _, tc := range []struct {
		from, to string
		want     []string
	}{
		{"1.31", "1.36", []string{"1.32", "1.33", "1.34", "1.35", "1.36"}},
		{"1.31", "1.32", []string{"1.32"}},
		{"1.31", "1.31", []string{}},
		{"1.33", "1.31", []string{}},
	} {
		if got := versions(HopTargets(kb.KB{}, mustMinor(tc.from), mustMinor(tc.to))); !slices.Equal(got, tc.want) {
			t.Errorf("HopTargets(%s → %s) = %v, want %v", tc.from, tc.to, got, tc.want)
		}
	}
}

// The path is data: a KB upgrade step that skips minors replaces the
// one-minor hops it spans. The steps here are made up for the test, not
// any provider's real path.
func TestHopTargetsCustomPath(t *testing.T) {
	k := kb.KB{UpgradeSteps: []kb.UpgradeStep{
		{From: mustMinor("1.31"), To: mustMinor("1.33"), Citation: "https://example.com/test-only"},
		{From: mustMinor("1.31"), To: mustMinor("1.34"), Citation: "https://example.com/test-only"},
		{From: mustMinor("1.34"), To: mustMinor("1.36"), Citation: "https://example.com/test-only"},
	}}
	for _, tc := range []struct {
		from, to string
		want     []string
	}{
		// The farthest step that does not pass the target, then the next.
		{"1.31", "1.37", []string{"1.34", "1.36", "1.37"}},
		{"1.31", "1.36", []string{"1.34", "1.36"}},
		// A step past the target is not taken.
		{"1.31", "1.33", []string{"1.33"}},
		{"1.31", "1.32", []string{"1.32"}},
		// Steps start only at their From.
		{"1.32", "1.35", []string{"1.33", "1.34", "1.35"}},
	} {
		if got := versions(HopTargets(k, mustMinor(tc.from), mustMinor(tc.to))); !slices.Equal(got, tc.want) {
			t.Errorf("HopTargets(%s → %s) = %v, want %v", tc.from, tc.to, got, tc.want)
		}
	}
}

// A finding first seen as a warning at hop 2 that becomes a blocker at hop
// 4 is listed in full once, at hop 2, carried by reference at hop 3, and
// shown as a warning → blocker change at hop 4. A finding that goes away
// is no longer carried.
func TestPlanSeverityTransitions(t *testing.T) {
	x := func(sev Severity, title string) Finding {
		return Finding{Category: CatVersionSkew, Severity: sev, Key: "version-skew/x", Title: title}
	}
	gone := Finding{Category: CatDeprecatedAPI, Severity: SevInfo, Key: "deprecated-api/y", Title: "y deprecated"}
	reports := []Report{
		{Target: mustMinor("1.32"), Verdict: VerdictReady, Score: 100, Findings: []Finding{gone}},
		{Target: mustMinor("1.33"), Verdict: VerdictReady, Score: 95, Findings: []Finding{x(SevWarning, "x at 1.33")}},
		{Target: mustMinor("1.34"), Verdict: VerdictReady, Score: 95, Findings: []Finding{x(SevWarning, "x at 1.34")}},
		{Target: mustMinor("1.35"), Verdict: VerdictBlocked, Score: 75, Findings: []Finding{x(SevBlocker, "x at 1.35")}},
	}
	hops := PlanReports(mustMinor("1.31"), reports)
	if len(hops) != 4 {
		t.Fatalf("got %d hops, want 4", len(hops))
	}
	for i, want := range []struct{ from, to string }{{"1.31", "1.32"}, {"1.32", "1.33"}, {"1.33", "1.34"}, {"1.34", "1.35"}} {
		if hops[i].From.String() != want.from || hops[i].To.String() != want.to {
			t.Errorf("hop %d is %s → %s, want %s → %s", i+1, hops[i].From, hops[i].To, want.from, want.to)
		}
	}
	if len(hops[0].Findings) != 1 || hops[0].Findings[0].Key != gone.Key {
		t.Errorf("hop 1 findings = %+v, want only %s", hops[0].Findings, gone.Key)
	}
	if len(hops[1].Findings) != 1 || hops[1].Findings[0].Severity != SevWarning || len(hops[1].Carried) != 0 {
		t.Errorf("hop 2 = %+v, want x listed in full as a warning and nothing carried (y went away)", hops[1])
	}
	wantCarried := []FindingRef{{Key: "version-skew/x", Severity: SevWarning, Title: "x at 1.34", Since: mustMinor("1.33")}}
	if len(hops[2].Findings) != 0 || len(hops[2].Changed) != 0 || !slices.Equal(hops[2].Carried, wantCarried) {
		t.Errorf("hop 3 = %+v, want x carried by reference only", hops[2])
	}
	wantChanged := []FindingRef{{Key: "version-skew/x", Severity: SevBlocker, Was: SevWarning, Title: "x at 1.35", Since: mustMinor("1.33")}}
	if len(hops[3].Findings) != 0 || len(hops[3].Carried) != 0 || !slices.Equal(hops[3].Changed, wantChanged) {
		t.Errorf("hop 4 = %+v, want x changed warning → blocker", hops[3])
	}
	if hops[3].Count(SevBlocker) != 1 || hops[3].Count(SevWarning) != 0 || hops[3].Verdict != VerdictBlocked || hops[3].Score != 75 {
		t.Errorf("hop 4 counts/verdict/score = %d/%d/%s/%d, want 1 blocker, 0 warnings, blocked, 75",
			hops[3].Count(SevBlocker), hops[3].Count(SevWarning), hops[3].Verdict, hops[3].Score)
	}
}

// The upgrade-path info Evaluate adds for a multi-minor target restates
// the plan itself, so no hop lists it.
func TestPlanOmitsUpgradePathInfo(t *testing.T) {
	inv := inventory.Inventory{ServerVersion: "v1.31.4", Source: inventory.SourceCluster}
	k := kb.KB{MaxKnownK8s: mustMinor("1.36")}
	if r := Evaluate(inv, k, mustMinor("1.34"), time.Time{}); !slices.ContainsFunc(r.Findings, func(f Finding) bool { return f.Key == upgradePathKey }) {
		t.Fatalf("Evaluate no longer reports %s; this test no longer covers it", upgradePathKey)
	}
	for _, h := range Plan(inv, k, mustMinor("1.31"), mustMinor("1.34"), time.Time{}) {
		for _, f := range h.Findings {
			if f.Key == upgradePathKey {
				t.Errorf("hop %s lists %s", h.To, upgradePathKey)
			}
		}
		for _, r := range append(slices.Clone(h.Changed), h.Carried...) {
			if r.Key == upgradePathKey {
				t.Errorf("hop %s refers to %s", h.To, upgradePathKey)
			}
		}
	}
}

// The 1.31 → 1.36 golden: five hops, each finding listed in full at its
// first hop only. Against testdata/kb.json at 2026-06-10: the batch/v1beta1
// CronJob, ComponentStatus and the EOL ingress-nginx come up at 1.32; the
// DRA v1alpha3 DeviceClass is a deprecated-api info at 1.32, a removed-api
// warning at 1.33 and a blocker from 1.34; ExternalDNS 0.14.2 (k8s ≤ 1.32)
// blocks from 1.33, ingress-nginx (≤ 1.33) from 1.34; the 1.31 kubelets
// fall out of skew at 1.35.
func TestPlanGolden(t *testing.T) {
	const name = "plan-1.31-to-1.36"
	var k kb.KB
	readJSON(t, "testdata/kb.json", &k)
	var inv inventory.Inventory
	readJSON(t, "testdata/"+name+".inventory.json", &inv)
	now := time.Date(2026, 6, 10, 0, 0, 0, 0, time.UTC)

	hops := Plan(inv, k, mustMinor("1.31"), mustMinor("1.36"), now)
	if len(hops) != 5 {
		t.Fatalf("got %d hops, want 5", len(hops))
	}
	checkPlanInvariants(t, inv, k, mustMinor("1.36"), now, hops)

	gotRaw, err := json.Marshal(hops)
	if err != nil {
		t.Fatal(err)
	}
	got := canonical(t, gotRaw)
	golden := "testdata/" + name + ".expected.json"
	if *update {
		if err := os.WriteFile(golden, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	wantRaw, err := os.ReadFile(golden)
	if err != nil {
		t.Fatal(err)
	}
	if want := canonical(t, wantRaw); got != want {
		t.Errorf("plan mismatch\n got:\n%s\nwant:\n%s", got, want)
	}
}

// Property: for random inventories and targets, against both the test KB
// and the embedded one, the final hop's blocker set and verdict are those
// of Evaluate at the target, and every finding is listed in full once.
func TestPlanFinalHopMatchesEvaluate(t *testing.T) {
	var testKB kb.KB
	readJSON(t, "testdata/kb.json", &testKB)
	embedded, err := kb.Load()
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 2, 0, 0, 0, 0, time.UTC)
	rng := rand.New(rand.NewPCG(78, 1))
	for i := range 400 {
		k := testKB
		if i%2 == 1 {
			k = embedded
		}
		inv, from := randomInventory(rng, k)
		to := inventory.Version{Major: 1, Minor: from.Minor + 1 + rng.IntN(6)}
		t.Run(fmt.Sprintf("%d/%s-to-%s", i, from, to), func(t *testing.T) {
			hops := Plan(inv, k, from, to, now)
			if len(hops) != to.Minor-from.Minor {
				t.Fatalf("got %d hops for %s → %s", len(hops), from, to)
			}
			checkPlanInvariants(t, inv, k, to, now, hops)
		})
	}
}

// checkPlanInvariants: hops chain from one to the next and end at to;
// each key is listed in full at most once, and never also by reference at
// the same hop; the final hop's blockers and verdict are Evaluate's at to.
func checkPlanInvariants(t *testing.T, inv inventory.Inventory, k kb.KB, to inventory.Version, now time.Time, hops []Hop) {
	t.Helper()
	listed := map[string]inventory.Version{}
	for i, h := range hops {
		if i > 0 && h.From != hops[i-1].To {
			t.Errorf("hop %d starts at %s, not at %s", i+1, h.From, hops[i-1].To)
		}
		for _, f := range h.Findings {
			if at, dup := listed[findingKey(f)]; dup {
				t.Errorf("%s listed in full at %s and again at %s", findingKey(f), at, h.To)
			}
			listed[findingKey(f)] = h.To
		}
		for _, ref := range slices.Concat(h.Changed, h.Carried) {
			if at, ok := listed[ref.Key]; !ok || at != ref.Since || at == h.To {
				t.Errorf("hop %s references %s since %s, but it was listed at %s", h.To, ref.Key, ref.Since, at)
			}
		}
	}
	last := hops[len(hops)-1]
	if last.To != to {
		t.Fatalf("last hop ends at %s, want %s", last.To, to)
	}
	want := Evaluate(inv, k, to, now)
	var wantBlockers []string
	for _, f := range want.Findings {
		if f.Severity == SevBlocker {
			wantBlockers = append(wantBlockers, findingKey(f))
		}
	}
	var gotBlockers []string
	for key, sev := range last.severities() {
		if sev == SevBlocker {
			gotBlockers = append(gotBlockers, key)
		}
	}
	slices.Sort(wantBlockers)
	slices.Sort(gotBlockers)
	if !slices.Equal(gotBlockers, wantBlockers) {
		t.Errorf("final hop blockers %v, Evaluate(%s) blockers %v", gotBlockers, to, wantBlockers)
	}
	if last.Verdict != want.Verdict || last.Score != want.Score {
		t.Errorf("final hop verdict/score %s/%d, Evaluate(%s) %s/%d", last.Verdict, last.Score, to, want.Verdict, want.Score)
	}
}

// randomInventory is a live cluster at a random minor using random KB
// APIs and add-ons, with kubelets and control-plane components spread
// around the server version, and now and then a capability that failed.
func randomInventory(rng *rand.Rand, k kb.KB) (inventory.Inventory, inventory.Version) {
	server := inventory.Version{Major: 1, Minor: 20 + rng.IntN(17)}
	inv := inventory.Inventory{
		ClusterID: "random", Source: inventory.SourceCluster, ServerVersion: "v" + server.String() + ".3",
		Capabilities: map[inventory.Capability]inventory.CapabilityStatus{
			inventory.CapAPIUsage: {Available: true}, inventory.CapVersions: {Available: true},
			inventory.CapAddOns: {Available: true}, inventory.CapHelm: {Available: true},
			inventory.CapDeprecatedCalls: {Available: true},
		},
		Namespaces: []inventory.NamespaceInfo{{Name: "a", Team: "ta"}, {Name: "b"}},
	}
	if rng.IntN(8) == 0 {
		inv.Capabilities[inventory.CapAddOns] = inventory.CapabilityStatus{Reason: "forbidden"}
	}
	used := map[kb.GVK]bool{} // collectors report each API once
	for range rng.IntN(7) {
		e := k.APILifecycle[rng.IntN(len(k.APILifecycle))]
		gvk := kb.GVK{Group: e.Group, Version: e.Version, Kind: e.Kind}
		if used[gvk] {
			continue
		}
		used[gvk] = true
		ns := []string{"a", "b", ""}[rng.IntN(3)]
		inv.APIUsage = append(inv.APIUsage, inventory.APIUsage{
			Group: e.Group, Version: e.Version, Kind: e.Kind, Count: 1, Namespaces: map[string]int{ns: 1},
			Objects: []inventory.ObjectRef{{Namespace: ns, Name: fmt.Sprintf("o%d", rng.IntN(100))}},
		})
	}
	for range rng.IntN(4) {
		kv := inventory.Version{Major: 1, Minor: server.Minor - rng.IntN(6)}
		inv.Nodes = append(inv.Nodes, inventory.NodeInfo{Name: fmt.Sprintf("n%d", len(inv.Nodes)), KubeletVersion: "v" + kv.String() + ".0"})
	}
	if rng.IntN(3) == 0 {
		for _, c := range []string{"kube-apiserver", "kube-scheduler", "kube-proxy"} {
			cv := inventory.Version{Major: 1, Minor: server.Minor - rng.IntN(4)}
			if c == "kube-apiserver" {
				cv = server
			}
			inv.ControlPlane = append(inv.ControlPlane, inventory.ComponentVersion{Component: c, Version: "v" + cv.String() + ".1"})
		}
	}
	for range rng.IntN(3) {
		a := k.AddOns[rng.IntN(len(k.AddOns))]
		ver := []string{"1.0.0", "4.7.1", "0.14.2", "1.27.3", "1.31.1", "2.0.5"}[rng.IntN(6)]
		inv.AddOns = append(inv.AddOns, inventory.AddOnInstance{ID: a.ID, Version: ver, Namespaces: []string{"a"}, Source: "image"})
	}
	return inv, server
}

func readJSON(t *testing.T, path string, v any) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, v); err != nil {
		t.Fatal(err)
	}
}
