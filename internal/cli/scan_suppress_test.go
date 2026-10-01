package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/abd-ulbasit/upgradescope/internal/engine"
	"github.com/abd-ulbasit/upgradescope/internal/sarif/sariftest"
)

var update = flag.Bool("update", false, "rewrite golden files under testdata")

// eolNginxReport is the issue's strongest case: a live cluster blocked by
// nothing but an EOL ingress-nginx.
func eolNginxReport() engine.Report {
	return engine.Report{
		ClusterID: "c", Score: 75, Verdict: engine.VerdictBlocked,
		Findings: []engine.Finding{{
			Category: engine.CatEOLAddon, Severity: engine.SevBlocker, Key: "eol-addon/ingress-nginx",
			Title: "ingress-nginx 1.11 is past end of life", Namespaces: []string{"ingress-nginx"},
		}},
	}
}

// execScanStderr is execScan that also returns stderr.
func execScanStderr(t *testing.T, args []string, stub func(scanOptions) (engine.Report, error)) (string, string, error) {
	t.Helper()
	orig := runScan
	runScan = stub
	t.Cleanup(func() { runScan = orig })
	cmd := newScanCmd()
	var out, errOut bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	cmd.SetArgs(args)
	err := cmd.Execute()
	return out.String(), errOut.String(), err
}

func writeConfig(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), ".upgradescope.yaml")
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// An ignore entry for the EOL ingress-nginx key makes a live scan pass at
// --fail-on blocker; the table says 1 suppressed and the JSON lists it
// with its reason.
func TestScanIgnoreRuleSuppressesEOLAddon(t *testing.T) {
	cfg := writeConfig(t, `ignore:
  - key: eol-addon/ingress-nginx
    reason: migrating to Gateway API, tracked in PLAT-123
    expires: 2099-12-31
`)
	out, _, err := execScanStderr(t, []string{"--target", "1.36", "--config", cfg, "--fail-on", "blocker"}, okStub(eolNginxReport()))
	if err != nil {
		t.Fatalf("err = %v, want a passing gate", err)
	}
	for _, want := range []string{"SCORE  100/100", "READY  yes", "1 suppressed", "SUPPRESSED (1)", "ingress-nginx 1.11 is past end of life", "reason: migrating to Gateway API, tracked in PLAT-123 (until 2099-12-31)"} {
		if !strings.Contains(out, want) {
			t.Errorf("table missing %q:\n%s", want, out)
		}
	}

	out, _, err = execScanStderr(t, []string{"--target", "1.36", "--config", cfg, "--output", "json"}, okStub(eolNginxReport()))
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Verdict    engine.Verdict             `json:"verdict"`
		Findings   []engine.Finding           `json:"findings"`
		Suppressed []engine.SuppressedFinding `json:"suppressed"`
	}
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatal(err)
	}
	if doc.Verdict != engine.VerdictReady || len(doc.Findings) != 0 || len(doc.Suppressed) != 1 ||
		doc.Suppressed[0].Key != "eol-addon/ingress-nginx" || doc.Suppressed[0].Source != cfg || doc.Suppressed[0].Reason == "" {
		t.Errorf("json = %s", out)
	}
}

func TestScanInvalidConfigIsExitOne(t *testing.T) {
	cases := map[string]string{
		"no reason":     "ignore:\n  - key: eol-addon/ingress-nginx\n",
		"unknown field": "ignore:\n  - key: eol-addon/ingress-nginx\n    reason: r\n    until: 2026-01-01\n",
	}
	for name, content := range cases {
		t.Run(name, func(t *testing.T) {
			called := false
			stub := func(scanOptions) (engine.Report, error) { called = true; return eolNginxReport(), nil }
			_, _, err := execScanStderr(t, []string{"--target", "1.36", "--config", writeConfig(t, content)}, stub)
			if err == nil || ExitCode(err) != 1 {
				t.Fatalf("err = %v (exit %d), want exit 1", err, ExitCode(err))
			}
			if called {
				t.Error("scanned despite an invalid config")
			}
		})
	}
	_, _, err := execScanStderr(t, []string{"--target", "1.36", "--config", filepath.Join(t.TempDir(), "missing.yaml")}, okStub(eolNginxReport()))
	if ExitCode(err) != 1 {
		t.Errorf("missing --config file: err = %v, want exit 1", err)
	}
}

func TestScanExpiredRuleWarnsAndFails(t *testing.T) {
	cfg := writeConfig(t, "ignore:\n  - key: eol-addon/ingress-nginx\n    reason: r\n    expires: 2020-01-31\n")
	_, stderr, err := execScanStderr(t, []string{"--target", "1.36", "--config", cfg}, okStub(eolNginxReport()))
	if !errors.Is(err, ErrGateFailed) {
		t.Fatalf("err = %v, want ErrGateFailed", err)
	}
	if !strings.Contains(stderr, "warning: "+cfg+": ignore[0] (key eol-addon/ingress-nginx) expired on 2020-01-31") {
		t.Errorf("stderr = %q", stderr)
	}
}

// .upgradescope.yaml in the --files directory is picked up without
// --config, and file globs are relative to it.
func TestScanFilesDiscoversConfig(t *testing.T) {
	dir := writeFiles(t, map[string]string{
		"legacy/cron.yaml": "apiVersion: batch/v1beta1\nkind: CronJob\nmetadata:\n  name: nightly\n",
		".upgradescope.yaml": `ignore:
  - category: removed-api
    file: legacy/**
    reason: legacy/ is not deployed
`,
	})
	out, _, err := execScanFiles(t, "--files", dir, "--output", "json")
	if err != nil {
		t.Fatalf("err = %v\n%s", err, out)
	}
	if !strings.Contains(out, `"suppressed": [`) || !strings.Contains(out, `"reason": "legacy/ is not deployed"`) {
		t.Errorf("json = %s", out)
	}
}

// Annotations suppress in files mode without any config file.
func TestScanFilesAnnotationSuppresses(t *testing.T) {
	dir := writeFiles(t, map[string]string{"cron.yaml": `apiVersion: batch/v1beta1
kind: CronJob
metadata:
  name: nightly
  annotations:
    upgradescope.dev/ignore: removed-api
    upgradescope.dev/ignore-reason: deleted in the next release
`})
	out, _, err := execScanFiles(t, "--files", dir)
	if err != nil {
		t.Fatalf("err = %v\n%s", err, out)
	}
	if !strings.Contains(out, "1 suppressed") || !strings.Contains(out, "reason: deleted in the next release (annotation)") {
		t.Errorf("table = %s", out)
	}
}

// The baseline golden: testdata/baseline/manifests against
// baseline.json. The CronJob (line moved) is unchanged; the Ingress has a
// new object and the PodDisruptionBudget a new key, so the gate fails on
// those two only.
func TestScanBaselineGolden(t *testing.T) {
	out, _, err := execScanFiles(t, "--files", "testdata/baseline/manifests", "--baseline", "testdata/baseline/baseline.json", "--output", "json")
	if !errors.Is(err, ErrGateFailed) {
		t.Fatalf("err = %v, want ErrGateFailed", err)
	}
	var doc struct {
		Findings []engine.Finding `json:"findings"`
	}
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatal(err)
	}
	var got strings.Builder
	for _, f := range doc.Findings {
		got.WriteString(string(f.BaselineState) + " " + f.Key + "\n")
	}
	golden := "testdata/baseline/expected.txt"
	if *update {
		if err := os.WriteFile(golden, []byte(got.String()), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatal(err)
	}
	if got.String() != string(want) {
		t.Errorf("baseline states:\n%s\nwant (%s):\n%s", got.String(), golden, want)
	}
}

// --write-baseline then --baseline on the same manifests: nothing is new,
// so the gate passes although the blockers (and the verdict) remain.
func TestScanWriteBaselineRoundTrip(t *testing.T) {
	baseline := filepath.Join(t.TempDir(), "baseline.json")
	_, _, err := execScanFiles(t, "--files", "testdata/baseline/manifests", "--write-baseline", baseline)
	if !errors.Is(err, ErrGateFailed) {
		t.Fatalf("writing run: err = %v, want ErrGateFailed (baseline written anyway)", err)
	}
	out, _, err := execScanFiles(t, "--files", "testdata/baseline/manifests", "--baseline", baseline)
	if err != nil {
		t.Fatalf("err = %v, want a passing gate\n%s", err, out)
	}
	for _, want := range []string{"READY  no", "BASELINE  3 unchanged, 0 new"} {
		if !strings.Contains(out, want) {
			t.Errorf("table missing %q:\n%s", want, out)
		}
	}
}

// SARIF carries suppressions and baseline states end to end.
func TestScanFilesSARIFSuppressedAndBaseline(t *testing.T) {
	cfg := writeConfig(t, "ignore:\n  - key: removed-api/batch/v1beta1/CronJob\n    reason: nightly job retires with 1.36\n")
	out, _, err := execScanFiles(t, "--files", "testdata/baseline/manifests", "--config", cfg,
		"--baseline", "testdata/baseline/baseline.json", "--output", "sarif")
	if !errors.Is(err, ErrGateFailed) {
		t.Fatalf("err = %v, want ErrGateFailed (new Ingress and PDB)", err)
	}
	sariftest.AssertGitHubAcceptable(t, []byte(out))
	for _, want := range []string{`"kind": "external"`, `"justification": "nightly job retires with 1.36"`, `"baselineState": "new"`} {
		if !strings.Contains(out, want) {
			t.Errorf("SARIF lacks %s:\n%s", want, out)
		}
	}
}

func TestScanBaselineErrors(t *testing.T) {
	notReport := filepath.Join(t.TempDir(), "x.json")
	if err := os.WriteFile(notReport, []byte(`{"version": "2.1.0"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{notReport, filepath.Join(t.TempDir(), "missing.json")} {
		_, _, err := execScanStderr(t, []string{"--target", "1.36", "--baseline", p}, okStub(eolNginxReport()))
		if err == nil || ExitCode(err) != 1 {
			t.Errorf("--baseline %s: err = %v, want exit 1", p, err)
		}
	}
}

// A required check that was not assessed still fails the gate when every
// blocker is in the baseline: a new blocker may be hiding behind it.
func TestScanBaselineStillGatesIncomplete(t *testing.T) {
	r := eolNginxReport()
	r.NotAssessed = []engine.CapabilityGap{{Capability: "api-usage", Reason: "forbidden", Required: true}}
	baseline := filepath.Join(t.TempDir(), "b.json")
	if err := os.WriteFile(baseline, []byte(`{"schemaVersion": 1, "findings": [{"key": "eol-addon/ingress-nginx"}]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	_, _, err := execScanStderr(t, []string{"--target", "1.36", "--baseline", baseline}, okStub(r))
	if !errors.Is(err, ErrIncomplete) {
		t.Fatalf("err = %v, want ErrIncomplete", err)
	}
	_, _, err = execScanStderr(t, []string{"--target", "1.36", "--baseline", baseline, "--allow-incomplete"}, okStub(r))
	if err != nil {
		t.Fatalf("--allow-incomplete: err = %v", err)
	}
}
