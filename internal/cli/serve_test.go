package cli

import (
	"bytes"
	"context"
	"math"
	"net/netip"
	"os"
	"path/filepath"
	"reflect"
	"runtime/debug"
	"slices"
	"strings"
	"testing"

	"github.com/abd-ulbasit/upgradescope/internal/inventory"
	"github.com/abd-ulbasit/upgradescope/internal/server"
)

// execServe runs the serve command with args, swapping the wiring for stub
// (same seam pattern as execScan).
func execServe(t *testing.T, args []string, stub func(context.Context, serveOptions) error) error {
	t.Helper()
	orig := runServe
	runServe = stub
	t.Cleanup(func() { runServe = orig })

	cmd := newServeCmd()
	var out, errOut bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	cmd.SetArgs(args)
	return cmd.Execute()
}

func serveOK() func(context.Context, serveOptions) error {
	return func(context.Context, serveOptions) error { return nil }
}

// The shared --ingest-token is optional: without it only per-cluster
// tokens authenticate pushes.
func TestServeWithoutIngestToken(t *testing.T) {
	var got serveOptions
	err := execServe(t, []string{}, func(_ context.Context, opts serveOptions) error {
		got = opts
		return nil
	})
	if err != nil {
		t.Fatalf("serve without --ingest-token: %v", err)
	}
	if got.ingestToken != "" {
		t.Fatalf("ingestToken = %q, want empty", got.ingestToken)
	}
}

func TestServeRejectsBadTargets(t *testing.T) {
	for _, targets := range []string{"1.37,banana", "2.0"} {
		err := execServe(t, []string{"--ingest-token", "t", "--targets", targets}, serveOK())
		if err == nil || !strings.Contains(err.Error(), "--targets") {
			t.Fatalf("--targets %s: want invalid --targets error, got %v", targets, err)
		}
	}
}

// --targets takes at most server.MaxExtraTargets distinct minors, the
// count the server's memory bound is measured at; one given twice counts
// once.
func TestServeCapsTargets(t *testing.T) {
	if err := execServe(t, []string{"--ingest-token", "t", "--targets", "1.36,1.37,1.38,1.39,v1.39"}, serveOK()); err != nil {
		t.Fatalf("--targets of 4 distinct minors: %v", err)
	}
	err := execServe(t, []string{"--ingest-token", "t", "--targets", "1.36,1.37,1.38,1.39,1.40"}, serveOK())
	if err == nil || !strings.Contains(err.Error(), "--targets") || !strings.Contains(err.Error(), "at most 4") ||
		!strings.Contains(err.Error(), "--max-snapshot-bytes") {
		t.Fatalf("--targets of 5 minors: want a refusal naming --targets, the limit of 4 and why, got %v", err)
	}
}

// --targets is parsed exactly once, in validateServeOptions; runServe
// receives []inventory.Version, never the raw CSV.
func TestServePassesParsedTargets(t *testing.T) {
	var got []inventory.Version
	err := execServe(t, []string{"--ingest-token", "t", "--targets", "1.37, 1.38"},
		func(_ context.Context, opts serveOptions) error {
			got = opts.parsedTargets
			return nil
		})
	if err != nil {
		t.Fatal(err)
	}
	want := []inventory.Version{{Major: 1, Minor: 37}, {Major: 1, Minor: 38}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("parsedTargets = %v, want %v", got, want)
	}
}

// --team-map is loaded exactly once, in validateServeOptions; runServe
// receives the parsed server.TeamMap, never the file path.
func TestServePassesParsedTeamMap(t *testing.T) {
	path := filepath.Join(t.TempDir(), "teams.yaml")
	if err := os.WriteFile(path, []byte("- pattern: \"payments-*\"\n  team: payments\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var got server.TeamMap
	err := execServe(t, []string{"--ingest-token", "t", "--team-map", path},
		func(_ context.Context, opts serveOptions) error {
			got = opts.parsedTeamMap
			return nil
		})
	if err != nil {
		t.Fatal(err)
	}
	want := server.TeamMap{server.TeamMapRule{Pattern: "payments-*", Team: "payments"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("parsedTeamMap = %v, want %v", got, want)
	}
}

func TestServeRejectsBadTeamMap(t *testing.T) {
	path := filepath.Join(t.TempDir(), "teams.yaml")
	if err := os.WriteFile(path, []byte("- pattern: \"[\"\n  team: x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for name, p := range map[string]string{"missing file": filepath.Join(t.TempDir(), "nope.yaml"), "bad glob": path} {
		err := execServe(t, []string{"--ingest-token", "t", "--team-map", p}, serveOK())
		if err == nil || !strings.Contains(err.Error(), "--team-map") {
			t.Fatalf("%s: want invalid --team-map error, got %v", name, err)
		}
	}
}

func TestServeDefaults(t *testing.T) {
	var got serveOptions
	err := execServe(t, []string{"--ingest-token", "t"},
		func(_ context.Context, opts serveOptions) error {
			got = opts
			return nil
		})
	if err != nil {
		t.Fatal(err)
	}
	// Loopback by default: a bare `serve` must not publish the read API.
	if got.listen != "127.0.0.1:8080" {
		t.Errorf("listen = %q, want 127.0.0.1:8080", got.listen)
	}
	if got.allowAnonymousRead {
		t.Error("allowAnonymousRead must default to false")
	}
	if got.db != "upgradescope.db" {
		t.Errorf("db = %q, want upgradescope.db", got.db)
	}
	if got.readToken != "" || got.slackWebhook != "" || got.webhook != "" {
		t.Errorf("optional flags must default empty, got %+v", got)
	}
	if got.parsedTargets != nil {
		t.Errorf("parsedTargets = %v, want nil when --targets omitted", got.parsedTargets)
	}
	if got.maxSnapshotBytes != server.DefaultMaxSnapshotBytes || got.maxGateBytes != server.DefaultMaxGateBytes {
		t.Errorf("body caps = %d/%d, want server defaults %d/%d", got.maxSnapshotBytes, got.maxGateBytes,
			server.DefaultMaxSnapshotBytes, server.DefaultMaxGateBytes)
	}
}

// Read tokens minted with 'tokens create --read' live in the store, so the
// CLI leaves an exposed --listen without --read-token to the server, which
// refuses it on the address it binds unless a read token, the trusted-proxy
// header or --allow-anonymous-read closes or opens the read API
// (server.TestStartRejectsNonLoopbackBoundAddr). Here: nothing refuses it
// early, and the refusal the server gives names the flags that fix it.
func TestServeAnonymousReadGuard(t *testing.T) {
	real := runServe // execServe stubs it until the test ends
	for _, args := range [][]string{
		nil,
		{"--listen", ":8080"},
		{"--listen", "0.0.0.0:8080"},
		{"--listen", "10.0.0.5:8080"},
		{"--listen", ":8080", "--read-token", "r"},
		{"--listen", ":8080", "--allow-anonymous-read"},
	} {
		if err := execServe(t, append([]string{"--ingest-token", "t"}, args...), serveOK()); err != nil {
			t.Errorf("%v: %v, want the server to decide", args, err)
		}
	}

	// The real wiring, on every interface, with an empty database.
	db := filepath.Join(t.TempDir(), "open.db")
	err := execServe(t, []string{"--listen", "0.0.0.0:0", "--db", db}, real)
	for _, flag := range []string{"--read-token", "tokens create --read", "--trust-team-header", "--allow-anonymous-read"} {
		if err == nil || !strings.Contains(err.Error(), flag) {
			t.Fatalf("serve on 0.0.0.0 with no read credential: %v, want a refusal naming %s", err, flag)
		}
	}
}

// --trust-team-header needs --trusted-proxy-cidr and the other way round;
// both are parsed before the server starts.
func TestServeTrustedProxyFlags(t *testing.T) {
	var got serveOptions
	capture := func(_ context.Context, opts serveOptions) error {
		got = opts
		return nil
	}
	if err := execServe(t, []string{"--trust-team-header", "x-forwarded-groups", "--trusted-proxy-cidr", "10.42.0.0/16,192.168.1.7", "--trusted-proxy-cidr", "fd00::/8"}, capture); err != nil {
		t.Fatal(err)
	}
	if got.trustTeamHeader != "X-Forwarded-Groups" {
		t.Errorf("header = %q, want the canonical X-Forwarded-Groups", got.trustTeamHeader)
	}
	want := []netip.Prefix{netip.MustParsePrefix("10.42.0.0/16"), netip.MustParsePrefix("192.168.1.7/32"), netip.MustParsePrefix("fd00::/8")}
	if !slices.Equal(got.parsedProxies, want) {
		t.Errorf("proxies = %v, want %v", got.parsedProxies, want)
	}
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"--trust-team-header", "X-Forwarded-Groups"}, "trusted-proxy-cidr"},
		{[]string{"--trusted-proxy-cidr", "10.0.0.0/8"}, "trust-team-header"},
		{[]string{"--trust-team-header", "X-Groups", "--trusted-proxy-cidr", "0.0.0.0/0"}, "every address"},
		{[]string{"--trust-team-header", "X-Groups", "--trusted-proxy-cidr", "::/0"}, "every address"},
		{[]string{"--trust-team-header", "X-Groups", "--trusted-proxy-cidr", "10.0.0/8"}, "invalid --trusted-proxy-cidr"},
		{[]string{"--trust-team-header", "X Groups", "--trusted-proxy-cidr", "10.0.0.0/8"}, "not an HTTP header name"},
		{[]string{"--trust-team-header", "authorization", "--trusted-proxy-cidr", "10.0.0.0/8"}, "credentials"},
	} {
		if err := execServe(t, tc.args, serveOK()); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%v: err = %v, want one naming %q", tc.args, err, tc.want)
		}
	}
}

// Every serve secret has an env var and a -file variant, so it can stay
// out of argv (ps, /proc/<pid>/cmdline, shell history).
func TestServeSecretsFromEnvAndFiles(t *testing.T) {
	t.Setenv("UPGRADESCOPE_INGEST_TOKEN", "env-ingest")
	t.Setenv("UPGRADESCOPE_READ_TOKEN", "env-read")
	t.Setenv("UPGRADESCOPE_SLACK_WEBHOOK", "https://hooks.slack.test/env")
	t.Setenv("UPGRADESCOPE_WEBHOOK_URL", "https://hook.test/env")
	t.Setenv("UPGRADESCOPE_DB_URL", "postgres://env/db")
	var got serveOptions
	capture := func(_ context.Context, opts serveOptions) error {
		got = opts
		return nil
	}
	if err := execServe(t, nil, capture); err != nil {
		t.Fatal(err)
	}
	if got.ingestToken != "env-ingest" || got.readToken != "env-read" ||
		got.slackWebhook != "https://hooks.slack.test/env" || got.webhook != "https://hook.test/env" ||
		got.dbURL != "postgres://env/db" {
		t.Fatalf("env secrets not applied: %+v", got)
	}

	// -file variants beat the environment; explicit flags beat both.
	dir := t.TempDir()
	files := map[string]string{}
	for _, name := range []string{"ingest-token", "read-token", "slack-webhook", "webhook", "db-url"} {
		p := filepath.Join(dir, name)
		v := "file-" + name
		if name == "slack-webhook" || name == "webhook" {
			v = "https://" + name + ".test/file" // a URL: serve checks it
		}
		if err := os.WriteFile(p, []byte(v+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		files[name] = p
	}
	args := []string{"--ingest-token", "flag-ingest"}
	for _, name := range []string{"read-token", "slack-webhook", "webhook", "db-url"} {
		args = append(args, "--"+name+"-file", files[name])
	}
	if err := execServe(t, args, capture); err != nil {
		t.Fatal(err)
	}
	if got.ingestToken != "flag-ingest" || got.readToken != "file-read-token" ||
		got.slackWebhook != "https://slack-webhook.test/file" || got.webhook != "https://webhook.test/file" ||
		got.dbURL != "file-db-url" {
		t.Fatalf("file/flag precedence wrong: %+v", got)
	}

	// An explicit --db selects SQLite even when $UPGRADESCOPE_DB_URL is set.
	if err := execServe(t, []string{"--db", "x.db"}, capture); err != nil {
		t.Fatal(err)
	}
	if got.dbURL != "" || got.db != "x.db" {
		t.Fatalf("--db must win over $UPGRADESCOPE_DB_URL, got db %q dbURL %q", got.db, got.dbURL)
	}
}

func TestServeTLSFlags(t *testing.T) {
	var got serveOptions
	err := execServe(t, []string{"--tls-cert-file", "tls.crt", "--tls-key-file", "tls.key"},
		func(_ context.Context, opts serveOptions) error {
			got = opts
			return nil
		})
	if err != nil {
		t.Fatal(err)
	}
	if got.tlsCertFile != "tls.crt" || got.tlsKeyFile != "tls.key" {
		t.Fatalf("tls files = %q/%q, want tls.crt/tls.key", got.tlsCertFile, got.tlsKeyFile)
	}
	for _, flag := range []string{"--tls-cert-file", "--tls-key-file"} {
		err := execServe(t, []string{flag, "x"}, serveOK())
		if err == nil || !strings.Contains(err.Error(), "tls-cert-file") || !strings.Contains(err.Error(), "tls-key-file") {
			t.Errorf("%s alone: want an error naming both TLS flags, got %v", flag, err)
		}
	}
}

func TestServeBodyLimitFlags(t *testing.T) {
	var got serveOptions
	err := execServe(t, []string{"--ingest-token", "t", "--max-snapshot-bytes", "1048576", "--max-gate-bytes", "4096"},
		func(_ context.Context, opts serveOptions) error {
			got = opts
			return nil
		})
	if err != nil {
		t.Fatal(err)
	}
	if got.maxSnapshotBytes != 1<<20 || got.maxGateBytes != 4096 {
		t.Fatalf("body caps = %d/%d, want 1048576/4096", got.maxSnapshotBytes, got.maxGateBytes)
	}
	for _, flag := range []string{"--max-snapshot-bytes", "--max-gate-bytes"} {
		err := execServe(t, []string{"--ingest-token", "t", flag, "0"}, serveOK())
		if err == nil || !strings.Contains(err.Error(), flag) {
			t.Errorf("%s 0: want an error naming the flag, got %v", flag, err)
		}
	}
}

func TestServeDBURLPassedThrough(t *testing.T) {
	var got serveOptions
	err := execServe(t, []string{"--ingest-token", "t", "--db-url", "postgres://u:p@h:5432/db"},
		func(_ context.Context, opts serveOptions) error {
			got = opts
			return nil
		})
	if err != nil {
		t.Fatal(err)
	}
	if got.dbURL != "postgres://u:p@h:5432/db" {
		t.Errorf("dbURL = %q, want the flag value", got.dbURL)
	}
}

// --db and --db-url are mutually exclusive; --db's DEFAULT value plus
// --db-url is fine (exclusivity is on explicitly set flags).
func TestServeDBAndDBURLMutuallyExclusive(t *testing.T) {
	err := execServe(t, []string{"--ingest-token", "t", "--db", "x.db", "--db-url", "postgres://h/db"}, serveOK())
	if err == nil || !strings.Contains(err.Error(), "db-url") {
		t.Fatalf("want mutual-exclusion error naming db-url, got %v", err)
	}
}

func TestServeReceivesContext(t *testing.T) {
	var got context.Context
	err := execServe(t, []string{"--ingest-token", "t"},
		func(ctx context.Context, _ serveOptions) error {
			got = ctx
			return nil
		})
	if err != nil {
		t.Fatal(err)
	}
	if got == nil {
		t.Fatal("runServe must receive the signal-aware context, got nil")
	}
}

// TestRunServeCreatesDBParentDir exercises the REAL runServe wiring
// (store.Open → kb.Load → server.New → Start) with an already-cancelled
// context: the server shuts down immediately (SERVER-API graceful-stop
// contract) and the nested --db parent directory must have been created.
func TestRunServeCreatesDBParentDir(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "nested", "dir", "db.sqlite")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := runServe(ctx, serveOptions{
		listen:      "127.0.0.1:0",
		db:          dbPath,
		ingestToken: "t",
	})
	if err != nil {
		t.Fatalf("runServe with cancelled ctx: %v", err)
	}
	if _, statErr := os.Stat(filepath.Dir(dbPath)); statErr != nil {
		t.Fatalf("db parent dir not created: %v", statErr)
	}
}

func TestRootRegistersServe(t *testing.T) {
	for _, c := range Root().Commands() {
		if c.Name() == "serve" {
			return
		}
	}
	t.Fatal("serve command not registered on root")
}

// Outside the chart (docker run -m, a plain cgroup), nothing sets
// GOMEMLIMIT, and the Go runtime does not read the container's limit: the
// red team's two concurrent /gate requests OOM-killed a 512m container on
// garbage alone (#121). serve and agent set the limit to 90% of the
// cgroup's memory limit, v2 or v1, unless GOMEMLIMIT is set, which wins.
func TestMemoryLimitFromCgroup(t *testing.T) {
	defer debug.SetMemoryLimit(debug.SetMemoryLimit(-1))
	write := func(t *testing.T, root, rel, content string) {
		t.Helper()
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	noEnv := func(string) string { return "" }
	for _, tc := range []struct {
		name, rel, content string
		want               int64 // 0 = none applied
	}{
		{"cgroup v2", "memory.max", "536870912\n", 483183820},
		{"cgroup v2, no limit", "memory.max", "max\n", 0},
		{"cgroup v1", "memory/memory.limit_in_bytes", "268435456\n", 241591910},
		{"cgroup v1, no limit", "memory/memory.limit_in_bytes", "9223372036854771712\n", 0},
		{"no cgroup files", "", "", 0},
		{"garbage", "memory.max", "lots\n", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			if tc.rel != "" {
				write(t, root, tc.rel, tc.content)
			}
			debug.SetMemoryLimit(math.MaxInt64)
			got, applied := applyMemoryLimit(noEnv, root)
			if applied != (tc.want != 0) || got != tc.want {
				t.Fatalf("applyMemoryLimit = %d, %v; want %d", got, applied, tc.want)
			}
			if now := debug.SetMemoryLimit(-1); tc.want != 0 && now != tc.want || tc.want == 0 && now != math.MaxInt64 {
				t.Fatalf("runtime memory limit = %d, want %d", now, tc.want)
			}
		})
	}
	// An explicit GOMEMLIMIT wins: the runtime has applied it already.
	root := t.TempDir()
	write(t, root, "memory.max", "536870912\n")
	debug.SetMemoryLimit(math.MaxInt64)
	if _, applied := applyMemoryLimit(func(k string) string {
		if k == "GOMEMLIMIT" {
			return "100MiB"
		}
		return ""
	}, root); applied || debug.SetMemoryLimit(-1) != math.MaxInt64 {
		t.Fatal("applyMemoryLimit overrode an explicit GOMEMLIMIT")
	}
}
