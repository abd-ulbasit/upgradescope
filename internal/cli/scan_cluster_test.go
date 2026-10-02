package cli

import (
	"strings"
	"testing"
	"time"

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

func TestBuildAgentRESTConfigAppliesRequestTimeout(t *testing.T) {
	cfg, err := buildAgentRESTConfig(writeKubeconfig(t), "", 9*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Timeout != 9*time.Second {
		t.Errorf("Timeout = %v, want 9s", cfg.Timeout)
	}
}
