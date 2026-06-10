package cli

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/abd-ulbasit/upgradescope/internal/engine"
	"github.com/abd-ulbasit/upgradescope/internal/inventory"
)

// execScan runs the scan command with args, swapping the I/O pipeline for stub.
func execScan(t *testing.T, args []string, stub func(scanOptions) (engine.Report, error)) (string, error) {
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
	return out.String(), err
}

func okStub(r engine.Report) func(scanOptions) (engine.Report, error) {
	return func(scanOptions) (engine.Report, error) { return r, nil }
}

func TestScanRequiresTarget(t *testing.T) {
	_, err := execScan(t, []string{}, okStub(engine.Report{}))
	if err == nil || !strings.Contains(err.Error(), "target") {
		t.Fatalf("want missing --target error, got %v", err)
	}
}

func TestScanRejectsBadTarget(t *testing.T) {
	_, err := execScan(t, []string{"--target", "banana"}, okStub(engine.Report{}))
	if err == nil || !strings.Contains(err.Error(), "--target") {
		t.Fatalf("want invalid --target error, got %v", err)
	}
}

func TestScanRejectsBadOutput(t *testing.T) {
	_, err := execScan(t, []string{"--target", "1.36", "--output", "xml"}, okStub(engine.Report{}))
	if err == nil || !strings.Contains(err.Error(), "--output") {
		t.Fatalf("want invalid --output error, got %v", err)
	}
}

func TestScanRejectsBadFailOn(t *testing.T) {
	_, err := execScan(t, []string{"--target", "1.36", "--fail-on", "sometimes"}, okStub(engine.Report{}))
	if err == nil || !strings.Contains(err.Error(), "--fail-on") {
		t.Fatalf("want invalid --fail-on error, got %v", err)
	}
}

func TestScanFilesMutuallyExclusive(t *testing.T) {
	for _, args := range [][]string{
		{"--target", "1.36", "--files", "./m", "--context", "prod"},
		{"--target", "1.36", "--files", "./m", "--kubeconfig", "/tmp/kc"},
	} {
		if _, err := execScan(t, args, okStub(engine.Report{})); err == nil {
			t.Errorf("args %v: want mutual-exclusion error, got nil", args)
		}
	}
}

func TestScanFailOnExitCodeMapping(t *testing.T) {
	blocker := engine.Report{Findings: []engine.Finding{{Category: engine.CatEOLAddon, Severity: engine.SevBlocker, Title: "b"}}}
	warning := engine.Report{Findings: []engine.Finding{{Category: engine.CatVersionSkew, Severity: engine.SevWarning, Title: "w"}}}
	clean := engine.Report{Ready: true, Score: 100}

	cases := []struct {
		name   string
		report engine.Report
		failOn string
		code   int
	}{
		{"blocker hits blocker threshold", blocker, "blocker", 2},
		{"warning passes blocker threshold", warning, "blocker", 0},
		{"warning hits warning threshold", warning, "warning", 2},
		{"blocker hits warning threshold", blocker, "warning", 2},
		{"blocker ignored with never", blocker, "never", 0},
		{"clean passes", clean, "blocker", 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := execScan(t, []string{"--target", "1.36", "--output", "json", "--fail-on", tc.failOn}, okStub(tc.report))
			if got := ExitCode(err); got != tc.code {
				t.Fatalf("ExitCode = %d, want %d (err = %v)", got, tc.code, err)
			}
			if tc.code == 2 && !errors.Is(err, ErrGateFailed) {
				t.Fatalf("want ErrGateFailed, got %v", err)
			}
		})
	}
}

func TestScanPipelineErrorIsExitOne(t *testing.T) {
	boom := errors.New("kubeconfig not found")
	_, err := execScan(t, []string{"--target", "1.36"},
		func(scanOptions) (engine.Report, error) { return engine.Report{}, boom })
	if !errors.Is(err, boom) {
		t.Fatalf("want pipeline error, got %v", err)
	}
	if got := ExitCode(err); got != 1 {
		t.Fatalf("ExitCode = %d, want 1", got)
	}
}

func TestScanWritesSelectedFormat(t *testing.T) {
	r := engine.Report{
		ClusterID: "c1",
		Target:    inventory.Version{Major: 1, Minor: 36},
		KBVersion: "test-kb",
		Score:     100,
		Ready:     true,
	}

	out, err := execScan(t, []string{"--target", "1.36", "--output", "json"}, okStub(r))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, `"clusterId": "c1"`) {
		t.Errorf("json output missing clusterId:\n%s", out)
	}

	out, err = execScan(t, []string{"--target", "1.36", "--output", "sarif"}, okStub(r))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, `"version": "2.1.0"`) {
		t.Errorf("sarif output:\n%s", out)
	}

	out, err = execScan(t, []string{"--target", "1.36"}, okStub(r)) // default: table
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "SCORE  100/100") {
		t.Errorf("table output:\n%s", out)
	}
}
