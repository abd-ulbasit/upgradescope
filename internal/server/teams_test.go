package server

import (
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/abd-ulbasit/upgradescope/internal/engine"
	"github.com/abd-ulbasit/upgradescope/internal/inventory"
)

func TestRenderTeamScores(t *testing.T) {
	in := map[string]engine.TeamScore{
		"":         {Score: 95, Ready: true, Warnings: 1},
		"payments": {Score: 75, Blockers: 1},
	}
	got := renderTeamScores(in)
	want := map[string]engine.TeamScore{
		"unattributed": {Score: 95, Ready: true, Warnings: 1},
		"payments":     {Score: 75, Blockers: 1},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("renderTeamScores = %+v, want %+v", got, want)
	}
	// Pathological collision: a real team named "unattributed" must not be
	// overwritten by the "" bucket.
	collide := map[string]engine.TeamScore{
		"":             {Score: 95},
		"unattributed": {Score: 50, Blockers: 2},
	}
	got = renderTeamScores(collide)
	if got["unattributed"] != (engine.TeamScore{Score: 50, Blockers: 2}) || got[""] != (engine.TeamScore{Score: 95}) {
		t.Fatalf("collision handling wrong: %+v", got)
	}
}

// seedTeamCluster pushes an inventory with one PSP blocker in a
// team-labelled namespace and one teamless warning-free info-only usage,
// then returns the cluster id.
func seedTeamCluster(t *testing.T, ts *httptest.Server) int64 {
	t.Helper()
	inv := testInventory()
	inv.Namespaces = []inventory.NamespaceInfo{{Name: "payments-prod", Team: "payments"}}
	inv.APIUsage = []inventory.APIUsage{{
		Group: "policy", Version: "v1beta1", Kind: "PodSecurityPolicy",
		Count: 1, Namespaces: map[string]int{"payments-prod": 1},
	}}
	resp, out := postSnapshot(t, ts, "ingest-tok", pushReqBody(t, inv), false)
	if resp.StatusCode != 202 {
		t.Fatalf("seed push status = %d (body %v), want 202", resp.StatusCode, out)
	}
	return 1
}

func TestTeamsEndpoint(t *testing.T) {
	st := newFakeStore()
	s := newTestServer(t, st)
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()
	id := seedTeamCluster(t, ts)

	var got struct {
		Target string                      `json:"target"`
		Teams  map[string]engine.TeamScore `json:"teams"`
	}
	resp := getJSON(t, ts, "/api/v1/clusters/1/teams?target=1.35", "", &got)
	if resp.StatusCode != 200 {
		t.Fatalf("GET /teams status = %d", resp.StatusCode)
	}
	if got.Target != "1.35" {
		t.Fatalf("target = %q, want 1.35", got.Target)
	}
	want := map[string]engine.TeamScore{"payments": {Score: 75, Ready: false, Blockers: 1}}
	if !reflect.DeepEqual(got.Teams, want) {
		t.Fatalf("teams = %+v, want %+v (cluster %d)", got.Teams, want, id)
	}

	// Unknown cluster → 404.
	if resp := getJSON(t, ts, "/api/v1/clusters/99/teams", "", nil); resp.StatusCode != 404 {
		t.Fatalf("unknown cluster status = %d, want 404", resp.StatusCode)
	}
}

// The report endpoint exposes the same scores as a `teams` field — computed
// at presentation time, not stored in the engine report.
func TestReportIncludesTeams(t *testing.T) {
	st := newFakeStore()
	s := newTestServer(t, st)
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()
	seedTeamCluster(t, ts)

	var got struct {
		Score int                         `json:"score"`
		Teams map[string]engine.TeamScore `json:"teams"`
	}
	resp := getJSON(t, ts, "/api/v1/clusters/1/report?target=1.35", "", &got)
	if resp.StatusCode != 200 {
		t.Fatalf("GET /report status = %d", resp.StatusCode)
	}
	if got.Score != 75 {
		t.Fatalf("score = %d, want 75", got.Score)
	}
	want := map[string]engine.TeamScore{"payments": {Score: 75, Ready: false, Blockers: 1}}
	if !reflect.DeepEqual(got.Teams, want) {
		t.Fatalf("report teams = %+v, want %+v", got.Teams, want)
	}
}
