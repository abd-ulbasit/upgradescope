package cli

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"

	apiextensionsclient "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"

	"github.com/abd-ulbasit/upgradescope/internal/collect"
)

// wantUserAgent is the audit-log identity of this build: the release, not
// client-go's own unstamped "v0.0.0" and "$Format" placeholder.
func wantUserAgent() string {
	return fmt.Sprintf("upgradescope/%s (%s/%s)", version, runtime.GOOS, runtime.GOARCH)
}

func TestKubernetesUserAgentNamesTheRelease(t *testing.T) {
	old := version
	t.Cleanup(func() { version = old })
	version = "9.8.7-rc.1"
	if got, want := kubernetesUserAgent(), "upgradescope/9.8.7-rc.1 ("+runtime.GOOS+"/"+runtime.GOARCH+")"; got != want {
		t.Errorf("kubernetesUserAgent() = %q, want %q", got, want)
	}
	for _, bad := range []string{"v0.0.0", "$Format", "kubernetes/"} {
		if strings.Contains(kubernetesUserAgent(), bad) {
			t.Errorf("user agent %q carries client-go's placeholder %q", kubernetesUserAgent(), bad)
		}
	}
}

// Both config builders stamp the agent; the in-cluster branch of the agent's
// builder cannot run outside a pod, so applyClientDefaults, which both of its
// branches go through, is checked directly.
func TestScanRESTConfigUserAgent(t *testing.T) {
	cfg, _, err := scanRESTConfig(writeKubeconfig(t), "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.UserAgent != wantUserAgent() {
		t.Errorf("scan UserAgent = %q, want %q", cfg.UserAgent, wantUserAgent())
	}
}

func TestBuildAgentRESTConfigUserAgent(t *testing.T) {
	cfg, err := buildAgentRESTConfig(writeKubeconfig(t), "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.UserAgent != wantUserAgent() {
		t.Errorf("agent (kubeconfig branch) UserAgent = %q, want %q", cfg.UserAgent, wantUserAgent())
	}
	inCluster := &rest.Config{Host: "https://10.0.0.1:443"}
	applyClientDefaults(inCluster, 0)
	if inCluster.UserAgent != wantUserAgent() {
		t.Errorf("agent (in-cluster branch) UserAgent = %q, want %q", inCluster.UserAgent, wantUserAgent())
	}
}

// Every client the scan and the agent build from the config sends the
// stamped header. This is the test that goes red if a later rebuild of the
// config drops the field, whatever the builders set.
func TestKubernetesRequestsNameTheRelease(t *testing.T) {
	var mu sync.Mutex
	var agents []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		agents = append(agents, r.Header.Get("User-Agent"))
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	kc := filepath.Join(t.TempDir(), "kubeconfig")
	body := strings.Replace(testKubeconfig, "https://127.0.0.1:6443", srv.URL, 1)
	if err := os.WriteFile(kc, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	cfgs := map[string]*rest.Config{}
	scanCfg, _, err := scanRESTConfig(kc, "", 0)
	if err != nil {
		t.Fatal(err)
	}
	cfgs["scan"] = scanCfg
	if cfgs["agent"], err = buildAgentRESTConfig(kc, "", 0); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for name, cfg := range cfgs {
		mu.Lock()
		agents = nil
		mu.Unlock()

		clients, err := collect.NewClients(cfg)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = clients.Kube.CoreV1().Namespaces().List(ctx, metav1.ListOptions{})
		_, _ = clients.Discovery.ServerVersion()
		_, _ = clients.Dynamic.Resource(schema.GroupVersionResource{Version: "v1", Resource: "secrets"}).List(ctx, metav1.ListOptions{})
		_, _ = clients.APIExtensions.ApiextensionsV1().CustomResourceDefinitions().List(ctx, metav1.ListOptions{})
		dyn, err := dynamic.NewForConfig(cfg)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = dyn.Resource(schema.GroupVersionResource{Group: "g.example", Version: "v1", Resource: "things"}).List(ctx, metav1.ListOptions{})
		ext, err := apiextensionsclient.NewForConfig(cfg)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = ext.ApiextensionsV1().CustomResourceDefinitions().List(ctx, metav1.ListOptions{})

		mu.Lock()
		got := append([]string(nil), agents...)
		mu.Unlock()
		if len(got) < 6 {
			t.Fatalf("%s: server saw %d requests, want at least 6", name, len(got))
		}
		for _, ua := range got {
			if ua != wantUserAgent() {
				t.Errorf("%s: a request carried User-Agent %q, want %q", name, ua, wantUserAgent())
			}
		}
	}
}
