package cli

import (
	"bytes"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/abd-ulbasit/upgradescope/internal/engine"
	"github.com/abd-ulbasit/upgradescope/internal/inventory"
)

// An unknown verdict means blockers may have gone unseen: the gate fails
// unless the caller explicitly accepts an incomplete assessment.
func TestScanGateOnUnknownVerdict(t *testing.T) {
	unknown := engine.Report{Score: 100, Verdict: engine.VerdictUnknown, NotAssessed: []engine.CapabilityGap{
		{Capability: inventory.CapAPIUsage, Reason: "discovery: forbidden", Required: true},
	}}
	unknownWithBlocker := unknown
	unknownWithBlocker.Verdict = engine.VerdictBlocked
	unknownWithBlocker.Findings = []engine.Finding{{Category: engine.CatEOLAddon, Severity: engine.SevBlocker, Title: "b"}}

	cases := []struct {
		name   string
		report engine.Report
		args   []string
		code   int
		err    error
	}{
		{"blocker threshold fails", unknown, []string{"--fail-on", "blocker"}, 2, ErrIncomplete},
		{"warning threshold fails", unknown, []string{"--fail-on", "warning"}, 2, ErrIncomplete},
		{"never never fails", unknown, []string{"--fail-on", "never"}, 0, nil},
		{"allow-incomplete passes", unknown, []string{"--fail-on", "blocker", "--allow-incomplete"}, 0, nil},
		{"allow-incomplete still fails on blockers", unknownWithBlocker, []string{"--fail-on", "blocker", "--allow-incomplete"}, 2, ErrGateFailed},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			args := append([]string{"--target", "1.36", "--output", "json"}, tc.args...)
			out, err := execScan(t, args, okStub(tc.report))
			if got := ExitCode(err); got != tc.code {
				t.Fatalf("ExitCode = %d, want %d (err = %v)", got, tc.code, err)
			}
			if tc.err != nil && !errors.Is(err, tc.err) {
				t.Fatalf("err = %v, want %v", err, tc.err)
			}
			if !strings.Contains(out, `"verdict": "`) {
				t.Errorf("report not printed before gating:\n%s", out)
			}
		})
	}
}

func TestWriteTableUnknownVerdictListsReasons(t *testing.T) {
	r := engine.Report{
		ClusterID: "c",
		Target:    inventory.Version{Major: 1, Minor: 38},
		KBVersion: "test-kb",
		Score:     95,
		Verdict:   engine.VerdictUnknown,
		Findings: []engine.Finding{
			{Category: engine.CatKBStale, Severity: engine.SevWarning, Title: "knowledge base does not cover Kubernetes 1.38 (newest known: 1.37)"},
		},
		NotAssessed: []engine.CapabilityGap{
			{Capability: inventory.CapHelm, Reason: "secrets list forbidden"},
			{Capability: engine.GapKBCoverage, Reason: "knowledge base covers Kubernetes up to 1.37; target 1.38 cannot be assessed", Required: true},
		},
	}
	var buf bytes.Buffer
	WriteTable(&buf, r)
	want := `upgradescope upgrade readiness report

Cluster:  c
Target:   1.38
KB:       test-kb

SCORE  95/100
READY  unknown (required checks were not assessed)
  kb-coverage: knowledge base covers Kubernetes up to 1.37; target 1.38 cannot be assessed

WARNING (1)
  [kb-stale] knowledge base does not cover Kubernetes 1.38 (newest known: 1.37)

NOT ASSESSED
  helm: secrets list forbidden
  kb-coverage: knowledge base covers Kubernetes up to 1.37; target 1.38 cannot be assessed
`
	if got := buf.String(); got != want {
		t.Errorf("table output mismatch\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

func TestWriteJSONIncludesVerdict(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteJSON(&buf, engine.Report{Verdict: engine.VerdictBlocked}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), `"verdict": "blocked"`) {
		t.Errorf("verdict missing:\n%s", buf.String())
	}
}

// writeServerKubeconfig writes a kubeconfig whose only context points at server.
func writeServerKubeconfig(t *testing.T, server string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "kubeconfig")
	cfg := fmt.Sprintf(`apiVersion: v1
kind: Config
clusters:
- name: test
  cluster:
    server: %s
users:
- name: test
  user:
    token: not-a-real-token
contexts:
- name: test-ctx
  context:
    cluster: test
    user: test
current-context: test-ctx
`, server)
	if err := os.WriteFile(path, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// execRealScan runs the scan command with the real I/O pipeline.
func execRealScan(t *testing.T, args []string) (string, error) {
	t.Helper()
	cmd := newScanCmd()
	var out, errOut bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	cmd.SetArgs(args)
	err := cmd.Execute()
	return out.String(), err
}

// A live scan that could not read anything (unreachable apiserver, bad
// credentials, RBAC forbidding everything) must not print a report that
// reads "SCORE 100/100": it is an error naming the context and server.
func TestScanUnreadableClusterIsError(t *testing.T) {
	forbidden := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"kind":"Status","apiVersion":"v1","status":"Failure","message":"forbidden","reason":"Forbidden","code":403}`))
	}))
	defer forbidden.Close()

	for name, server := range map[string]string{
		"connection refused": "https://127.0.0.1:1",
		"403 on everything":  forbidden.URL,
	} {
		t.Run(name, func(t *testing.T) {
			kc := writeServerKubeconfig(t, server)
			out, err := execRealScan(t, []string{"--target", "1.36", "--kubeconfig", kc, "--fail-on", "never"})
			if ExitCode(err) != 1 {
				t.Fatalf("ExitCode = %d, want 1 (err = %v)", ExitCode(err), err)
			}
			if out != "" {
				t.Errorf("no report may be printed, got:\n%s", out)
			}
			msg := err.Error()
			t.Logf("error: %s", msg)
			if strings.Contains(msg, "\n") {
				t.Errorf("error must be one line, got %q", msg)
			}
			for _, want := range []string{`"test-ctx"`, server} {
				if !strings.Contains(msg, want) {
					t.Errorf("error %q does not name %s", msg, want)
				}
			}
		})
	}
}
