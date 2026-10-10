package cli

import (
	"bytes"
	"context"
	"crypto/x509"
	"encoding/pem"
	"errors"
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

// execClustersStderr is execClusters returning stderr too.
func execClustersStderr(t *testing.T, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	cmd := newClustersCmd()
	var out, errOut bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	cmd.SetArgs(args)
	err = cmd.Execute()
	return out.String(), errOut.String(), err
}

// TestClustersServerURLIsTheFlagAndServerStaysAsAHiddenAlias: --server-url
// is the name agent and mcp use; --server keeps working, prints cobra's
// deprecation note, and is not in --help. Both stay exclusive with --db and
// with each other.
func TestClustersServerURLIsTheFlagAndServerStaysAsAHiddenAlias(t *testing.T) {
	ts, _ := clustersServer(t)
	t.Setenv("UPGRADESCOPE_READ_TOKEN", "read-tok")

	out, stderr, err := execClustersStderr(t, "list", "--server-url", ts.URL)
	if err != nil || !strings.Contains(out, "uid-prod") || strings.Contains(stderr, "deprecated") {
		t.Errorf("list --server-url: (%q, stderr %q, %v), want the clusters and no deprecation note", out, stderr, err)
	}
	out, stderr, err = execClustersStderr(t, "list", "--server", ts.URL)
	if err != nil || !strings.Contains(out, "uid-prod") {
		t.Errorf("list --server: (%q, %v), want the clusters", out, err)
	}
	if !strings.Contains(out+stderr, "--server has been deprecated, use --server-url") {
		t.Errorf("list --server: output %q, stderr %q, want the deprecation note naming --server-url", out, stderr)
	}

	for _, sub := range []string{"list", "delete x", "rename x y"} {
		cmd := newClustersCmd()
		var help bytes.Buffer
		cmd.SetOut(&help)
		cmd.SetErr(&help)
		cmd.SetArgs(append(strings.Fields(sub)[:1], "--help"))
		if err := cmd.Execute(); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(help.String(), "--server-url") || !strings.Contains(help.String(), "--server-ca-file") {
			t.Errorf("clusters %s --help:\n%s", sub, help.String())
		}
		if strings.Contains(help.String(), "former name") || strings.Contains(help.String(), "--server ") {
			t.Errorf("clusters %s --help shows the hidden --server alias:\n%s", sub, help.String())
		}
	}

	for _, flag := range []string{"--server-url", "--server"} {
		for _, db := range [][]string{{"--db", "a.db"}, {"--db-url", "postgres://x"}, {"--db-url-file", "f"}} {
			args := append([]string{"list", flag, "https://x.example"}, db...)
			if _, err := execClusters(t, args...); err == nil || !strings.Contains(err.Error(), "none of the others can be") {
				t.Errorf("clusters %v: err = %v, want the mutual exclusion error", args, err)
			}
		}
	}
	if _, err := execClusters(t, "list", "--server-url", "https://x.example", "--server", "https://y.example"); err == nil {
		t.Error("--server-url with --server succeeded, want an error")
	}
	if _, err := execClusters(t, "list", "--server-url", "x.example"); err == nil || !strings.Contains(err.Error(), "--server-url") {
		t.Errorf("--server-url without a scheme: err = %v, want a --server-url error", err)
	}
	if _, err := execClusters(t, "list"); err == nil || !strings.Contains(err.Error(), "--server-url URL") {
		t.Errorf("no target: err = %v, want the hint to name --server-url", err)
	}
}

// TestClustersRedirectErrorNamesServerURL: the redirect refusal tells the
// operator which flag to change.
func TestClustersRedirectErrorNamesServerURL(t *testing.T) {
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "https://elsewhere.example"+r.URL.Path, http.StatusMovedPermanently)
	}))
	defer redirect.Close()
	_, err := execClusters(t, "list", "--server-url", redirect.URL)
	if err == nil || !strings.Contains(err.Error(), "use that URL as --server-url") {
		t.Errorf("err = %v, want the redirect refusal to name --server-url", err)
	}
}

// TestClustersWarnOfCleartextTokens: an admin or read token sent to a plain
// http server that is not loopback is warned about once, on stderr, without
// the token; loopback and https servers, and calls with no token, are not.
// The request still goes ahead (a warning, not a refusal). 192.0.2.1 is
// TEST-NET-1, so nothing answers: the connection fails after the warning.
func TestClustersWarnOfCleartextTokens(t *testing.T) {
	const secret = "s3cret-admin-token"
	for _, tc := range []struct {
		name string
		args []string
	}{
		{"list", []string{"list", "--read-token", secret}},
		{"delete", []string{"delete", "prod", "--admin-token", secret}},
		{"rename", []string{"rename", "prod", "prod-eu", "--admin-token", secret}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("UPGRADESCOPE_READ_TOKEN", "")
			t.Setenv("UPGRADESCOPE_ADMIN_TOKEN", "")
			// An unroutable address with a refused connection: use a closed
			// local port under a non-loopback name via a custom check is
			// not possible, so aim at TEST-NET-1 with a short context.
			ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
			defer cancel()
			cmd := newClustersCmd()
			var out, errOut bytes.Buffer
			cmd.SetOut(&out)
			cmd.SetErr(&errOut)
			cmd.SetArgs(append(append([]string{}, tc.args...), "--server-url", "http://192.0.2.1:8080"))
			err := cmd.ExecuteContext(ctx)
			if err == nil {
				t.Fatal("the command reached a server at TEST-NET-1?")
			}
			stderr := errOut.String()
			if n := strings.Count(stderr, "warning:"); n != 1 || !strings.Contains(stderr, "plain http") || !strings.Contains(stderr, "192.0.2.1:8080") {
				t.Errorf("stderr = %q, want one cleartext warning naming the host", stderr)
			}
			if strings.Contains(stderr+out.String()+err.Error(), secret) {
				t.Errorf("the token leaked into the output: %q %q %v", stderr, out.String(), err)
			}
		})
	}

	ts, _ := clustersServer(t) // loopback http
	for name, args := range map[string][]string{
		"loopback http":  {"list", "--server-url", ts.URL, "--read-token", "read-tok"},
		"no token":       {"list", "--server-url", "http://192.0.2.1:8080"},
		"https, no CA":   {"list", "--server-url", "https://192.0.2.1:8080", "--read-token", secret},
		"localhost name": {"list", "--server-url", "http://localhost:1", "--read-token", secret},
		"ipv6 loopback":  {"list", "--server-url", "http://[::1]:1", "--read-token", secret},
	} {
		t.Run(name, func(t *testing.T) {
			t.Setenv("UPGRADESCOPE_READ_TOKEN", "")
			ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
			defer cancel()
			cmd := newClustersCmd()
			var errOut bytes.Buffer
			cmd.SetOut(&bytes.Buffer{})
			cmd.SetErr(&errOut)
			cmd.SetArgs(args)
			_ = cmd.ExecuteContext(ctx)
			if strings.Contains(errOut.String(), "warning:") {
				t.Errorf("stderr = %q, want no cleartext warning", errOut.String())
			}
		})
	}
}

// writeCABundle writes srv's certificate as a PEM bundle, as an operator
// would export their private CA.
func writeCABundle(t *testing.T, srv *httptest.Server) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "ca.crt")
	block := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw})
	if err := os.WriteFile(path, block, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestClustersTrustServerCAFile: a server behind a private CA is reached
// with --server-ca-file and is refused without it (an x509 error; there is
// no option to skip verification). Modeled on TestPushTrustsServerCAFile.
func TestClustersTrustServerCAFile(t *testing.T) {
	ts, st := clustersServer(t)
	tlsSrv := httptest.NewTLSServer(ts.Config.Handler)
	t.Cleanup(tlsSrv.Close)
	ca := writeCABundle(t, tlsSrv)
	t.Setenv("UPGRADESCOPE_READ_TOKEN", "read-tok")
	t.Setenv("UPGRADESCOPE_ADMIN_TOKEN", "admin-tok")
	ctx := context.Background()

	out, err := execClusters(t, "list", "--server-url", tlsSrv.URL, "--server-ca-file", ca)
	if err != nil || !strings.Contains(out, "uid-prod") {
		t.Fatalf("list with the CA file: (%q, %v)", out, err)
	}
	if out, err := execClusters(t, "rename", "dev", "dev-1", "--server-url", tlsSrv.URL, "--server-ca-file", ca); err != nil || !strings.Contains(out, "dev-1") {
		t.Fatalf("rename with the CA file: (%q, %v)", out, err)
	}
	if out, err := execClusters(t, "delete", "dev-1", "--server-url", tlsSrv.URL, "--server-ca-file", ca); err != nil || !strings.Contains(out, "deleted") {
		t.Fatalf("delete with the CA file: (%q, %v)", out, err)
	}
	if _, err := st.ClusterByName(ctx, "dev-1"); err == nil {
		t.Error("dev-1 still exists after the delete")
	}

	// Without the CA, verification fails and nothing changes.
	for _, args := range [][]string{
		{"list"},
		{"delete", "prod"},
		{"rename", "prod", "prod-eu"},
	} {
		_, err := execClusters(t, append(args, "--server-url", tlsSrv.URL)...)
		var uerr x509.UnknownAuthorityError
		if !errors.As(err, &uerr) {
			t.Errorf("clusters %v without the CA: err = %v, want an unknown-authority error", args, err)
		}
	}
	if _, err := st.ClusterByName(ctx, "prod"); err != nil {
		t.Errorf("prod after the unverified commands: %v", err)
	}

	// A CA file leaves redirects refused.
	redirect := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, tlsSrv.URL+r.URL.Path, http.StatusMovedPermanently)
	}))
	defer redirect.Close()
	if _, err := execClusters(t, "list", "--server-url", redirect.URL, "--server-ca-file", writeCABundle(t, redirect)); err == nil || !strings.Contains(err.Error(), "301") {
		t.Errorf("a redirect with a CA file: err = %v, want the refusal", err)
	}
}

// TestClustersServerCAFileIsChecked: --server-ca-file needs an https
// --server-url, and a bundle that is not one stops the command.
func TestClustersServerCAFileIsChecked(t *testing.T) {
	ts, _ := clustersServer(t)
	ca := writeCABundle(t, httptest.NewTLSServer(http.NotFoundHandler()))
	for name, tc := range map[string]struct {
		args []string
		want string
	}{
		"http url":      {[]string{"list", "--server-url", ts.URL, "--server-ca-file", ca}, "needs an https --server-url"},
		"HTTP url":      {[]string{"list", "--server-url", strings.Replace(ts.URL, "http", "HTTP", 1), "--server-ca-file", ca}, "needs an https --server-url"},
		"no server url": {[]string{"list", "--db", filepath.Join(t.TempDir(), "x.db"), "--server-ca-file", ca}, "needs --server-url"},
		"missing file":  {[]string{"list", "--server-url", "https://x.example", "--server-ca-file", filepath.Join(t.TempDir(), "nope.pem")}, "--server-ca-file"},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := execClusters(t, tc.args...); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %v, want one containing %q", err, tc.want)
			}
		})
	}
}
