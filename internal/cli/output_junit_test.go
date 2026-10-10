package cli

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/abd-ulbasit/upgradescope/internal/engine"
	"github.com/abd-ulbasit/upgradescope/internal/inventory"
	"github.com/abd-ulbasit/upgradescope/internal/junit/junittest"
)

// outputFormats is every --output value.
var outputFormats = []string{"table", "json", "sarif", "markdown", "junit", "gitlab-codequality"}

// The exit code is the gate's, whatever --output says (#73): a CI system
// that reads the JUnit or Code Quality report must not be the only thing
// that knows the gate failed, nor a format turn a failure into a pass.
func TestScanExitCodeIgnoresOutputFormat(t *testing.T) {
	warning := engine.Report{Score: 90, Verdict: engine.VerdictReady, Ready: true, Findings: []engine.Finding{{
		Category: engine.CatDeprecatedAPI, Severity: engine.SevWarning, Key: "deprecated-api/x/v1/K", Title: "x/v1 K deprecated"}}}
	unknown := engine.Report{Score: 100, Verdict: engine.VerdictUnknown, NotAssessed: []engine.CapabilityGap{
		{Capability: inventory.CapAPIUsage, Reason: "forbidden", Required: true}}}
	notUpgrade := engine.Report{Score: 100, Verdict: engine.VerdictUnknown, NotAssessed: []engine.CapabilityGap{
		{Capability: engine.GapTarget, Reason: "target 1.4 is not an upgrade", Required: true}}}
	ready := engine.Report{Score: 100, Verdict: engine.VerdictReady, Ready: true}
	cases := []struct {
		name string
		r    engine.Report
		args []string
		exit int
	}{
		{"blocker", eolNginxReport(), nil, 2},
		{"blocker, fail-on never", eolNginxReport(), []string{"--fail-on", "never"}, 0},
		{"warning, fail-on blocker", warning, nil, 0},
		{"warning, fail-on warning", warning, []string{"--fail-on", "warning"}, 2},
		{"unknown", unknown, nil, 2},
		{"unknown, allow-incomplete", unknown, []string{"--allow-incomplete"}, 0},
		{"not an upgrade, allow-incomplete", notUpgrade, []string{"--allow-incomplete"}, 2},
		{"ready", ready, nil, 0},
	}
	for _, tc := range cases {
		for _, format := range outputFormats {
			args := append([]string{"--target", "1.36", "--output", format}, tc.args...)
			out, _, err := execScanStderr(t, args, okStub(tc.r))
			if got := ExitCode(err); got != tc.exit {
				t.Errorf("%s, --output %s: exit %d (err %v), want %d", tc.name, format, got, err, tc.exit)
			}
			if out == "" {
				t.Errorf("%s, --output %s: no report written", tc.name, format)
			}
		}
	}
}

// A report that could not be written is exit 1 in the new formats too.
func TestScanCIFormatsPropagateWriteErrors(t *testing.T) {
	for _, format := range []string{"junit", "gitlab-codequality"} {
		orig := runScan
		runScan = okStub(eolNginxReport())
		cmd := newScanCmd()
		cmd.SetOut(failingWriter{})
		cmd.SetErr(&bytes.Buffer{})
		cmd.SetArgs([]string{"--target", "1.36", "--output", format})
		err := cmd.Execute()
		runScan = orig
		if got := ExitCode(err); got != 1 || !strings.Contains(err.Error(), "no space left on device") {
			t.Errorf("--output %s: err = %v (exit %d), want the write error, exit 1", format, err, got)
		}
	}
}

// --output junit end to end: the test outcomes follow --fail-on, the
// suppressed finding and the baseline are visible, and the object paths
// are relative to the working directory like SARIF's.
func TestScanFilesJUnit(t *testing.T) {
	cfg := writeConfig(t, "ignore:\n  - key: removed-api/batch/v1beta1/CronJob\n    reason: nightly job retires with 1.36\n")
	out, _, err := execScanFiles(t, "--files", "testdata/baseline/manifests", "--config", cfg,
		"--baseline", "testdata/baseline/baseline.json", "--output", "junit")
	if !errors.Is(err, ErrGateFailed) {
		t.Fatalf("err = %v, want ErrGateFailed (new Ingress and PDB)", err)
	}
	doc := junittest.Read(t, []byte(out))
	got := junittest.Outcomes(doc)
	for k, want := range map[string]string{
		"removed-api/removed-api/networking.k8s.io/v1beta1/Ingress":  "failure",
		"removed-api/removed-api/batch/v1beta1/CronJob (suppressed)": "skipped",
		"removed-api/removed-api/policy/v1beta1/PodDisruptionBudget": "failure",
		"not-assessed/versions": "skipped",
	} {
		if got[k] != want {
			t.Errorf("%s = %q, want %q (all: %v)", k, got[k], want, got)
		}
	}
	for _, want := range []string{"testdata/baseline/manifests/app.yaml:", "nightly job retires with 1.36"} {
		if !strings.Contains(out, want) {
			t.Errorf("JUnit lacks %q:\n%s", want, out)
		}
	}
	if *doc.Failures == 0 || *doc.Errors != 0 {
		t.Errorf("failures %d, errors %d: want the new blockers as failures, no errors (files mode needs no versions)", *doc.Failures, *doc.Errors)
	}
}

// A clean files-mode scan is not "readiness/no findings" (#308): files mode
// never runs deprecated-calls, helm or versions, so the report holds one
// skipped test per optional check that did not run, and nothing passed.
// Jenkins still sees three tests, so its empty-report check does not fire.
// docs/guides/other-ci.md describes exactly this shape
// (TestDocsOtherCIJUnitCleanReport).
func TestScanFilesCleanJUnit(t *testing.T) {
	dir := writeFiles(t, map[string]string{"web.yaml": "apiVersion: apps/v1\nkind: Deployment\nmetadata: {name: web, namespace: shop}\n" +
		"spec:\n  selector: {matchLabels: {a: b}}\n  template:\n    metadata: {labels: {a: b}}\n    spec:\n      containers: [{name: c, image: nginx:1.27}]\n"})
	out, _, err := execScanFiles(t, "--files", dir, "--output", "junit")
	if err != nil {
		t.Fatalf("a clean files-mode scan must pass the gate: %v", err)
	}
	doc := junittest.Read(t, []byte(out))
	want := map[string]string{
		"not-assessed/deprecated-calls": "skipped",
		"not-assessed/helm":             "skipped",
		"not-assessed/versions":         "skipped",
	}
	got := junittest.Outcomes(doc)
	if len(got) != len(want) {
		t.Errorf("outcomes = %v, want exactly %v", got, want)
	}
	for k, w := range want {
		if got[k] != w {
			t.Errorf("%s = %q, want %q (all: %v)", k, got[k], w, got)
		}
	}
	if *doc.Tests != 3 || *doc.Failures != 0 || *doc.Errors != 0 {
		t.Errorf("tests %d, failures %d, errors %d: want 3 skipped tests and nothing else", *doc.Tests, *doc.Failures, *doc.Errors)
	}
	if strings.Contains(out, "no findings") {
		t.Errorf("a files-mode scan has unassessed optional checks, so it must not claim `no findings` with every check assessed:\n%s", out)
	}
}
