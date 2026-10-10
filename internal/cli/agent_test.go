package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"k8s.io/client-go/rest"
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

// --interval 0 used to pass the flag and silently mean the 10m default in the
// agent (#192). It now fails like any other value below the minimum, before
// any cluster access; leaving the flag out still gives 10m.
func TestAgentIntervalMinimum(t *testing.T) {
	for _, v := range []string{"0", "0s", "30s", "59s", "-1m"} {
		_, err := execAgent(t, "--interval", v)
		if err == nil || !strings.Contains(err.Error(), "below minimum 1m") {
			t.Errorf("--interval %s: err = %v, want a below-minimum-1m error", v, err)
		}
	}
	for v, want := range map[string]time.Duration{"1m": time.Minute, "90s": 90 * time.Second, "1h": time.Hour} {
		if got, err := execAgent(t, "--interval", v); err != nil || got.interval != want {
			t.Errorf("--interval %s: interval %v, err %v, want %v", v, got.interval, err, want)
		}
	}
	if got, err := execAgent(t); err != nil || got.interval != 10*time.Minute {
		t.Errorf("no --interval: interval %v, err %v, want 10m", got.interval, err)
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
		podPassEvery: 3, podPassMaxAge: time.Hour,
		manageCRD: true, healthAddr: ":8081", logFormat: "text", logLevel: "info", requestTimeout: 30 * time.Second,
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("opts = %+v, want %+v", got, want)
	}
}

// The chart passes agent.targets as --targets and agent.manageCRD as
// --manage-crd; both default to "leave it to the CR / manage the CRD".
func TestAgentTargetsAndManageCRDFlags(t *testing.T) {
	got, err := execAgent(t)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.targets) != 0 || !got.manageCRD {
		t.Errorf("defaults: targets = %v manageCRD = %v, want [] true", got.targets, got.manageCRD)
	}
	got, err = execAgent(t, "--targets=1.37,1.38", "--manage-crd=false")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got.targets, []string{"1.37", "1.38"}) || got.manageCRD {
		t.Errorf("set: targets = %v manageCRD = %v, want [1.37 1.38] false", got.targets, got.manageCRD)
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
	cfg, err := buildAgentRESTConfig(writeKubeconfig(t), "", 0)
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
	cfg, err := buildAgentRESTConfig("", "", 0)
	if err != nil {
		t.Fatalf("fallback failed: %v", err)
	}
	if cfg.Host != "https://127.0.0.1:6443" {
		t.Errorf("Host = %q, want kubeconfig host (fallback path)", cfg.Host)
	}
}

// Probes, metrics and log shape are flags; the defaults match the chart.
func TestAgentObservabilityFlags(t *testing.T) {
	got, err := execAgent(t)
	if err != nil {
		t.Fatal(err)
	}
	if got.healthAddr != ":8081" || got.logFormat != "text" || got.logLevel != "info" {
		t.Errorf("defaults: healthAddr/logFormat/logLevel = %q/%q/%q, want :8081/text/info",
			got.healthAddr, got.logFormat, got.logLevel)
	}
	got, err = execAgent(t, "--health-addr=", "--log-format=json", "--log-level=debug")
	if err != nil {
		t.Fatal(err)
	}
	if got.healthAddr != "" || got.logFormat != "json" || got.logLevel != "debug" {
		t.Errorf("set: healthAddr/logFormat/logLevel = %q/%q/%q, want \"\"/json/debug",
			got.healthAddr, got.logFormat, got.logLevel)
	}
}

// The profiler is off by default and loopback-only when on (#247).
func TestAgentPprofAddrFlag(t *testing.T) {
	got, err := execAgent(t)
	if err != nil {
		t.Fatal(err)
	}
	if got.pprofAddr != "" {
		t.Errorf("default pprofAddr = %q, want empty (off)", got.pprofAddr)
	}
	got, err = execAgent(t, "--pprof-addr=127.0.0.1:6060")
	if err != nil {
		t.Fatal(err)
	}
	if got.pprofAddr != "127.0.0.1:6060" {
		t.Errorf("pprofAddr = %q, want 127.0.0.1:6060", got.pprofAddr)
	}
	for _, bad := range []string{":6060", "0.0.0.0:6060", "10.1.2.3:6060", "6060"} {
		if _, err := execAgent(t, "--pprof-addr="+bad); err == nil || !strings.Contains(err.Error(), "--pprof-addr") {
			t.Errorf("--pprof-addr=%s: error = %v, want a refusal naming the flag", bad, err)
		}
	}
}

func TestAgentRejectsBadLogFlags(t *testing.T) {
	for _, args := range [][]string{{"--log-format=xml"}, {"--log-level=loud"}} {
		if _, err := execAgent(t, args...); err == nil {
			t.Errorf("agent %v: want an error", args)
		}
	}
}

func TestNewAgentLogger(t *testing.T) {
	var buf bytes.Buffer
	log, err := newAgentLogger(&buf, "json", "warn")
	if err != nil {
		t.Fatal(err)
	}
	log.Info("dropped")
	log.Warn("kept", "k", "v")
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 1 {
		t.Fatalf("lines = %q, want only the WARN line", lines)
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &m); err != nil || m["msg"] != "kept" || m["k"] != "v" {
		t.Errorf("line %q (err %v), want JSON with msg=kept k=v", lines[0], err)
	}

	buf.Reset()
	log, err = newAgentLogger(&buf, "text", "info")
	if err != nil {
		t.Fatal(err)
	}
	log.Info("hello", "k", "v")
	if got := buf.String(); !strings.Contains(got, "msg=hello") || !strings.Contains(got, "k=v") {
		t.Errorf("text line = %q, want logfmt msg=hello k=v", got)
	}
}

// The agent warns at startup when it would push its bearer token over
// plain http:// to a host that is not loopback (#126 SE-09).
func TestRunAgentWarnsOnCleartextPush(t *testing.T) {
	origDefault, origStderr, origBuild := slog.Default(), os.Stderr, buildAgentRESTConfig
	t.Cleanup(func() { slog.SetDefault(origDefault); os.Stderr = origStderr; buildAgentRESTConfig = origBuild })
	buildAgentRESTConfig = func(string, string, time.Duration) (*rest.Config, error) {
		return nil, errors.New("no cluster in this test")
	}
	for _, tc := range []struct {
		url  string
		warn bool
	}{
		{"http://upgradescope-server.upgradescope.svc:8080", true},
		{"https://upgradescope-server.upgradescope.svc:8080", false},
	} {
		r, w, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		os.Stderr = w
		_ = runAgent(context.Background(), agentOptions{serverURL: tc.url, serverToken: "t", logFormat: "text", logLevel: "info"})
		w.Close()
		out, _ := io.ReadAll(r)
		if got := strings.Contains(string(out), "level=WARN") && strings.Contains(string(out), "plain http"); got != tc.warn {
			t.Errorf("%s: logged %q, want a plain-http warning: %v", tc.url, out, tc.warn)
		}
	}
}

// --server-ca-file names a private CA bundle for pushes; it needs
// --server-url.
func TestAgentServerCAFileFlag(t *testing.T) {
	got, err := execAgent(t, "--server-url", "https://hub.internal", "--server-token", "t", "--server-ca-file", "/etc/ca/ca.crt")
	if err != nil {
		t.Fatal(err)
	}
	if got.serverCAFile != "/etc/ca/ca.crt" {
		t.Errorf("serverCAFile = %q, want /etc/ca/ca.crt", got.serverCAFile)
	}
	if _, err := execAgent(t, "--server-ca-file", "/etc/ca/ca.crt"); err == nil || !strings.Contains(err.Error(), "--server-ca-file") {
		t.Errorf("--server-ca-file without --server-url: err = %v, want one naming the flag", err)
	}
	// Over plain http the CA would verify nothing; saying so beats a flag
	// that silently does nothing.
	if _, err := execAgent(t, "--server-url", "HTTP://hub.internal", "--server-token", "t", "--server-ca-file", "/etc/ca/ca.crt"); err == nil || !strings.Contains(err.Error(), "https") {
		t.Errorf("--server-ca-file with an http --server-url: err = %v, want one asking for https", err)
	}
}

// An unusable --server-ca-file fails the start before any cluster access,
// instead of every push.
func TestRunAgentRejectsUnusableServerCAFile(t *testing.T) {
	origDefault, origBuild := slog.Default(), buildAgentRESTConfig
	t.Cleanup(func() { slog.SetDefault(origDefault); buildAgentRESTConfig = origBuild })
	buildAgentRESTConfig = func(string, string, time.Duration) (*rest.Config, error) {
		return nil, errors.New("no cluster in this test")
	}
	missing := filepath.Join(t.TempDir(), "missing.crt")
	err := runAgent(context.Background(), agentOptions{serverURL: "https://hub.internal", serverToken: "t",
		serverCAFile: missing, logFormat: "text", logLevel: "error"})
	if err == nil || !strings.Contains(err.Error(), "--server-ca-file") || !strings.Contains(err.Error(), missing) {
		t.Errorf("runAgent with a missing CA file: err = %v, want one naming --server-ca-file and the file", err)
	}
}

// #238: push settings that can never work stop the agent before it
// touches the cluster, with a message naming the flag.
func TestAgentRefusesUnusablePushSettings(t *testing.T) {
	for _, tc := range []struct {
		args []string
		env  string
		want string
	}{
		{[]string{"--server-url", "upgradescope.example.com", "--server-token", "t"}, "", "--server-url"},
		{[]string{"--server-url", "localhost:8080", "--server-token", "t"}, "", "--server-url"},
		{[]string{"--server-url", "ftp://upgradescope.example.com", "--server-token", "t"}, "", "--server-url"},
		{[]string{"--server-url", "https://upgradescope.example.com"}, "ab cd", "--server-token"},
		{[]string{"--server-url", "https://upgradescope.example.com", "--server-token", "a\tb"}, "", "--server-token"},
		{[]string{"--cluster-name", ""}, "", "--cluster-name"},
		{[]string{"--force-sync-every", "0"}, "", "--force-sync-every"},
		{[]string{"--force-sync-every", "-1s"}, "", "--force-sync-every"},
	} {
		t.Setenv("UPGRADESCOPE_SERVER_TOKEN", tc.env)
		ran := false
		orig := runAgent
		runAgent = func(context.Context, agentOptions) error { ran = true; return nil }
		root := Root()
		root.SetArgs(append([]string{"agent"}, tc.args...))
		err := root.Execute()
		runAgent = orig
		if err == nil || !strings.Contains(err.Error(), tc.want) || ran {
			t.Errorf("agent %q (env token %q): err = %v, ran = %v; want a refusal naming %s before the agent runs", tc.args, tc.env, err, ran, tc.want)
		}
	}
}

// The trailing newline of a token Secret made from a file (kubectl create
// secret --from-file, echo without -n) is trimmed from the environment as
// it is from --server-token-file.
func TestAgentTrimsTheEnvironmentToken(t *testing.T) {
	t.Setenv("UPGRADESCOPE_SERVER_TOKEN", "env-tok\n")
	got, err := execAgent(t, "--server-url", "https://upgradescope.example.com")
	if err != nil || got.serverToken != "env-tok" {
		t.Fatalf("serverToken = %q, err %v; want env-tok", got.serverToken, err)
	}
}

func TestAgentForceSyncEveryFlag(t *testing.T) {
	got, err := execAgent(t, "--force-sync-every", "2h")
	if err != nil || got.forceSyncEvery != 2*time.Hour {
		t.Fatalf("forceSyncEvery = %v, err %v; want 2h", got.forceSyncEvery, err)
	}
	// Below the interval is accepted here; the agent raises it at start.
	if _, err := execAgent(t, "--force-sync-every", "1m"); err != nil {
		t.Errorf("--force-sync-every 1m: %v, want accepted", err)
	}
}

// A push token from --server-token-file is handed to the agent as a path it
// re-reads, so a rotated Secret needs no restart; one from the flag or the
// environment is a fixed value.
func TestAgentServerTokenFileFollowsRotation(t *testing.T) {
	file := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(file, []byte("file-tok\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := execAgent(t, "--server-token-file", file)
	if err != nil || got.serverTokenFile != file {
		t.Fatalf("serverTokenFile = %q, err %v; want %q", got.serverTokenFile, err, file)
	}
	t.Setenv("UPGRADESCOPE_SERVER_TOKEN", "env-tok")
	if got, err = execAgent(t); err != nil || got.serverTokenFile != "" {
		t.Fatalf("env token: serverTokenFile = %q, err %v; want none", got.serverTokenFile, err)
	}
	if got, err = execAgent(t, "--server-token", "flag-tok"); err != nil || got.serverTokenFile != "" {
		t.Fatalf("flag token: serverTokenFile = %q, err %v; want none", got.serverTokenFile, err)
	}
}

// #228: --pod-pass-every and --pod-pass-max-age default to 3 and 1h, are
// passed through, and refuse a value that could never work, 0 included,
// before any cluster access.
func TestAgentPodPassFlags(t *testing.T) {
	got, err := execAgent(t)
	if err != nil || got.podPassEvery != 3 || got.podPassMaxAge != time.Hour {
		t.Fatalf("defaults = %d, %v, err %v; want 3 and 1h", got.podPassEvery, got.podPassMaxAge, err)
	}
	got, err = execAgent(t, "--pod-pass-every", "1", "--pod-pass-max-age", "20m")
	if err != nil || got.podPassEvery != 1 || got.podPassMaxAge != 20*time.Minute {
		t.Fatalf("set = %d, %v, err %v; want 1 and 20m", got.podPassEvery, got.podPassMaxAge, err)
	}
	// No upper bound is set: a count of ticks beyond the lifetime of any
	// process is accepted, and the agent's start-up warning saturates its
	// floor instead of overflowing (agent.TestPodPassMaxAgeFloorSaturates).
	got, err = execAgent(t, "--pod-pass-every", "9223372036854775807")
	if err != nil || got.podPassEvery != math.MaxInt {
		t.Fatalf("--pod-pass-every at the largest int = %d, err %v; want it accepted", got.podPassEvery, err)
	}
	for _, tc := range []struct{ flag, val string }{
		{"--pod-pass-every", "0"}, {"--pod-pass-every", "-2"}, {"--pod-pass-max-age", "0"}, {"--pod-pass-max-age", "-1m"},
	} {
		if _, err := execAgent(t, tc.flag, tc.val); err == nil || !strings.Contains(err.Error(), "invalid "+tc.flag) {
			t.Errorf("%s %s: err = %v, want it refused, naming the flag", tc.flag, tc.val, err)
		}
	}
}
