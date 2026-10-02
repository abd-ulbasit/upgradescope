package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/abd-ulbasit/upgradescope/internal/engine"
	"github.com/abd-ulbasit/upgradescope/internal/inventory"
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
