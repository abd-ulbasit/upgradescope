package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"reflect"
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

// Without a read token the read API and /gate are open, so serve refuses a
// non-loopback listen address unless open reads are explicitly accepted.
func TestServeAnonymousReadGuard(t *testing.T) {
	cases := []struct {
		name    string
		args    []string
		wantErr bool
	}{
		{"default loopback", nil, false},
		{"ipv4 loopback", []string{"--listen", "127.0.0.1:9000"}, false},
		{"ipv6 loopback", []string{"--listen", "[::1]:9000"}, false},
		{"localhost", []string{"--listen", "localhost:9000"}, false},
		{"all interfaces", []string{"--listen", ":8080"}, true},
		{"wildcard ip", []string{"--listen", "0.0.0.0:8080"}, true},
		{"routable ip", []string{"--listen", "10.0.0.5:8080"}, true},
		{"hostname", []string{"--listen", "uscope.internal:8080"}, true},
		{"all interfaces with read token", []string{"--listen", ":8080", "--read-token", "r"}, false},
		{"all interfaces, explicitly open", []string{"--listen", ":8080", "--allow-anonymous-read"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := execServe(t, append([]string{"--ingest-token", "t"}, tc.args...), serveOK())
			if tc.wantErr {
				if err == nil || !strings.Contains(err.Error(), "--allow-anonymous-read") ||
					!strings.Contains(err.Error(), "--read-token") {
					t.Fatalf("want a refusal naming --read-token and --allow-anonymous-read, got %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
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
		if err := os.WriteFile(p, []byte("file-"+name+"\n"), 0o600); err != nil {
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
		got.slackWebhook != "file-slack-webhook" || got.webhook != "file-webhook" ||
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
