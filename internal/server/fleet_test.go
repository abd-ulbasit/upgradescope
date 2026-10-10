package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/abd-ulbasit/upgradescope/internal/engine"
	"github.com/abd-ulbasit/upgradescope/internal/inventory"
	"github.com/abd-ulbasit/upgradescope/internal/kb"
	"github.com/abd-ulbasit/upgradescope/internal/server/store"
)

// pushCluster pushes inv under clusterName and fails the test on non-202.
func pushCluster(t *testing.T, ts *httptest.Server, clusterName string, inv inventory.Inventory) {
	t.Helper()
	invJSON, err := json.Marshal(inv)
	if err != nil {
		t.Fatalf("marshal inventory: %v", err)
	}
	body, err := json.Marshal(map[string]any{
		"schemaVersion": 1,
		"clusterName":   clusterName,
		"agentVersion":  "test",
		"kbVersion":     "test-kb",
		"inventory":     json.RawMessage(invJSON),
	})
	if err != nil {
		t.Fatal(err)
	}
	resp, out := postSnapshot(t, ts, "ingest-tok", body, false)
	if resp.StatusCode != 202 {
		t.Fatalf("push %s status = %d (body %v), want 202", clusterName, resp.StatusCode, out)
	}
}

// fleetFixture: two clusters on v1.34 — "alpha" clean, "bravo" with one PSP
// blocker (testKB: PSP removed in 1.35). Default target for both is 1.35.
func fleetFixture(t *testing.T) (*httptest.Server, func()) {
	t.Helper()
	st := newFakeStore()
	s := newTestServer(t, st)
	ts := httptest.NewServer(s.Handler())

	alpha := testInventory()
	pushCluster(t, ts, "alpha", alpha)

	bravo := testInventory()
	bravo.ClusterID = "uid-bravo"
	bravo.APIUsage = []inventory.APIUsage{{
		Group: "policy", Version: "v1beta1", Kind: "PodSecurityPolicy",
		Count: 1, Namespaces: map[string]int{"team-ns": 1},
	}}
	bravo.Namespaces = []inventory.NamespaceInfo{{Name: "team-ns", Team: "payments"}}
	pushCluster(t, ts, "bravo", bravo)

	return ts, ts.Close
}

type fleetMatrix struct {
	Targets  []string `json:"targets"`
	Clusters []struct {
		ClusterID int64  `json:"clusterId"`
		Name      string `json:"name"`
		Cells     map[string]*struct {
			Score    int  `json:"score"`
			Ready    bool `json:"ready"`
			Blockers int  `json:"blockers"`
		} `json:"cells"`
	} `json:"clusters"`
}

func TestFleetMatrixDefaultTargets(t *testing.T) {
	ts, done := fleetFixture(t)
	defer done()

	var got fleetMatrix
	resp := getJSON(t, ts, "/api/v1/fleet", "", &got)
	if resp.StatusCode != 200 {
		t.Fatalf("GET /fleet status = %d", resp.StatusCode)
	}
	if !reflect.DeepEqual(got.Targets, []string{"1.35"}) {
		t.Fatalf("targets = %v, want [1.35]", got.Targets)
	}
	if len(got.Clusters) != 2 {
		t.Fatalf("clusters = %d, want 2", len(got.Clusters))
	}
	alpha, bravo := got.Clusters[0], got.Clusters[1]
	if alpha.Name != "alpha" || bravo.Name != "bravo" {
		t.Fatalf("cluster order = %s,%s", alpha.Name, bravo.Name)
	}
	a := alpha.Cells["1.35"]
	if a == nil || a.Score != 100 || !a.Ready || a.Blockers != 0 {
		t.Fatalf("alpha cell = %+v, want score 100 ready", a)
	}
	b := bravo.Cells["1.35"]
	if b == nil || b.Score != 75 || b.Ready || b.Blockers != 1 {
		t.Fatalf("bravo cell = %+v, want score 75, 1 blocker", b)
	}
}

func TestFleetMatrixExplicitTargets(t *testing.T) {
	ts, done := fleetFixture(t)
	defer done()

	var got fleetMatrix
	resp := getJSON(t, ts, "/api/v1/fleet?targets=1.35,1.38", "", &got)
	if resp.StatusCode != 200 {
		t.Fatalf("GET /fleet status = %d", resp.StatusCode)
	}
	if !reflect.DeepEqual(got.Targets, []string{"1.35", "1.38"}) {
		t.Fatalf("targets = %v, want [1.35 1.38]", got.Targets)
	}
	// 1.38 was never evaluated (no recompute): cell must be JSON null.
	for _, c := range got.Clusters {
		if c.Cells["1.38"] != nil {
			t.Fatalf("%s cell[1.38] = %+v, want null (latest evals only)", c.Name, c.Cells["1.38"])
		}
		if c.Cells["1.35"] == nil {
			t.Fatalf("%s cell[1.35] missing", c.Name)
		}
	}
}

func TestFleetMatrixBadTargets(t *testing.T) {
	ts, done := fleetFixture(t)
	defer done()
	if resp := getJSON(t, ts, "/api/v1/fleet?targets=1.3", "", nil); resp.StatusCode != 422 {
		t.Errorf("targets=1.3 (below the knowledge base) status = %d, want 422", resp.StatusCode)
	}
	if resp := getJSON(t, ts, "/api/v1/fleet?targets=banana", "", nil); resp.StatusCode != 422 {
		t.Fatalf("bad targets status = %d, want 422", resp.StatusCode)
	}
}

// ?targets= took any number of distinct minors, bounded only by the 64 KiB
// URL: 8,718 of them against a 500-cluster fleet held a fleet slot for
// 2m13s (a store query per cell), grew the heap 418 MiB and answered
// 57 MiB. At most maxFleetTargets distinct minors are taken (a repeat
// does not count); more is 422 before any cell is read.
func TestFleetTargetsAreCapped(t *testing.T) {
	ts, done := fleetFixture(t)
	defer done()
	minors := func(n int) string {
		var out []string
		for i := range n {
			out = append(out, fmt.Sprintf("1.%d", 30+i))
		}
		return strings.Join(out, ",")
	}
	var got fleetMatrix
	if resp := getJSON(t, ts, "/api/v1/fleet?targets="+minors(maxFleetTargets)+",1.30", "", &got); resp.StatusCode != http.StatusOK ||
		len(got.Targets) != maxFleetTargets {
		t.Fatalf("%d distinct targets and a repeat: status %d, %d targets; want 200 with %d", maxFleetTargets, resp.StatusCode, len(got.Targets), maxFleetTargets)
	}
	resp, body := getRaw(t, ts, "/api/v1/fleet?targets="+minors(maxFleetTargets+1), "")
	if resp.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(body, fmt.Sprint(maxFleetTargets)) {
		t.Fatalf("%d distinct targets: status %d (%s), want 422 naming the limit", maxFleetTargets+1, resp.StatusCode, body)
	}
}

// Issue #122: what an evaluation could not assess reaches the fleet
// matrix and the cluster views, not only the full report, so an unknown
// (or a qualified ready) cell says why.
func TestFleetAndClusterViewsCarryGaps(t *testing.T) {
	ts, done := fleetFixture(t)
	defer done()
	charlie := testInventory()
	charlie.ClusterID = "uid-charlie"
	charlie.Capabilities = map[inventory.Capability]inventory.CapabilityStatus{
		inventory.CapVersions: {Available: true},
		inventory.CapCRDs:     {Available: true},
		inventory.CapVolumes:  {Available: true},
		inventory.CapAPIUsage: {Available: true, Partial: true, Reason: "list policy/v1beta1 podsecuritypolicies: forbidden",
			Skipped: []string{"policy/v1beta1 PodSecurityPolicy"}},
	}
	pushCluster(t, ts, "charlie", charlie)
	want := []engine.CapabilityGap{{Capability: inventory.CapAPIUsage, Reason: "list policy/v1beta1 podsecuritypolicies: forbidden",
		Partial: true, Skipped: []string{"policy/v1beta1 PodSecurityPolicy"}, Required: true}}

	var fleet struct {
		Clusters []struct {
			Name  string `json:"name"`
			Cells map[string]*struct {
				Verdict     engine.Verdict         `json:"verdict"`
				NotAssessed []engine.CapabilityGap `json:"notAssessed"`
			} `json:"cells"`
		} `json:"clusters"`
	}
	getJSON(t, ts, "/api/v1/fleet", "", &fleet)
	for _, c := range fleet.Clusters {
		cell := c.Cells["1.35"]
		switch {
		case cell == nil:
			t.Errorf("%s: no 1.35 cell", c.Name)
		case c.Name == "charlie" && (cell.Verdict != engine.VerdictUnknown || !reflect.DeepEqual(cell.NotAssessed, want)):
			t.Errorf("charlie cell = %+v, want unknown with the partial gap", cell)
		case c.Name != "charlie" && len(cell.NotAssessed) != 0:
			t.Errorf("%s cell gaps = %+v, want none", c.Name, cell.NotAssessed)
		}
	}

	var clusters []struct {
		ID     int64  `json:"id"`
		Name   string `json:"name"`
		Latest *struct {
			NotAssessed []engine.CapabilityGap `json:"notAssessed"`
		} `json:"latest"`
	}
	getJSON(t, ts, "/api/v1/clusters", "", &clusters)
	for _, c := range clusters {
		if c.Name == "charlie" && (c.Latest == nil || !reflect.DeepEqual(c.Latest.NotAssessed, want)) {
			t.Errorf("GET /clusters charlie latest = %+v, want the partial gap", c.Latest)
		}
	}
}

// The fleet-wide reads carry what each evaluation could not assess for
// every cluster and target, so what they list of it is bounded per
// evaluation: a push within the limits may name 32 capabilities, each
// 16 KiB long, with 64 KiB reasons and long skipped lists, and 50 such
// clusters made /fleet a 108 MB answer that grew the heap 516 MiB. A
// summary there lists the required gaps first, each cut, within
// fleetSummaryBytes, and counts the rest in notAssessedOmitted; the
// cluster's own detail lists every gap, cut.
func TestFleetSummariesAreBoundedPerEvaluation(t *testing.T) {
	ts := httptest.NewServer(newTestServer(t, newFakeStore()).Handler())
	defer ts.Close()
	inv := testInventory()
	inv.Capabilities = map[inventory.Capability]inventory.CapabilityStatus{
		inventory.CapAPIUsage: {Available: true},
		inventory.CapCRDs:     {Available: true},
		inventory.CapVolumes:  {Available: true},
		inventory.CapVersions: {Available: false, Reason: strings.Repeat("v", 2000)}, // required: listed first
	}
	for c := range inventory.MaxCapabilities - 4 {
		var skipped []string
		for k := range 10 {
			skipped = append(skipped, fmt.Sprint(k)+strings.Repeat("s", 600))
		}
		inv.Capabilities[inventory.Capability(fmt.Sprintf("%02d", c)+strings.Repeat("c", inventory.MaxStringBytes-2))] = inventory.CapabilityStatus{
			Available: true, Partial: true, Reason: strings.Repeat("é", 1000), Skipped: skipped}
	}
	pushCluster(t, ts, "wide", inv)
	const gapsInAll = inventory.MaxCapabilities - 3 // api-usage, crds and volumes are available

	type gap struct {
		Capability     string   `json:"capability"`
		Reason         string   `json:"reason"`
		Required       bool     `json:"required"`
		Partial        bool     `json:"partial"`
		Skipped        []string `json:"skipped"`
		SkippedOmitted int      `json:"skippedOmitted"`
	}
	type summary struct {
		NotAssessed        json.RawMessage `json:"notAssessed"`
		NotAssessedOmitted int             `json:"notAssessedOmitted"`
	}
	check := func(what string, sum summary, budget int) {
		t.Helper()
		var gaps []gap
		if err := json.Unmarshal(sum.NotAssessed, &gaps); err != nil || len(gaps) == 0 {
			t.Fatalf("%s: notAssessed %.200s (%v), want gaps", what, sum.NotAssessed, err)
		}
		if len(gaps)+sum.NotAssessedOmitted != gapsInAll {
			t.Errorf("%s: %d gaps listed and %d omitted, want %d in all", what, len(gaps), sum.NotAssessedOmitted, gapsInAll)
		}
		if gaps[0].Capability != string(inventory.CapVersions) || !gaps[0].Required {
			t.Errorf("%s: first gap %.40q (required %v), want the required versions gap", what, gaps[0].Capability, gaps[0].Required)
		}
		for _, g := range gaps {
			if len(g.Capability) > store.SummaryCapabilityBytes+len("…") || len(g.Reason) > store.SummaryReasonBytes+len("…") ||
				len(g.Skipped) > store.SummarySkipped || g.Partial && g.SkippedOmitted != 10-store.SummarySkipped {
				t.Errorf("%s: gap %.40q not cut: capability %d bytes, reason %d, %d skipped (%d omitted)", what, g.Capability, len(g.Capability), len(g.Reason), len(g.Skipped), g.SkippedOmitted)
			}
			for _, s := range g.Skipped {
				if len(s) > store.SummarySkippedBytes+len("…") {
					t.Errorf("%s: skipped entry of %d bytes", what, len(s))
				}
			}
		}
		if budget > 0 && len(sum.NotAssessed) > budget {
			t.Errorf("%s: the summary is %d bytes, want at most %d", what, len(sum.NotAssessed), budget)
		}
		if budget == 0 && sum.NotAssessedOmitted != 0 {
			t.Errorf("%s: %d gaps omitted, want every gap listed", what, sum.NotAssessedOmitted)
		}
	}
	var fleet struct {
		Clusters []struct {
			Cells map[string]*summary `json:"cells"`
		} `json:"clusters"`
	}
	getJSON(t, ts, "/api/v1/fleet", "", &fleet)
	if len(fleet.Clusters) != 1 || fleet.Clusters[0].Cells["1.35"] == nil {
		t.Fatalf("GET /fleet = %+v", fleet)
	}
	check("/fleet cell", *fleet.Clusters[0].Cells["1.35"], fleetSummaryBytes)

	var clusters []struct {
		ID     int64    `json:"id"`
		Latest *summary `json:"latest"`
	}
	getJSON(t, ts, "/api/v1/clusters", "", &clusters)
	if len(clusters) != 1 || clusters[0].Latest == nil {
		t.Fatalf("GET /clusters = %+v", clusters)
	}
	check("/clusters latest", *clusters[0].Latest, fleetSummaryBytes)

	var detail struct {
		Evaluations []summary `json:"evaluations"`
	}
	getJSON(t, ts, fmt.Sprintf("/api/v1/clusters/%d", clusters[0].ID), "", &detail)
	if len(detail.Evaluations) == 0 {
		t.Fatal("cluster detail has no evaluations")
	}
	check("cluster detail", detail.Evaluations[0], 0)
}

func TestFleetTeams(t *testing.T) {
	ts, done := fleetFixture(t)
	defer done()

	var got struct {
		Target string `json:"target"`
		Teams  map[string]struct {
			WorstScore int      `json:"worstScore"`
			Blockers   int      `json:"blockers"`
			Clusters   []string `json:"clusters"`
		} `json:"teams"`
	}
	resp := getJSON(t, ts, "/api/v1/fleet/teams?target=1.35", "", &got)
	if resp.StatusCode != 200 {
		t.Fatalf("GET /fleet/teams status = %d", resp.StatusCode)
	}
	if got.Target != "1.35" {
		t.Fatalf("target = %q", got.Target)
	}
	pay, ok := got.Teams["payments"]
	if !ok {
		t.Fatalf("teams = %+v, want payments entry", got.Teams)
	}
	if pay.WorstScore != 75 || pay.Blockers != 1 || !reflect.DeepEqual(pay.Clusters, []string{"bravo"}) {
		t.Fatalf("payments = %+v", pay)
	}
	// alpha has no findings at all → contributes no team entries.
	if len(got.Teams) != 1 {
		t.Fatalf("teams = %+v, want only payments", got.Teams)
	}

	// target is required.
	if resp := getJSON(t, ts, "/api/v1/fleet/teams", "", nil); resp.StatusCode != 422 {
		t.Fatalf("missing target status = %d, want 422", resp.StatusCode)
	}
}

// TestFleetTeamsUnstoredTargetMatchesPerCluster: for a target nothing has
// stored (1.36 is neither a default nor in --targets), the rollup must give
// the same answer /clusters/{id}/teams gives — a what-if — and say so, never
// an empty teams map that reads as an all-clear.
func TestFleetTeamsUnstoredTargetMatchesPerCluster(t *testing.T) {
	ts, done := fleetFixture(t)
	defer done()

	type teamScore struct {
		Score    int `json:"score"`
		Blockers int `json:"blockers"`
	}
	var fleet struct {
		Teams map[string]struct {
			WorstScore int      `json:"worstScore"`
			Blockers   int      `json:"blockers"`
			Clusters   []string `json:"clusters"`
		} `json:"teams"`
		Evaluated []struct {
			Name   string `json:"name"`
			Source string `json:"source"`
		} `json:"evaluated"`
		Missing       []string `json:"missing"`
		NotApplicable []string `json:"notApplicable"`
	}
	if resp := getJSON(t, ts, "/api/v1/fleet/teams?target=1.36", "", &fleet); resp.StatusCode != 200 {
		t.Fatalf("GET /fleet/teams?target=1.36 status = %d", resp.StatusCode)
	}
	var perCluster struct {
		Teams  map[string]teamScore `json:"teams"`
		Source string               `json:"source"`
	}
	getJSON(t, ts, "/api/v1/clusters/4/teams?target=1.36", "", &perCluster) // bravo (fakeStore shares one id sequence)
	want, ok := perCluster.Teams["payments"]
	if !ok || want.Blockers != 1 || perCluster.Source != "what-if" {
		t.Fatalf("per-cluster teams = %+v (source %q), want a what-if payments blocker", perCluster.Teams, perCluster.Source)
	}
	pay, ok := fleet.Teams["payments"]
	if !ok || pay.WorstScore != want.Score || pay.Blockers != want.Blockers || !reflect.DeepEqual(pay.Clusters, []string{"bravo"}) {
		t.Fatalf("fleet payments = %+v (teams %+v), want the per-cluster answer %+v from bravo", pay, fleet.Teams, want)
	}
	if len(fleet.Evaluated) != 2 || fleet.Evaluated[0].Source != "what-if" || fleet.Evaluated[1].Source != "what-if" {
		t.Errorf("evaluated = %+v, want both clusters as what-if", fleet.Evaluated)
	}
	if len(fleet.Missing) != 0 || len(fleet.NotApplicable) != 0 {
		t.Errorf("missing %v / n/a %v, want none", fleet.Missing, fleet.NotApplicable)
	}

	// The stored target reports its source as stored.
	getJSON(t, ts, "/api/v1/fleet/teams?target=1.35", "", &fleet)
	for _, e := range fleet.Evaluated {
		if e.Source != "stored" {
			t.Errorf("1.35 %s source = %q, want stored", e.Name, e.Source)
		}
	}
}

func TestFleetReadAuth(t *testing.T) {
	st := newFakeStore()
	s := newTestServer(t, st, func(c *Config) { c.ReadToken = "read-tok" })
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	for _, path := range []string{"/api/v1/fleet", "/api/v1/fleet/teams?target=1.35"} {
		if resp := getJSON(t, ts, path, "", nil); resp.StatusCode != 401 {
			t.Fatalf("GET %s without token status = %d, want 401", path, resp.StatusCode)
		}
		if resp := getJSON(t, ts, path, "read-tok", nil); resp.StatusCode != 200 {
			t.Fatalf("GET %s with token status = %d, want 200", path, resp.StatusCode)
		}
	}
}

// Without ?targets=, /fleet opens at most maxFleetTargets columns, the
// minors with the most clusters to fill them (the older on a tie), and
// counts the rest in targetsOmitted: a holder of the ingest token who
// pushes clusters at 500 minors widens it no further than ?targets= can.
func TestFleetDefaultColumnsAreCapped(t *testing.T) {
	ts := httptest.NewServer(newTestServer(t, newFakeStore(), func(c *Config) { c.ExtraTargets = []string{"1.40"} }).Handler())
	defer ts.Close()
	push := func(name string, minor int) {
		inv := testInventory()
		inv.ClusterID = "uid-" + name
		inv.ServerVersion = fmt.Sprintf("v1.%d.0", minor)
		pushCluster(t, ts, name, inv)
	}
	// One cluster at each of 1.10-1.29 (next minors 1.11-1.30), and a
	// second at 1.28 and 1.29: twenty next minors plus the extra target.
	for m := 10; m < 30; m++ {
		push(fmt.Sprintf("c-%d", m), m)
	}
	push("c-28b", 28)
	push("c-29b", 29)

	var got struct {
		Targets        []string `json:"targets"`
		TargetsOmitted int      `json:"targetsOmitted"`
	}
	if resp := getJSON(t, ts, "/api/v1/fleet", "", &got); resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
	// 1.40 (every cluster's), 1.29 and 1.30 (two clusters each), then the
	// older minors of one cluster each: 1.11-1.23.
	var want []string
	for m := 11; m <= 23; m++ {
		want = append(want, fmt.Sprintf("1.%d", m))
	}
	want = append(want, "1.29", "1.30", "1.40")
	if !reflect.DeepEqual(got.Targets, want) || got.TargetsOmitted != 5 {
		t.Errorf("targets = %v (omitted %d), want %v (omitted 5)", got.Targets, got.TargetsOmitted, want)
	}

	// Within the cap nothing is omitted, and the field is left out.
	ts2, done := fleetFixture(t)
	defer done()
	resp, body := getRaw(t, ts2, "/api/v1/fleet", "")
	if resp.StatusCode != http.StatusOK || strings.Contains(body, "targetsOmitted") {
		t.Errorf("a two-cluster fleet: status %d, body %s; want 200 without targetsOmitted", resp.StatusCode, body)
	}
}

// fleetVerdictFixture pushes the named clusters of four on v1.34 whose
// "payments" team owns one warning-level finding (a deprecated HPA) and
// nothing else of its own:
//
//	calm      the warning only                                  payments: ready
//	orphaned  + a PSP blocker in a namespace no team owns       payments: blocked
//	partial   + a required check that did not run               payments: unknown
//	noisy     + both                                            payments: blocked
func fleetVerdictFixture(t *testing.T, names ...string) *httptest.Server {
	t.Helper()
	deprecated, removed := inventory.Version{Major: 1, Minor: 30}, inventory.Version{Major: 1, Minor: 40}
	s := newTestServer(t, newFakeStore(), func(c *Config) {
		c.KB.APILifecycle = append(c.KB.APILifecycle, kb.APILifecycleEntry{
			Group: "autoscaling", Version: "v2beta1", Kind: "HorizontalPodAutoscaler",
			Introduced: inventory.Version{Major: 1, Minor: 8}, Deprecated: &deprecated, Removed: &removed,
		})
	})
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)

	hpa := inventory.APIUsage{Group: "autoscaling", Version: "v2beta1", Kind: "HorizontalPodAutoscaler",
		Count: 1, Namespaces: map[string]int{"pay": 1}}
	psp := inventory.APIUsage{Group: "policy", Version: "v1beta1", Kind: "PodSecurityPolicy",
		Count: 1, Namespaces: map[string]int{"": 1}}
	shapes := map[string]struct {
		usage []inventory.APIUsage
		gap   bool
	}{
		"calm":     {[]inventory.APIUsage{hpa}, false},
		"orphaned": {[]inventory.APIUsage{hpa, psp}, false},
		"partial":  {[]inventory.APIUsage{hpa}, true},
		"noisy":    {[]inventory.APIUsage{hpa, psp}, true},
	}
	for _, name := range names {
		shape := shapes[name]
		inv := testInventory()
		inv.ClusterID = "uid-" + name
		inv.Namespaces = []inventory.NamespaceInfo{{Name: "pay", Team: "payments"}}
		inv.APIUsage = shape.usage
		if shape.gap {
			inv.Capabilities[inventory.CapAPIUsage] = inventory.CapabilityStatus{Available: true, Partial: true,
				Reason: "list policy/v1beta1 podsecuritypolicies: forbidden", Skipped: []string{"policy/v1beta1 PodSecurityPolicy"}}
		}
		pushCluster(t, ts, name, inv)
	}
	return ts
}

// #243, VS-14: a team's score says nothing about a blocker no team owns or
// a check that did not run, so the rollup carries the team's verdict, the
// worst across clusters (blocked over unknown over ready).
func TestFleetTeamsCarryTheTeamVerdict(t *testing.T) {
	for _, tc := range []struct {
		name     string
		clusters []string
		want     engine.Verdict
	}{
		{"ready", []string{"calm"}, engine.VerdictReady},
		{"an unattributed blocker blocks a team with only a warning", []string{"orphaned"}, engine.VerdictBlocked},
		{"a required gap makes the team unknown", []string{"partial"}, engine.VerdictUnknown},
		{"blocked beats unknown beats ready", []string{"calm", "partial", "orphaned"}, engine.VerdictBlocked},
		{"unknown beats ready", []string{"calm", "partial"}, engine.VerdictUnknown},
		{"blocked and unknown in one cluster", []string{"noisy"}, engine.VerdictBlocked},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ts := fleetVerdictFixture(t, tc.clusters...)
			var got struct {
				Teams map[string]struct {
					WorstScore int            `json:"worstScore"`
					Blockers   int            `json:"blockers"`
					Verdict    engine.Verdict `json:"verdict"`
				} `json:"teams"`
			}
			if resp := getJSON(t, ts, "/api/v1/fleet/teams?target=1.35", "", &got); resp.StatusCode != 200 {
				t.Fatalf("status = %d", resp.StatusCode)
			}
			pay := got.Teams["payments"]
			// The team's own findings are one warning everywhere: no
			// blockers and a high score. Only the verdict knows better.
			if pay.Blockers != 0 || pay.WorstScore < 90 {
				t.Fatalf("payments = %+v, want a warning-only score of 90 or more and no blockers", pay)
			}
			if pay.Verdict != tc.want {
				t.Errorf("payments verdict = %q, want %q", pay.Verdict, tc.want)
			}
		})
	}
}

// #243: a cluster left out of the rollup says why (`excluded`), and
// `missing` keeps listing every one of them for older readers.
func TestFleetTeamsSaysWhyAClusterWasLeftOut(t *testing.T) {
	st := newFakeStore()
	s := newTestServer(t, st)
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()
	pushCluster(t, ts, "fine", testInventory())
	ctx := context.Background()
	empty, err := st.UpsertCluster(ctx, store.Cluster{Name: "empty"})
	if err != nil {
		t.Fatal(err)
	}
	broken, err := st.UpsertCluster(ctx, store.Cluster{Name: "broken"})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.InsertSnapshot(ctx, store.Snapshot{ClusterID: broken, Hash: "x", Inventory: []byte(`{`)}); err != nil {
		t.Fatal(err)
	}
	var got struct {
		Missing  []string        `json:"missing"`
		Excluded []fleetExcluded `json:"excluded"`
	}
	if resp := getJSON(t, ts, "/api/v1/fleet/teams?target=1.35", "", &got); resp.StatusCode != 200 {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	want := []fleetExcluded{
		{Name: "empty", ClusterID: empty, Reason: "no-snapshot"},
		{Name: "broken", ClusterID: broken, Reason: "unreadable"},
	}
	slices.SortFunc(got.Excluded, func(a, b fleetExcluded) int { return strings.Compare(a.Name, b.Name) })
	slices.SortFunc(want, func(a, b fleetExcluded) int { return strings.Compare(a.Name, b.Name) })
	if !reflect.DeepEqual(got.Excluded, want) {
		t.Errorf("excluded = %+v, want %+v", got.Excluded, want)
	}
	slices.Sort(got.Missing)
	if !reflect.DeepEqual(got.Missing, []string{"broken", "empty"}) {
		t.Errorf("missing = %v, want both names", got.Missing)
	}
}
