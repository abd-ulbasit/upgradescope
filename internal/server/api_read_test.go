package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/abd-ulbasit/upgradescope/internal/engine"
	"github.com/abd-ulbasit/upgradescope/internal/server/store"
)

// getJSON GETs path with optional bearer token and decodes JSON into `into`
// (skipped when into is nil — e.g. for 405s, whose body is ServeMux plain text).
func getJSON(t *testing.T, ts *httptest.Server, path, token string, into any) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, ts.URL+path, nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if into != nil && len(raw) > 0 {
		if err := json.Unmarshal(raw, into); err != nil {
			t.Fatalf("GET %s: status %d, non-JSON body %q", path, resp.StatusCode, raw)
		}
	}
	return resp
}

// seedViaPush ingests testInventoryWithPSP through the real ingest endpoint
// so cluster, snapshot, and the 1.35 evaluation all exist. fakeStore assigns
// the first cluster ID 1.
func seedViaPush(t *testing.T, ts *httptest.Server) int64 {
	t.Helper()
	resp, out := postSnapshot(t, ts, "ingest-tok", pushReqBody(t, testInventoryWithPSP()), true)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("seed push status = %d (body %v), want 202", resp.StatusCode, out)
	}
	return 1
}

func TestHealthz(t *testing.T) {
	// ReadToken configured — /healthz must still be open (probes have no tokens).
	s := newTestServer(t, newFakeStore(), func(c *Config) { c.ReadToken = "read-tok" })
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()
	var out map[string]string
	resp := getJSON(t, ts, "/healthz", "", &out)
	if resp.StatusCode != http.StatusOK || out["status"] != "ok" {
		t.Fatalf("healthz = %d %v, want 200 {status:ok}", resp.StatusCode, out)
	}
}

func TestReadAuth(t *testing.T) {
	s := newTestServer(t, newFakeStore(), func(c *Config) { c.ReadToken = "read-tok" })
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()
	for _, tc := range []struct {
		name  string
		token string
		want  int
	}{
		{"no token", "", http.StatusUnauthorized},
		{"wrong token", "nope", http.StatusUnauthorized},
		{"ingest token is not a read token", "ingest-tok", http.StatusUnauthorized},
		{"correct token", "read-tok", http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out json.RawMessage
			resp := getJSON(t, ts, "/api/v1/clusters", tc.token, &out)
			if resp.StatusCode != tc.want {
				t.Fatalf("status = %d, want %d", resp.StatusCode, tc.want)
			}
		})
	}

	t.Run("open when ReadToken empty", func(t *testing.T) {
		tsOpen := httptest.NewServer(newTestServer(t, newFakeStore()).Handler())
		defer tsOpen.Close()
		var out json.RawMessage
		if resp := getJSON(t, tsOpen, "/api/v1/clusters", "", &out); resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200 (read API open)", resp.StatusCode)
		}
	})
}

func TestListClusters(t *testing.T) {
	st := newFakeStore()
	ts := httptest.NewServer(newTestServer(t, st).Handler())
	defer ts.Close()
	seedViaPush(t, ts)
	// A second cluster with no snapshots → no latest summary.
	if _, err := st.UpsertCluster(context.Background(), store.Cluster{Name: "empty", LastSeen: time.Now()}); err != nil {
		t.Fatal(err)
	}
	var got []struct {
		ID     int64  `json:"id"`
		Name   string `json:"name"`
		Latest *struct {
			Target   string `json:"target"`
			Score    int    `json:"score"`
			Ready    bool   `json:"ready"`
			Blockers int    `json:"blockers"`
		} `json:"latest"`
	}
	resp := getJSON(t, ts, "/api/v1/clusters", "", &got)
	if resp.StatusCode != http.StatusOK || len(got) != 2 {
		t.Fatalf("status %d, %d clusters, want 200 with 2", resp.StatusCode, len(got))
	}
	if got[0].Name != "prod-eu-1" || got[0].Latest == nil {
		t.Fatalf("cluster[0] = %+v, want prod-eu-1 with latest summary", got[0])
	}
	if got[0].Latest.Target != "1.35" || got[0].Latest.Score != 75 || got[0].Latest.Ready || got[0].Latest.Blockers != 1 {
		t.Fatalf("latest = %+v, want target 1.35 score 75 ready false blockers 1", got[0].Latest)
	}
	if got[1].Name != "empty" || got[1].Latest != nil {
		t.Fatalf("cluster[1] = %+v, want empty cluster without latest", got[1])
	}
}

func TestGetCluster(t *testing.T) {
	st := newFakeStore()
	s := newTestServer(t, st, func(c *Config) { c.ExtraTargets = []string{"1.37"} })
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()
	id := seedViaPush(t, ts)

	var got struct {
		ID           int64  `json:"id"`
		Name         string `json:"name"`
		Capabilities map[string]struct {
			Available bool `json:"available"`
		} `json:"capabilities"`
		Evaluations []struct {
			Target string `json:"target"`
			Score  int    `json:"score"`
		} `json:"evaluations"`
	}
	resp := getJSON(t, ts, "/api/v1/clusters/1", "", &got)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if got.ID != id || got.Name != "prod-eu-1" {
		t.Fatalf("cluster = %+v", got)
	}
	if cap, ok := got.Capabilities["versions"]; !ok || !cap.Available {
		t.Fatalf("capabilities = %+v, want versions available", got.Capabilities)
	}
	targets := map[string]bool{}
	for _, e := range got.Evaluations {
		targets[e.Target] = true
	}
	if !targets["1.35"] || !targets["1.37"] || len(got.Evaluations) != 2 {
		t.Fatalf("evaluations = %+v, want default 1.35 + extra 1.37", got.Evaluations)
	}

	t.Run("unknown id is JSON 404", func(t *testing.T) {
		var out map[string]string
		resp := getJSON(t, ts, "/api/v1/clusters/999", "", &out)
		if resp.StatusCode != http.StatusNotFound || out["error"] == "" {
			t.Fatalf("status %d body %v, want 404 with error", resp.StatusCode, out)
		}
	})
	t.Run("non-numeric id is 400", func(t *testing.T) {
		var out map[string]string
		resp := getJSON(t, ts, "/api/v1/clusters/abc", "", &out)
		if resp.StatusCode != http.StatusBadRequest || out["error"] == "" {
			t.Fatalf("status %d body %v, want 400 with error", resp.StatusCode, out)
		}
	})
}

func TestReport(t *testing.T) {
	st := newFakeStore()
	ts := httptest.NewServer(newTestServer(t, st).Handler())
	defer ts.Close()
	seedViaPush(t, ts)

	// Overwrite the stored 1.35 report with a marker so the stored-vs-what-if
	// path is distinguishable (a fresh what-if would carry kbVersion test-kb).
	st.mu.Lock()
	for i := range st.evals {
		if st.evals[i].Target == "1.35" {
			st.evals[i].Report = []byte(`{"clusterId":"uid-123","target":"1.35","kbVersion":"stored-marker","score":42,"ready":false,"findings":[]}`)
		}
	}
	st.mu.Unlock()

	t.Run("stored evaluation wins", func(t *testing.T) {
		var rep engine.Report
		resp := getJSON(t, ts, "/api/v1/clusters/1/report?target=1.35", "", &rep)
		if resp.StatusCode != http.StatusOK || rep.KBVersion != "stored-marker" || rep.Score != 42 {
			t.Fatalf("status %d kbVersion %q score %d, want 200 stored-marker 42", resp.StatusCode, rep.KBVersion, rep.Score)
		}
	})
	t.Run("missing target falls back to default target", func(t *testing.T) {
		var rep engine.Report
		resp := getJSON(t, ts, "/api/v1/clusters/1/report", "", &rep)
		if resp.StatusCode != http.StatusOK || rep.KBVersion != "stored-marker" {
			t.Fatalf("status %d kbVersion %q, want 200 stored-marker (default target 1.35)", resp.StatusCode, rep.KBVersion)
		}
	})
	t.Run("what-if for unstored target", func(t *testing.T) {
		var rep engine.Report
		resp := getJSON(t, ts, "/api/v1/clusters/1/report?target=1.40", "", &rep)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200", resp.StatusCode)
		}
		if rep.KBVersion != "test-kb" || rep.Score != 75 || len(rep.Findings) != 1 {
			t.Fatalf("what-if report = kb %q score %d findings %d, want test-kb 75 1", rep.KBVersion, rep.Score, len(rep.Findings))
		}
	})
	t.Run("unparseable target is 422", func(t *testing.T) {
		var out map[string]string
		resp := getJSON(t, ts, "/api/v1/clusters/1/report?target=bogus", "", &out)
		if resp.StatusCode != http.StatusUnprocessableEntity || out["error"] == "" {
			t.Fatalf("status %d body %v, want 422 with error", resp.StatusCode, out)
		}
	})
	t.Run("unknown cluster is 404", func(t *testing.T) {
		var out map[string]string
		resp := getJSON(t, ts, "/api/v1/clusters/999/report?target=1.35", "", &out)
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("status = %d, want 404", resp.StatusCode)
		}
	})
	t.Run("cluster without snapshots is 404", func(t *testing.T) {
		if _, err := st.UpsertCluster(context.Background(), store.Cluster{Name: "bare", LastSeen: time.Now()}); err != nil {
			t.Fatal(err)
		}
		var out map[string]string
		// fakeStore IDs are sequential; the bare cluster is the next ID after
		// seed (cluster 1, snapshot 2, eval 3 → bare cluster 4).
		resp := getJSON(t, ts, "/api/v1/clusters/4/report?target=1.40", "", &out)
		if resp.StatusCode != http.StatusNotFound || out["error"] == "" {
			t.Fatalf("status %d body %v, want 404 with error", resp.StatusCode, out)
		}
	})
}

func TestFindingsFilters(t *testing.T) {
	st := newFakeStore()
	ts := httptest.NewServer(newTestServer(t, st).Handler())
	defer ts.Close()
	seedViaPush(t, ts) // one removed-api blocker at target 1.35

	type findingsResp struct {
		Target   string           `json:"target"`
		Findings []engine.Finding `json:"findings"`
	}
	cases := []struct {
		name  string
		query string
		want  int
	}{
		{"no filter", "", 1},
		{"severity match", "&severity=blocker", 1},
		{"severity no match", "&severity=info", 0},
		{"category match", "&category=removed-api", 1},
		{"category no match", "&category=eol-addon", 0},
		{"both match", "&severity=blocker&category=removed-api", 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var got findingsResp
			resp := getJSON(t, ts, "/api/v1/clusters/1/findings?target=1.35"+tc.query, "", &got)
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status = %d, want 200", resp.StatusCode)
			}
			if got.Target != "1.35" || len(got.Findings) != tc.want {
				t.Fatalf("target %q findings %d, want 1.35 with %d", got.Target, len(got.Findings), tc.want)
			}
			if got.Findings == nil {
				t.Fatal(`findings must render as [] (non-nil), not null`)
			}
		})
	}
}

func TestHistory(t *testing.T) {
	st := newFakeStore()
	ts := httptest.NewServer(newTestServer(t, st).Handler())
	defer ts.Close()
	// Three distinct snapshots (PSP object count varies → different canonical
	// hashes; CollectedAt is zeroed for hashing) → three evaluations for target 1.35.
	for i := 0; i < 3; i++ {
		inv := testInventoryWithPSP()
		inv.APIUsage[0].Count = i + 1 // vary real content so each push is a new snapshot
		resp, _ := postSnapshot(t, ts, "ingest-tok", pushReqBody(t, inv), true)
		if resp.StatusCode != http.StatusAccepted {
			t.Fatalf("push %d status = %d, want 202", i, resp.StatusCode)
		}
	}

	t.Run("default limit", func(t *testing.T) {
		var pts []store.ScorePoint
		resp := getJSON(t, ts, "/api/v1/clusters/1/history?target=1.35", "", &pts)
		if resp.StatusCode != http.StatusOK || len(pts) != 3 {
			t.Fatalf("status %d points %d, want 200 with 3", resp.StatusCode, len(pts))
		}
		for _, p := range pts {
			if p.Score != 75 || p.Ready {
				t.Fatalf("point = %+v, want score 75 ready false", p)
			}
		}
	})
	t.Run("limit applies", func(t *testing.T) {
		var pts []store.ScorePoint
		resp := getJSON(t, ts, "/api/v1/clusters/1/history?target=1.35&limit=2", "", &pts)
		if resp.StatusCode != http.StatusOK || len(pts) != 2 {
			t.Fatalf("status %d points %d, want 200 with 2", resp.StatusCode, len(pts))
		}
	})
	t.Run("missing target uses default", func(t *testing.T) {
		var pts []store.ScorePoint
		resp := getJSON(t, ts, "/api/v1/clusters/1/history", "", &pts)
		if resp.StatusCode != http.StatusOK || len(pts) != 3 {
			t.Fatalf("status %d points %d, want 200 with 3", resp.StatusCode, len(pts))
		}
	})
	t.Run("bad limit is 422", func(t *testing.T) {
		for _, bad := range []string{"abc", "0", "-3"} {
			var out map[string]string
			resp := getJSON(t, ts, "/api/v1/clusters/1/history?target=1.35&limit="+bad, "", &out)
			if resp.StatusCode != http.StatusUnprocessableEntity {
				t.Fatalf("limit=%s status = %d, want 422", bad, resp.StatusCode)
			}
		}
	})
}

func TestMethodNotAllowed(t *testing.T) {
	ts := httptest.NewServer(newTestServer(t, newFakeStore()).Handler())
	defer ts.Close()
	cases := []struct{ method, path string }{
		{http.MethodDelete, "/api/v1/clusters"},
		{http.MethodGet, "/api/v1/snapshots"},
		{http.MethodPost, "/healthz"},
	}
	for _, tc := range cases {
		req, err := http.NewRequest(tc.method, ts.URL+tc.path, nil)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := ts.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusMethodNotAllowed {
			t.Fatalf("%s %s status = %d, want 405 (ServeMux method patterns)", tc.method, tc.path, resp.StatusCode)
		}
		if resp.Header.Get("Allow") == "" {
			t.Fatalf("%s %s: missing Allow header on 405", tc.method, tc.path)
		}
	}
}
