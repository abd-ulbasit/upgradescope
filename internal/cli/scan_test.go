package cli

import (
	"bytes"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/abd-ulbasit/upgradescope/internal/engine"
	"github.com/abd-ulbasit/upgradescope/internal/inventory"
	"github.com/abd-ulbasit/upgradescope/internal/kb"
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

func TestScanRejectsNonMajorOneTarget(t *testing.T) {
	_, err := execScan(t, []string{"--target", "2.0"}, okStub(engine.Report{}))
	if err == nil || !strings.Contains(err.Error(), "major version must be 1") {
		t.Fatalf("want major-version --target error, got %v", err)
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
		{"--target", "1.36", "--files", "./m", "--team-label", "owner"},
	} {
		if _, err := execScan(t, args, okStub(engine.Report{})); err == nil {
			t.Errorf("args %v: want mutual-exclusion error, got nil", args)
		}
	}
}

// --target is parsed exactly once, in validateScanOptions; runScan receives
// the already-parsed version instead of re-parsing the raw string.
func TestScanPassesParsedTarget(t *testing.T) {
	var got inventory.Version
	_, err := execScan(t, []string{"--target", "v1.36.2"},
		func(opts scanOptions) (engine.Report, error) {
			got = opts.targetVersion
			return engine.Report{}, nil
		})
	if err != nil {
		t.Fatal(err)
	}
	if want := (inventory.Version{Major: 1, Minor: 36}); got != want {
		t.Fatalf("targetVersion = %v, want %v", got, want)
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

	out, err = execScan(t, []string{"--target", "1.36", "--output", "markdown"}, okStub(r))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "score **100/100**") {
		t.Errorf("markdown output:\n%s", out)
	}

	out, err = execScan(t, []string{"--target", "1.36"}, okStub(r)) // default: table
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "SCORE  100/100") {
		t.Errorf("table output:\n%s", out)
	}
}

// evalStub evaluates inv against the embedded KB at the scan's parsed
// --target, the way runScan does after collecting.
func evalStub(t *testing.T, inv inventory.Inventory) func(scanOptions) (engine.Report, error) {
	t.Helper()
	k, err := kb.Load()
	if err != nil {
		t.Fatal(err)
	}
	return func(opts scanOptions) (engine.Report, error) {
		return engine.Evaluate(inv, k, opts.targetVersion, time.Date(2026, 10, 2, 0, 0, 0, 0, time.UTC)), nil
	}
}

// liveInventory is a fully assessed live inventory of a cluster at server.
func liveInventory(server string, cp ...inventory.ComponentVersion) inventory.Inventory {
	return inventory.Inventory{
		ClusterID: "c", Source: inventory.SourceCluster, ServerVersion: server,
		Capabilities: map[inventory.Capability]inventory.CapabilityStatus{
			inventory.CapAPIUsage: {Available: true}, inventory.CapVersions: {Available: true},
			inventory.CapAddOns: {Available: true}, inventory.CapHelm: {Available: true},
			inventory.CapDeprecatedCalls: {Available: true},
		},
		ControlPlane: cp,
	}
}

// A kube-scheduler that was too far behind (warning, in the baseline) and
// is now newer than kube-apiserver (blocker) is a new problem: the gate
// must fail on it, not match it to the baselined warning.
func TestBaselineSkewEscalationIsNew(t *testing.T) {
	apiserver := inventory.ComponentVersion{Component: "kube-apiserver", Version: "v1.35.2"}
	behind := liveInventory("v1.35.2", apiserver, inventory.ComponentVersion{Component: "kube-scheduler", Version: "v1.33.0"})
	newer := liveInventory("v1.35.2", apiserver, inventory.ComponentVersion{Component: "kube-scheduler", Version: "v1.36.0"})

	baseline := filepath.Join(t.TempDir(), "baseline.json")
	if _, _, err := execScanStderr(t, []string{"--target", "1.36", "--write-baseline", baseline}, evalStub(t, behind)); err != nil {
		t.Fatalf("baseline run: err = %v, want a passing gate (warning only)", err)
	}
	out, _, err := execScanStderr(t, []string{"--target", "1.36", "--baseline", baseline}, evalStub(t, newer))
	if !errors.Is(err, ErrGateFailed) {
		t.Fatalf("err = %v, want ErrGateFailed for the new blocker\n%s", err, out)
	}
	if !strings.Contains(out, "BASELINE  0 unchanged, 1 new") {
		t.Errorf("table does not count the blocker as new:\n%s", out)
	}
}
