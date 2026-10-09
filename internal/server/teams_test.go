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
		engine.UnattributedTeam: {Score: 95, Ready: true, Warnings: 1},
		"payments":              {Score: 75, Blockers: 1},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("renderTeamScores = %+v, want %+v", got, want)
	}
	// A real team named "unattributed" is its own row: the bucket of
	// findings no team owns carries a name no label value can take (#243).
	collide := map[string]engine.TeamScore{
		"":             {Score: 95},
		"unattributed": {Score: 50, Blockers: 2},
	}
	got = renderTeamScores(collide)
	if len(got) != 2 || got["unattributed"] != (engine.TeamScore{Score: 50, Blockers: 2}) ||
		got[engine.UnattributedTeam] != (engine.TeamScore{Score: 95}) {
		t.Fatalf("collision handling wrong: %+v", got)
	}
	if _, kept := got[""]; kept {
		t.Fatalf("the empty key must not reach the wire: %+v", got)
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
	want := map[string]engine.TeamScore{"payments": {Score: 75, Ready: false, Verdict: engine.VerdictBlocked, Blockers: 1}}
	if !reflect.DeepEqual(got.Teams, want) {
		t.Fatalf("teams = %+v, want %+v (cluster %d)", got.Teams, want, id)
	}

	// Unknown cluster → 404.
	if resp := getJSON(t, ts, "/api/v1/clusters/99/teams", "", nil); resp.StatusCode != 404 {
		t.Fatalf("unknown cluster status = %d, want 404", resp.StatusCode)
	}
}

// TestTeamVerdictFollowsTheCluster (#196 VS-14): a team was scored over
// its own findings only, so payments read ready:true on a cluster whose
// verdict was unknown (versions not assessed) or blocked by kubelet skew
// no team owns. Its verdict now says so, on /report and /teams alike, and
// ready is true only when that verdict is ready.
func TestTeamVerdictFollowsTheCluster(t *testing.T) {
	base := testInventory()
	base.ServerVersion = "v1.33.4"
	base.Nodes = []inventory.NodeInfo{{Name: "n1", KubeletVersion: "v1.33.4"}}
	base.Namespaces = []inventory.NamespaceInfo{{Name: "shop", Team: "payments"}, {Name: "web", Team: "frontend"}}
	base.APIUsage = []inventory.APIUsage{{ // a warning at 1.34: removed in 1.35
		Group: "policy", Version: "v1beta1", Kind: "PodSecurityPolicy", Count: 1, Namespaces: map[string]int{"shop": 1},
	}}
	unknown := base
	unknown.Capabilities = collectedCaps()
	unknown.Capabilities[inventory.CapVersions] = inventory.CapabilityStatus{Reason: "list nodes: forbidden"}
	skewed := base
	skewed.Nodes = []inventory.NodeInfo{{Name: "n1", KubeletVersion: "v1.30.4"}}

	for _, tc := range []struct {
		name    string
		inv     inventory.Inventory
		verdict engine.Verdict
	}{
		{"required gap", unknown, engine.VerdictUnknown},
		{"unattributed blocker", skewed, engine.VerdictBlocked},
		{"clean", base, engine.VerdictReady},
	} {
		ts := httptest.NewServer(newTestServer(t, newFakeStore()).Handler()) // the cluster is id 1
		pushCluster(t, ts, "vs14", tc.inv)
		var rep struct {
			Verdict engine.Verdict              `json:"verdict"`
			Teams   map[string]engine.TeamScore `json:"teams"`
		}
		getJSON(t, ts, "/api/v1/clusters/1/report?target=1.34", "", &rep)
		var teams struct {
			Teams map[string]engine.TeamScore `json:"teams"`
		}
		getJSON(t, ts, "/api/v1/clusters/1/teams?target=1.34", "", &teams)
		if rep.Verdict != tc.verdict {
			t.Errorf("%s: cluster verdict %s, want %s", tc.name, rep.Verdict, tc.verdict)
		}
		want := engine.TeamScore{Score: 95, Ready: tc.verdict == engine.VerdictReady, Verdict: tc.verdict, Warnings: 1}
		for what, got := range map[string]engine.TeamScore{"/report": rep.Teams["payments"], "/teams": teams.Teams["payments"]} {
			if got != want {
				t.Errorf("%s: %s payments = %+v, want %+v", tc.name, what, got, want)
			}
		}
		ts.Close()
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
	want := map[string]engine.TeamScore{"payments": {Score: 75, Ready: false, Verdict: engine.VerdictBlocked, Blockers: 1}}
	if !reflect.DeepEqual(got.Teams, want) {
		t.Fatalf("report teams = %+v, want %+v", got.Teams, want)
	}
}
