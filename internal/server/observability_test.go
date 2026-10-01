package server

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/abd-ulbasit/upgradescope/internal/server/store"
)

// handleReadyz finds Ping by type assertion; keep the real stores on it.
var (
	_ interface{ Ping(context.Context) error } = (*store.SQLite)(nil)
	_ interface{ Ping(context.Context) error } = (*store.Postgres)(nil)
)

// Ping makes the fake a store /readyz can ping; errs["Ping"] fails it.
func (f *fakeStore) Ping(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.errs["Ping"]
}

func getRaw(t *testing.T, ts *httptest.Server, path, token string) (*http.Response, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, ts.URL+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp, string(body)
}

// /readyz pings the store: the chart's readinessProbe takes the pod out of
// the Service while the database is unreachable. /healthz stays process
// liveness only, so a database outage never restarts the server.
func TestReadyzPingsStore(t *testing.T) {
	st := newFakeStore()
	ts := httptest.NewServer(newTestServer(t, st).Handler())
	defer ts.Close()

	if resp, body := getRaw(t, ts, "/readyz", ""); resp.StatusCode != http.StatusOK {
		t.Fatalf("/readyz with a healthy store = %d %s, want 200", resp.StatusCode, body)
	}
	st.mu.Lock()
	st.errs["Ping"] = errors.New("connection refused")
	st.mu.Unlock()
	resp, body := getRaw(t, ts, "/readyz", "")
	if resp.StatusCode != http.StatusServiceUnavailable || !strings.Contains(resp.Header.Get("Content-Type"), "application/json") {
		t.Fatalf("/readyz with a failing store = %d %q %s, want 503 JSON", resp.StatusCode, resp.Header.Get("Content-Type"), body)
	}
	if resp, _ := getRaw(t, ts, "/healthz", ""); resp.StatusCode != http.StatusOK {
		t.Errorf("/healthz with a failing store = %d, want 200 (liveness)", resp.StatusCode)
	}
}

func TestMetricsAfterIngest(t *testing.T) {
	ts := httptest.NewServer(newTestServer(t, newFakeStore(), func(c *Config) {
		c.ExtraTargets = []string{"1.36"}
	}).Handler())
	defer ts.Close()
	if resp, _ := postSnapshot(t, ts, "ingest-tok", pushReqBody(t, testInventoryWithPSP()), true); resp.StatusCode != http.StatusAccepted {
		t.Fatalf("ingest = %d, want 202", resp.StatusCode)
	}
	if resp, _ := postSnapshot(t, ts, "wrong", pushReqBody(t, testInventory()), true); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("bad-token ingest = %d, want 401", resp.StatusCode)
	}
	getRaw(t, ts, "/api/v1/clusters", "")

	resp, body := getRaw(t, ts, "/metrics", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/metrics = %d %s", resp.StatusCode, body)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/plain; version=0.0.4") {
		t.Errorf("/metrics Content-Type = %q, want Prometheus text format", ct)
	}
	// testInventoryWithPSP on v1.34.2: default target 1.35 (one blocker,
	// score 75) plus the extra target 1.36.
	for _, want := range []string{
		`upgradescope_cluster_score{cluster="prod-eu-1",target="1.35"} 75`,
		`upgradescope_cluster_score{cluster="prod-eu-1",target="1.36"}`,
		`upgradescope_cluster_verdict{cluster="prod-eu-1",target="1.35",verdict="blocked"} 1`,
		`upgradescope_cluster_verdict{cluster="prod-eu-1",target="1.35",verdict="ready"} 0`,
		`upgradescope_cluster_blockers{cluster="prod-eu-1",target="1.35"} 1`,
		`upgradescope_cluster_last_push_age_seconds{cluster="prod-eu-1"} 0`,
		`upgradescope_ingest_total{result="accepted"} 1`,
		`upgradescope_ingest_total{result="unauthorized"} 1`,
		`upgradescope_http_requests_total{code="200",route="GET /api/v1/clusters"} 1`,
		`upgradescope_http_request_duration_seconds_count{route="POST /api/v1/snapshots"} 2`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("/metrics has no %s", want)
		}
	}
}

// With a read token, /metrics is read API like any other: per-cluster
// scores are exactly what the token protects.
func TestMetricsNeedReadToken(t *testing.T) {
	ts := httptest.NewServer(newTestServer(t, newFakeStore(), func(c *Config) { c.ReadToken = "read-tok" }).Handler())
	defer ts.Close()
	if resp, _ := getRaw(t, ts, "/metrics", ""); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("/metrics without token = %d, want 401", resp.StatusCode)
	}
	if resp, _ := getRaw(t, ts, "/metrics", "read-tok"); resp.StatusCode != http.StatusOK {
		t.Errorf("/metrics with token = %d, want 200", resp.StatusCode)
	}
}

// Operational and API paths never fall through to the dashboard: a probe
// or ServiceMonitor pointed at a path the server does not serve must fail
// loudly, not get index.html with 200. Run against the real embedded
// bundle, so it holds whether or not `make web` has staged one.
func TestReservedPathsNeverServeDashboard(t *testing.T) {
	ts := httptest.NewServer(newTestServer(t, newFakeStore()).Handler())
	defer ts.Close()
	for _, p := range []string{"/livez", "/api/v1/nope", "/api/v2/clusters", "/metrics/extra", "/readyz/x"} {
		resp, body := getRaw(t, ts, p, "")
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("GET %s = %d, want 404", p, resp.StatusCode)
		}
		if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "application/json") || !strings.Contains(body, `"error"`) {
			t.Errorf("GET %s: Content-Type %q body %q, want a JSON error", p, ct, body)
		}
	}
	// Wrong methods on registered paths keep their 405 + Allow.
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/readyz", nil)
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed || resp.Header.Get("Allow") == "" {
		t.Errorf("POST /readyz = %d (Allow %q), want 405 with Allow", resp.StatusCode, resp.Header.Get("Allow"))
	}
}
