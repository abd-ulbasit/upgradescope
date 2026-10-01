package cli

import (
	"fmt"
	"os"
	"strings"
	"testing"

	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
)

// itContextEnv names the kubeconfig context integration tests should use
// when it is not a kind cluster (or to pin one of several kind clusters).
const itContextEnv = "UPGRADESCOPE_IT_CONTEXT"

// itKubeContext picks the kubeconfig context integration tests may write to.
// The agent IT installs a cluster-scoped CRD, so a shell whose context has
// drifted to a real cluster must never be used: an explicit context (from
// UPGRADESCOPE_IT_CONTEXT) must exist, and otherwise the current context must
// be a kind cluster (kind-*). A non-empty skip is the reason to refuse.
func itKubeContext(cfg clientcmdapi.Config, explicit string) (name, skip string, err error) {
	if explicit != "" {
		if _, ok := cfg.Contexts[explicit]; !ok {
			return "", "", fmt.Errorf("%s=%q: no such context in the kubeconfig", itContextEnv, explicit)
		}
		return explicit, "", nil
	}
	cur := cfg.CurrentContext
	if cur == "" {
		return "", "integration test: kubeconfig has no current context; run ./hack/demo/kind-setup.sh or set " + itContextEnv, nil
	}
	if !strings.HasPrefix(cur, "kind-") {
		return "", fmt.Sprintf("integration test: refusing to run against current context %q (not a kind-* cluster); "+
			"run ./hack/demo/kind-setup.sh, or set %s to a disposable cluster's context", cur, itContextEnv), nil
	}
	return cur, "", nil
}

// itRESTConfig gates an integration test (UPGRADESCOPE_IT=1 plus the context
// guard above) and returns a client config pinned to the chosen context, so
// nothing later in the test can follow a context switch.
func itRESTConfig(t *testing.T) (*rest.Config, string) {
	t.Helper()
	if os.Getenv("UPGRADESCOPE_IT") != "1" {
		t.Skip("integration test: set UPGRADESCOPE_IT=1 to run (needs the kind cluster from hack/demo/kind-setup.sh)")
	}
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	raw, err := rules.Load()
	if err != nil {
		t.Fatalf("load kubeconfig: %v\nIs the demo cluster up? Run: ./hack/demo/kind-setup.sh", err)
	}
	name, skip, err := itKubeContext(*raw, os.Getenv(itContextEnv))
	if err != nil {
		t.Fatal(err)
	}
	if skip != "" {
		t.Skip(skip)
	}
	cfg, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		rules, &clientcmd.ConfigOverrides{CurrentContext: name}).ClientConfig()
	if err != nil {
		t.Fatalf("kubeconfig context %q: %v", name, err)
	}
	t.Logf("integration test using kube context %q", name)
	return cfg, name
}

// TestITKubeContext covers the guard every integration test goes through
// before it touches a cluster. It is a plain unit test (no cluster, no
// UPGRADESCOPE_IT): the guard is what keeps `make it` out of a real cluster.
func TestITKubeContext(t *testing.T) {
	kubeconfig := func(current string, contexts ...string) clientcmdapi.Config {
		c := clientcmdapi.Config{CurrentContext: current, Contexts: map[string]*clientcmdapi.Context{}}
		for _, name := range contexts {
			c.Contexts[name] = &clientcmdapi.Context{Cluster: name}
		}
		return c
	}
	gke := "gke_prod-project_europe-west1_prod"
	demo := "kind-upgradescope-demo"

	tests := []struct {
		name     string
		cfg      clientcmdapi.Config
		explicit string
		want     string
		skip     string // substring of the skip reason; "" = must not skip
		wantErr  string // substring of the error; "" = no error
	}{
		{name: "current kind context is used", cfg: kubeconfig(demo, demo, gke), want: demo},
		{name: "any kind-* context qualifies", cfg: kubeconfig("kind-other", "kind-other"), want: "kind-other"},
		{name: "non-kind current context is refused", cfg: kubeconfig(gke, demo, gke), skip: gke},
		{name: "refusal names the override", cfg: kubeconfig(gke, gke), skip: "UPGRADESCOPE_IT_CONTEXT"},
		{name: "no current context is refused", cfg: kubeconfig("", demo), skip: "no current context"},
		{name: "explicit context wins over a drifted current one", cfg: kubeconfig(gke, demo, gke), explicit: demo, want: demo},
		{name: "explicit non-kind context is allowed", cfg: kubeconfig(demo, demo, "minikube"), explicit: "minikube", want: "minikube"},
		{name: "explicit context missing from kubeconfig fails", cfg: kubeconfig(demo, demo), explicit: "kind-missing", wantErr: "kind-missing"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, skip, err := itKubeContext(tt.cfg, tt.explicit)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want one mentioning %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected err: %v", err)
			}
			if tt.skip != "" {
				if !strings.Contains(skip, tt.skip) {
					t.Fatalf("skip = %q, want it to mention %q", skip, tt.skip)
				}
				if got != "" {
					t.Errorf("context = %q alongside a refusal, want empty", got)
				}
				return
			}
			if skip != "" {
				t.Fatalf("unexpected refusal: %s", skip)
			}
			if got != tt.want {
				t.Errorf("context = %q, want %q", got, tt.want)
			}
		})
	}
}
