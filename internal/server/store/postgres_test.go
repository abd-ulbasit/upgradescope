package store_test

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"fmt"
	"os"
	"testing"

	"github.com/abd-ulbasit/upgradescope/internal/server/store"
	"github.com/abd-ulbasit/upgradescope/internal/server/store/storetest"
)

// pgDSN returns the test DSN or skips: Postgres conformance is env-gated so
// `go test ./...` stays hermetic. hack/pg-test.sh (make pg-test) provides a
// real postgres:17 container and sets the variable.
func pgDSN(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv("UPGRADESCOPE_PG_TEST_DSN")
	if dsn == "" {
		t.Skip("UPGRADESCOPE_PG_TEST_DSN not set; run via hack/pg-test.sh")
	}
	return dsn
}

// freshPostgres gives each conformance subtest its own empty schema (cheaper
// than a database per subtest), opened through store.OpenPostgres with
// search_path pinned, and drops it on cleanup.
func freshPostgres(t *testing.T) store.Store {
	t.Helper()
	dsn := pgDSN(t)

	var raw [8]byte
	if _, err := rand.Read(raw[:]); err != nil {
		t.Fatalf("rand: %v", err)
	}
	schema := "conf_" + hex.EncodeToString(raw[:])

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
	for _, r := range dsn {
		if r == '?' {
			sep = "&"
			break
		}
	}
	s, err := store.OpenPostgres(fmt.Sprintf("%s%ssearch_path=%s", dsn, sep, schema))
	if err != nil {
		t.Fatalf("OpenPostgres: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestPostgresConformance(t *testing.T) {
	pgDSN(t)
	storetest.RunStoreConformance(t, func(t *testing.T) store.Store { return freshPostgres(t) })
}

// TestPostgresOpenBadDSN pins that OpenPostgres fails fast (ping at open)
// instead of deferring connection errors to the first query.
func TestPostgresOpenBadDSN(t *testing.T) {
	pgDSN(t) // gate: only meaningful where a pg environment exists at all
	if _, err := store.OpenPostgres("postgres://nobody:wrong@127.0.0.1:1/none?connect_timeout=1&sslmode=disable"); err == nil {
		t.Fatal("OpenPostgres(bad dsn) succeeded, want error")
	}
}
