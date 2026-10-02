package codequality

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/abd-ulbasit/upgradescope/internal/codequality/codequalitytest"
	"github.com/abd-ulbasit/upgradescope/internal/engine"
	"github.com/abd-ulbasit/upgradescope/internal/inventory"
)

var update = flag.Bool("update", false, "rewrite golden files under testdata/golden")

func write(t *testing.T, r engine.Report) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := Write(&buf, r); err != nil {
		t.Fatalf("Write: %v", err)
	}
	return buf.Bytes()
}

// TestCodeQualityGolden renders every engine golden report (the reports
// internal/engine's TestEvaluateGolden pins) as a Code Quality report,
// twice: the output, fingerprints included, is stable, GitLab's schema and
// documented requirements accept it, and it matches
// testdata/golden/<case>.json (go test ./internal/codequality -update
// rewrites them).
func TestCodeQualityGolden(t *testing.T) {
	entries, err := os.ReadDir("../engine/testdata")
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		n++
		t.Run(name, func(t *testing.T) {
			raw, err := os.ReadFile(filepath.Join("../engine/testdata", name, "expected.json"))
			if err != nil {
				t.Fatal(err)
			}
			var r engine.Report
			if err := json.Unmarshal(raw, &r); err != nil {
				t.Fatal(err)
			}
			got := write(t, r)
			if again := write(t, r); !bytes.Equal(got, again) {
				t.Fatalf("two runs differ:\n%s\n---\n%s", got, again)
			}
			issues := codequalitytest.AssertGitLabAcceptable(t, got)
			// Every finding is in the report.
			checks := map[string]bool{}
			for _, is := range issues {
				checks[is.CheckName] = true
			}
			for _, f := range r.Findings {
				if !checks[f.Key] {
					t.Errorf("finding %s is not in the report", f.Key)
				}
			}
			golden := filepath.Join("testdata", "golden", name+".json")
			if *update {
				if err := os.WriteFile(golden, got, 0o644); err != nil {
					t.Fatal(err)
				}
				return
			}
			want, err := os.ReadFile(golden)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, want) {
				t.Errorf("%s differs (rerun with -update and inspect the diff):\n got:\n%s\nwant:\n%s", golden, got, want)
			}
		})
	}
	if n < 10 {
		t.Fatalf("found %d engine golden cases; the fixture walk is broken", n)
	}
}

func testReport() engine.Report {
	return engine.Report{
		Target:  inventory.Version{Major: 1, Minor: 36},
		Verdict: engine.VerdictBlocked,
		Findings: []engine.Finding{
			{Category: engine.CatRemovedAPI, Severity: engine.SevBlocker, Key: "removed-api/networking.k8s.io/v1beta1/Ingress",
				Title: "networking.k8s.io/v1beta1 Ingress removed in 1.22 (4 objects)", Remediation: "migrate to networking.k8s.io/v1 Ingress",
				ObjectsOmitted: 1,
				Objects: []inventory.ObjectRef{
					{Name: "admin", File: "legacy/ingress.yaml", Line: 4},
					{Namespace: "shop", Name: "web", File: "rendered/all.yaml", Line: 3, RenderedFrom: "shop/templates/ingress.yaml"},
					{Namespace: "shop", Name: "posted", Line: 9}, // no file
				}},
			{Category: engine.CatEOLAddon, Severity: engine.SevBlocker, Key: "eol-addon/ingress-nginx",
				Title: "ingress-nginx is end-of-life", Detail: "Detected ingress-nginx 1.8.1.", Remediation: "migrate to Gateway API",
				BaselineState: engine.BaselineUnchanged},
			{Category: engine.CatDeprecatedAPI, Severity: engine.SevWarning, Key: "deprecated-api/policy/v1beta1/PodDisruptionBudget",
				Title: "policy/v1beta1 PodDisruptionBudget deprecated", Objects: []inventory.ObjectRef{{Name: "pdb"}}}, // live: no file
			{Category: engine.CatAddOnNoData, Severity: engine.SevInfo, Key: "addon-no-data/external-dns", Title: "no lifecycle data for external-dns"},
		},
		Suppressed: []engine.SuppressedFinding{{
			Finding: engine.Finding{Category: engine.CatRemovedAPI, Severity: engine.SevBlocker, Key: "removed-api/batch/v1beta1/CronJob",
				Title: "batch/v1beta1 CronJob removed", Objects: []inventory.ObjectRef{{Name: "nightly", File: "rendered/all.yaml", Line: 14}}},
			Reason: "deleted next sprint", Source: ".upgradescope.yaml",
		}},
		NotAssessed: []engine.CapabilityGap{
			{Capability: inventory.CapAddOns, Reason: "list pods: forbidden", Required: true},
			{Capability: inventory.CapAPIUsage, Reason: "list ingresses: forbidden", Partial: true, Skipped: []string{"networking.k8s.io/v1 Ingress"}},
			{Capability: inventory.CapHelm, Reason: "files mode"},
		},
	}
}

// TestWriteEntries: one entry per finding and located object, on its file
// and line, with severity blocker → critical, warning → minor, info →
// info. A finding without a file, and the affected objects of a located
// one that have none, are anchored to the virtual path
// upgradescope/<key>, line 1, so they still reach the merge request
// widget. Suppressed findings are left out (GitLab has no dismissed
// state); a required gap is a critical entry, since the verdict is
// unknown; a partial gap is info; an optional check that did not run is
// left out, as in SARIF.
func TestWriteEntries(t *testing.T) {
	issues := codequalitytest.AssertGitLabAcceptable(t, write(t, testReport()))
	type want struct {
		check, severity, path string
		line                  int
		desc                  []string
	}
	wants := []want{
		{"removed-api/networking.k8s.io/v1beta1/Ingress", "critical", "legacy/ingress.yaml", 4,
			[]string{"removed in 1.22", "admin", "Fix: migrate to networking.k8s.io/v1 Ingress."}},
		{"removed-api/networking.k8s.io/v1beta1/Ingress", "critical", "rendered/all.yaml", 3,
			[]string{"shop/web", "rendered from shop/templates/ingress.yaml"}},
		{"removed-api/networking.k8s.io/v1beta1/Ingress", "critical", "upgradescope/removed-api/networking.k8s.io/v1beta1/Ingress", 1,
			[]string{"2 more affected object(s) are not listed with a file and line", "shop/posted (line 9)"}},
		{"eol-addon/ingress-nginx", "critical", "upgradescope/eol-addon/ingress-nginx", 1,
			[]string{"ingress-nginx is end-of-life.", "Detected ingress-nginx 1.8.1.", "Fix: migrate to Gateway API."}},
		{"deprecated-api/policy/v1beta1/PodDisruptionBudget", "minor", "upgradescope/deprecated-api/policy/v1beta1/PodDisruptionBudget", 1,
			[]string{"Objects: pdb."}},
		{"addon-no-data/external-dns", "info", "upgradescope/addon-no-data/external-dns", 1, nil},
		{"not-assessed/addons", "critical", "upgradescope/not-assessed/addons", 1,
			[]string{"Not assessed: addons (required): list pods: forbidden.", "verdict is unknown"}},
		{"not-assessed/api-usage", "info", "upgradescope/not-assessed/api-usage", 1,
			[]string{"Skipped: networking.k8s.io/v1 Ingress."}},
	}
	if len(issues) != len(wants) {
		t.Fatalf("entries = %+v, want %d", issues, len(wants))
	}
	for i, w := range wants {
		is := issues[i]
		if is.CheckName != w.check || is.Severity != w.severity || is.Location.Path != w.path || is.Location.Lines.Begin != w.line {
			t.Errorf("entry %d = %s %s %s:%d, want %s %s %s:%d", i, is.CheckName, is.Severity, is.Location.Path, is.Location.Lines.Begin, w.check, w.severity, w.path, w.line)
		}
		for _, d := range w.desc {
			if !strings.Contains(is.Description, d) {
				t.Errorf("entry %d description %q does not contain %q", i, is.Description, d)
			}
		}
	}
}

// Fingerprints identify a finding's object, not its line or the counts in
// its title: GitLab compares the merge request's report with the target
// branch's by fingerprint, so a finding whose object moved within its file,
// or whose title says "3 objects" instead of "4", is not reported as
// resolved and new again.
func TestFingerprintsStable(t *testing.T) {
	fps := func(r engine.Report) []string {
		var out []string
		for _, is := range codequalitytest.AssertGitLabAcceptable(t, write(t, r)) {
			out = append(out, is.Fingerprint)
		}
		return out
	}
	before := fps(testReport())
	moved := testReport()
	f := &moved.Findings[0]
	f.Title, f.ObjectsOmitted = "networking.k8s.io/v1beta1 Ingress removed in 1.22 (3 objects)", 0
	f.Objects[0].Line, f.Objects[1].Line = 40, 30
	moved.Findings[1].Detail = "Detected ingress-nginx 1.8.2."
	if after := fps(moved); strings.Join(after, ",") != strings.Join(before, ",") {
		t.Errorf("fingerprints changed with lines and counts:\nbefore %v\nafter  %v", before, after)
	}

	renamed := testReport()
	renamed.Findings[0].Objects[0].Name = "admin-v2"
	if after := fps(renamed); after[0] == before[0] || after[1] != before[1] {
		t.Errorf("renaming the first object: fingerprints %v, want only the first to change from %v", after, before)
	}
}

// An object whose file is known but not its line is on that file, line 1,
// not on the virtual path: GitLab can still link the real file.
func TestWriteFileWithoutLine(t *testing.T) {
	r := engine.Report{Findings: []engine.Finding{{
		Category: engine.CatRemovedAPI, Severity: engine.SevBlocker, Key: "removed-api/x/v1/K", Title: "x/v1 K removed",
		Objects: []inventory.ObjectRef{{Name: "a", File: "rendered/all.yaml"}},
	}}}
	issues := codequalitytest.AssertGitLabAcceptable(t, write(t, r))
	if len(issues) != 1 || issues[0].Location.Path != "rendered/all.yaml" || issues[0].Location.Lines.Begin != 1 {
		t.Fatalf("entries = %+v, want one on rendered/all.yaml:1", issues)
	}
	moved := r
	moved.Findings = []engine.Finding{r.Findings[0]}
	moved.Findings[0].Objects = []inventory.ObjectRef{{Name: "a", File: "rendered/all.yaml", Line: 7}}
	if got := codequalitytest.AssertGitLabAcceptable(t, write(t, moved)); got[0].Fingerprint != issues[0].Fingerprint {
		t.Errorf("fingerprint changed once the line is known: %s, was %s", got[0].Fingerprint, issues[0].Fingerprint)
	}
}

// Objects that share an identity (unnamed, or one name twice in a file)
// still get distinct fingerprints, the first one's unchanged.
func TestFingerprintsDistinctForRepeatedIdentity(t *testing.T) {
	r := engine.Report{Findings: []engine.Finding{{
		Category: engine.CatRemovedAPI, Severity: engine.SevBlocker, Key: "removed-api/x/v1/K", Title: "x/v1 K removed",
		Objects: []inventory.ObjectRef{{File: "a.yaml", Line: 1}, {File: "a.yaml", Line: 9}},
	}}}
	issues := codequalitytest.AssertGitLabAcceptable(t, write(t, r))
	one := engine.Report{Findings: []engine.Finding{r.Findings[0]}}
	one.Findings[0].Objects = one.Findings[0].Objects[:1]
	if first := codequalitytest.AssertGitLabAcceptable(t, write(t, one)); len(issues) != 2 || issues[0].Fingerprint != first[0].Fingerprint {
		t.Errorf("entries %+v: want two, the first fingerprinted as when alone (%+v)", issues, first)
	}
}

// An empty report is an empty array, not null: GitLab reads null as no
// report.
func TestWriteEmpty(t *testing.T) {
	if got := string(write(t, engine.Report{Verdict: engine.VerdictReady})); got != "[]\n" {
		t.Errorf("empty report = %q, want []", got)
	}
}

func TestWritePropagatesWriteErrors(t *testing.T) {
	if err := Write(failingWriter{}, testReport()); err == nil {
		t.Fatal("Write to a failing writer returned nil")
	}
}

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errors.New("no space left on device") }
