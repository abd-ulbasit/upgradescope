package cli

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

const testKubeconfig = `apiVersion: v1
kind: Config
clusters:
- name: test
  cluster:
    server: https://127.0.0.1:6443
contexts:
- name: test
  context:
    cluster: test
    user: test
current-context: test
users:
- name: test
  user:
    token: abc
`

func writeKubeconfig(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "kubeconfig")
	if err := os.WriteFile(path, []byte(testKubeconfig), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func execAgent(t *testing.T, args ...string) (agentOptions, error) {
	t.Helper()
	orig := runAgent
	t.Cleanup(func() { runAgent = orig })
	var got agentOptions
	runAgent = func(_ context.Context, opts agentOptions) error {
		got = opts
		return nil
	}
	root := Root()
	root.SetArgs(append([]string{"agent"}, args...))
	err := root.Execute()
	return got, err
}

// The agent's push token can come from $UPGRADESCOPE_SERVER_TOKEN or
// --server-token-file instead of argv; an explicit flag still wins.
func TestAgentServerTokenSources(t *testing.T) {
	t.Setenv("UPGRADESCOPE_SERVER_TOKEN", "env-tok")
	got, err := execAgent(t)
	if err != nil || got.serverToken != "env-tok" {
		t.Fatalf("env: serverToken = %q, err %v; want env-tok", got.serverToken, err)
	}
	file := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(file, []byte("file-tok\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err = execAgent(t, "--server-token-file", file)
	if err != nil || got.serverToken != "file-tok" {
		t.Fatalf("file: serverToken = %q, err %v; want file-tok", got.serverToken, err)
	}
	got, err = execAgent(t, "--server-token", "flag-tok")
	if err != nil || got.serverToken != "flag-tok" {
		t.Fatalf("flag: serverToken = %q, err %v; want flag-tok", got.serverToken, err)
	}
}

func TestAgentCmdFlagDefaults(t *testing.T) {
	orig := runAgent
	defer func() { runAgent = orig }()
	var got agentOptions
	runAgent = func(_ context.Context, opts agentOptions) error {
		got = opts
		return nil
	}

	root := Root()
	root.SetArgs([]string{"agent"})
	if err := root.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if got.interval != 10*time.Minute {
		t.Errorf("interval = %v, want 10m", got.interval)
	}
	if got.crName != "cluster" || got.teamLabel != "team" {
		t.Errorf("crName/teamLabel = %q/%q, want cluster/team", got.crName, got.teamLabel)
	}
	if got.forceSyncEvery != time.Hour {
		t.Errorf("forceSyncEvery = %v, want 1h", got.forceSyncEvery)
	}
	if got.serverURL != "" || got.serverToken != "" {
		t.Errorf("server flags should default empty: %+v", got)
	}
}

func TestAgentCmdFlagsParsed(t *testing.T) {
	orig := runAgent
	defer func() { runAgent = orig }()
	var got agentOptions
	runAgent = func(_ context.Context, opts agentOptions) error {
		got = opts
		return nil
	}

	root := Root()
	root.SetArgs([]string{"agent",
		"--interval", "5m",
		"--server-url", "http://scope:8080",
		"--server-token", "tok",
		"--cluster-name", "prod-eu-1",
		"--cr-name", "main",
		"--team-label", "squad",
		"--force-sync-every", "30m",
		"--kubeconfig", "/tmp/kc",
		"--context", "ctx1",
	})
	if err := root.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}
	want := agentOptions{
		interval: 5 * time.Minute, serverURL: "http://scope:8080", serverToken: "tok",
		clusterName: "prod-eu-1", crName: "main", teamLabel: "squad",
		forceSyncEvery: 30 * time.Minute, kubeconfig: "/tmp/kc", kubecontext: "ctx1",
	}
	if got != want {
		t.Errorf("opts = %+v, want %+v", got, want)
	}
}

func TestRootHasAgentSubcommand(t *testing.T) {
	for _, c := range Root().Commands() {
		if c.Name() == "agent" {
			return
		}
	}
	t.Fatal("root command has no agent subcommand")
}

func TestBuildAgentRESTConfigExplicitKubeconfig(t *testing.T) {
	cfg, err := buildAgentRESTConfig(writeKubeconfig(t), "")
	if err != nil {
		t.Fatalf("buildAgentRESTConfig: %v", err)
	}
	if cfg.Host != "https://127.0.0.1:6443" {
		t.Errorf("Host = %q, want https://127.0.0.1:6443", cfg.Host)
	}
}

// Outside a pod, rest.InClusterConfig fails (even with the env vars set there
// is no service-account token file) and the loader must fall back to
// kubeconfig loading rules ($KUBECONFIG here).
func TestBuildAgentRESTConfigFallsBackFromInCluster(t *testing.T) {
	t.Setenv("KUBERNETES_SERVICE_HOST", "10.0.0.1")
	t.Setenv("KUBERNETES_SERVICE_PORT", "443")
	t.Setenv("KUBECONFIG", writeKubeconfig(t))
	cfg, err := buildAgentRESTConfig("", "")
	if err != nil {
		t.Fatalf("fallback failed: %v", err)
	}
	if cfg.Host != "https://127.0.0.1:6443" {
		t.Errorf("Host = %q, want kubeconfig host (fallback path)", cfg.Host)
	}
}
