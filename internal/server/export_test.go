package server

import (
	"flag"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/abd-ulbasit/upgradescope/internal/inventory"
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
	for _, want := range []string{"<svg", "payments", "score 75/100", "@media print"} {
		if !strings.Contains(body, want) {
			t.Errorf("HTML missing %q", want)
		}
	}
	checkGolden(t, "export_golden.html", raw)
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
		"=HYPERLINK(\"x\")": "'=HYPERLINK(\"x\")",
		"+1":                "'+1",
		"-cmd":              "'-cmd",
		"@SUM":              "'@SUM",
		"":                  "",
		"payments":          "payments",
	} {
		if got := csvSafe(in); got != want {
			t.Errorf("csvSafe(%q) = %q, want %q", in, got, want)
		}
	}
}
