package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/abd-ulbasit/upgradescope/internal/engine"
	"github.com/abd-ulbasit/upgradescope/internal/inventory"
	"github.com/abd-ulbasit/upgradescope/internal/kb"
	"github.com/abd-ulbasit/upgradescope/internal/mcp"
	"github.com/abd-ulbasit/upgradescope/internal/server"
	"github.com/abd-ulbasit/upgradescope/internal/server/store"
	"github.com/abd-ulbasit/upgradescope/internal/suppress"
)

// mixedInventory is the engine's mixed-everything fixture: an inventory with
// blockers, warnings and info findings at target 1.38.
const mixedInventory = "../engine/testdata/mixed-everything/inventory.json"

// startMCP runs the real `upgradescope mcp` command with args, on pipes in
// place of stdin and stdout, and returns an MCP client session connected to
// it. Closing the client ends the command, which must then return nil.
func startMCP(t *testing.T, args ...string) *mcpsdk.ClientSession {
	t.Helper()
	return startMCPStderr(t, io.Discard, args...)
}

// startMCPStderr is startMCP with the command's stderr written to stderr.
func startMCPStderr(t *testing.T, stderr io.Writer, args ...string) *mcpsdk.ClientSession {
	t.Helper()
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	cmd := Root()
	cmd.SetArgs(append([]string{"mcp"}, args...))
	cmd.SetIn(inR)
	cmd.SetOut(outW)
	cmd.SetErr(stderr)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		err := cmd.ExecuteContext(ctx)
		_ = outW.Close()
		done <- err
	}()
	cs, err := mcpsdk.NewClient(&mcpsdk.Implementation{Name: "test", Version: "0"}, nil).
		Connect(ctx, &mcpsdk.IOTransport{Reader: outR, Writer: inW}, nil)
	if err != nil {
		cancel()
		t.Fatalf("connect to `upgradescope mcp %v`: %v (the command returned %v)", args, err, <-done)
	}
	t.Cleanup(func() {
		_ = cs.Close()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("mcp command ended with %v after the client hung up", err)
			}
		case <-time.After(10 * time.Second):
			t.Error("mcp command did not end after the client hung up")
		}
		cancel()
	})
	return cs
}

func callMCP(t *testing.T, cs *mcpsdk.ClientSession, tool string, args map[string]any) *mcpsdk.CallToolResult {
	t.Helper()
	res, err := cs.CallTool(context.Background(), &mcpsdk.CallToolParams{Name: tool, Arguments: args})
	if err != nil {
		t.Fatalf("%s: %v", tool, err)
	}
	return res
}

func mcpText(res *mcpsdk.CallToolResult) string {
	var b strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*mcpsdk.TextContent); ok {
			b.WriteString(tc.Text)
		}
	}
	return b.String()
}

// reportSchemaAt compiles api/report.schema.json (with the repository's
// validator, not the one the MCP SDK uses) and returns the schema at
// fragment ("" = the report, "#/$defs/finding" = a finding).
func reportSchemaAt(t *testing.T, fragment string) *jsonschema.Schema {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(repoRoot, "api", "report.schema.json"))
	if err != nil {
		t.Fatal(err)
	}
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	c := jsonschema.NewCompiler()
	if err := c.AddResource("report.schema.json", doc); err != nil {
		t.Fatal(err)
	}
	sch, err := c.Compile("report.schema.json" + fragment)
	if err != nil {
		t.Fatal(err)
	}
	return sch
}

func validateJSON(t *testing.T, sch *jsonschema.Schema, doc any) error {
	t.Helper()
	raw, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	inst, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	return sch.Validate(inst)
}

// findingsResult is list_findings' structured output.
type findingsResult struct {
	Target    string           `json:"target"`
	Cluster   string           `json:"cluster"`
	Total     int              `json:"total"`
	Truncated bool             `json:"truncated"`
	Findings  []map[string]any `json:"findings"`
}

// validateFindings checks every finding a list_findings result holds
// against the published schema.
func validateFindings(t *testing.T, res *mcpsdk.CallToolResult) findingsResult {
	t.Helper()
	if res.IsError {
		t.Fatalf("tool error: %s", mcpText(res))
	}
	raw, _ := json.Marshal(res.StructuredContent)
	var out findingsResult
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	finding := reportSchemaAt(t, "#/$defs/finding")
	for _, f := range out.Findings {
		if err := validateJSON(t, finding, f); err != nil {
			t.Errorf("finding %v does not validate against api/report.schema.json: %v", f["key"], err)
		}
	}
	return out
}

// TestMCPStdioListsOnlyReadOnlyToolsAndListsFindings is the issue's
// integration test: an MCP client talks to the real command over its stdio
// framing, lists the tools (all read-only), calls list_findings on a fixture
// inventory, and the findings validate against the published report schema.
func TestMCPStdioListsOnlyReadOnlyToolsAndListsFindings(t *testing.T) {
	cs := startMCP(t)
	tools, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tool := range tools.Tools {
		names = append(names, tool.Name)
		if tool.Annotations == nil || !tool.Annotations.ReadOnlyHint {
			t.Errorf("tool %s is not read-only", tool.Name)
		}
	}
	slices.Sort(names)
	if want := mcp.ToolNames(false); !slices.Equal(names, want) {
		t.Fatalf("tools = %v, want %v", names, want)
	}

	all := validateFindings(t, callMCP(t, cs, mcp.ToolListFindings, map[string]any{"inventory_file": mixedInventory, "target": "1.38", "limit": 500}))
	if all.Target != "1.38" || len(all.Findings) == 0 || all.Total != len(all.Findings) {
		t.Fatalf("list_findings = target %s, %d findings of %d", all.Target, len(all.Findings), all.Total)
	}
	blockers := validateFindings(t, callMCP(t, cs, mcp.ToolListFindings, map[string]any{"inventory_file": mixedInventory, "target": "1.38", "severity": "blocker"}))
	if len(blockers.Findings) == 0 || len(blockers.Findings) >= len(all.Findings) {
		t.Errorf("severity=blocker returned %d of %d findings", len(blockers.Findings), len(all.Findings))
	}
	for _, f := range blockers.Findings {
		if f["severity"] != "blocker" {
			t.Errorf("a %v finding passed the blocker filter", f["severity"])
		}
	}
	cat := all.Findings[0]["category"].(string)
	byCat := validateFindings(t, callMCP(t, cs, mcp.ToolListFindings, map[string]any{"inventory_file": mixedInventory, "target": "1.38", "category": cat}))
	for _, f := range byCat.Findings {
		if f["category"] != cat {
			t.Errorf("category %s filter let %v through", cat, f["category"])
		}
	}

	// get_report is the whole document `scan --output json` writes.
	rep := callMCP(t, cs, mcp.ToolGetReport, map[string]any{"inventory_file": mixedInventory, "target": "1.38"})
	if rep.IsError {
		t.Fatal(mcpText(rep))
	}
	if err := validateJSON(t, reportSchemaAt(t, ""), rep.StructuredContent); err != nil {
		t.Errorf("get_report does not validate against api/report.schema.json: %v", err)
	}

	// A report is not an inventory, and says so rather than scoring 100.
	res := callMCP(t, cs, mcp.ToolGetReport, map[string]any{"inventory_file": "../engine/testdata/mixed-everything/expected.json", "target": "1.38"})
	if !res.IsError || !strings.Contains(mcpText(res), "not an upgradescope inventory") {
		t.Errorf("an expected.json as inventory_file: isError=%v %q", res.IsError, mcpText(res))
	}
}

// fixtureEvaluator returns the engine's judgement of the mixed-everything
// inventory at a target: real findings of every severity, as a scan of a
// cluster with that inventory would produce. It loads everything up front,
// by absolute path, so it can run on the server's goroutine (where t.Fatal
// must not be called) and after the test has changed directory.
func fixtureEvaluator(t *testing.T) func(inventory.Version) engine.Report {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(repoRoot, "internal", "engine", "testdata", "mixed-everything", "inventory.json"))
	if err != nil {
		t.Fatal(err)
	}
	var inv inventory.Inventory
	if err := json.Unmarshal(raw, &inv); err != nil {
		t.Fatal(err)
	}
	k, err := kb.Load()
	if err != nil {
		t.Fatal(err)
	}
	return func(target inventory.Version) engine.Report { return engine.Evaluate(inv, k, target, time.Now()) }
}

// The real cluster read and kubeconfig lookup of `mcp`, for the tests that
// exercise them against a stand-in; every other test gets the stand-ins
// init installs, so no test here reads the user's kubeconfig or cluster.
var (
	realRunMCPScan         = runMCPScan
	realCurrentKubeContext = currentKubeContext
)

func init() {
	runMCPScan = func(context.Context, []scanOptions) ([]engine.Report, error) {
		return nil, errors.New("test bug: runMCPScan is not stubbed (stubScan)")
	}
	currentKubeContext = func(string) (string, error) { return stubContext, nil }
}

// stubContext is the context the stand-in for the kubeconfig lookup says
// is current.
const stubContext = "stub-current-context"

// stubScan replaces the cluster read of `scan` with the fixture inventory,
// recording the options each target of a scan was given.
func stubScan(t *testing.T) *[]scanOptions {
	t.Helper()
	var got []scanOptions
	evaluate := fixtureEvaluator(t)
	orig := runMCPScan
	runMCPScan = func(_ context.Context, opts []scanOptions) ([]engine.Report, error) {
		got = append(got, opts...)
		reports := make([]engine.Report, len(opts))
		for i, o := range opts {
			reports[i] = evaluate(o.targetVersion)
		}
		return reports, nil
	}
	t.Cleanup(func() { runMCPScan = orig })
	return &got
}

// TestMCPScanFeedsTheOtherTools runs scan through the command's whole
// pipeline (option validation, the scan, the JSON writer) on the fixture
// inventory, then reads the result back with list_findings and get_report;
// everything validates against the published schema.
func TestMCPScanFeedsTheOtherTools(t *testing.T) {
	scans := stubScan(t)
	cs := startMCP(t)
	res := callMCP(t, cs, mcp.ToolScan, map[string]any{"targets": []any{"1.38"}})
	if res.IsError {
		t.Fatal(mcpText(res))
	}
	sc := struct {
		Reports []map[string]any `json:"reports"`
	}{}
	raw, _ := json.Marshal(res.StructuredContent)
	if err := json.Unmarshal(raw, &sc); err != nil {
		t.Fatal(err)
	}
	if len(sc.Reports) != 1 {
		t.Fatalf("%d reports, want 1", len(sc.Reports))
	}
	if err := validateJSON(t, reportSchemaAt(t, ""), sc.Reports[0]); err != nil {
		t.Errorf("scan's report does not validate against api/report.schema.json: %v", err)
	}
	if sc.Reports[0]["verdict"] != "blocked" || sc.Reports[0]["target"] != "1.38" || sc.Reports[0]["schemaVersion"] != float64(1) {
		t.Errorf("report = verdict %v target %v schemaVersion %v, want the blocked 1.38 report `scan --output json` writes", sc.Reports[0]["verdict"], sc.Reports[0]["target"], sc.Reports[0]["schemaVersion"])
	}
	if len(*scans) != 1 || (*scans)[0].output != "json" || (*scans)[0].filesDir != "" {
		t.Errorf("scans = %+v, want one live JSON scan", *scans)
	}

	found := validateFindings(t, callMCP(t, cs, mcp.ToolListFindings, map[string]any{"severity": "blocker"}))
	if found.Target != "1.38" || found.Total == 0 {
		t.Errorf("list_findings after scan = target %s, %d blockers", found.Target, found.Total)
	}
	rep := callMCP(t, cs, mcp.ToolGetReport, nil)
	if rep.IsError {
		t.Fatal(mcpText(rep))
	}
	if err := validateJSON(t, reportSchemaAt(t, ""), rep.StructuredContent); err != nil {
		t.Errorf("get_report after scan does not validate against api/report.schema.json: %v", err)
	}
}

// TestMCPScanHonoursOnlyKubeconfigContextAndTheEnvironment: the cluster a
// scan reads is exactly what the command's --kubeconfig and --context name;
// with no --context, the kubeconfig's current context when the server
// starts, by clientcmd's standard rules ($KUBECONFIG, or --kubeconfig), as
// for `scan`, and pinned there. A call cannot name another cluster, nor
// read manifests, nor change the output; the scan carries no other default
// (no ignore file, no gate, no plan).
func TestMCPScanHonoursOnlyKubeconfigContextAndTheEnvironment(t *testing.T) {
	scans := stubScan(t)
	useRealCurrentKubeContext(t)
	envKubeconfig := writeKubeconfig(t) // current-context: test
	flagKubeconfig := filepath.Join(t.TempDir(), "kc")
	if err := os.WriteFile(flagKubeconfig, []byte(strings.Replace(testKubeconfig, "current-context: test", "current-context: from-file", 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name           string
		kubeconfigEnv  string
		flags          []string
		wantKubeconfig string
		wantContext    string
	}{
		{"nothing named: the current context, pinned", envKubeconfig, nil, "", "test"},
		{"no kubeconfig, but a context named", filepath.Join(t.TempDir(), "none"), []string{"--context", "from-flag"}, "", "from-flag"},
		{"kubeconfig flag: its current context", envKubeconfig, []string{"--kubeconfig", flagKubeconfig}, flagKubeconfig, "from-file"},
		{"kubeconfig and context flags", envKubeconfig, []string{"--kubeconfig", "/flags/kc", "--context", "from-flag"}, "/flags/kc", "from-flag"},
		{"context flag only", envKubeconfig, []string{"--context", "from-flag"}, "", "from-flag"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			*scans = nil
			t.Setenv("KUBECONFIG", tc.kubeconfigEnv)
			cs := startMCP(t, tc.flags...)
			// Switching the context after the start does not move the server.
			if err := os.WriteFile(envKubeconfig, []byte(strings.Replace(testKubeconfig, "current-context: test", "current-context: switched", 1)), 0o600); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.WriteFile(envKubeconfig, []byte(testKubeconfig), 0o600) })
			if res := callMCP(t, cs, mcp.ToolScan, map[string]any{"targets": []any{"1.37"}}); res.IsError {
				t.Fatal(mcpText(res))
			}
			if len(*scans) != 1 {
				t.Fatalf("%d scans, want 1", len(*scans))
			}
			got := (*scans)[0]
			if got.kubeconfig != tc.wantKubeconfig || got.kubecontext != tc.wantContext {
				t.Errorf("scan read kubeconfig %q context %q, want %q and %q", got.kubeconfig, got.kubecontext, tc.wantKubeconfig, tc.wantContext)
			}
			want := scanOptions{
				target: "1.37", targetVersion: got.targetVersion,
				kubeconfig: tc.wantKubeconfig, kubecontext: tc.wantContext,
				requestTimeout: defaultRequestTimeout, output: "json", failOn: "never", stderr: got.stderr,
			}
			if !reflect.DeepEqual(got, want) {
				t.Errorf("scan options = %+v\nwant              %+v", got, want)
			}
		})
	}

	t.Run("a call cannot choose the cluster", func(t *testing.T) {
		*scans = nil
		cs := startMCP(t, "--kubeconfig", "/flags/kc")
		for name, extra := range map[string]map[string]any{
			"kubeconfig": {"kubeconfig": "/call/kc"},
			"context":    {"context": "from-call"},
			"files":      {"files": t.TempDir()},
		} {
			args := map[string]any{"targets": []any{"1.37"}}
			maps.Copy(args, extra)
			if res := callMCP(t, cs, mcp.ToolScan, args); !res.IsError {
				t.Errorf("a call naming %s was accepted", name)
			}
		}
		if len(*scans) != 0 {
			t.Errorf("%d scans ran for calls that named a cluster", len(*scans))
		}
	})
}

func useRealCurrentKubeContext(t *testing.T) {
	t.Helper()
	currentKubeContext = realCurrentKubeContext
	t.Cleanup(func() { currentKubeContext = func(string) (string, error) { return stubContext, nil } })
}

// TestMCPScanWithoutAContextAtStartIsRefused: when no --context is given
// and no current context can be read when the server starts (no
// kubeconfig, one that names none, one that cannot be read), there is no
// context to pin, so scan is refused, saying to pass --context, and stays
// refused when a kubeconfig with a current context appears later or
// switches: a scan never reads a context chosen after the start. The
// start message says so, and the other tools work.
func TestMCPScanWithoutAContextAtStartIsRefused(t *testing.T) {
	scans := stubScan(t)
	useRealCurrentKubeContext(t)
	noCurrent := strings.Replace(testKubeconfig, "current-context: test", `current-context: ""`, 1)
	for _, tc := range []struct {
		name    string
		initial *string // the kubeconfig at start; nil: none
		flag    bool    // named by --kubeconfig rather than $KUBECONFIG
	}{
		{"no kubeconfig at all", nil, false},
		{"no current context", &noCurrent, false},
		{"no current context in --kubeconfig", &noCurrent, true},
		{"an unreadable kubeconfig", ptr("{{ not a kubeconfig"), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			*scans = nil
			kc := filepath.Join(t.TempDir(), "kc")
			if tc.initial != nil {
				if err := os.WriteFile(kc, []byte(*tc.initial), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			var args []string
			if tc.flag {
				args = []string{"--kubeconfig", kc}
				t.Setenv("KUBECONFIG", filepath.Join(t.TempDir(), "none"))
			} else {
				t.Setenv("KUBECONFIG", kc)
			}
			var stderr syncBuffer
			cs := startMCPStderr(t, &stderr, args...)
			if msg := stderr.String(); !strings.Contains(msg, "scan is off") || !strings.Contains(msg, "--context") {
				t.Errorf("the start message does not say scan is off and how to turn it on: %q", msg)
			}
			for _, current := range []string{"test", "switched"} {
				if err := os.WriteFile(kc, []byte(strings.Replace(testKubeconfig, "current-context: test", "current-context: "+current, 1)), 0o600); err != nil {
					t.Fatal(err)
				}
				res := callMCP(t, cs, mcp.ToolScan, map[string]any{"targets": []any{"1.37"}})
				if !res.IsError || !strings.Contains(mcpText(res), "--context") || !strings.Contains(mcpText(res), "when the server started") {
					t.Errorf("scan with the kubeconfig now at %q: isError=%v %q, want it refused, naming --context", current, res.IsError, mcpText(res))
				}
			}
			if len(*scans) != 0 {
				t.Errorf("%d scans ran with no context pinned at start: %+v", len(*scans), *scans)
			}
			if res := callMCP(t, cs, mcp.ToolRegistryLookup, map[string]any{"query": "ingress-nginx"}); res.IsError {
				t.Errorf("registry_lookup with scan off: %s", mcpText(res))
			}
		})
	}
}

func ptr[T any](v T) *T { return &v }

// TestMCPScanLooksUpNoIgnoreFile: the working directory is the MCP client's
// choice, so an .upgradescope.yaml found there does not hide findings from
// the assistant; the report holds every finding of the scan.
func TestMCPScanLooksUpNoIgnoreFile(t *testing.T) {
	stubScan(t)
	want := fixtureEvaluator(t)(inventory.Version{Major: 1, Minor: 38})
	if len(want.Findings) == 0 {
		t.Fatal("the fixture has no findings to hide")
	}
	dir := writeFiles(t, map[string]string{suppress.ConfigFile: "ignore:\n  - category: " + string(want.Findings[0].Category) + "\n    reason: not for the assistant\n"})
	t.Chdir(dir)

	cs := startMCP(t)
	callMCP(t, cs, mcp.ToolScan, map[string]any{"targets": []any{"1.38"}})
	res := callMCP(t, cs, mcp.ToolListFindings, map[string]any{"limit": 500})
	var found findingsResult // not validateFindings: the schema is found relative to the test's directory
	raw, _ := json.Marshal(res.StructuredContent)
	if err := json.Unmarshal(raw, &found); err != nil {
		t.Fatal(err)
	}
	if found.Total != len(want.Findings) {
		t.Errorf("list_findings = %d findings, want the scan's %d: an ignore file in the working directory was applied", found.Total, len(want.Findings))
	}
}

// TestMCPScanTargetsAndErrors: several targets are judged in order from one
// read of the cluster, and a cluster that cannot be read is a tool error
// carrying the reason.
func TestMCPScanTargetsAndErrors(t *testing.T) {
	var reads [][]string
	fail := false
	evaluate := fixtureEvaluator(t)
	orig := runMCPScan
	runMCPScan = func(_ context.Context, opts []scanOptions) ([]engine.Report, error) {
		var targets []string
		for _, o := range opts {
			targets = append(targets, o.targetVersion.String())
		}
		reads = append(reads, targets)
		if fail {
			return nil, fmt.Errorf("cannot read the cluster at context %q: unauthorized", opts[0].kubecontext)
		}
		reports := make([]engine.Report, len(opts))
		for i, o := range opts {
			reports[i] = evaluate(o.targetVersion)
		}
		return reports, nil
	}
	t.Cleanup(func() { runMCPScan = orig })
	cs := startMCP(t, "--context", "gone")
	res := callMCP(t, cs, mcp.ToolScan, map[string]any{"targets": []any{"1.37", "1.38"}})
	if res.IsError || len(reads) != 1 || !slices.Equal(reads[0], []string{"1.37", "1.38"}) {
		t.Errorf("cluster reads = %v (error %v), want one, judged at 1.37 then 1.38", reads, res.IsError)
	}
	var sc struct {
		Reports []struct {
			Target string `json:"target"`
		} `json:"reports"`
	}
	raw, _ := json.Marshal(res.StructuredContent)
	if err := json.Unmarshal(raw, &sc); err != nil || len(sc.Reports) != 2 || sc.Reports[0].Target != "1.37" || sc.Reports[1].Target != "1.38" {
		t.Errorf("reports = %+v (%v), want 1.37 then 1.38", sc.Reports, err)
	}
	fail = true
	res = callMCP(t, cs, mcp.ToolScan, map[string]any{"targets": []any{"1.37"}})
	if !res.IsError || !strings.Contains(mcpText(res), "unauthorized") {
		t.Errorf("an unreadable cluster: isError=%v %q", res.IsError, mcpText(res))
	}
}

// fleetServer runs a real `serve` handler with a read token and one cluster
// "prod" that has pushed the fixture inventory.
func fleetServer(t *testing.T, readToken string) *httptest.Server {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "srv.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	kbData, err := kb.Load()
	if err != nil {
		t.Fatal(err)
	}
	srv, err := server.New(server.Config{Store: st, KB: kbData, ReadToken: readToken, IngestToken: "ingest-tok", Version: "test"})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	inv, err := os.ReadFile(mixedInventory)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(map[string]any{"schemaVersion": 1, "clusterName": "prod", "inventory": json.RawMessage(inv)})
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/v1/snapshots", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer ingest-tok")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("seeding the fleet server: %s %s", resp.Status, b)
	}
	return ts
}

// TestMCPFleetModeHonoursTheServersReadAuth: with a server that requires a
// read token, calls without one are rejected and the tool says so; with the
// token (flag file or environment) the same calls work, and their reports
// validate against the schema.
func TestMCPFleetModeHonoursTheServersReadAuth(t *testing.T) {
	ts := fleetServer(t, "read-tok")

	t.Run("no token", func(t *testing.T) {
		t.Setenv("UPGRADESCOPE_READ_TOKEN", "")
		cs := startMCP(t, "--server-url", ts.URL)
		for tool, args := range map[string]map[string]any{
			mcp.ToolFleetSummary: nil,
			mcp.ToolGetReport:    {"cluster": "prod"},
			mcp.ToolListFindings: {"cluster": "prod"},
		} {
			res := callMCP(t, cs, tool, args)
			if !res.IsError || !strings.Contains(mcpText(res), "401") {
				t.Errorf("%s without a token: isError=%v %q, want the server's 401", tool, res.IsError, mcpText(res))
			}
		}
	})

	t.Run("wrong token", func(t *testing.T) {
		cs := startMCP(t, "--server-url", ts.URL, "--read-token", "nope")
		if res := callMCP(t, cs, mcp.ToolFleetSummary, nil); !res.IsError || !strings.Contains(mcpText(res), "401") {
			t.Errorf("fleet_summary with a wrong token: %q", mcpText(res))
		}
	})

	for name, setup := range map[string]func(t *testing.T) []string{
		"token from the environment": func(t *testing.T) []string {
			t.Setenv("UPGRADESCOPE_READ_TOKEN", "read-tok")
			return nil
		},
		"token file": func(t *testing.T) []string {
			t.Setenv("UPGRADESCOPE_READ_TOKEN", "")
			p := filepath.Join(t.TempDir(), "tok")
			if err := os.WriteFile(p, []byte("read-tok\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			return []string{"--read-token-file", p}
		},
	} {
		t.Run(name, func(t *testing.T) {
			cs := startMCP(t, append([]string{"--server-url", ts.URL}, setup(t)...)...)

			tools, err := cs.ListTools(context.Background(), nil)
			if err != nil {
				t.Fatal(err)
			}
			var names []string
			for _, tool := range tools.Tools {
				names = append(names, tool.Name)
				if !tool.Annotations.ReadOnlyHint {
					t.Errorf("tool %s is not read-only", tool.Name)
				}
			}
			slices.Sort(names)
			if want := mcp.ToolNames(true); !slices.Equal(names, want) {
				t.Errorf("fleet mode tools = %v, want %v", names, want)
			}

			rep := callMCP(t, cs, mcp.ToolGetReport, map[string]any{"cluster": "prod"})
			if rep.IsError {
				t.Fatal(mcpText(rep))
			}
			if err := validateJSON(t, reportSchemaAt(t, ""), rep.StructuredContent); err != nil {
				t.Errorf("the server's report, through get_report, does not validate against api/report.schema.json: %v", err)
			}
			found := validateFindings(t, callMCP(t, cs, mcp.ToolListFindings, map[string]any{"cluster": "prod", "severity": "blocker"}))
			if found.Cluster != "prod" || found.Total == 0 {
				t.Errorf("list_findings for prod = cluster %q, %d blockers", found.Cluster, found.Total)
			}
			sum := callMCP(t, cs, mcp.ToolFleetSummary, nil)
			if sum.IsError || !strings.Contains(mcpText(sum), "prod") {
				t.Errorf("fleet_summary = isError %v %q", sum.IsError, mcpText(sum))
			}
		})
	}
}

// TestMCPFleetModeOpenServer: a server with no read token (loopback only)
// needs none from the client.
func TestMCPFleetModeOpenServer(t *testing.T) {
	t.Setenv("UPGRADESCOPE_READ_TOKEN", "")
	ts := fleetServer(t, "")
	cs := startMCP(t, "--server-url", ts.URL)
	if res := callMCP(t, cs, mcp.ToolFleetSummary, nil); res.IsError {
		t.Errorf("fleet_summary against an open server: %s", mcpText(res))
	}
}

func TestMCPOptionsAreChecked(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"token without server", []string{"--read-token", "x"}, "--read-token needs --server-url"},
		{"bad server url", []string{"--server-url", "upgradescope.example.com"}, "http(s) URL"},
		{"remote bind", []string{"--http", "0.0.0.0:8808"}, "not a loopback address"},
		{"named remote host", []string{"--http", "example.com:8808"}, "not a loopback address"},
		{"allow-remote needs http", []string{"--allow-remote"}, "--allow-remote needs --http"},
		{"http-token needs http", []string{"--http-token", "x"}, "--http-token needs --http"},
		{"bad port", []string{"--http", "127.0.0.1:http"}, "bad port"},
		{"negative timeout", []string{"--request-timeout", "-1s"}, "request-timeout"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cmd := Root()
			cmd.SetArgs(append([]string{"mcp"}, tc.args...))
			cmd.SetIn(strings.NewReader(""))
			cmd.SetOut(io.Discard)
			cmd.SetErr(io.Discard)
			if err := cmd.Execute(); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %v, want one containing %q", err, tc.want)
			}
		})
	}
}

func TestNormalizeMCPAddr(t *testing.T) {
	for in, want := range map[string]struct {
		addr   string
		remote bool
	}{
		"8808":           {"127.0.0.1:8808", false},
		":8808":          {"127.0.0.1:8808", false},
		"127.0.0.1:1":    {"127.0.0.1:1", false},
		"localhost:9":    {"localhost:9", false},
		"[::1]:8808":     {"[::1]:8808", false},
		"0.0.0.0:8808":   {"0.0.0.0:8808", true},
		"10.0.0.5:8808":  {"10.0.0.5:8808", true},
		"[::]:8808":      {"[::]:8808", true},
		"example.com:80": {"example.com:80", true},
	} {
		addr, remote, err := normalizeMCPAddr(in)
		if err != nil || addr != want.addr || remote != want.remote {
			t.Errorf("normalizeMCPAddr(%q) = %q, %v, %v; want %q, %v", in, addr, remote, err, want.addr, want.remote)
		}
	}
}

// TestMCPHTTP serves MCP over streamable HTTP on loopback and talks to it
// with the SDK's HTTP client; a request that carries a foreign Host header
// (DNS rebinding) is refused.
func TestMCPHTTP(t *testing.T) {
	addr, stderr := startMCPHTTP(t)
	if !strings.Contains(stderr.String(), "no authentication: any local user or process") {
		t.Errorf("the start message does not say the endpoint is open: %q", stderr.String())
	}
	ctx := context.Background()
	cs, err := mcpsdk.NewClient(&mcpsdk.Implementation{Name: "test", Version: "0"}, nil).
		Connect(ctx, &mcpsdk.StreamableClientTransport{Endpoint: "http://" + addr + "/mcp"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()
	tools, err := cs.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(tools.Tools) != len(mcp.ToolNames(false)) {
		t.Errorf("%d tools over HTTP, want %d", len(tools.Tools), len(mcp.ToolNames(false)))
	}
	found := validateFindings(t, callMCP(t, cs, mcp.ToolListFindings, map[string]any{"inventory_file": mixedInventory, "target": "1.38"}))
	if found.Total == 0 {
		t.Error("list_findings over HTTP found nothing")
	}

	req, _ := http.NewRequest(http.MethodPost, "http://"+addr+"/mcp", strings.NewReader(`{}`))
	req.Host = "evil.example.com"
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("a request with a foreign Host header = %s, want 403", resp.Status)
	}

	// A page in a browser on another origin cannot drive the endpoint,
	// whether the browser says so (Sec-Fetch-Site) or only names its Origin.
	for name, header := range map[string]http.Header{
		"cross-site fetch": {"Sec-Fetch-Site": {"cross-site"}, "Origin": {"http://evil.example.com"}},
		"foreign origin":   {"Origin": {"http://evil.example.com"}},
	} {
		req, _ := http.NewRequest(http.MethodPost, "http://"+addr+"/mcp", strings.NewReader(`{}`))
		req.Header = header
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden {
			t.Errorf("%s = %s, want 403", name, resp.Status)
		}
	}
}

// bearer adds an Authorization header to every request.
type bearer string

func (b bearer) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+string(b))
	return http.DefaultTransport.RoundTrip(r)
}

// TestMCPHTTPToken: with --http-token, a request without the token, or
// with another, is refused with 401, and a client that sends it works.
func TestMCPHTTPToken(t *testing.T) {
	t.Setenv("UPGRADESCOPE_MCP_HTTP_TOKEN", "from-env")
	addr, stderr := startMCPHTTP(t, "--http-token-file", writeFiles(t, map[string]string{"tok": "s3cret\n"})+"/tok")
	if !strings.Contains(stderr.String(), "bearer token required") {
		t.Errorf("the start message does not say a token is required: %q", stderr.String())
	}
	for name, header := range map[string]string{"no token": "", "the environment's, overridden by the file": "Bearer from-env", "a wrong one": "Bearer nope", "not bearer": "Basic czNjcmV0"} {
		req, _ := http.NewRequest(http.MethodPost, "http://"+addr+"/mcp", strings.NewReader(`{}`))
		if header != "" {
			req.Header.Set("Authorization", header)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized || resp.Header.Get("WWW-Authenticate") == "" {
			t.Errorf("%s = %s, want 401 with a challenge", name, resp.Status)
		}
	}
	ctx := context.Background()
	if cs, err := mcpsdk.NewClient(&mcpsdk.Implementation{Name: "test", Version: "0"}, nil).
		Connect(ctx, &mcpsdk.StreamableClientTransport{Endpoint: "http://" + addr + "/mcp"}, nil); err == nil {
		_ = cs.Close()
		t.Error("a client without the token connected")
	}
	cs, err := mcpsdk.NewClient(&mcpsdk.Implementation{Name: "test", Version: "0"}, nil).
		Connect(ctx, &mcpsdk.StreamableClientTransport{Endpoint: "http://" + addr + "/mcp", HTTPClient: &http.Client{Transport: bearer("s3cret")}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()
	if _, err := cs.ListTools(ctx, nil); err != nil {
		t.Errorf("list tools with the token: %v", err)
	}
}

// startMCPHTTP runs `upgradescope mcp --http` on a free loopback port with
// args and returns the address and its stderr once it serves; the command
// is stopped, and must return nil, when the test ends.
func startMCPHTTP(t *testing.T, args ...string) (string, *syncBuffer) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("no loopback listener: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()

	cmd := Root()
	cmd.SetArgs(append([]string{"mcp", "--http", addr}, args...))
	cmd.SetIn(strings.NewReader(""))
	var stderr syncBuffer
	cmd.SetOut(io.Discard)
	cmd.SetErr(&stderr)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- cmd.ExecuteContext(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("mcp --http ended with %v on shutdown", err)
			}
		case <-time.After(15 * time.Second):
			t.Error("mcp --http did not shut down")
		}
	})
	waitFor(t, func() bool { return strings.Contains(stderr.String(), "serving MCP on http://") })
	return addr, &stderr
}

// syncBuffer is a bytes.Buffer safe to read while a command writes to it.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		if cond() {
			return
		}
	}
	t.Fatal("timed out waiting for the condition")
}
