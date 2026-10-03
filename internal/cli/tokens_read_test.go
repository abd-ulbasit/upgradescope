package cli

import (
	"context"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/abd-ulbasit/upgradescope/internal/server/store"
)

// createReadTokenCLI mints a read token via the CLI and returns its
// plaintext and the id reported on stderr.
func createReadTokenCLI(t *testing.T, db string, teams ...string) (token, id string) {
	t.Helper()
	args := []string{"create", "--read", "--db", db}
	for _, team := range teams {
		args = append(args, "--teams", team)
	}
	stdout, stderr, err := execTokens(t, args...)
	if err != nil {
		t.Fatalf("tokens create --read --teams %q: %v", teams, err)
	}
	token = strings.TrimSpace(stdout)
	if !hexToken.MatchString(token) {
		t.Fatalf("stdout = %q, want exactly one 64-hex-char token", stdout)
	}
	if strings.Contains(stderr, token) {
		t.Fatal("plaintext read token leaked to stderr")
	}
	m := regexp.MustCompile(`\bid (\d+)\b`).FindStringSubmatch(stderr)
	if m == nil {
		t.Fatalf("create stderr %q does not report the token id", stderr)
	}
	return token, m[1]
}

// tokens create --read --teams stores a hashed read token with its scope,
// one that is not an ingest token.
func TestTokensCreateRead(t *testing.T) {
	db := filepath.Join(t.TempDir(), "tokens.db")
	payTok, _ := createReadTokenCLI(t, db, "web", "payments", "web")
	fleetTok, _ := createReadTokenCLI(t, db, "*")

	st, err := store.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	for _, tc := range []struct {
		token string
		want  []string
	}{{payTok, []string{"payments", "web"}}, {fleetTok, []string{store.ReadScopeFleet}}} {
		teams, ok, err := st.ValidReadToken(ctx, tc.token)
		if err != nil || !ok || !slices.Equal(teams, tc.want) {
			t.Errorf("ValidReadToken = (%v, %v, %v), want (%v, true, nil)", teams, ok, err, tc.want)
		}
		if _, ok, _ := st.ValidToken(ctx, tc.token); ok {
			t.Error("a read token pushes snapshots: it is an ingest token too")
		}
	}
}

func TestTokensCreateReadRejectsBadScopes(t *testing.T) {
	db := filepath.Join(t.TempDir(), "tokens.db")
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"create", "--read"}, "--teams"},
		{[]string{"create", "--read", "--teams", ""}, "--teams"},
		{[]string{"create", "--read", "--teams", "a", "--teams", ""}, "non-empty"},
		{[]string{"create", "--read", "--teams", "*", "--teams", "payments"}, "alone"},
		{[]string{"create", "--read", "--teams", "pay\nments"}, "printable"},
		{[]string{"create", "--read", "--teams", "payments", "prod"}, "unknown command"},
		{[]string{"create", "prod", "--teams", "payments"}, "--read"},
	} {
		_, _, err := execTokens(t, append(tc.args, "--db", db)...)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%v: err = %v, want one naming %q", tc.args, err, tc.want)
		}
	}
	st, err := store.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if toks, err := st.ListReadTokens(context.Background()); err != nil || len(toks) != 0 {
		t.Errorf("read tokens after only refusals = %v, %v, want none", toks, err)
	}
}

func TestTokensListAndRevokeRead(t *testing.T) {
	db := filepath.Join(t.TempDir(), "tokens.db")
	payTok, payID := createReadTokenCLI(t, db, "payments")
	_, _ = createReadTokenCLI(t, db, "*")
	ingestTok, _ := createToken(t, db, "prod")

	if _, _, err := execTokens(t, "revoke", "--read", "--id", payID, "--db", db); err != nil {
		t.Fatalf("tokens revoke --read --id: %v", err)
	}
	stdout, _, err := execTokens(t, "list", "--read", "--db", db)
	if err != nil {
		t.Fatalf("tokens list --read: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(stdout), "\n")
	if len(lines) != 3 {
		t.Fatalf("list --read = %q, want a header and 2 rows (the ingest token is not one)", stdout)
	}
	for _, col := range []string{"ID", "TEAMS", "PREFIX", "CREATED", "REVOKED"} {
		if !strings.Contains(lines[0], col) {
			t.Errorf("header %q lacks %s", lines[0], col)
		}
	}
	if f := strings.Fields(lines[1]); f[0] != payID || f[1] != "payments" || f[2] != payTok[:8] || f[len(f)-1] == "-" {
		t.Errorf("revoked row = %q, want id %s, payments, prefix %s, a revoked time", lines[1], payID, payTok[:8])
	}
	if f := strings.Fields(lines[2]); f[1] != "*" || f[len(f)-1] != "-" {
		t.Errorf("fleet-wide row = %q, want teams * and REVOKED -", lines[2])
	}
	if strings.Contains(stdout, payTok) || strings.Contains(stdout, ingestTok) {
		t.Error("list --read leaked a plaintext token")
	}

	st, err := store.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := st.ValidReadToken(context.Background(), payTok); ok {
		t.Error("read token still valid after revoke --read --id")
	}
	_ = st.Close()

	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"revoke", "--read", "--id", payID}, "no active read token"},
		{[]string{"revoke", "--read", "--id", "99"}, "no active read token"},
		{[]string{"revoke", "--read", "--all"}, "--id"},
		{[]string{"revoke", "--read", "prod", "--id", payID}, "unknown command"},
		{[]string{"list", "--read", "--cluster", "prod"}, "cluster"},
	} {
		if _, _, err := execTokens(t, append(tc.args, "--db", db)...); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%v: err = %v, want one naming %q", tc.args, err, tc.want)
		}
	}
}

// A team is free text (#72): one --teams flag is one team, never split
// or trimmed, so a --team-map team like "Platform Team", one with a comma
// or a non-ASCII one can be given a token. A comma draws a note, since it
// is most likely a list passed to one flag; list --read quotes the names
// that need it.
func TestTokensCreateReadFreeTextTeams(t *testing.T) {
	db := filepath.Join(t.TempDir(), "tokens.db")
	_, stderr, err := execTokens(t, "create", "--read", "--teams", "Platform Team", "--teams", "Équipe, Paris", "--teams", "équipe", "--db", db)
	if err != nil {
		t.Fatalf("tokens create: %v", err)
	}
	if !strings.Contains(stderr, "repeat the flag") {
		t.Errorf("no note for a team with a comma: %q", stderr)
	}
	st, err := store.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	toks, err := st.ListReadTokens(context.Background())
	_ = st.Close()
	if want := []string{"Platform Team", "Équipe, Paris", "équipe"}; err != nil || len(toks) != 1 || !slices.Equal(toks[0].Teams, want) {
		t.Fatalf("read tokens = %+v, %v, want one for %q", toks, err, want)
	}
	stdout, _, err := execTokens(t, "list", "--read", "--db", db)
	if err != nil {
		t.Fatal(err)
	}
	if want := `"Platform Team","Équipe, Paris",équipe`; !strings.Contains(stdout, want) {
		t.Errorf("list --read = %q, want teams %s", stdout, want)
	}
}
