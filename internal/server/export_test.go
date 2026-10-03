package server

import (
	"bytes"
	"flag"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/abd-ulbasit/upgradescope/internal/engine"
	"github.com/abd-ulbasit/upgradescope/internal/inventory"
	"github.com/abd-ulbasit/upgradescope/internal/server/store"
)

var updateGolden = flag.Bool("update", false, "rewrite export golden files")

// exportFixture seeds one cluster with two snapshots (1 then 2 PSP objects
// in a team-labelled namespace) so the export has findings, teams, and a
// two-point score history — all under the pinned test clock.
func exportFixture(t *testing.T) (*httptest.Server, func()) {
	t.Helper()
	st := newFakeStore()
	s := newTestServer(t, st)
	ts := httptest.NewServer(s.Handler())

	for _, count := range []int{1, 2} {
		inv := testInventory()
		inv.Namespaces = []inventory.NamespaceInfo{{Name: "payments-prod", Team: "payments"}}
		inv.APIUsage = []inventory.APIUsage{{
			Group: "policy", Version: "v1beta1", Kind: "PodSecurityPolicy",
			Count: count, Namespaces: map[string]int{"payments-prod": count},
		}}
		resp, out := postSnapshot(t, ts, "ingest-tok", pushReqBody(t, inv), false)
		if resp.StatusCode != 202 {
			t.Fatalf("seed push status = %d (%v)", resp.StatusCode, out)
		}
	}
	return ts, ts.Close
}

func getExport(t *testing.T, ts *httptest.Server, query string) (*http.Response, []byte) {
	t.Helper()
	resp, err := ts.Client().Get(ts.URL + "/api/v1/clusters/1/export" + query)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp, raw
}

// checkGolden compares got against testdata/name, rewriting with -update.
func checkGolden(t *testing.T, name string, got []byte) {
	t.Helper()
	path := filepath.Join("testdata", name)
	if *updateGolden {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden (run with -update to create): %v", err)
	}
	if string(got) != string(want) {
		t.Errorf("output differs from %s (re-run with -update after intentional changes)\n--- got ---\n%s", path, got)
	}
}

func TestExportCSVGolden(t *testing.T) {
	ts, done := exportFixture(t)
	defer done()

	resp, raw := getExport(t, ts, "?target=1.35&format=csv")
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, body %s", resp.StatusCode, raw)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/csv") {
		t.Errorf("Content-Type = %q, want text/csv", ct)
	}
	if cd := resp.Header.Get("Content-Disposition"); !strings.Contains(cd, "upgradescope-prod-eu-1-1.35.csv") {
		t.Errorf("Content-Disposition = %q", cd)
	}
	checkGolden(t, "export_golden.csv", raw)
}

// exportCSVOf pushes inv for cluster prod-eu-1 and returns the target 1.35
// CSV export.
func exportCSVOf(t *testing.T, inv inventory.Inventory) []byte {
	t.Helper()
	ts := httptest.NewServer(newTestServer(t, newFakeStore()).Handler())
	defer ts.Close()
	if resp, out := postSnapshot(t, ts, "ingest-tok", pushReqBody(t, inv), false); resp.StatusCode != 202 {
		t.Fatalf("push = %d %v", resp.StatusCode, out)
	}
	resp, raw := getExport(t, ts, "?target=1.35&format=csv")
	if resp.StatusCode != 200 {
		t.Fatalf("export = %d %s", resp.StatusCode, raw)
	}
	return raw
}

// TestExportCSVZeroFindingsHasVerdict: a clean cluster's CSV used to be the
// header alone, indistinguishable from a broken export. The summary row
// carries the verdict, score, KB version and evaluation time.
func TestExportCSVZeroFindingsHasVerdict(t *testing.T) {
	checkGolden(t, "export_clean.csv", exportCSVOf(t, testInventory()))
}

// TestExportCSVNotAssessedRows: a capability the agent could not collect
// is a not-assessed row, so a partly assessed cluster does not look like a
// clean one.
func TestExportCSVNotAssessedRows(t *testing.T) {
	inv := testInventory()
	inv.Capabilities[inventory.CapAPIUsage] = inventory.CapabilityStatus{Available: false, Reason: "customresourcedefinitions list forbidden"}
	raw := exportCSVOf(t, inv)
	checkGolden(t, "export_not_assessed.csv", raw)
	if !strings.Contains(string(raw), ",unknown,") || !strings.Contains(string(raw), "not-assessed,") {
		t.Errorf("CSV lacks the unknown verdict or a not-assessed row:\n%s", raw)
	}
}

// A finding lists at most 100 namespaces; the CSV says how many more
// there are, as the HTML export does.
func TestExportCSVCountsOmittedNamespaces(t *testing.T) {
	rep := engine.Report{Findings: []engine.Finding{{Severity: engine.SevWarning, Title: "wide",
		Namespaces: []string{"a", "b"}, NamespacesOmitted: 98}}}
	var b strings.Builder
	if err := writeExportCSV(&b, "prod", store.Evaluation{}, rep); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(b.String(), ",a;b;and 98 more,") {
		t.Errorf("CSV = %s, want the namespaces column to end in \"and 98 more\"", b.String())
	}
}

// TestSparklineTimeProportional: points are placed by time, not index, so
// a burst of changes and a quiet month do not look alike, and the SVG
// labels the dates it spans.
func TestSparklineTimeProportional(t *testing.T) {
	t0 := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	d := exportData{History: []store.ScorePoint{
		{At: t0, Score: 50}, {At: t0.Add(24 * time.Hour), Score: 60}, {At: t0.Add(240 * time.Hour), Score: 100},
	}}
	svg := string(d.Sparkline())
	// width 260, pad 4: x = 4 + 252 * (t - t0) / 10d → 4.0, 29.2, 256.0
	if !strings.Contains(svg, `points="4.0,`) || !strings.Contains(svg, " 29.2,") || !strings.Contains(svg, " 256.0,") {
		t.Errorf("sparkline x positions are not time-proportional: %s", svg)
	}
	if !strings.Contains(svg, "2026-06-01 to 2026-06-11") {
		t.Errorf("sparkline does not label its date range: %s", svg)
	}
}

func TestExportHTMLGolden(t *testing.T) {
	ts, done := exportFixture(t)
	defer done()

	resp, raw := getExport(t, ts, "?target=1.35&format=html")
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, body %s", resp.StatusCode, raw)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("Content-Type = %q, want text/html", ct)
	}
	body := string(raw)
	// Self-contained: no external resources, no scripts.
	for _, banned := range []string{"<script", "http-equiv", "src=", "@import"} {
		if strings.Contains(body, banned) {
			t.Errorf("HTML must be self-contained; found %q", banned)
		}
	}
	// Per-team scores (#125 SV-09): payments owns the PSP blocker.
	for _, want := range []string{"<svg", "payments", "score 75/100", "@media print", "<h2>team scores</h2>", "<td>payments</td><td>75/100</td><td>blocked</td><td>1</td><td>0</td>"} {
		if !strings.Contains(body, want) {
			t.Errorf("HTML missing %q", want)
		}
	}
	checkGolden(t, "export_golden.html", raw)
}

// The auditor report marks a partial gap and what it skipped (issue #122).
func TestExportHTMLPartialGap(t *testing.T) {
	st := newFakeStore()
	ts := httptest.NewServer(newTestServer(t, st).Handler())
	defer ts.Close()
	inv := testInventory()
	inv.Capabilities[inventory.CapAPIUsage] = inventory.CapabilityStatus{Available: true, Partial: true,
		Reason: "list policy/v1beta1 podsecuritypolicies: forbidden", Skipped: []string{"policy/v1beta1 PodSecurityPolicy"}}
	if resp, out := postSnapshot(t, ts, "ingest-tok", pushReqBody(t, inv), false); resp.StatusCode != 202 {
		t.Fatalf("push status = %d (%v)", resp.StatusCode, out)
	}
	_, raw := getExport(t, ts, "?target=1.35&format=html")
	const want = "<li>api-usage (partial, required): list policy/v1beta1 podsecuritypolicies: forbidden (skipped: policy/v1beta1 PodSecurityPolicy)</li>"
	if !strings.Contains(string(raw), want) {
		t.Errorf("HTML lacks %q:\n%s", want, raw)
	}
}

// A finding lists at most engine.MaxFindingNamespaces namespaces; the
// auditor report says how many more it affects.
func TestExportHTMLNamespacesOmitted(t *testing.T) {
	var b bytes.Buffer
	f := engine.Finding{Category: engine.CatRemovedAPI, Severity: engine.SevBlocker, Title: "t", Detail: "d",
		Namespaces: []string{"a", "b"}, NamespacesOmitted: 7}
	if err := writeExportHTML(&b, exportData{Report: engine.Report{Findings: []engine.Finding{f}}}); err != nil {
		t.Fatal(err)
	}
	if want := "<td>a, b and 7 more</td>"; !strings.Contains(b.String(), want) {
		t.Errorf("HTML lacks %q:\n%s", want, b.String())
	}
}

func TestExportErrors(t *testing.T) {
	ts, done := exportFixture(t)
	defer done()

	cases := []struct {
		name string
		path string
		want int
	}{
		{"bad format", "/api/v1/clusters/1/export?target=1.35&format=pdf", 422},
		{"missing format", "/api/v1/clusters/1/export?target=1.35", 422},
		{"no evaluation for target", "/api/v1/clusters/1/export?target=1.50&format=csv", 404},
		{"unknown cluster", "/api/v1/clusters/9/export?target=1.35&format=csv", 404},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if resp := getJSON(t, ts, tc.path, "", nil); resp.StatusCode != tc.want {
				t.Fatalf("status = %d, want %d", resp.StatusCode, tc.want)
			}
		})
	}
}

func TestCSVSafeBlocksFormulaInjection(t *testing.T) {
	for in, want := range map[string]string{
		"=HYPERLINK(\"x\")":     "'=HYPERLINK(\"x\")",
		"+1":                    "'+1",
		"-cmd":                  "'-cmd",
		"@SUM":                  "'@SUM",
		"\t=1":                  "'\t'=1",
		"\r=1":                  "'\r'=1",
		"＝1+1":                  "'＝1+1",
		"x;=1+1;":               "x;'=1+1;",
		"a, -b":                 "a, '-b",
		"\u00a0=1":              "\u00a0'=1",
		"x;\u3000\u2003@x":      "x;\u3000\u2003'@x",
		"kubectl get x -o yaml": "kubectl get x -o yaml",
		"":                      "",
		"payments":              "payments",
	} {
		if got := csvSafe(in); got != want {
			t.Errorf("csvSafe(%q) = %q, want %q", in, got, want)
		}
	}
}

// A spreadsheet imports the export with ; as the separator in many locales
// (and LibreOffice lets the user pick ,;tab freely), which re-splits a
// field: a field-manager name `x;=1+1;` in a finding's detail became the
// cell `=1+1` and evaluated (#126 SE-07). Whatever the separator, no cell
// of the export starts with a formula trigger.
func TestWriteExportCSV_FormulaAfterSeparator(t *testing.T) {
	rep := engine.Report{
		Target: inventory.Version{Major: 1, Minor: 35}, Verdict: engine.VerdictBlocked, KBVersion: "kb",
		Findings: []engine.Finding{{
			Severity: engine.SevBlocker, Category: engine.CatRemovedAPI, Key: "k",
			Title:  "t",
			Detail: "written by managers kube-controller-manager, x;=1+1;, y;=HYPERLINK(CHAR(104)&\"x\");, z\t@SUM(1)",
			Teams:  []string{"\n=1+1", "+team"}, Namespaces: []string{"ns", "-ns"},
			Citations: []string{"https://example.com/a,=1"},
		}},
	}
	var buf bytes.Buffer
	if err := writeExportCSV(&buf, "c;-1", store.Evaluation{}, rep); err != nil {
		t.Fatal(err)
	}
	for _, sep := range []string{",", ";", "\t", "\n", "\r"} {
		for _, cell := range strings.Split(buf.String(), sep) {
			cell = strings.TrimLeft(cell, " \"")
			if cell != "" && strings.ContainsRune("=+-@＝＋－＠", []rune(cell)[0]) {
				t.Errorf("split on %q: cell %q starts with a formula trigger", sep, cell)
			}
		}
	}
}
