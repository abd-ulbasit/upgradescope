package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// connect starts a server for cfg and returns a client session on it over an
// in-memory transport, with the same JSON-RPC framing as stdio.
func connect(t *testing.T, cfg Config) *mcpsdk.ClientSession {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	ct, st := mcpsdk.NewInMemoryTransports()
	if _, err := New(cfg).Connect(ctx, st, nil); err != nil {
		t.Fatal(err)
	}
	cs, err := mcpsdk.NewClient(&mcpsdk.Implementation{Name: "test", Version: "0"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}

func call(t *testing.T, cs *mcpsdk.ClientSession, name string, args map[string]any) *mcpsdk.CallToolResult {
	t.Helper()
	res, err := cs.CallTool(context.Background(), &mcpsdk.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("%s: protocol error: %v", name, err)
	}
	return res
}

func text(res *mcpsdk.CallToolResult) string {
	var b strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*mcpsdk.TextContent); ok {
			b.WriteString(tc.Text)
		}
	}
	return b.String()
}

// structured re-encodes a result's structuredContent.
func structured(t *testing.T, res *mcpsdk.CallToolResult) []byte {
	t.Helper()
	if res.IsError {
		t.Fatalf("tool error: %s", text(res))
	}
	raw, err := json.Marshal(res.StructuredContent)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// goldenReport is an engine golden report with the CLI's envelope, written
// to a file as 'scan --output json' would.
func goldenReport(t *testing.T, name string) (path string, doc []byte) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "engine", "testdata", name, "expected.json"))
	if err != nil {
		t.Fatal(err)
	}
	doc = withEnvelope(t, raw)
	path = filepath.Join(t.TempDir(), name+".json")
	if err := os.WriteFile(path, doc, 0o644); err != nil {
		t.Fatal(err)
	}
	return path, doc
}

func localConfig() Config {
	return Config{
		Version: "test",
		Scan: func(context.Context, ScanRequest) (json.RawMessage, error) {
			return nil, errors.New("no scans in this test")
		},
		Inventory: func(string, string) (json.RawMessage, error) {
			return nil, errors.New("no inventories in this test")
		},
	}
}

// TestToolsAreReadOnly: the server exposes the documented tools and no
// others, and every one says it is read-only. A mutating tool added later
// fails here (and so does a read-only claim that is dropped).
func TestToolsAreReadOnly(t *testing.T) {
	fleet := httptest.NewServer(http.NotFoundHandler())
	defer fleet.Close()
	for _, mode := range []struct {
		name  string
		fleet bool
	}{{"local", false}, {"fleet", true}} {
		t.Run(mode.name, func(t *testing.T) {
			cfg := localConfig()
			if mode.fleet {
				f, err := NewFleet(fleet.URL, "")
				if err != nil {
					t.Fatal(err)
				}
				cfg.Fleet = f
			}
			cs := connect(t, cfg)
			res, err := cs.ListTools(context.Background(), nil)
			if err != nil {
				t.Fatal(err)
			}
			var got []string
			for _, tool := range res.Tools {
				got = append(got, tool.Name)
				a := tool.Annotations
				if a == nil || !a.ReadOnlyHint {
					t.Errorf("tool %s is not annotated readOnlyHint", tool.Name)
				}
				if a != nil && a.DestructiveHint != nil && *a.DestructiveHint {
					t.Errorf("tool %s is annotated destructive", tool.Name)
				}
				if tool.InputSchema == nil || tool.OutputSchema == nil {
					t.Errorf("tool %s lacks an input or output schema", tool.Name)
				}
			}
			slices.Sort(got)
			if want := ToolNames(mode.fleet); !slices.Equal(got, want) {
				t.Errorf("tools = %v, want exactly the documented %v", got, want)
			}
			for _, name := range got {
				for _, verb := range []string{"delete", "create", "update", "write", "set", "apply", "rename", "revoke", "remove"} {
					if strings.Contains(name, verb) {
						t.Errorf("tool %s sounds mutating (%s)", name, verb)
					}
				}
			}
		})
	}
}

// TestOutputSchemasAreThePublishedContract: get_report's output schema is
// api/report.schema.json, and the tools that hold reports or findings point
// into its definitions, as the client sees them over the wire.
func TestOutputSchemasAreThePublishedContract(t *testing.T) {
	cs := connect(t, localConfig())
	res, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, tool := range res.Tools {
		raw, _ := json.Marshal(tool.OutputSchema)
		s := string(raw)
		switch tool.Name {
		case ToolGetReport:
			if !strings.Contains(s, `"report.schema.json`) && !strings.Contains(s, "schemaVersion 1") {
				t.Errorf("get_report output schema is not the report schema: %.200s", s)
			}
		case ToolListFindings, ToolScan:
			if !strings.Contains(s, `#/$defs/finding`) && !strings.Contains(s, `#/$defs/report`) {
				t.Errorf("%s output schema does not refer to the report schema's definitions: %.200s", tool.Name, s)
			}
		}
	}
}

func TestListFindingsFromReportFile(t *testing.T) {
	path, doc := goldenReport(t, "mixed-everything")
	cs := connect(t, localConfig())
	var rep struct {
		Target   string `json:"target"`
		Findings []struct {
			Severity string `json:"severity"`
			Category string `json:"category"`
		} `json:"findings"`
	}
	if err := json.Unmarshal(doc, &rep); err != nil {
		t.Fatal(err)
	}
	var blockers int
	for _, f := range rep.Findings {
		if f.Severity == "blocker" {
			blockers++
		}
	}
	if blockers == 0 || blockers == len(rep.Findings) {
		t.Fatalf("fixture has %d blockers of %d findings: it cannot tell a filter from none", blockers, len(rep.Findings))
	}

	out := structured(t, call(t, cs, ToolListFindings, map[string]any{"report_file": path, "severity": "blocker"}))
	var got struct {
		Target    string `json:"target"`
		Total     int    `json:"total"`
		Truncated bool   `json:"truncated"`
		Findings  []struct {
			Severity string `json:"severity"`
		} `json:"findings"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	if got.Target != rep.Target || got.Total != blockers || len(got.Findings) != blockers || got.Truncated {
		t.Errorf("severity=blocker: target %s total %d returned %d truncated %v; want target %s and %d blockers", got.Target, got.Total, len(got.Findings), got.Truncated, rep.Target, blockers)
	}
	for _, f := range got.Findings {
		if f.Severity != "blocker" {
			t.Errorf("severity filter let a %s through", f.Severity)
		}
	}

	// A category with no findings is an empty list, not an error.
	out = structured(t, call(t, cs, ToolListFindings, map[string]any{"report_file": path, "category": "no-such-category"}))
	if !strings.Contains(string(out), `"findings":[]`) || !strings.Contains(string(out), `"total":0`) {
		t.Errorf("empty match = %s", out)
	}

	// limit cuts the list and says so.
	out = structured(t, call(t, cs, ToolListFindings, map[string]any{"report_file": path, "limit": 1}))
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Findings) != 1 || got.Total != len(rep.Findings) || !got.Truncated {
		t.Errorf("limit=1: returned %d of total %d, truncated %v", len(got.Findings), got.Total, got.Truncated)
	}
}

func TestGetReportReturnsTheDocument(t *testing.T) {
	path, doc := goldenReport(t, "clean-cluster")
	cs := connect(t, localConfig())
	out := structured(t, call(t, cs, ToolGetReport, map[string]any{"report_file": path}))
	var a, b map[string]any
	if err := json.Unmarshal(out, &a); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(doc, &b); err != nil {
		t.Fatal(err)
	}
	if a["clusterId"] != b["clusterId"] || a["score"] != b["score"] || a["verdict"] != b["verdict"] {
		t.Errorf("get_report changed the report: %v vs %v", a, b)
	}
	if err := validate(t, resolve(t, ReportOutputSchema()), out); err != nil {
		t.Errorf("get_report output does not validate: %v", err)
	}
}

func TestSourceErrorsAreToolErrors(t *testing.T) {
	path, _ := goldenReport(t, "clean-cluster")
	notReport := filepath.Join(t.TempDir(), "x.json")
	if err := os.WriteFile(notReport, []byte(`{"hello":"world"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	cs := connect(t, localConfig())
	for _, tc := range []struct {
		name string
		tool string
		args map[string]any
		want string
	}{
		{"nothing scanned yet", ToolListFindings, nil, "call scan first"},
		{"two sources", ToolGetReport, map[string]any{"report_file": path, "inventory_file": path}, "name one source"},
		{"not a report", ToolGetReport, map[string]any{"report_file": notReport}, "not an upgradescope JSON report"},
		{"missing file", ToolGetReport, map[string]any{"report_file": filepath.Join(t.TempDir(), "gone.json")}, "no such file"},
		{"wrong target", ToolGetReport, map[string]any{"report_file": path, "target": "1.99"}, "not 1.99"},
		{"inventory needs target", ToolGetReport, map[string]any{"inventory_file": path}, "needs target"},
		{"cluster without fleet mode", ToolGetReport, map[string]any{"cluster": "prod"}, "needs fleet mode"},
		{"bad severity", ToolListFindings, map[string]any{"report_file": path, "severity": "critical"}, "severity"},
		{"bad target", ToolGetReport, map[string]any{"report_file": path, "target": "latest"}, "does not match"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res, err := cs.CallTool(context.Background(), &mcpsdk.CallToolParams{Name: tc.tool, Arguments: tc.args})
			if err != nil {
				t.Fatalf("a bad call must be a tool error the assistant can read, got protocol error: %v", err)
			}
			if !res.IsError || !strings.Contains(text(res), tc.want) {
				t.Errorf("isError=%v text=%q, want an error containing %q", res.IsError, text(res), tc.want)
			}
		})
	}
}

func TestInventoryFileIsJudgedAtTheTarget(t *testing.T) {
	_, doc := goldenReport(t, "clean-cluster")
	var gotPath, gotTarget string
	cfg := localConfig()
	cfg.Inventory = func(path, target string) (json.RawMessage, error) {
		gotPath, gotTarget = path, target
		return doc, nil
	}
	cs := connect(t, cfg)
	structured(t, call(t, cs, ToolListFindings, map[string]any{"inventory_file": "/some/inventory.json", "target": "1.34"}))
	if gotPath != "/some/inventory.json" || gotTarget != "1.34" {
		t.Errorf("Inventory(%q, %q)", gotPath, gotTarget)
	}
}

// TestScanFeedsTheOtherTools: scan returns a report per target, in order,
// and list_findings and get_report then read the latest scan.
func TestScanFeedsTheOtherTools(t *testing.T) {
	_, clean := goldenReport(t, "clean-cluster")
	var reqs []ScanRequest
	cfg := localConfig()
	cfg.Scan = func(_ context.Context, req ScanRequest) (json.RawMessage, error) {
		reqs = append(reqs, req)
		var m map[string]any
		_ = json.Unmarshal(clean, &m)
		m["target"] = req.Target
		return json.Marshal(m)
	}
	cs := connect(t, cfg)

	res := call(t, cs, ToolScan, map[string]any{"targets": []any{"1.34", "1.35"}})
	out := structured(t, res)
	if err := validate(t, resolve(t, ScanOutputSchema()), out); err != nil {
		t.Fatalf("scan output does not validate: %v", err)
	}
	var scanned struct {
		Reports []struct {
			Target string `json:"target"`
		} `json:"reports"`
	}
	if err := json.Unmarshal(out, &scanned); err != nil {
		t.Fatal(err)
	}
	if len(scanned.Reports) != 2 || scanned.Reports[0].Target != "1.34" || scanned.Reports[1].Target != "1.35" {
		t.Errorf("reports = %+v, want 1.34 then 1.35", scanned.Reports)
	}
	if len(reqs) != 2 || reqs[0] != (ScanRequest{Target: "1.34"}) || reqs[1] != (ScanRequest{Target: "1.35"}) {
		t.Errorf("scan requests = %+v, want one per target and nothing else", reqs)
	}

	// Two targets scanned: the other tools need to be told which.
	if r := call(t, cs, ToolGetReport, nil); !r.IsError || !strings.Contains(text(r), "pass target") {
		t.Errorf("get_report with two scanned targets and none named: %q", text(r))
	}
	out = structured(t, call(t, cs, ToolGetReport, map[string]any{"target": "1.35"}))
	if !strings.Contains(string(out), `"target":"1.35"`) {
		t.Errorf("get_report target=1.35 = %.120s", out)
	}
	if r := call(t, cs, ToolGetReport, map[string]any{"target": "1.40"}); !r.IsError || !strings.Contains(text(r), "1.34, 1.35") {
		t.Errorf("get_report for an unscanned target: %q", text(r))
	}
}

func TestScanInputIsChecked(t *testing.T) {
	cfg := localConfig()
	cfg.Scan = func(context.Context, ScanRequest) (json.RawMessage, error) {
		t.Error("an invalid scan request reached the scanner")
		return nil, errors.New("unreachable")
	}
	cs := connect(t, cfg)
	for _, tc := range []struct {
		name string
		args map[string]any
	}{
		{"no targets", map[string]any{"targets": []any{}}},
		{"too many", map[string]any{"targets": []any{"1.30", "1.31", "1.32", "1.33", "1.34"}}},
		{"not a minor", map[string]any{"targets": []any{"v1.34.2"}}},
		{"duplicate", map[string]any{"targets": []any{"1.34", "1.34"}}},
		// Which cluster is read is the command's flags and the environment, never a call's.
		{"kubeconfig", map[string]any{"targets": []any{"1.34"}, "kubeconfig": "/etc/other/kubeconfig"}},
		{"context", map[string]any{"targets": []any{"1.34"}, "context": "other"}},
		{"files", map[string]any{"targets": []any{"1.34"}, "files": "/some/dir"}},
		{"unknown input", map[string]any{"targets": []any{"1.34"}, "namespace": "x"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if res := call(t, cs, ToolScan, tc.args); !res.IsError {
				t.Errorf("accepted: %s", text(res))
			}
		})
	}
}

func TestRegistryLookup(t *testing.T) {
	cs := connect(t, localConfig())
	out := structured(t, call(t, cs, ToolRegistryLookup, map[string]any{"query": "Ingress-NGINX"}))
	var got struct {
		AddOns []struct {
			ID      string `json:"id"`
			Support struct {
				Status string `json:"status"`
			} `json:"support"`
		} `json:"addons"`
		Total int `json:"total"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, a := range got.AddOns {
		if a.ID == "ingress-nginx" {
			found = true
			if a.Support.Status != "eol" {
				t.Errorf("ingress-nginx support = %q, want eol (retired upstream)", a.Support.Status)
			}
		}
	}
	if !found || got.Total != len(got.AddOns) {
		t.Errorf("lookup ingress-nginx = %d add-ons, total %d, found %v", len(got.AddOns), got.Total, found)
	}
	// A chart name finds its add-on too.
	out = structured(t, call(t, cs, ToolRegistryLookup, map[string]any{"query": "containerd", "limit": 1}))
	if !strings.Contains(string(out), `"id":"containerd"`) && !strings.Contains(string(out), `"truncated":true`) {
		t.Errorf("lookup containerd = %.200s", out)
	}
	if r := call(t, cs, ToolRegistryLookup, map[string]any{"query": " "}); !r.IsError {
		t.Error("an empty query was accepted")
	}
}

// fakeFleet is a stand-in for `serve`: a read token, three endpoints.
func fakeFleet(t *testing.T, token string, report []byte) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var calls atomic.Int32
	mux := http.NewServeMux()
	auth := func(h http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			calls.Add(1)
			if token != "" && r.Header.Get("Authorization") != "Bearer "+token {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = w.Write([]byte(`{"error":"missing or invalid read token"}`))
				return
			}
			h(w, r)
		}
	}
	mux.HandleFunc("GET /api/v1/clusters", auth(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`[{"id":7,"name":"prod"},{"id":9,"name":"dev"}]`))
	}))
	mux.HandleFunc("GET /api/v1/clusters/{id}/report", auth(func(w http.ResponseWriter, r *http.Request) {
		if r.PathValue("id") != "7" {
			http.NotFound(w, r)
			return
		}
		var m map[string]any
		_ = json.Unmarshal(report, &m)
		if tg := r.URL.Query().Get("target"); tg != "" {
			m["target"] = tg
		}
		_ = json.NewEncoder(w).Encode(m)
	}))
	mux.HandleFunc("GET /api/v1/fleet", auth(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"targets":["` + r.URL.Query().Get("targets") + `"],"clusters":[]}`))
	}))
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return ts, &calls
}

func TestFleetModeUsesTheServersReadAuth(t *testing.T) {
	_, doc := goldenReport(t, "mixed-everything")
	ts, _ := fakeFleet(t, "s3cret", doc)

	connectFleet := func(token string) *mcpsdk.ClientSession {
		f, err := NewFleet(ts.URL, token)
		if err != nil {
			t.Fatal(err)
		}
		cfg := localConfig()
		cfg.Fleet = f
		return connect(t, cfg)
	}

	// No token: the server's 401 comes back as the tool's error.
	cs := connectFleet("")
	for _, tc := range []struct {
		tool string
		args map[string]any
	}{
		{ToolFleetSummary, nil},
		{ToolGetReport, map[string]any{"cluster": "prod"}},
		{ToolListFindings, map[string]any{"cluster": "prod"}},
	} {
		res := call(t, cs, tc.tool, tc.args)
		if !res.IsError || !strings.Contains(text(res), "401") || !strings.Contains(text(res), "read token") {
			t.Errorf("%s without a token: isError=%v %q, want the server's 401", tc.tool, res.IsError, text(res))
		}
	}
	// A wrong token is rejected the same way.
	if res := call(t, connectFleet("wrong"), ToolFleetSummary, nil); !res.IsError || !strings.Contains(text(res), "401") {
		t.Errorf("fleet_summary with a wrong token: %q", text(res))
	}

	// The right token works for all three.
	cs = connectFleet("s3cret")
	out := structured(t, call(t, cs, ToolGetReport, map[string]any{"cluster": "prod", "target": "1.38"}))
	if err := validate(t, resolve(t, ReportOutputSchema()), out); err != nil {
		t.Errorf("fleet report does not validate against the report schema: %v", err)
	}
	if !strings.Contains(string(out), `"target":"1.38"`) {
		t.Errorf("target was not passed to the server: %.150s", out)
	}
	out = structured(t, call(t, cs, ToolListFindings, map[string]any{"cluster": "prod", "severity": "blocker"}))
	if !strings.Contains(string(out), `"cluster":"prod"`) || !strings.Contains(string(out), `"severity":"blocker"`) {
		t.Errorf("list_findings via the server = %.200s", out)
	}
	out = structured(t, call(t, cs, ToolFleetSummary, map[string]any{"targets": []any{"1.37", "1.38"}}))
	if !strings.Contains(string(out), "1.37,1.38") {
		t.Errorf("fleet_summary did not pass the targets: %s", out)
	}

	// A cluster the server does not know, by name or id.
	if res := call(t, cs, ToolGetReport, map[string]any{"cluster": "nope"}); !res.IsError || !strings.Contains(text(res), `no cluster "nope"`) {
		t.Errorf("unknown cluster: %q", text(res))
	}
	out = structured(t, call(t, cs, ToolGetReport, map[string]any{"cluster": "7"}))
	if len(out) == 0 {
		t.Error("a cluster named by numeric id was not found")
	}

	// Without a cluster, in fleet mode, there is nothing to read.
	if res := call(t, cs, ToolGetReport, nil); !res.IsError || !strings.Contains(text(res), "or cluster") {
		t.Errorf("fleet mode, no source: %q", text(res))
	}
}

func TestFleetRefusesAServerThatIsNotOne(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/report") {
			_, _ = w.Write([]byte(`{"hello":"world"}`))
			return
		}
		_, _ = w.Write([]byte(`[{"id":1,"name":"prod"}]`))
	}))
	defer ts.Close()
	f, _ := NewFleet(ts.URL, "")
	cfg := localConfig()
	cfg.Fleet = f
	res := call(t, connect(t, cfg), ToolGetReport, map[string]any{"cluster": "prod"})
	if !res.IsError || !strings.Contains(text(res), "is not a report") {
		t.Errorf("a non-report response: %q", text(res))
	}
	if _, err := NewFleet("ftp://example.com", ""); err == nil {
		t.Error("NewFleet accepted an ftp URL")
	}
}
