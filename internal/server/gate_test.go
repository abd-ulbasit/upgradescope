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
	"github.com/abd-ulbasit/upgradescope/internal/sarif/sariftest"
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

	resp, raw := postGate(t, ts, "?target=1.35&fail-on=never", "", pspManifest, "application/x-yaml")
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

// A kind: List body is expanded like --files does: removed APIs inside the
// List must block, not pass as one harmless v1/List object.
func TestGateExpandsList(t *testing.T) {
	s := newTestServer(t, newFakeStore())
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	// testKB knows only PodSecurityPolicy: one direct item, one in a
	// nested List.
	list := `apiVersion: v1
kind: List
items:
- apiVersion: policy/v1beta1
  kind: PodSecurityPolicy
  metadata: {name: restricted}
- apiVersion: v1
  kind: List
  items:
  - apiVersion: policy/v1beta1
    kind: PodSecurityPolicy
    metadata: {name: privileged}
`
	resp, raw := postGate(t, ts, "?target=1.35&fail-on=never", "", list, "application/x-yaml")
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, body %s", resp.StatusCode, raw)
	}
	var rep engine.Report
	if err := json.Unmarshal(raw, &rep); err != nil {
		t.Fatalf("response is not a report: %v\n%s", err, raw)
	}
	if rep.Ready || len(rep.Findings) != 1 {
		t.Fatalf("ready=%v findings=%+v, want not ready with one finding", rep.Ready, rep.Findings)
	}
	f := rep.Findings[0]
	if f.Severity != engine.SevBlocker || f.Key != "removed-api/policy/v1beta1/PodSecurityPolicy" || !strings.Contains(f.Title, "(2 objects)") {
		t.Fatalf("finding = %+v, want the removed PSP blocker covering both List items", f)
	}
}

// A concatenated JSON body (NDJSON, `jq '.items[]'`) is decoded object by
// object, as kubectl decodes it (#119): a removed API after the first
// object blocks.
func TestGateConcatenatedJSON(t *testing.T) {
	ts := httptest.NewServer(newTestServer(t, newFakeStore()).Handler())
	defer ts.Close()

	body := `{"apiVersion":"apps/v1","kind":"Deployment","metadata":{"name":"web"}}` + "\n" +
		`{"apiVersion":"policy/v1beta1","kind":"PodSecurityPolicy","metadata":{"name":"restricted"}}` + "\n"
	resp, raw := postGate(t, ts, "?target=1.35&fail-on=blocker", "", body, "application/json")
	if resp.StatusCode != http.StatusUnprocessableEntity || resp.Header.Get("X-Upgradescope-Verdict") != "blocked" {
		t.Fatalf("status = %d verdict %q, want 422 blocked (body %s)", resp.StatusCode, resp.Header.Get("X-Upgradescope-Verdict"), raw)
	}
	var rep engine.Report
	if err := json.Unmarshal(raw, &rep); err != nil {
		t.Fatal(err)
	}
	if len(rep.Findings) != 1 || rep.Findings[0].Key != "removed-api/policy/v1beta1/PodSecurityPolicy" ||
		len(rep.Findings[0].Objects) != 1 || rep.Findings[0].Objects[0].Line != 2 {
		t.Errorf("findings = %+v, want the PSP on stream line 2", rep.Findings)
	}
}

func TestGateSARIF(t *testing.T) {
	s := newTestServer(t, newFakeStore(), func(c *Config) { c.Version = "v-test" })
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	resp, raw := postGate(t, ts, "?target=1.35&format=sarif&fail-on=never", "", pspManifest, "application/x-yaml")
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
			Invocations []struct {
				ExecutionSuccessful        bool `json:"executionSuccessful"`
				ToolExecutionNotifications []struct {
					Level   string `json:"level"`
					Message struct {
						Text string `json:"text"`
					} `json:"message"`
					Properties map[string]string `json:"properties"`
				} `json:"toolExecutionNotifications"`
			} `json:"invocations"`
			Results []struct {
				RuleID string `json:"ruleId"`
				Level  string `json:"level"`
			} `json:"results"`
			Properties struct {
				Ready           *bool `json:"ready"`
				Score           int   `json:"score"`
				OmittedFindings int   `json:"omittedFindings"`
			} `json:"properties"`
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
	sariftest.AssertGitHubAcceptable(t, raw)
	// A posted stream has no file names, and GitHub rejects a SARIF result
	// without a physical location, so the gate's findings carry no SARIF
	// results...
	run := log.Runs[0]
	if raw := string(raw); len(run.Results) != 0 || !strings.Contains(raw, `"results": []`) {
		t.Fatalf("results = %+v, want none (no file locations in a posted stream)\n%s", run.Results, raw)
	}
	// ...but the document must not read as a clean pass: the blocker is an
	// error notification naming the object and its stream line, and the
	// run records the failed verdict.
	if p := run.Properties; p.Ready == nil || *p.Ready || p.Score != 75 || p.OmittedFindings != 1 {
		t.Errorf("run.properties = %+v, want ready=false score=75 omittedFindings=1", p)
	}
	if len(run.Invocations) != 1 || !run.Invocations[0].ExecutionSuccessful || len(run.Invocations[0].ToolExecutionNotifications) != 1 {
		t.Fatalf("invocations = %+v, want one notification for the omitted blocker\n%s", run.Invocations, raw)
	}
	n := run.Invocations[0].ToolExecutionNotifications[0]
	if n.Level != "error" || n.Properties["findingKey"] != "removed-api/policy/v1beta1/PodSecurityPolicy" {
		t.Errorf("notification = %+v, want an error for the removed PSP", n)
	}
	for _, part := range []string{"PodSecurityPolicy", "payments-prod/restricted (line 1)"} {
		if !strings.Contains(n.Message.Text, part) {
			t.Errorf("notification message %q lacks %q", n.Message.Text, part)
		}
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
		resp, raw := postGate(t, ts, "?target=1.35&fail-on=never&cluster="+clusterRef, "", pspManifest, "application/x-yaml")
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
		// 400, not the gate's own 422: a target the knowledge base cannot
		// judge is a mistake in the request (#237).
		{"bad target", "?target=banana", "read-tok", pspManifest, "application/x-yaml", 400},
		{"truncated target (YAML 1.30)", "?target=1.3", "read-tok", pspManifest, "application/x-yaml", 400},
		{"target below the knowledge base", "?target=1.15", "read-tok", pspManifest, "application/x-yaml", 400},
		{"major 2", "?target=2.30", "read-tok", pspManifest, "application/x-yaml", 400},
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

// A truncated target (what YAML makes of target: 1.30) is a 400 that says
// to quote the version, never a verdict: the manifests would otherwise be
// judged against a Kubernetes older than anything the knowledge base
// covers and read ready (#237). 1.16, the oldest minor covered, and above
// are judged as before.
func TestGateRefusesTargetBelowTheKnowledgeBase(t *testing.T) {
	s := newTestServer(t, newFakeStore(), func(*Config) {})
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()
	for _, target := range []string{"1.3", "1.4", "1.5", "1.0", "1.15"} {
		resp, raw := postGate(t, ts, "?target="+target, "", pspManifest, "application/x-yaml")
		if resp.StatusCode != http.StatusBadRequest || resp.Header.Get("X-Upgradescope-Verdict") != "" ||
			!strings.Contains(string(raw), "oldest minor the knowledge base covers is 1.16") {
			t.Errorf("target %s: status = %d, verdict %q, body %s; want 400, no verdict, the floor named", target, resp.StatusCode, resp.Header.Get("X-Upgradescope-Verdict"), raw)
		}
	}
	_, raw := postGate(t, ts, "?target=1.3", "", pspManifest, "application/x-yaml")
	if !strings.Contains(string(raw), `is this 1.30 written as a YAML number? quote it`) {
		t.Errorf("body %s does not name the YAML pitfall", raw)
	}
	for target, want := range map[string]int{"1.16": 200, "1.24": 200, "1.35": 422} {
		if resp, raw := postGate(t, ts, "?target="+target, "", pspManifest, "application/x-yaml"); resp.StatusCode != want {
			t.Errorf("target %s: status = %d, want %d (body %s)", target, resp.StatusCode, want, raw)
		}
	}
}

// serve --targets (Config.ExtraTargets) below the knowledge base fails the
// start, naming the value, instead of evaluating every push against a
// Kubernetes older than anything covered (#237).
func TestNewRefusesExtraTargetsBelowTheKnowledgeBase(t *testing.T) {
	_, err := New(Config{Store: newFakeStore(), KB: testKB(), IngestToken: "t", ExtraTargets: []string{"1.37", "1.3"}})
	if err == nil || !strings.Contains(err.Error(), `"1.3"`) || !strings.Contains(err.Error(), "oldest minor the knowledge base covers is 1.16") {
		t.Fatalf("New with extra target 1.3: err = %v, want one naming the value and the floor", err)
	}
	if _, err := New(Config{Store: newFakeStore(), KB: testKB(), IngestToken: "t", ExtraTargets: []string{"1.16", "1.30"}}); err != nil {
		t.Fatalf("New with extra targets 1.16 and 1.30: %v", err)
	}
}
