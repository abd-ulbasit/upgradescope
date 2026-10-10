package mcp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// forged is a value no report holds; a refusal that quotes it quotes the file.
const forged = "FORGED-9f3a"

// compactGolden is a golden report with the CLI's envelope as compact JSON
// text, so a test can add members to it.
func compactGolden(t *testing.T, name string) string {
	t.Helper()
	_, doc := goldenReport(t, name)
	return string(doc)
}

// withRootMember adds `"name":value` as the last member of the report.
func withRootMember(t *testing.T, doc, member string) string {
	t.Helper()
	if !strings.HasSuffix(doc, "}") {
		t.Fatal("not an object")
	}
	return doc[:len(doc)-1] + "," + member + "}"
}

func writeDoc(t *testing.T, doc string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "report.json")
	if err := os.WriteFile(p, []byte(doc), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// rawFleet serves doc, byte for byte, as the report of cluster "prod".
func rawFleet(t *testing.T, doc string) *Fleet {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/report") {
			_, _ = w.Write([]byte(doc))
			return
		}
		_, _ = w.Write([]byte(`[{"id":7,"name":"prod"}]`))
	}))
	t.Cleanup(ts.Close)
	f, err := NewFleet(ts.URL, "")
	if err != nil {
		t.Fatal(err)
	}
	return f
}

// TestTheReportReadersDecodeExactlyTheNamesTheSchemaCheckJudged: list_findings
// and the report_file target check read a report's "target", "kbVersion" and
// "findings", and a finding's "severity" and "category", by those exact
// names. encoding/json, decoding into a struct, would read a member of
// another case instead, and the last of two equal names.
func TestTheReportReadersDecodeExactlyTheNamesTheSchemaCheckJudged(t *testing.T) {
	doc := `{"schemaVersion":1,"target":"1.38","kbVersion":"exact","findings":[` +
		`{"severity":"blocker","SEVERITY":"info","category":"eol-addon","CATEGORY":"removed-api"},` +
		`{"severity":"warning","Severity":"blocker"}],` +
		`"FINDINGS":[],"Target":"1.40","TARGET":"1.41","KBVERSION":"forged"}`
	rep, err := decodeReport(json.RawMessage(doc))
	if err != nil {
		t.Fatal(err)
	}
	if rep.Target != "1.38" || rep.KBVersion != "exact" || len(rep.Findings) != 2 {
		t.Errorf("decodeReport read target %q kbVersion %q and %d findings; want 1.38, exact and the 2 of \"findings\"", rep.Target, rep.KBVersion, len(rep.Findings))
	}
	if got := reportTarget(json.RawMessage(doc)); got != "1.38" {
		t.Errorf("reportTarget = %q, want the exact member, 1.38", got)
	}
	var blockers int
	for _, raw := range rep.Findings {
		sev, cat, err := findingKey(raw)
		if err != nil {
			t.Fatal(err)
		}
		if sev == "blocker" {
			blockers++
			if cat != "eol-addon" {
				t.Errorf("category = %q, want the exact member, eol-addon", cat)
			}
		}
	}
	if blockers != 1 {
		t.Errorf("%d blockers counted, want 1 (from the exact severity members)", blockers)
	}
}

// TestAReportWithAnAmbiguousMemberNameIsRefused: a report with a member
// beside a declared one that differs from it only in case, or with a name
// twice, is a tool error naming the rule, from report_file, from the fleet
// server and from an inventory judged at a target; get_report and
// list_findings agree, and nothing of the file is quoted. A report judged at
// 1.38 is never accepted for target 1.40.
func TestAReportWithAnAmbiguousMemberNameIsRefused(t *testing.T) {
	base := compactGolden(t, "mixed-everything")
	blocker := `"severity":"blocker"`
	if strings.Count(base, blocker) == 0 {
		t.Fatal("the fixture has no blocker")
	}
	cases := []struct {
		name string
		doc  string
		want string
	}{
		{"Target beside target", withRootMember(t, base, `"Target":"`+forged+`"`), `differs only in case from the property "target"`},
		{"TARGET beside target", withRootMember(t, base, `"TARGET":"1.40"`), `differs only in case from the property "target"`},
		{"FINDINGS beside findings", withRootMember(t, base, `"FINDINGS":["`+forged+`"]`), `differs only in case from the property "findings"`},
		{"SEVERITY beside a finding's severity", strings.ReplaceAll(base, blocker, blocker+`,"SEVERITY":"`+forged+`"`), `differs only in case from the property "severity"`},
		{"Category beside a finding's category", strings.Replace(base, `"category":`, `"Category":"`+forged+`","category":`, 1), `differs only in case from the property "category"`},
		{"target twice", withRootMember(t, base, `"target":"1.40"`), "repeated"},
		{"findings twice", withRootMember(t, base, `"findings":[]`), "repeated"},
		{"severity twice", strings.ReplaceAll(base, blocker, blocker+`,"severity":"info"`), "repeated"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := writeDoc(t, tc.doc)
			cfg := localConfig()
			cfg.Fleet = rawFleet(t, tc.doc)
			cfg.Inventory = func(context.Context, string, string) (json.RawMessage, error) { return json.RawMessage(tc.doc), nil }
			cs := connect(t, cfg)
			for _, tool := range []string{ToolGetReport, ToolListFindings} {
				for _, src := range []map[string]any{
					{"report_file": path},
					{"report_file": path, "target": "1.40"},
					{"cluster": "prod"},
					{"inventory_file": "/inv.json", "target": "1.38"},
				} {
					res := call(t, cs, tool, src)
					msg := text(res)
					if !res.IsError || !strings.Contains(msg, tc.want) || !strings.Contains(msg, "it has a member name") {
						t.Errorf("%s %v: isError=%v %.300q, want a tool error saying %q", tool, src, res.IsError, msg, tc.want)
					}
					if strings.Contains(msg, forged) {
						t.Errorf("%s %v: the refusal quotes the file: %.300q", tool, src, msg)
					}
					if !strings.HasPrefix(msg, errorNotice()) {
						t.Errorf("%s %v: the refusal does not open with the error notice: %.200q", tool, src, msg)
					}
				}
			}
		})
	}
}

// TestACaseFoldedMemberCannotMakeGetReportAndListFindingsDisagree is the
// issue's repro: a trailing "FINDINGS":[] and "SEVERITY":"info" after every
// blocker used to make list_findings answer zero blockers for a blocked
// report, and "TARGET":"1.40" let a report judged at 1.38 through a
// target check for 1.40. A plain report still gives both tools the same
// blockers, and get_report returns the document it read.
func TestACaseFoldedMemberCannotMakeGetReportAndListFindingsDisagree(t *testing.T) {
	base := compactGolden(t, "mixed-everything")
	cs := connect(t, localConfig())

	path := writeDoc(t, base)
	list := structured(t, call(t, cs, ToolListFindings, map[string]any{"report_file": path, "severity": "blocker"}))
	var got struct {
		Target string `json:"target"`
		Total  int    `json:"total"`
	}
	if err := json.Unmarshal(list, &got); err != nil {
		t.Fatal(err)
	}
	var want struct {
		Findings []struct {
			Severity string `json:"severity"`
		} `json:"findings"`
	}
	if err := json.Unmarshal([]byte(base), &want); err != nil {
		t.Fatal(err)
	}
	var blockers int
	for _, f := range want.Findings {
		if f.Severity == "blocker" {
			blockers++
		}
	}
	if blockers == 0 || got.Total != blockers {
		t.Errorf("list_findings counted %d blockers, the report has %d", got.Total, blockers)
	}
	if got.Target == "1.40" {
		t.Fatal("the fixture is judged at 1.40: it cannot tell a forged target")
	}
	// get_report returns the document it read, unchanged.
	if out := structured(t, call(t, cs, ToolGetReport, map[string]any{"report_file": path})); !jsonEqual(t, out, []byte(base)) {
		t.Errorf("get_report changed a plain report: %.200s", out)
	}

	forgedDoc := withRootMember(t, strings.ReplaceAll(base, `"severity":"blocker"`, `"severity":"blocker","SEVERITY":"info"`), `"TARGET":"1.40"`)
	forgedDoc = withRootMember(t, forgedDoc, `"FINDINGS":[]`)
	fp := writeDoc(t, forgedDoc)
	for _, tool := range []string{ToolGetReport, ToolListFindings} {
		res := call(t, cs, tool, map[string]any{"report_file": fp, "target": "1.40"})
		if !res.IsError {
			t.Errorf("%s accepted a report judged at %s for target 1.40: %.200s", tool, got.Target, text(res))
		}
	}
}

// overlong is a value over MaxClusterTextBytes that fits the minor pattern.
func overlong() string { return "1." + strings.Repeat("0", 3*MaxClusterTextBytes) }

// docWithHop is the report with a hop whose from and to are given and whose
// changed finding has the given since.
func docWithHop(t *testing.T, base, from, to, since string) string {
	t.Helper()
	hop := `{"from":"` + from + `","to":"` + to + `","score":90,"verdict":"ready","findings":[],` +
		`"changed":[{"key":"k","severity":"info","was":"warning","title":"t","since":"` + since + `"}],"carried":[]}`
	return withRootMember(t, base, `"hops":[`+hop+`]`)
}

// TestAnOverlongPatternValueIsRefusedNotCut: a string the schema gives a
// pattern (a minor, annualCostDelta) longer than MaxClusterTextBytes would be
// cut by the marking and no longer match its pattern, which the SDK reports
// as a JSON-RPC protocol error. checkReport refuses it as a tool error with
// the field and the rule, from report_file and the fleet server alike, and
// quotes none of it.
func TestAnOverlongPatternValueIsRefusedNotCut(t *testing.T) {
	long := overlong()
	clean := compactGolden(t, "clean-cluster")
	extended := compactGolden(t, "support-extended")
	cases := []struct {
		name string
		doc  string
		at   string
	}{
		{"target", strings.Replace(clean, `"target":"`, `"target":"`+long+`","x":"`, 1), "at /target"},
		{"hop from", docWithHop(t, clean, long, "1.40", "1.40"), "at /hops/0/from"},
		{"hop to", docWithHop(t, clean, "1.39", long, "1.40"), "at /hops/0/to"},
		{"finding since", docWithHop(t, clean, "1.39", "1.40", long), "at /hops/0/changed/0/since"},
		{"support minor", strings.Replace(extended, `"minor":"`, `"minor":"`+long+`","x":"`, 1), "at /support/minor"},
		{"annualCostDelta", strings.Replace(extended, `"annualCostDelta":"4380.00"`, `"annualCostDelta":"`+strings.Repeat("0", 3*MaxClusterTextBytes)+`.00"`, 1), "at /support/annualCostDelta"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := validate(t, resolve(t, ReportOutputSchema()), []byte(tc.doc)); err != nil {
				t.Fatalf("the fixture does not follow the schema, so it cannot show the refusal is checkReport's: %.120v", err)
			}
			path := writeDoc(t, tc.doc)
			cfg := localConfig()
			cfg.Fleet = rawFleet(t, tc.doc)
			cs := connect(t, cfg)
			for _, tool := range []string{ToolGetReport, ToolListFindings} {
				for _, src := range []map[string]any{{"report_file": path}, {"cluster": "prod"}} {
					res, err := cs.CallTool(context.Background(), &mcpsdk.CallToolParams{Name: tool, Arguments: src})
					if err != nil {
						t.Fatalf("%s %v: a protocol error, not a tool error: %.300v", tool, src, err)
					}
					msg := text(res)
					if !res.IsError || !strings.HasPrefix(msg, errorNotice()) {
						t.Fatalf("%s %v: isError=%v %.200q, want a tool error opening with the notice", tool, src, res.IsError, msg)
					}
					if _, ok := res.Meta[MetaClusterText]; !ok {
						t.Errorf("%s %v: no %s marker in _meta", tool, src, MetaClusterText)
					}
					if !strings.Contains(msg, tc.at) || !strings.Contains(msg, "a value with a pattern is at most 2048 bytes") {
						t.Errorf("%s %v: %.400q, want the location %q and the rule", tool, src, msg, tc.at)
					}
					if strings.Contains(msg, "0000000000") {
						t.Errorf("%s %v: the refusal quotes the value", tool, src)
					}
				}
			}
		})
	}
}

// TestACutThatBreaksTheOutputSchemaIsAToolError: where a document reaches
// markedResult without checkReport (scan trusts the engine), a cut that
// leaves a pattern-constrained value outside its pattern is the tool's error,
// by the schema's rule and location and without the value, never the SDK's
// own output check, which fails the call as a protocol error.
func TestACutThatBreaksTheOutputSchemaIsAToolError(t *testing.T) {
	_, golden := goldenReport(t, "clean-cluster")
	var rep map[string]any
	if err := json.Unmarshal(golden, &rep); err != nil {
		t.Fatal(err)
	}
	rep["target"] = overlong()
	bad, err := json.Marshal(rep)
	if err != nil {
		t.Fatal(err)
	}

	cfg := localConfig()
	cfg.Scan = func(_ context.Context, req ScanRequest) ([]json.RawMessage, error) {
		return []json.RawMessage{bad}, nil
	}
	cs := connect(t, cfg)
	res, err := cs.CallTool(context.Background(), &mcpsdk.CallToolParams{Name: ToolScan, Arguments: map[string]any{"targets": []any{"1.37"}}})
	if err != nil {
		t.Fatalf("scan: a protocol error, not a tool error: %.300v", err)
	}
	msg := text(res)
	if !res.IsError || !strings.HasPrefix(msg, errorNotice()) {
		t.Fatalf("scan: isError=%v %.200q", res.IsError, msg)
	}
	if !strings.Contains(msg, "would break the result's output schema") || !strings.Contains(msg, "rule pattern") || strings.Contains(msg, "0000000000") {
		t.Errorf("scan: %.400q, want the schema rule named and the value not quoted", msg)
	}

	// The same through the function every tool uses.
	if _, _, err := markedResult(bad, "x", reportResolved()); err == nil || !strings.Contains(err.Error(), "rule pattern") {
		t.Errorf("markedResult of a cut that breaks the schema = %v", err)
	}
	// A document with nothing cut is not changed or re-checked.
	if _, out, err := markedResult(golden, "x", reportResolved()); err != nil || !jsonEqual(t, out, golden) {
		t.Errorf("markedResult of a plain report: err %v", err)
	}
}

// TestANormalReportIsUnchangedByTheChecks: the tools return a golden report
// as before, through report_file, the fleet server and an inventory.
func TestANormalReportIsUnchangedByTheChecks(t *testing.T) {
	for _, name := range []string{"clean-cluster", "mixed-everything", "support-extended"} {
		t.Run(name, func(t *testing.T) {
			path, doc := goldenReport(t, name)
			cfg := localConfig()
			cfg.Fleet = rawFleet(t, string(doc))
			cfg.Inventory = func(context.Context, string, string) (json.RawMessage, error) { return doc, nil }
			cs := connect(t, cfg)
			for _, src := range []map[string]any{{"report_file": path}, {"cluster": "prod"}, {"inventory_file": "/inv.json", "target": "1.37"}} {
				if out := structured(t, call(t, cs, ToolGetReport, src)); !jsonEqual(t, out, doc) {
					t.Errorf("get_report %v changed the report: %.200s", src, out)
				}
				structured(t, call(t, cs, ToolListFindings, src))
			}
		})
	}
}
