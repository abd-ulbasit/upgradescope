package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/abd-ulbasit/upgradescope/internal/server/store"
)

// adminRequest sends method to path with an optional bearer token and
// JSON body, returning the status and the decoded JSON body (if any).
func adminRequest(t *testing.T, ts *httptest.Server, method, path, token, body string) (int, map[string]any) {
	t.Helper()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, ts.URL+path, rd)
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var out map[string]any
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &out); err != nil {
			t.Fatalf("%s %s: non-JSON body %q", method, path, raw)
		}
	}
	return resp.StatusCode, out
}

// TestClusterAdminRefusedWithoutAdminToken: deletes and renames are
// refused outright on a server started without --admin-token; the read
// token (or none, on an open read API) never authorizes them.
func TestClusterAdminRefusedWithoutAdminToken(t *testing.T) {
	st := newFakeStore()
	ts := httptest.NewServer(newTestServer(t, st, func(c *Config) { c.ReadToken = "read-tok" }).Handler())
	defer ts.Close()
	seedViaPush(t, ts)
	for _, tc := range []struct{ method, body string }{{http.MethodDelete, ""}, {http.MethodPatch, `{"name":"x"}`}} {
		code, out := adminRequest(t, ts, tc.method, "/api/v1/clusters/1", "read-tok", tc.body)
		if code != http.StatusForbidden || !strings.Contains(out["error"].(string), "--admin-token") {
			t.Errorf("%s without an admin token configured = %d %v, want 403 naming --admin-token", tc.method, code, out)
		}
	}
	if len(st.clusters) != 1 || st.clusters[1].Name != "prod-eu-1" {
		t.Errorf("refused requests changed clusters: %+v", st.clusters)
	}
}

// TestClusterDeleteAPI: DELETE /api/v1/clusters/{id} with the admin token
// removes the cluster, its history and its tokens; any other token is 403,
// none is 401.
func TestClusterDeleteAPI(t *testing.T) {
	st := newFakeStore()
	ts := httptest.NewServer(newTestServer(t, st, func(c *Config) {
		c.ReadToken, c.AdminToken = "read-tok", "admin-tok"
	}).Handler())
	defer ts.Close()
	seedViaPush(t, ts)
	if _, err := st.CreateToken(context.Background(), "prod-eu-1", "cluster-tok"); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		token string
		want  int
	}{{"", http.StatusUnauthorized}, {"read-tok", http.StatusForbidden}, {"ingest-tok", http.StatusForbidden}, {"cluster-tok", http.StatusForbidden}} {
		if code, out := adminRequest(t, ts, http.MethodDelete, "/api/v1/clusters/1", tc.token, ""); code != tc.want {
			t.Errorf("DELETE with token %q = %d %v, want %d", tc.token, code, out, tc.want)
		}
	}
	if code, out := adminRequest(t, ts, http.MethodDelete, "/api/v1/clusters/1", "admin-tok", ""); code != http.StatusNoContent {
		t.Fatalf("DELETE with the admin token = %d %v, want 204", code, out)
	}
	if len(st.clusters) != 0 || len(st.snapshots) != 0 || len(st.evals) != 0 || len(st.tokens) != 0 {
		t.Errorf("after delete: %d clusters, %d snapshots, %d evaluations, %d tokens, want none",
			len(st.clusters), len(st.snapshots), len(st.evals), len(st.tokens))
	}
	if code, _ := adminRequest(t, ts, http.MethodGet, "/api/v1/clusters/1", "read-tok", ""); code != http.StatusNotFound {
		t.Errorf("GET after delete = %d, want 404", code)
	}
	if code, _ := adminRequest(t, ts, http.MethodDelete, "/api/v1/clusters/1", "admin-tok", ""); code != http.StatusNotFound {
		t.Errorf("second DELETE = %d, want 404", code)
	}
}

// TestClusterRenameAPI: PATCH /api/v1/clusters/{id} {"name": ...} renames
// with the admin token; a taken name is 409, an empty or malformed one 422.
func TestClusterRenameAPI(t *testing.T) {
	st := newFakeStore()
	ts := httptest.NewServer(newTestServer(t, st, func(c *Config) { c.AdminToken = "admin-tok" }).Handler())
	defer ts.Close()
	seedViaPush(t, ts)
	if _, err := st.UpsertCluster(context.Background(), store.Cluster{Name: "dev"}); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		body string
		want int
	}{
		{`{"name":"dev"}`, http.StatusConflict},
		{`{"name":""}`, http.StatusUnprocessableEntity},
		{`{"name":" padded "}`, http.StatusUnprocessableEntity},
		{`{"name":"a\nb"}`, http.StatusUnprocessableEntity},
		{`{"nam":"x"}`, http.StatusUnprocessableEntity},
		{`not json`, http.StatusUnprocessableEntity},
	} {
		if code, out := adminRequest(t, ts, http.MethodPatch, "/api/v1/clusters/1", "admin-tok", tc.body); code != tc.want {
			t.Errorf("PATCH %s = %d %v, want %d", tc.body, code, out, tc.want)
		}
	}
	if code, _ := adminRequest(t, ts, http.MethodPatch, "/api/v1/clusters/99", "admin-tok", `{"name":"x"}`); code != http.StatusNotFound {
		t.Errorf("PATCH unknown cluster = %d, want 404", code)
	}
	code, out := adminRequest(t, ts, http.MethodPatch, "/api/v1/clusters/1", "admin-tok", `{"name":"prod-eu-west-1"}`)
	if code != http.StatusOK || out["name"] != "prod-eu-west-1" || out["id"] != float64(1) {
		t.Fatalf("PATCH rename = %d %v, want 200 with the renamed cluster", code, out)
	}
	if st.clusters[1].Name != "prod-eu-west-1" {
		t.Errorf("stored name = %q", st.clusters[1].Name)
	}
	// Its own name again is a no-op, not a conflict with itself.
	if code, out := adminRequest(t, ts, http.MethodPatch, "/api/v1/clusters/1", "admin-tok", `{"name":"prod-eu-west-1"}`); code != http.StatusOK {
		t.Errorf("PATCH to its own name = %d %v, want 200", code, out)
	}
}

// TestReadAPIAcceptsAdminToken: the admin token is a superset of the read
// token, so one credential can list clusters and then delete one.
func TestReadAPIAcceptsAdminToken(t *testing.T) {
	ts := httptest.NewServer(newTestServer(t, newFakeStore(), func(c *Config) {
		c.ReadToken, c.AdminToken = "read-tok", "admin-tok"
	}).Handler())
	defer ts.Close()
	if resp := getJSON(t, ts, "/api/v1/clusters", "admin-tok", nil); resp.StatusCode != http.StatusOK {
		t.Errorf("GET /api/v1/clusters with the admin token = %d, want 200", resp.StatusCode)
	}
}
