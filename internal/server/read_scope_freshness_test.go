package server

import (
	"bytes"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/abd-ulbasit/upgradescope/internal/inventory"
)

// payInventory is a cluster with one namespace, pay-prod, labelled for
// payments, and a PSP there.
func payInventory() inventory.Inventory {
	inv := testInventory()
	inv.Namespaces = []inventory.NamespaceInfo{{Name: "pay-prod", Team: "payments"}}
	inv.APIUsage = []inventory.APIUsage{usage("policy", "PodSecurityPolicy", "pay-prod")}
	return inv
}

// scopedReads lists what token reads of the cluster called name: the
// status of its own endpoint, and whether /clusters, /fleet and
// /fleet/teams name it.
func scopedReads(t *testing.T, h *harness, name, token string) (status int, listed []string) {
	t.Helper()
	id := fmt.Sprint(h.clusterID(name))
	resp, _ := fetch(t, h.ts, "/api/v1/clusters/"+id, token)
	for _, p := range []string{"/api/v1/clusters", "/api/v1/fleet", "/api/v1/fleet/teams?target=1.35"} {
		if _, raw := fetch(t, h.ts, p, token); bytes.Contains(raw, []byte(`"`+name+`"`)) {
			listed = append(listed, p)
		}
	}
	return resp.StatusCode, listed
}

// A --team-map change takes a cluster out of the scope of the team that
// lost its namespace, even with a target removed from --targets in the
// same restart, whose evaluation is never rewritten and still names the
// old team (#72). Until the startup pass rewrites the current targets'
// evaluations, the cluster is in no team's scope, the new owner's
// included: an evaluation of another team map is not read for scope.
func TestTeamMapChangeLeavesTheOldTeamsScope(t *testing.T) {
	h := newHarness(t, Config{KB: testKB(), ExtraTargets: []string{"1.36"}}, aug1)
	h.push("pay", payInventory())
	mintReadToken(t, h.st, "pay-tok", "payments")
	mintReadToken(t, h.st, "bill-tok", "billing")
	if status, listed := scopedReads(t, h, "pay", "pay-tok"); status != http.StatusOK || len(listed) != 3 {
		t.Fatalf("payments before the change: %d, listed by %v, want 200 and every list", status, listed)
	}

	h.restart(Config{KB: testKB(), TeamMap: TeamMap{{Pattern: "pay-*", Team: "billing"}}})
	for _, tok := range []string{"pay-tok", "bill-tok"} {
		if status, listed := scopedReads(t, h, "pay", tok); status != http.StatusNotFound || len(listed) != 0 {
			t.Errorf("%s before the pass: %d, listed by %v, want 404 and no list", tok, status, listed)
		}
	}
	h.tick()
	if status, listed := scopedReads(t, h, "pay", "pay-tok"); status != http.StatusNotFound || len(listed) != 0 {
		t.Errorf("payments after the pass: %d, listed by %v, want 404 and no list: 1.36's stale row still names payments", status, listed)
	}
	if status, listed := scopedReads(t, h, "pay", "bill-tok"); status != http.StatusOK || len(listed) != 3 {
		t.Errorf("billing after the pass: %d, listed by %v, want 200 and every list", status, listed)
	}
}

// A scoped read of a cluster outside its scope never looks the cluster
// up, so it costs what an unknown id does (one scope query) and its
// timing does not say the id exists. A broken GetCluster shows it: the
// out-of-scope id still answers 404, and an in-scope one the 500.
func TestOutOfScopeClusterIsNeverLookedUp(t *testing.T) {
	st := newFakeStore()
	s := newTestServer(t, st)
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	pushScopeCluster(t, ts, "pay", payInventory())
	web := payInventory()
	web.Namespaces[0].Team = "web"
	pushScopeCluster(t, ts, "web", web)
	mintReadToken(t, st, "pay-tok", "payments")
	ids := map[string]string{}
	for _, c := range st.clusters {
		ids[c.Name] = fmt.Sprint(c.ID)
	}
	st.mu.Lock()
	st.errs["GetCluster"] = errors.New("GetCluster called")
	st.mu.Unlock()
	if resp, raw := fetch(t, ts, "/api/v1/clusters/"+ids["web"], "pay-tok"); resp.StatusCode != http.StatusNotFound {
		t.Errorf("out-of-scope id = %d %s, want 404 without a cluster lookup", resp.StatusCode, raw)
	}
	if resp, _ := fetch(t, ts, "/api/v1/clusters/"+ids["pay"], "pay-tok"); resp.StatusCode != http.StatusInternalServerError {
		t.Errorf("in-scope id = %d, want the broken lookup's 500: the fixture does not test the order", resp.StatusCode)
	}
}

// The upgrade path (#72): an evaluation written before the teams column
// existed (NULL) puts its cluster in no team's scope until the startup
// pass re-evaluates it, and in its teams' scope from then on.
func TestEvaluationWithoutTeamsEntersScopeAfterThePass(t *testing.T) {
	h := newHarness(t, Config{KB: testKB()}, aug1)
	h.push("pay", payInventory())
	mintReadToken(t, h.st, "pay-tok", "payments")
	db, err := sql.Open("sqlite", h.path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if res, err := db.Exec(`UPDATE evaluations SET teams = NULL`); err != nil {
		t.Fatal(err)
	} else if n, _ := res.RowsAffected(); n == 0 {
		t.Fatal("no evaluation to age")
	}

	h.restart(Config{KB: testKB()})
	if status, listed := scopedReads(t, h, "pay", "pay-tok"); status != http.StatusNotFound || len(listed) != 0 {
		t.Errorf("before the pass: %d, listed by %v, want 404 and no list", status, listed)
	}
	h.tick()
	if status, listed := scopedReads(t, h, "pay", "pay-tok"); status != http.StatusOK || len(listed) != 3 {
		t.Errorf("after the pass: %d, listed by %v, want 200 and every list", status, listed)
	}
}
