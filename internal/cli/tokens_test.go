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

	if _, _, err := execTokens(t, "revoke", "prod", "--all", "--db", db); err != nil {
		t.Fatalf("tokens revoke --all: %v", err)
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
	if _, _, err := execTokens(t, "revoke", "prod", "--all", "--db", db); err == nil ||
		!strings.Contains(err.Error(), "no active tokens") {
		t.Fatalf("second revoke err = %v, want 'no active tokens'", err)
	}
}

// revoke must say which tokens: exactly one of --id or --all. A bare
// `revoke <cluster>` used to revoke everything; now it is an error.
func TestTokensRevokeNeedsIDOrAll(t *testing.T) {
	db := filepath.Join(t.TempDir(), "tokens.db")
	if _, _, err := execTokens(t, "create", "prod", "--db", db); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"revoke", "prod", "--db", db},
		{"revoke", "prod", "--id", "1", "--all", "--db", db},
	} {
		if _, _, err := execTokens(t, args...); err == nil ||
			!strings.Contains(err.Error(), "id") || !strings.Contains(err.Error(), "all") {
			t.Errorf("%v: err = %v, want one naming --id and --all", args, err)
		}
	}
}

// createToken mints a token via the CLI and returns its plaintext and the
// id reported on stderr.
func createToken(t *testing.T, db, cluster string) (token, id string) {
	t.Helper()
	stdout, stderr, err := execTokens(t, "create", cluster, "--db", db)
	if err != nil {
		t.Fatalf("tokens create %s: %v", cluster, err)
	}
	m := regexp.MustCompile(`\bid (\d+)\b`).FindStringSubmatch(stderr)
	if m == nil {
		t.Fatalf("create stderr %q does not report the token id", stderr)
	}
	return strings.TrimSpace(stdout), m[1]
}

// Zero-downtime rotation: mint the new token, roll it out, revoke only the
// old one by id — the new one keeps authenticating.
func TestTokensRevokeByID(t *testing.T) {
	db := filepath.Join(t.TempDir(), "tokens.db")
	oldTok, oldID := createToken(t, db, "prod")
	newTok, _ := createToken(t, db, "prod")

	stdout, _, err := execTokens(t, "revoke", "prod", "--id", oldID, "--db", db)
	if err != nil {
		t.Fatalf("tokens revoke --id: %v", err)
	}
	if !strings.Contains(stdout, "revoked ingest token "+oldID) {
		t.Errorf("stdout = %q, want it to name the revoked id", stdout)
	}
	st, err := store.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if _, ok, _ := st.ValidToken(context.Background(), oldTok); ok {
		t.Error("old token still valid after revoke --id")
	}
	if name, ok, _ := st.ValidToken(context.Background(), newTok); !ok || name != "prod" {
		t.Error("new token must survive revoking the old one by id")
	}

	// Wrong cluster or already revoked: a clear error.
	for _, args := range [][]string{
		{"revoke", "dev", "--id", oldID, "--db", db},
		{"revoke", "prod", "--id", oldID, "--db", db},
	} {
		if _, _, err := execTokens(t, args...); err == nil || !strings.Contains(err.Error(), "no active token") {
			t.Errorf("%v: err = %v, want 'no active token'", args, err)
		}
	}
}

func TestTokensList(t *testing.T) {
	db := filepath.Join(t.TempDir(), "tokens.db")
	prodTok, prodID := createToken(t, db, "prod")
	_, _ = createToken(t, db, "prod")
	devTok, _ := createToken(t, db, "dev")
	if _, _, err := execTokens(t, "revoke", "prod", "--id", prodID, "--db", db); err != nil {
		t.Fatal(err)
	}

	stdout, _, err := execTokens(t, "list", "--db", db)
	if err != nil {
		t.Fatalf("tokens list: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(stdout), "\n")
	if len(lines) != 4 {
		t.Fatalf("list output = %q, want a header and 3 rows", stdout)
	}
	for _, col := range []string{"ID", "CLUSTER", "PREFIX", "CREATED", "REVOKED"} {
		if !strings.Contains(lines[0], col) {
			t.Errorf("header %q lacks %s", lines[0], col)
		}
	}
	revokedRow := lines[1]
	if f := strings.Fields(revokedRow); f[0] != prodID || f[1] != "prod" || f[2] != prodTok[:8] || f[len(f)-1] == "-" {
		t.Errorf("revoked row = %q, want id %s, prod, prefix %s, a revoked time", revokedRow, prodID, prodTok[:8])
	}
	if f := strings.Fields(lines[3]); f[1] != "dev" || f[len(f)-1] != "-" {
		t.Errorf("active dev row = %q, want REVOKED '-'", lines[3])
	}
	if strings.Contains(stdout, prodTok) || strings.Contains(stdout, devTok) {
		t.Error("list leaked a plaintext token")
	}

	stdout, _, err = execTokens(t, "list", "--cluster", "dev", "--db", db)
	if err != nil {
		t.Fatal(err)
	}
	if n := len(strings.Split(strings.TrimSpace(stdout), "\n")); n != 2 {
		t.Fatalf("list --cluster dev = %q, want a header and 1 row", stdout)
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
		// A Secret written with a trailing newline (kubectl create secret
		// --from-file, Vault and ESO templates) reaches serve through the
		// environment too: trimmed as the file is (#240).
		{"env trimmed like a file", " from-env\n", nil, "from-env", ""},
		{"whitespace-only env", " \n\t", nil, "", "UPGRADESCOPE_TEST_SECRET"},
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

// tokens create says what it stores: the sha256 hash and the token's first
// 8 characters, which `tokens list` prints to tell tokens apart (#126
// SE-03; it used to claim only the hash is stored).
func TestTokensCreateMessage(t *testing.T) {
	stdout, stderr, err := execTokens(t, "create", "prod", "--db", filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	prefix := strings.TrimSpace(stdout)[:8]
	if !strings.Contains(stderr, prefix) || !strings.Contains(stderr, "sha256") || !strings.Contains(stderr, "first 8 characters") ||
		strings.Contains(stderr, "only its hash") {
		t.Errorf("stderr = %q: want it to say a sha256 hash and the first 8 characters (%s) are stored", stderr, prefix)
	}
}

// --db is the file opened, whatever escapes it holds (#195): `x%20y.db` was
// created as `x y.db` 0644 beside an empty decoy named as given.
func TestTokensCreateDBPathIsExact(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"x%20y.db", "a%3Fb.db", "c%23d.db", "e?f#g.db"} {
		if _, stderr, err := execTokens(t, "create", "c1", "--db", filepath.Join(dir, name)); err != nil {
			t.Fatalf("tokens create --db %q: %v (stderr %q)", name, err, stderr)
		}
		fi, err := os.Stat(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		if fi.Size() == 0 || fi.Mode().Perm()&0o077 != 0 {
			t.Errorf("%s: size %d mode %o, want the database itself, owner-only", name, fi.Size(), fi.Mode().Perm())
		}
	}
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(ents) != 4 {
		var names []string
		for _, e := range ents {
			names = append(names, e.Name())
		}
		t.Errorf("directory holds %q, want exactly the 4 databases named", names)
	}
}

// The database directory openStore creates is the owner's only, like the
// database in it.
func TestOpenStoreCreatesPrivateDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "new", "dir")
	st, err := dbFlags{db: filepath.Join(dir, "u.db")}.openStore()
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	for _, d := range []string{dir, filepath.Dir(dir)} {
		fi, err := os.Stat(d)
		if err != nil {
			t.Fatal(err)
		}
		if mode := fi.Mode().Perm(); mode&0o077 != 0 {
			t.Errorf("%s mode = %o, want no group or other access", d, mode)
		}
	}
}
