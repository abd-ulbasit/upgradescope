package server

import (
	"encoding/json"
	"net/http/httptest"
	"reflect"
	"testing"

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
