package cli

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

// acmeEntry is an operator's registry entry for a product the embedded
// registry does not know.
const acmeEntry = `schema_version: 2
id: acme-gateway
display_name: Acme Gateway
matchers:
  images:
    - acme/gateway
support:
  status: eol
  eol_date: "2026-01-31"
  citations:
    - https://acme.dev/gateway/lifecycle
`

const acmeDeployment = `apiVersion: apps/v1
kind: Deployment
metadata:
  name: gateway
  namespace: edge
spec:
  template:
    spec:
      containers:
        - name: gateway
          image: harbor.corp.example/acme/gateway:2.4.1
`

// #49: --registry-dir covers the operator's own add-ons: with it, an image
// no embedded entry knows is judged by the entry; without it, it is only
// listed as unrecognized.
func TestScanRegistryDirJudgesOperatorEntries(t *testing.T) {
	reg := writeFiles(t, map[string]string{"acme-gateway.yaml": acmeEntry})
	manifests := writeFiles(t, map[string]string{"gateway.yaml": acmeDeployment})

	out, _, err := execScanFiles(t, "--files", manifests, "--registry-dir", reg)
	if ExitCode(err) != 2 {
		t.Fatalf("err = %v, want exit 2 (blocked by the extra entry)\n%s", err, out)
	}
	if want := "[eol-addon] Acme Gateway is end-of-life since 2026-01-31\n"; !strings.Contains(out, want) {
		t.Errorf("output lacks %q:\n%s", want, out)
	}

	// A single file works too.
	if _, _, err := execScanFiles(t, "--files", manifests, "--registry-dir", filepath.Join(reg, "acme-gateway.yaml")); ExitCode(err) != 2 {
		t.Errorf("single file: err = %v, want exit 2", err)
	}

	out, _, _ = execScanFiles(t, "--files", manifests)
	if strings.Contains(out, "Acme Gateway") || !strings.Contains(out, "harbor.corp.example/acme/gateway") {
		t.Errorf("without --registry-dir the image must only be unrecognized:\n%s", out)
	}
}

// An invalid extra entry fails the command, naming the file; so does an
// entry the validator rejects in the same way as an embedded one.
func TestScanRegistryDirRejectsInvalidEntries(t *testing.T) {
	manifests := writeFiles(t, map[string]string{"gateway.yaml": acmeDeployment})
	reg := writeFiles(t, map[string]string{
		"acme-gateway.yaml": strings.Replace(acmeEntry, "https://acme.dev/gateway/lifecycle", "https://example.com/lifecycle", 1),
	})
	_, _, err := execScanFiles(t, "--files", manifests, "--registry-dir", reg)
	if err == nil || ExitCode(err) == 2 || !strings.Contains(err.Error(), "acme-gateway.yaml") {
		t.Errorf("err = %v, want a load error naming acme-gateway.yaml", err)
	}
	if _, _, err := execScanFiles(t, "--files", manifests, "--registry-dir", filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Error("a missing --registry-dir must fail, not scan without it")
	}
}

// agent and serve load the knowledge base before touching a cluster or
// listening, so a bad --registry-dir fails the start naming the file.
func TestAgentAndServeRegistryDirFailTheStart(t *testing.T) {
	reg := writeFiles(t, map[string]string{
		"acme-gateway.yaml": strings.Replace(acmeEntry, "status: eol", "status: deprecated", 1),
	})
	err := runAgent(context.Background(), agentOptions{registryDir: reg, logFormat: "text", logLevel: "info"})
	if err == nil || !strings.Contains(err.Error(), "acme-gateway.yaml") {
		t.Errorf("agent: err = %v, want a load error naming acme-gateway.yaml", err)
	}

	err = runServe(context.Background(), serveOptions{db: filepath.Join(t.TempDir(), "u.db"), registryDir: reg})
	if err == nil || !strings.Contains(err.Error(), "acme-gateway.yaml") {
		t.Errorf("serve: err = %v, want a load error naming acme-gateway.yaml", err)
	}
}

func TestRegistryDirFlagIsOnScanAgentAndServe(t *testing.T) {
	for _, c := range []string{"scan", "agent", "serve"} {
		sub, _, err := Root().Find([]string{c})
		if err != nil {
			t.Fatal(err)
		}
		if sub.Flags().Lookup("registry-dir") == nil {
			t.Errorf("%s has no --registry-dir", c)
		}
	}
}
