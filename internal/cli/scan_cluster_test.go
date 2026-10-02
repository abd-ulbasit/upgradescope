package cli

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"k8s.io/client-go/rest"

	"github.com/abd-ulbasit/upgradescope/internal/engine"
)

// --request-timeout bounds every API request of a live scan; 0 turns the
// per-request bound off (the step and scan deadlines still apply).
func TestScanRequestTimeoutFlag(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want time.Duration
	}{
		{nil, 30 * time.Second},
		{[]string{"--request-timeout", "5s"}, 5 * time.Second},
		{[]string{"--request-timeout", "0"}, 0},
	} {
		var got time.Duration
		_, err := execScan(t, append([]string{"--target", "1.36"}, tc.args...), func(opts scanOptions) (engine.Report, error) {
			got = opts.requestTimeout
			return engine.Report{}, nil
		})
		if err != nil {
			t.Fatalf("%v: %v", tc.args, err)
		}
		if got != tc.want {
			t.Errorf("%v: requestTimeout = %v, want %v", tc.args, got, tc.want)
		}
	}
	if _, err := execScan(t, []string{"--target", "1.36", "--request-timeout", "-1s"}, okStub(engine.Report{})); err == nil || !strings.Contains(err.Error(), "--request-timeout") {
		t.Errorf("negative --request-timeout: err = %v, want an --request-timeout error", err)
	}
	if _, err := execScan(t, []string{"--target", "1.36", "--files", "./m", "--request-timeout", "5s"}, okStub(engine.Report{})); err == nil {
		t.Error("--files with --request-timeout: want a mutual-exclusion error")
	}
}

func TestScanRESTConfigAppliesRequestTimeout(t *testing.T) {
	kc := writeKubeconfig(t)
	cfg, ctxName, err := scanRESTConfig(kc, "", 7*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Timeout != 7*time.Second {
		t.Errorf("Timeout = %v, want 7s", cfg.Timeout)
	}
	if ctxName != "test" {
		t.Errorf("context = %q, want the kubeconfig's current context \"test\"", ctxName)
	}
	if cfg, _, err = scanRESTConfig(kc, "", 0); err != nil || cfg.Timeout != 0 {
		t.Errorf("--request-timeout 0: Timeout = %v (err %v), want 0 (no per-request timeout)", cfg.Timeout, err)
	}
}

func TestAgentRequestTimeoutFlag(t *testing.T) {
	got, err := execAgent(t)
	if err != nil || got.requestTimeout != 30*time.Second {
		t.Errorf("default: requestTimeout = %v (err %v), want 30s", got.requestTimeout, err)
	}
	if got, err = execAgent(t, "--request-timeout", "0"); err != nil || got.requestTimeout != 0 {
		t.Errorf("0: requestTimeout = %v (err %v), want 0", got.requestTimeout, err)
	}
	if _, err = execAgent(t, "--request-timeout", "-1s"); err == nil || !strings.Contains(err.Error(), "--request-timeout") {
		t.Errorf("negative: err = %v, want an --request-timeout error", err)
	}
}

// The report names the API server by scheme, host and port: no
// credentials, path prefix or query from the kubeconfig.
func TestAPIServerURL(t *testing.T) {
	for _, tc := range []struct {
		cfg  rest.Config
		want string
	}{
		{rest.Config{Host: "https://10.0.0.1:6443"}, "https://10.0.0.1:6443"},
		{rest.Config{Host: "https://admin:s3cret@rancher.example.com/k8s/clusters/c-1?token=x#f"}, "https://rancher.example.com"},
		// No scheme: what client-go itself dials (TLS when a CA is set).
		{rest.Config{Host: "10.0.0.1:6443", TLSClientConfig: rest.TLSClientConfig{CAData: []byte("ca")}}, "https://10.0.0.1:6443"},
		{rest.Config{Host: "localhost:8080"}, "http://localhost:8080"},
	} {
		if got := apiServerURL(&tc.cfg); got != tc.want {
			t.Errorf("apiServerURL(%q) = %q, want %q", tc.cfg.Host, got, tc.want)
		}
	}
}

// #94: a live scan's report says which cluster it read, in every output
// that has a header. The fake API server answers only /metrics, which is
// enough for the scan not to be unreadable.
func TestScanNamesTheCluster(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/metrics" {
			w.Header().Set("Content-Type", "text/plain")
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()
	kc := writeServerKubeconfig(t, srv.URL)
	for format, want := range map[string]string{
		"table":    "Context:  test-ctx (API server " + srv.URL + ")\n",
		"markdown": "Context `test-ctx` · API server `" + srv.URL + "`\n",
		"json":     fmt.Sprintf("\"kubeContext\": \"test-ctx\",\n  \"apiServer\": %q,", srv.URL),
	} {
		out, err := execRealScan(t, []string{"--target", "1.35", "--kubeconfig", kc, "--fail-on", "never", "--output", format})
		if err != nil {
			t.Fatalf("%s: %v", format, err)
		}
		if !strings.Contains(out, want) {
			t.Errorf("%s output lacks %q:\n%s", format, want, out)
		}
	}
	var buf bytes.Buffer
	if err := WriteJSON(&buf, engine.Report{}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(buf.String(), "kubeContext") || strings.Contains(buf.String(), "apiServer") {
		t.Errorf("files-mode JSON names a cluster:\n%s", buf.String())
	}
}

func TestBuildAgentRESTConfigAppliesRequestTimeout(t *testing.T) {
	cfg, err := buildAgentRESTConfig(writeKubeconfig(t), "", 9*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Timeout != 9*time.Second {
		t.Errorf("Timeout = %v, want 9s", cfg.Timeout)
	}
}
