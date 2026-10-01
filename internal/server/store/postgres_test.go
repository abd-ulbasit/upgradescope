package store_test

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"fmt"
	"os"
	"sync"
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
	s, err := store.OpenPostgres(freshSchemaDSN(t))
	if err != nil {
		t.Fatalf("OpenPostgres: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// freshSchemaDSN creates an empty schema, drops it on cleanup, and returns
// the test DSN with search_path pinned to it.
func freshSchemaDSN(t *testing.T) string {
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
	return fmt.Sprintf("%s%ssearch_path=%s", dsn, sep, schema)
}

// TestPostgresConformance also carries the Postgres-only migration checks:
// CI runs exactly this test name against a real server.
func TestPostgresConformance(t *testing.T) {
	pgDSN(t)
	storetest.RunStoreConformance(t, func(t *testing.T) store.Store { return freshPostgres(t) })
	t.Run("ConcurrentMigrations", testConcurrentMigrations)
	t.Run("PreFreshnessWriter", testPreFreshnessWriter)
}

// testPreFreshnessWriter: during a rolling upgrade (or after a rollback) a
// binary that predates migration 0004 inserts evaluations without
// evaluated_at. The column's DEFAULT must let that insert succeed — NOT
// NULL alone would drop the old replica's evaluations — and the row must
// read back with a real evaluatedAt.
func testPreFreshnessWriter(t *testing.T) {
	ctx := context.Background()
	dsn := freshSchemaDSN(t)
	s, err := store.OpenPostgres(dsn)
	if err != nil {
		t.Fatalf("OpenPostgres: %v", err)
	}
	defer s.Close()
	cid, err := s.UpsertCluster(ctx, store.Cluster{Name: "prod", ClusterUID: "uid-prod"})
	if err != nil {
		t.Fatal(err)
	}
	sid, _, err := s.InsertSnapshot(ctx, store.Snapshot{ClusterID: cid, Hash: "aaa", Inventory: []byte(`{}`)})
	if err != nil {
		t.Fatal(err)
	}

	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()
	// The pre-0004 INSERT column list.
	if _, err := db.ExecContext(ctx, `
		INSERT INTO evaluations (cluster_id, snapshot_id, target, kb_version, score, ready, blockers, warnings, report, created_at)
		VALUES ($1, $2, '1.36', 'kb-old', 70, false, 1, 0, $3, now())`, cid, sid, []byte(`{}`)); err != nil {
		t.Fatalf("pre-0004 insert: %v", err)
	}
	got, err := s.CurrentEvaluation(ctx, cid, "1.36")
	if err != nil {
		t.Fatalf("CurrentEvaluation: %v", err)
	}
	if got.EvaluatedAt.IsZero() || got.KBVersion != "kb-old" {
		t.Errorf("read back evaluated %v kb %q, want a set evaluatedAt and kb-old", got.EvaluatedAt, got.KBVersion)
	}
}

// testConcurrentMigrations starts several stores against one empty schema at
// once, as replicas of a Deployment do on first rollout. The advisory lock
// must serialize them: every open succeeds and each migration is recorded
// exactly once.
func testConcurrentMigrations(t *testing.T) {
	dsn := freshSchemaDSN(t)
	const replicas = 6
	errs := make(chan error, replicas)
	var wg sync.WaitGroup
	for range replicas {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s, err := store.OpenPostgres(dsn)
			if err == nil {
				err = s.Close()
			}
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Errorf("concurrent OpenPostgres: %v", err)
		}
	}

	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()
	rows, err := db.QueryContext(context.Background(),
		`SELECT version, COUNT(*) FROM schema_migrations GROUP BY version ORDER BY version`)
	if err != nil {
		t.Fatalf("query schema_migrations: %v", err)
	}
	defer rows.Close()
	var versions []string
	for rows.Next() {
		var v string
		var n int
		if err := rows.Scan(&v, &n); err != nil {
			t.Fatalf("scan: %v", err)
		}
		if n != 1 {
			t.Errorf("migration %s recorded %d times, want 1", v, n)
		}
		versions = append(versions, v)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	if len(versions) == 0 || versions[0] != "0001_init.sql" {
		t.Errorf("recorded migrations = %v, want 0001_init.sql first", versions)
	}
}

// TestPostgresOpenBadDSN pins that OpenPostgres fails fast (ping at open)
// instead of deferring connection errors to the first query.
func TestPostgresOpenBadDSN(t *testing.T) {
	pgDSN(t) // gate: only meaningful where a pg environment exists at all
	if _, err := store.OpenPostgres("postgres://nobody:wrong@127.0.0.1:1/none?connect_timeout=1&sslmode=disable"); err == nil {
		t.Fatal("OpenPostgres(bad dsn) succeeded, want error")
	}
}
