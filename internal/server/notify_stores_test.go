package server

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"os"
	"strings"
	"testing"

	"github.com/abd-ulbasit/upgradescope/internal/server/store"
)

// forEachStore runs a notification test on SQLite and, with
// UPGRADESCOPE_PG_TEST_DSN set (hack/pg-test.sh), on Postgres too, so the
// baseline each store selects is pinned end to end through the server.
func forEachStore(t *testing.T, run func(t *testing.T, st store.Store)) {
	t.Helper()
	t.Run("sqlite", func(t *testing.T) { run(t, openSQLite(t)) })
	t.Run("postgres", func(t *testing.T) { run(t, openPostgres(t)) })
}

// openPostgres opens a Postgres store in a fresh schema of the test
// database, dropped on cleanup; it skips without UPGRADESCOPE_PG_TEST_DSN.
func openPostgres(t *testing.T) store.Store {
	t.Helper()
	dsn := os.Getenv("UPGRADESCOPE_PG_TEST_DSN")
	if dsn == "" {
		t.Skip("UPGRADESCOPE_PG_TEST_DSN not set; run via hack/pg-test.sh")
	}
	var raw [8]byte
	_, _ = rand.Read(raw[:])
	schema := "notify_" + hex.EncodeToString(raw[:])
	admin, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open admin connection: %v", err)
	}
	if _, err := admin.ExecContext(context.Background(), "CREATE SCHEMA "+schema); err != nil {
		_ = admin.Close()
		t.Fatalf("create schema %s: %v", schema, err)
	}
	t.Cleanup(func() {
		_, _ = admin.ExecContext(context.Background(), "DROP SCHEMA "+schema+" CASCADE")
		_ = admin.Close()
	})
	sep := "?"
	if strings.Contains(dsn, "?") {
		sep = "&"
	}
	st, err := store.OpenPostgres(dsn + sep + "search_path=" + schema)
	if err != nil {
		t.Fatalf("OpenPostgres: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}
