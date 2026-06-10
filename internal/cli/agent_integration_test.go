package cli

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	apiextensionsclient "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/abd-ulbasit/upgradescope/internal/agent"
	"github.com/abd-ulbasit/upgradescope/internal/collect"
	"github.com/abd-ulbasit/upgradescope/internal/crd"
	"github.com/abd-ulbasit/upgradescope/internal/kb"
)

// TestAgentIntegration_CRDStatusOnKind runs the agent loop in-process against
// the kind demo cluster (hack/demo/kind-setup.sh) in CRD-only mode (no
// server), waits for the first tick, and asserts the ClusterReadiness status
// through the dynamic client: observed server version, KB version, a 0–100
// score, ready=false, and an eol-addon top finding (the EOL ingress-nginx).
//
// Run: ./hack/demo/kind-setup.sh && UPGRADESCOPE_IT=1 go test ./internal/cli/ -run Integration -v
func TestAgentIntegration_CRDStatusOnKind(t *testing.T) {
	if os.Getenv("UPGRADESCOPE_IT") != "1" {
		t.Skip("integration test: set UPGRADESCOPE_IT=1 to run (needs the kind cluster from hack/demo/kind-setup.sh)")
	}

	cfg, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		clientcmd.NewDefaultClientConfigLoadingRules(), &clientcmd.ConfigOverrides{}).ClientConfig()
	if err != nil {
		t.Fatalf("load kubeconfig: %v\nIs the demo cluster up? Run: ./hack/demo/kind-setup.sh", err)
	}
	clients, err := collect.NewClients(cfg)
	if err != nil {
		t.Fatalf("build collect clients: %v", err)
	}
	dyn, err := dynamic.NewForConfig(cfg)
	if err != nil {
		t.Fatalf("dynamic client: %v", err)
	}
	apiext, err := apiextensionsclient.NewForConfig(cfg)
	if err != nil {
		t.Fatalf("apiextensions client: %v", err)
	}
	k, err := kb.Load()
	if err != nil {
		t.Fatalf("kb.Load: %v", err)
	}

	const crName = "it-agent" // distinct from the chart-managed "cluster"
	gvr := schema.GroupVersionResource{Group: crd.Group, Version: crd.Version, Resource: crd.Plural}
	t.Cleanup(func() {
		cctx, ccancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer ccancel()
		_ = dyn.Resource(gvr).Delete(cctx, crName, metav1.DeleteOptions{})
	})

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	errCh := make(chan error, 1)
	go func() {
		errCh <- agent.Run(ctx, clients, dyn, apiext, k, agent.Config{
			Interval: time.Minute, // first tick fires immediately; we cancel after it
			CRName:   crName,      // ServerURL empty: CRD-only mode
		})
	}()

	// Poll until the first tick lands status.targets.
	var u *unstructured.Unstructured
	deadline := time.Now().Add(2 * time.Minute)
	for {
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for ClusterReadiness status.targets after first agent tick")
		}
		got, gerr := dyn.Resource(gvr).Get(ctx, crName, metav1.GetOptions{})
		if gerr == nil {
			if targets, found, _ := unstructured.NestedSlice(got.Object, "status", "targets"); found && len(targets) > 0 {
				u = got
				break
			}
		}
		select {
		case e := <-errCh:
			t.Fatalf("agent.Run exited before producing status: %v", e)
		case <-time.After(2 * time.Second):
		}
	}
	cancel() // graceful stop
	if e := <-errCh; e != nil && !errors.Is(e, context.Canceled) {
		t.Errorf("agent.Run on cancel = %v, want nil or context.Canceled", e)
	}

	if v, _, _ := unstructured.NestedString(u.Object, "status", "observedServerVersion"); v == "" {
		t.Error("status.observedServerVersion is empty")
	}
	if v, _, _ := unstructured.NestedString(u.Object, "status", "kbVersion"); v == "" {
		t.Error("status.kbVersion is empty")
	}

	targets, _, _ := unstructured.NestedSlice(u.Object, "status", "targets")
	first, ok := targets[0].(map[string]any)
	if !ok {
		t.Fatalf("status.targets[0] is %T, want object", targets[0])
	}
	score, found, err := unstructured.NestedInt64(first, "score")
	if err != nil || !found {
		t.Fatalf("targets[0].score missing (found=%v err=%v): %v", found, err, first)
	}
	if score < 0 || score > 100 {
		t.Errorf("targets[0].score = %d, want 0..100", score)
	}
	if ready, _, _ := unstructured.NestedBool(first, "ready"); ready {
		t.Error("targets[0].ready = true, want false (EOL ingress-nginx blocker expected)")
	}
	if blockers, _, _ := unstructured.NestedInt64(first, "blockers"); blockers < 1 {
		t.Errorf("targets[0].blockers = %d, want >= 1", blockers)
	}

	foundEOL := false
	tfs, _, _ := unstructured.NestedSlice(first, "topFindings")
	for _, tf := range tfs {
		if m, ok := tf.(map[string]any); ok && m["category"] == "eol-addon" {
			foundEOL = true
			t.Logf("eol-addon top finding: %v — %v", m["title"], m["remediation"])
		}
	}
	if !foundEOL {
		t.Errorf("no eol-addon category in topFindings: %v", tfs)
	}
	t.Logf("ClusterReadiness/%s: target=%v score=%d", crName, first["target"], score)
}
