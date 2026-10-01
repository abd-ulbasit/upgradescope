package cli

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/abd-ulbasit/upgradescope/internal/server/store"
)

// execTokens runs `tokens <args...>` against the real SQLite wiring (no
// stub seam: the command IS store plumbing, so tests exercise it for real).
func execTokens(t *testing.T, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	cmd := newTokensCmd()
	var out, errOut bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	cmd.SetArgs(args)
	err = cmd.Execute()
	return out.String(), errOut.String(), err
}

var hexToken = regexp.MustCompile(`^[0-9a-f]{64}$`)

func TestTokensCreatePrintsTokenOnceAndStoresHash(t *testing.T) {
	db := filepath.Join(t.TempDir(), "tokens.db")
	stdout, stderr, err := execTokens(t, "create", "prod-eu-1", "--db", db)
	if err != nil {
		t.Fatalf("tokens create: %v (stderr %q)", err, stderr)
	}
	token := strings.TrimSpace(stdout)
	if !hexToken.MatchString(token) {
		t.Fatalf("stdout = %q, want exactly one 64-hex-char token", stdout)
	}
	if !strings.Contains(stderr, "prod-eu-1") {
		t.Errorf("stderr should mention the cluster, got %q", stderr)
	}
	if strings.Contains(stderr, token) {
		t.Errorf("plaintext token leaked to stderr")
	}

	st, err := store.Open(db)
	if err != nil {
		t.Fatalf("re-open store: %v", err)
	}
	defer st.Close()
	name, ok, err := st.ValidToken(context.Background(), token)
	if err != nil || !ok || name != "prod-eu-1" {
		t.Fatalf("ValidToken = (%q, %v, %v), want (prod-eu-1, true, nil)", name, ok, err)
	}
}

func TestTokensCreateTwiceIssuesDistinctTokens(t *testing.T) {
	db := filepath.Join(t.TempDir(), "tokens.db")
	out1, _, err := execTokens(t, "create", "prod", "--db", db)
	if err != nil {
		t.Fatal(err)
	}
	out2, _, err := execTokens(t, "create", "prod", "--db", db)
	if err != nil {
		t.Fatal(err)
	}
	tok1, tok2 := strings.TrimSpace(out1), strings.TrimSpace(out2)
	if tok1 == tok2 {
		t.Fatalf("two creates issued the same token")
	}
	st, err := store.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	for _, tok := range []string{tok1, tok2} {
		if name, ok, _ := st.ValidToken(context.Background(), tok); !ok || name != "prod" {
			t.Errorf("token %s... not valid for prod after rotation-style create", tok[:8])
		}
	}
}

func TestTokensRevoke(t *testing.T) {
	db := filepath.Join(t.TempDir(), "tokens.db")
	stdout, _, err := execTokens(t, "create", "prod", "--db", db)
	if err != nil {
		t.Fatal(err)
	}
	token := strings.TrimSpace(stdout)

	if _, _, err := execTokens(t, "revoke", "prod", "--db", db); err != nil {
		t.Fatalf("tokens revoke: %v", err)
	}
	st, err := store.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok, err := st.ValidToken(context.Background(), token); err != nil || ok {
		t.Fatalf("token still valid after revoke (ok %v, err %v)", ok, err)
	}
	_ = st.Close()

	// Second revoke: nothing active — a clear error, not silent success.
	if _, _, err := execTokens(t, "revoke", "prod", "--db", db); err == nil ||
		!strings.Contains(err.Error(), "no active tokens") {
		t.Fatalf("second revoke err = %v, want 'no active tokens'", err)
	}
}

func TestTokensRequireClusterArg(t *testing.T) {
	for _, sub := range []string{"create", "revoke"} {
		if _, _, err := execTokens(t, sub, "--db", filepath.Join(t.TempDir(), "x.db")); err == nil {
			t.Errorf("tokens %s without cluster arg succeeded, want error", sub)
		}
	}
}

func TestTokensDBFlagsMutuallyExclusive(t *testing.T) {
	_, _, err := execTokens(t, "create", "prod", "--db", "x.db", "--db-url", "postgres://h/db")
	if err == nil || !strings.Contains(err.Error(), "db-url") {
		t.Fatalf("want mutual-exclusion error naming db-url, got %v", err)
	}
}

func TestRootRegistersTokens(t *testing.T) {
	for _, c := range Root().Commands() {
		if c.Name() == "tokens" {
			return
		}
	}
	t.Fatal("tokens command not registered on root")
}

// runSecretCmd registers one secret flag (env UPGRADESCOPE_TEST_SECRET) on
// a throwaway command and returns the resolved value.
func runSecretCmd(t *testing.T, args ...string) (string, error) {
	t.Helper()
	var value string
	cmd := &cobra.Command{Use: "x", SilenceUsage: true, SilenceErrors: true}
	secret := addSecretFlag(cmd, &value, "secret", "UPGRADESCOPE_TEST_SECRET", "a secret")
	cmd.RunE = func(cmd *cobra.Command, _ []string) error { return secret.resolve(cmd) }
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	cmd.SetArgs(args)
	err := cmd.Execute()
	return value, err
}

// Precedence: an explicit flag wins, then --<name>-file, then the env var.
func TestSecretFlagPrecedence(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "secret")
	if err := os.WriteFile(file, []byte("from-file\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	empty := filepath.Join(dir, "empty")
	if err := os.WriteFile(empty, []byte("\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name    string
		env     string
		args    []string
		want    string
		wantErr string
	}{
		{"nothing set", "", nil, "", ""},
		{"env only", "from-env", nil, "from-env", ""},
		{"file only, trailing newline trimmed", "", []string{"--secret-file", file}, "from-file", ""},
		{"flag beats env", "from-env", []string{"--secret", "from-flag"}, "from-flag", ""},
		{"explicit empty flag beats env", "from-env", []string{"--secret", ""}, "", ""},
		{"file beats env", "from-env", []string{"--secret-file", file}, "from-file", ""},
		{"flag and file conflict", "", []string{"--secret", "x", "--secret-file", file}, "", "secret-file"},
		{"empty file", "", []string{"--secret-file", empty}, "", "empty"},
		{"missing file", "", []string{"--secret-file", filepath.Join(dir, "nope")}, "", "--secret-file"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("UPGRADESCOPE_TEST_SECRET", tc.env)
			got, err := runSecretCmd(t, tc.args...)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want one containing %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Fatalf("value = %q, want %q", got, tc.want)
			}
		})
	}
}

// tokens commands read the Postgres URL from $UPGRADESCOPE_DB_URL too, but
// an explicit --db (SQLite) must win over it.
func TestTokensDBURLFromEnv(t *testing.T) {
	t.Chdir(t.TempDir()) // a regression would create ./upgradescope.db
	t.Setenv("UPGRADESCOPE_DB_URL", "postgres://127.0.0.1:1/nope?connect_timeout=1")
	_, _, err := execTokens(t, "create", "prod")
	if err == nil || !strings.Contains(err.Error(), "postgres") {
		t.Fatalf("create with $UPGRADESCOPE_DB_URL: err = %v, want a postgres open error", err)
	}
	if _, _, err := execTokens(t, "create", "prod", "--db", filepath.Join(t.TempDir(), "t.db")); err != nil {
		t.Fatalf("explicit --db must win over $UPGRADESCOPE_DB_URL: %v", err)
	}
}
