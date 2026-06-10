package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/abd-ulbasit/upgradescope/internal/engine"
	"github.com/abd-ulbasit/upgradescope/internal/inventory"
	"github.com/abd-ulbasit/upgradescope/internal/server/store"
)

const pspManifest = `apiVersion: policy/v1beta1
kind: PodSecurityPolicy
metadata:
  name: restricted
  namespace: payments-prod
`

// postGate POSTs a YAML manifest stream to /api/v1/gate.
func postGate(t *testing.T, ts *httptest.Server, query, token, body, contentType string) (*http.Response, []byte) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, ts.URL+"/api/v1/gate"+query, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatalf("POST /api/v1/gate: %v", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp, raw
}

func TestGateManifestsOnly(t *testing.T) {
	s := newTestServer(t, newFakeStore())
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	resp, raw := postGate(t, ts, "?target=1.35", "", pspManifest, "application/x-yaml")
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, body %s", resp.StatusCode, raw)
	}
	var rep engine.Report
	if err := json.Unmarshal(raw, &rep); err != nil {
		t.Fatalf("response is not a report: %v\n%s", err, raw)
	}
	if rep.ClusterID != "manifests" {
		t.Errorf("clusterId = %q, want manifests (no cluster context)", rep.ClusterID)
	}
	if rep.Ready || rep.Score != 75 || len(rep.Findings) != 1 {
		t.Fatalf("report = score %d ready %v findings %d, want 75/not-ready/1", rep.Score, rep.Ready, len(rep.Findings))
	}
	f := rep.Findings[0]
	if f.Severity != engine.SevBlocker || f.Category != engine.CatRemovedAPI {
		t.Fatalf("finding = %+v, want removed-api blocker", f)
	}
	if len(f.Teams) != 0 {
		t.Errorf("teams = %v, want none without cluster context", f.Teams)
	}
}

func TestGateSARIF(t *testing.T) {
	s := newTestServer(t, newFakeStore(), func(c *Config) { c.Version = "v-test" })
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	resp, raw := postGate(t, ts, "?target=1.35&format=sarif", "", pspManifest, "application/x-yaml")
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, body %s", resp.StatusCode, raw)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/sarif+json" {
		t.Errorf("Content-Type = %q, want application/sarif+json", ct)
	}
	var log struct {
		Version string `json:"version"`
		Runs    []struct {
			Tool struct {
				Driver struct {
					Name    string `json:"name"`
					Version string `json:"version"`
				} `json:"driver"`
			} `json:"tool"`
			Results []struct {
				RuleID string `json:"ruleId"`
				Level  string `json:"level"`
			} `json:"results"`
		} `json:"runs"`
	}
	if err := json.Unmarshal(raw, &log); err != nil {
		t.Fatalf("not SARIF JSON: %v\n%s", err, raw)
	}
	if log.Version != "2.1.0" || len(log.Runs) != 1 {
		t.Fatalf("sarif envelope = %+v", log)
	}
	if log.Runs[0].Tool.Driver.Version != "v-test" {
		t.Errorf("tool version = %q, want v-test", log.Runs[0].Tool.Driver.Version)
	}
	if len(log.Runs[0].Results) != 1 || log.Runs[0].Results[0].Level != "error" || log.Runs[0].Results[0].RuleID != "removed-api" {
		t.Fatalf("results = %+v, want one removed-api error", log.Runs[0].Results)
	}
}

// With ?cluster= the manifests are evaluated inside the cluster's stored
// context: namespace team labels (and add-ons/skew data) come from the
// cluster's latest inventory; only APIUsage is replaced by the manifests.
func TestGateClusterContextMerge(t *testing.T) {
	st := newFakeStore()
	s := newTestServer(t, st)
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	inv := testInventory() // uid-123, v1.34.2
	inv.Namespaces = []inventory.NamespaceInfo{{Name: "payments-prod", Team: "payments"}}
	resp, out := postSnapshot(t, ts, "ingest-tok", pushReqBody(t, inv), false)
	if resp.StatusCode != 202 {
		t.Fatalf("seed push = %d (%v)", resp.StatusCode, out)
	}

	for _, clusterRef := range []string{"1", "prod-eu-1"} {
		resp, raw := postGate(t, ts, "?target=1.35&cluster="+clusterRef, "", pspManifest, "application/x-yaml")
		if resp.StatusCode != 200 {
			t.Fatalf("cluster=%s status = %d, body %s", clusterRef, resp.StatusCode, raw)
		}
		var rep engine.Report
		if err := json.Unmarshal(raw, &rep); err != nil {
			t.Fatal(err)
		}
		if rep.ClusterID != "uid-123" {
			t.Errorf("cluster=%s clusterId = %q, want uid-123 (cluster context)", clusterRef, rep.ClusterID)
		}
		if len(rep.Findings) != 1 {
			t.Fatalf("cluster=%s findings = %+v", clusterRef, rep.Findings)
		}
		if got := rep.Findings[0].Teams; !reflect.DeepEqual(got, []string{"payments"}) {
			t.Errorf("cluster=%s teams = %v, want [payments] from cluster namespace labels", clusterRef, got)
		}
	}
}

func TestGateErrors(t *testing.T) {
	st := newFakeStore()
	s := newTestServer(t, st, func(c *Config) { c.ReadToken = "read-tok" })
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()
	// A cluster that exists but has no snapshots: nothing to merge → 404.
	if _, err := st.UpsertCluster(t.Context(), store.Cluster{Name: "empty"}); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name        string
		query       string
		token       string
		body        string
		contentType string
		want        int
	}{
		{"no token", "?target=1.35", "", pspManifest, "application/x-yaml", 401},
		{"missing target", "", "read-tok", pspManifest, "application/x-yaml", 422},
		{"bad target", "?target=banana", "read-tok", pspManifest, "application/x-yaml", 422},
		{"bad format", "?target=1.35&format=xml", "read-tok", pspManifest, "application/x-yaml", 422},
		{"bad yaml", "?target=1.35", "read-tok", "kind: [broken", "application/x-yaml", 422},
		{"unknown cluster", "?target=1.35&cluster=nope", "read-tok", pspManifest, "application/x-yaml", 404},
		{"cluster without snapshots", "?target=1.35&cluster=empty", "read-tok", pspManifest, "application/x-yaml", 404},
		{"unsupported content type", "?target=1.35", "read-tok", pspManifest, "application/gzip", 415},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp, raw := postGate(t, ts, tc.query, tc.token, tc.body, tc.contentType)
			if resp.StatusCode != tc.want {
				t.Fatalf("status = %d, want %d (body %s)", resp.StatusCode, tc.want, raw)
			}
		})
	}
}
