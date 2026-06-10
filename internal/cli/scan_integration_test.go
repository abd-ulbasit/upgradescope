package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/abd-ulbasit/upgradescope/internal/engine"
	"github.com/abd-ulbasit/upgradescope/internal/inventory"
)

// TestScanIntegration_KindEOLIngressNginx is the gated end-to-end test: it
// scans a real kind cluster (created by hack/demo/kind-setup.sh, which
// installs an EOL ingress-nginx chart) against the next Kubernetes minor and
// asserts the scan gates with a blocker that includes an eol-addon finding
// for ingress-nginx.
//
// Run: ./hack/demo/kind-setup.sh && UPGRADESCOPE_IT=1 go test ./internal/cli/ -run Integration -v
func TestScanIntegration_KindEOLIngressNginx(t *testing.T) {
	if os.Getenv("UPGRADESCOPE_IT") != "1" {
		t.Skip("integration test: set UPGRADESCOPE_IT=1 to run (needs the kind cluster from hack/demo/kind-setup.sh)")
	}

	// Use the current kubeconfig context — kind-setup.sh leaves it pointing
	// at kind-upgradescope-demo. Same loading rules the CLI itself uses.
	clients, err := buildClients("", "")
	if err != nil {
		t.Fatalf("load kubeconfig: %v\nIs the demo cluster up? Run: ./hack/demo/kind-setup.sh", err)
	}

	sv, err := clients.Discovery.ServerVersion()
	if err != nil {
		t.Fatalf("discovery ServerVersion: %v\nIs the kind cluster 'upgradescope-demo' running? Run: ./hack/demo/kind-setup.sh", err)
	}
	cur, err := inventory.ParseVersion(sv.GitVersion)
	if err != nil {
		t.Fatalf("parse server version %q: %v", sv.GitVersion, err)
	}
	// Target the next minor: the test must not hardcode a version because
	// kind's default node image moves over time.
	target := fmt.Sprintf("%d.%d", cur.Major, cur.Minor+1)
	t.Logf("server version %s, scanning with --target %s", sv.GitVersion, target)

	// Precondition with a clear message: the EOL add-on must be installed.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := clients.Kube.CoreV1().Namespaces().Get(ctx, "ingress-nginx", metav1.GetOptions{}); err != nil {
		t.Fatalf("namespace ingress-nginx not found: %v\nThe demo cluster is missing the EOL add-on. Run: ./hack/demo/kind-setup.sh", err)
	}

	// Invoke the cobra command in-process (real runScan pipeline, counted
	// in coverage) exactly as `upgradescope scan --target X --output json`.
	cmd := newScanCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(io.Discard)
	cmd.SetArgs([]string{"--target", target, "--output", "json"})
	err = cmd.Execute()

	if !errors.Is(err, ErrGateFailed) {
		t.Fatalf("Execute() = %v, want ErrGateFailed (blockers expected from EOL ingress-nginx)\noutput:\n%s", err, out.String())
	}
	if got := ExitCode(err); got != 2 {
		t.Errorf("ExitCode = %d, want 2 (gate failed)", got)
	}

	var report engine.Report
	if err := json.Unmarshal(out.Bytes(), &report); err != nil {
		t.Fatalf("unmarshal JSON report: %v\noutput:\n%s", err, out.String())
	}

	found := false
	for _, f := range report.Findings {
		if f.Category != engine.CatEOLAddon {
			continue
		}
		if strings.Contains(f.Title, "Ingress NGINX") || strings.Contains(strings.Join(f.Namespaces, ","), "ingress-nginx") {
			found = true
			if f.Severity != engine.SevBlocker {
				t.Errorf("eol-addon finding severity = %q, want %q", f.Severity, engine.SevBlocker)
			}
			t.Logf("eol-addon finding: %s — %s", f.Title, f.Detail)
		}
	}
	if !found {
		t.Errorf("no eol-addon finding for ingress-nginx in report\noutput:\n%s", out.String())
	}
	if report.Ready {
		t.Errorf("report.Ready = true, want false (blockers present)")
	}
}
