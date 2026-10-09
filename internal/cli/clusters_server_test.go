package cli

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/abd-ulbasit/upgradescope/internal/kb"
	"github.com/abd-ulbasit/upgradescope/internal/server"
	"github.com/abd-ulbasit/upgradescope/internal/server/store"
)

// clustersServer runs a real server on a SQLite store with read and admin
// tokens, seeded with clusters prod (with an ingest token) and dev.
func clustersServer(t *testing.T) (*httptest.Server, store.Store) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "srv.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	ctx := context.Background()
	for _, name := range []string{"prod", "dev"} {
		if _, err := st.UpsertCluster(ctx, store.Cluster{Name: name, ClusterUID: "uid-" + name, LastSeen: time.Now()}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := st.CreateToken(ctx, "prod", "prod-ingest-token"); err != nil {
		t.Fatal(err)
	}
	srv, err := server.New(server.Config{Store: st, KB: kb.KB{Version: "test"}, ReadToken: "read-tok", AdminToken: "admin-tok"})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts, st
}

// TestClustersAgainstServer drives list, rename and delete through a
// server's API, with tokens from the environment and a file.
func TestClustersAgainstServer(t *testing.T) {
	ts, st := clustersServer(t)
	ctx := context.Background()

	if _, err := execClusters(t, "list", "--server", ts.URL); err == nil || !strings.Contains(err.Error(), "401") {
		t.Errorf("list without a token: err = %v, want the server's 401", err)
	}
	t.Setenv("UPGRADESCOPE_READ_TOKEN", "read-tok")
	out, err := execClusters(t, "list", "--server", ts.URL)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	for _, want := range []string{"STALE", "prod", "uid-prod", "dev", "false"} {
		if !strings.Contains(out, want) {
			t.Errorf("list output lacks %q:\n%s", want, out)
		}
	}

	// The read token cannot change anything.
	if _, err := execClusters(t, "delete", "dev", "--server", ts.URL, "--admin-token", "read-tok"); err == nil || !strings.Contains(err.Error(), "403") {
		t.Errorf("delete with the read token: err = %v, want the server's 403", err)
	}

	t.Setenv("UPGRADESCOPE_ADMIN_TOKEN", "admin-tok")
	if out, err := execClusters(t, "rename", "prod", "prod-eu", "--server", ts.URL); err != nil || !strings.Contains(out, `"prod-eu"`) {
		t.Fatalf("rename: (%q, %v)", out, err)
	}
	if c, err := st.ClusterByName(ctx, "prod-eu"); err != nil || c.ClusterUID != "uid-prod" {
		t.Errorf("after rename: (%+v, %v), want prod-eu bound to uid-prod", c, err)
	}
	if name, ok, _ := st.ValidToken(ctx, "prod-ingest-token"); !ok || name != "prod-eu" {
		t.Errorf("ingest token after rename bound to %q (%v), want prod-eu", name, ok)
	}
	if _, err := execClusters(t, "rename", "prod-eu", "dev", "--server", ts.URL); err == nil || !strings.Contains(err.Error(), "already registered") {
		t.Errorf("rename onto dev: err = %v, want a taken-name error", err)
	}

	tokenFile := filepath.Join(t.TempDir(), "admin")
	if err := os.WriteFile(tokenFile, []byte("admin-tok\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("UPGRADESCOPE_ADMIN_TOKEN", "")
	if out, err := execClusters(t, "delete", "prod-eu", "--server", ts.URL, "--admin-token-file", tokenFile); err != nil || !strings.Contains(out, `"prod-eu"`) {
		t.Fatalf("delete: (%q, %v)", out, err)
	}
	if _, err := st.ClusterByName(ctx, "prod-eu"); err == nil {
		t.Error("prod-eu still registered after delete")
	}
	if _, ok, _ := st.ValidToken(ctx, "prod-ingest-token"); ok {
		t.Error("prod's ingest token survived the delete")
	}
	if _, err := execClusters(t, "delete", "prod-eu", "--server", ts.URL, "--admin-token-file", tokenFile); err == nil || err.Error() != `no cluster "prod-eu"` {
		t.Errorf("second delete: err = %v, want no cluster \"prod-eu\"", err)
	}
}

// TestClustersFlagRules: --server and --db pick different backends and
// cannot be combined; --server must be an http(s) URL.
func TestClustersFlagRules(t *testing.T) {
	if _, err := execClusters(t, "list", "--server", "https://x.example", "--db", "a.db"); err == nil {
		t.Error("--server with --db succeeded, want an error")
	}
	if _, err := execClusters(t, "list", "--server", "x.example"); err == nil || !strings.Contains(err.Error(), "--server") {
		t.Errorf("--server without a scheme: err = %v, want a --server error", err)
	}
}

// TestClustersAgainstDatabase: without --server, list and rename work on
// the database file directly (the server may be stopped).
func TestClustersAgainstDatabase(t *testing.T) {
	db := filepath.Join(t.TempDir(), "clusters.db")
	st, err := store.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.UpsertCluster(context.Background(), store.Cluster{Name: "prod", ClusterUID: "uid-prod"}); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	if out, err := execClusters(t, "rename", "prod", "prod-eu", "--db", db); err != nil || !strings.Contains(out, "prod-eu") {
		t.Fatalf("rename: (%q, %v)", out, err)
	}
	out, err := execClusters(t, "list", "--db", db)
	if err != nil || !strings.Contains(out, "prod-eu") || !strings.Contains(out, "uid-prod") {
		t.Errorf("list = (%q, %v), want prod-eu with uid-prod", out, err)
	}
	if _, err := execClusters(t, "rename", "nope", "x", "--db", db); err == nil || err.Error() != `no cluster "nope"` {
		t.Errorf("rename unknown: err = %v", err)
	}
}

// A load balancer's http→https 301 or 302 turned clusters delete's DELETE
// (and rename's PATCH) into a GET, which the admin token reads, and the CLI
// said it had deleted the cluster (#240). The admin client never follows a
// redirect: every command fails naming the status and Location, and the
// redirect target is never asked anything.
func TestClustersRefuseRedirects(t *testing.T) {
	ts, st := clustersServer(t)
	var reached []string
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached = append(reached, r.Method+" "+r.URL.Path)
		ts.Config.Handler.ServeHTTP(w, r)
	}))
	defer target.Close()
	for _, code := range []int{http.StatusMovedPermanently, http.StatusFound, http.StatusSeeOther, http.StatusTemporaryRedirect, http.StatusPermanentRedirect} {
		redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, target.URL+r.URL.Path, code)
		}))
		for _, args := range [][]string{
			{"list", "--read-token", "read-tok"},
			{"delete", "prod", "--admin-token", "admin-tok"},
			{"rename", "prod", "prod-eu", "--admin-token", "admin-tok"},
		} {
			_, err := execClusters(t, append(args, "--server", redirect.URL)...)
			if err == nil || !strings.Contains(err.Error(), strconv.Itoa(code)) || !strings.Contains(err.Error(), target.URL) {
				t.Errorf("%d: clusters %v: err = %v, want a refusal naming the status and the Location", code, args, err)
			}
		}
		redirect.Close()
	}
	if len(reached) != 0 {
		t.Errorf("the redirect target was asked %v, want nothing", reached)
	}
	if _, err := st.ClusterByName(context.Background(), "prod"); err != nil {
		t.Errorf("prod after the refused commands: %v", err)
	}
	// Direct, they still work.
	if _, err := execClusters(t, "rename", "prod", "prod-eu", "--server", ts.URL, "--admin-token", "admin-tok"); err != nil {
		t.Errorf("direct rename: %v", err)
	}
	if _, err := execClusters(t, "delete", "prod-eu", "--server", ts.URL, "--admin-token", "admin-tok"); err != nil {
		t.Errorf("direct delete: %v", err)
	}
}
