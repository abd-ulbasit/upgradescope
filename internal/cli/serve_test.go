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

func TestServeRequiresIngestToken(t *testing.T) {
	err := execServe(t, []string{}, serveOK())
	if err == nil || !strings.Contains(err.Error(), "ingest-token") {
		t.Fatalf("want missing --ingest-token error, got %v", err)
	}
}

func TestServeRejectsBadTargets(t *testing.T) {
	err := execServe(t, []string{"--ingest-token", "t", "--targets", "1.37,banana"}, serveOK())
	if err == nil || !strings.Contains(err.Error(), "--targets") {
		t.Fatalf("want invalid --targets error, got %v", err)
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
	if got.listen != ":8080" {
		t.Errorf("listen = %q, want :8080", got.listen)
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
