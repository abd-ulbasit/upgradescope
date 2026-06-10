package cli

import (
	"bytes"
	"context"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

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
