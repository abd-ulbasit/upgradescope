package cli

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/abd-ulbasit/upgradescope/internal/server/store"
)

// Against a live Postgres (UPGRADESCOPE_IT=1 and UPGRADESCOPE_PG_TEST_DSN,
// as hack/pg-test.sh sets it), `tokens create --read` mints into the
// database $UPGRADESCOPE_DB_URL names, not into ./upgradescope.db (#382).
func TestTokensCreateReadPostgresIT(t *testing.T) {
	dsn := os.Getenv("UPGRADESCOPE_PG_TEST_DSN")
	if os.Getenv("UPGRADESCOPE_IT") != "1" || dsn == "" {
		t.Skip("set UPGRADESCOPE_IT=1 and UPGRADESCOPE_PG_TEST_DSN (see hack/pg-test.sh)")
	}
	ctx := context.Background()
	admin, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	schema := fmt.Sprintf("tokens_it_%d", time.Now().UnixNano())
	if _, err := admin.ExecContext(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	defer admin.ExecContext(context.WithoutCancel(ctx), "DROP SCHEMA "+schema+" CASCADE") //nolint:errcheck // cleanup
	sep := "?"
	if strings.Contains(dsn, "?") {
		sep = "&"
	}
	schemaDSN := dsn + sep + "search_path=" + schema

	t.Chdir(t.TempDir())
	t.Setenv("UPGRADESCOPE_DB_URL", schemaDSN)
	stdout, stderr, err := execTokens(t, "create", "--read", "--teams", "web")
	if err != nil {
		t.Fatalf("create --read: %v (stderr %q)", err, stderr)
	}
	noStrayDB(t)

	st, err := store.OpenPostgres(schemaDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	toks, err := st.ListReadTokens(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(toks) != 1 || toks[0].Prefix != store.TokenPrefix(strings.TrimSpace(stdout)) {
		t.Fatalf("read_tokens = %+v, want the one token just minted", toks)
	}
}
