package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// readmeGateCommand returns the README's server gate example: the fenced
// sh block that POSTs to /api/v1/gate.
func readmeGateCommand(t *testing.T) string {
	t.Helper()
	readme, err := os.ReadFile("../../README.md")
	if err != nil {
		t.Fatal(err)
	}
	for _, block := range strings.Split(string(readme), "```sh\n")[1:] {
		block, _, _ = strings.Cut(block, "```")
		if strings.Contains(block, "/api/v1/gate") {
			return block
		}
	}
	t.Fatal("README.md has no sh block that POSTs to /api/v1/gate")
	return ""
}

// TestGateREADMEExample runs the README's server gate command as written
// (#120 FS-04), against a server holding the cluster it names, with a
// removed API in rendered.yaml: the step must fail. When the command keeps
// the body on failure (--fail-with-body), the SARIF file must hold the
// report.
func TestGateREADMEExample(t *testing.T) {
	if _, err := exec.LookPath("curl"); err != nil {
		t.Skip("curl not installed")
	}
	command := readmeGateCommand(t)
	ts := httptest.NewServer(newTestServer(t, newFakeStore(), func(c *Config) { c.ReadToken = "read-tok" }).Handler())
	defer ts.Close()
	if resp, out := postSnapshot(t, ts, "ingest-tok", pushReqBody(t, testInventory()), false); resp.StatusCode != http.StatusAccepted {
		t.Fatalf("seed push = %d %v", resp.StatusCode, out)
	}

	run := func(manifest string) (error, []byte) {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "rendered.yaml"), []byte(manifest), 0o644); err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command("sh", "-ec", command)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "SERVER="+ts.URL, "READ_TOKEN=read-tok")
		out, err := cmd.CombinedOutput()
		if err != nil && !errors.As(err, new(*exec.ExitError)) {
			t.Fatalf("running the README command: %v\n%s", err, out)
		}
		sarifOut, _ := os.ReadFile(filepath.Join(dir, "results.sarif"))
		return err, sarifOut
	}

	if err, body := run(deploymentManifest); err != nil {
		t.Errorf("clean manifest: the README command failed: %v (%s)", err, body)
	}
	err, body := run(pspManifest)
	if err == nil {
		t.Fatalf("README command passed a removed API:\n%s\n%s", command, body)
	}
	if strings.Contains(command, "--fail-with-body") {
		var log struct {
			Runs []struct {
				Properties struct {
					Ready bool `json:"ready"`
				} `json:"properties"`
			} `json:"runs"`
		}
		if err := json.Unmarshal(body, &log); err != nil || len(log.Runs) != 1 || log.Runs[0].Properties.Ready {
			t.Errorf("results.sarif = %v %q, want the failing report", err, body)
		}
	} else {
		t.Logf("the README command discards the report when the gate fails (curl -f): use --fail-with-body")
	}
}
