package store

import (
	"context"
	"database/sql"
	"fmt"
	"io/fs"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"
)

// pgTestSchema opens an empty schema of the test Postgres for one test
// (hack/pg-test.sh provides it), dropped on cleanup, and returns a DSN
// pinned to it; it skips without UPGRADESCOPE_PG_TEST_DSN.
func pgTestSchema(t *testing.T, prefix string) string {
	t.Helper()
	dsn := os.Getenv("UPGRADESCOPE_PG_TEST_DSN")
	if dsn == "" {
		t.Skip("UPGRADESCOPE_PG_TEST_DSN not set; run via hack/pg-test.sh")
	}
	ctx := context.Background()
	admin, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	schema := fmt.Sprintf("%s_%d", prefix, time.Now().UnixNano())
	if _, err := admin.ExecContext(ctx, "CREATE SCHEMA "+schema); err != nil {
		_ = admin.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = admin.ExecContext(context.Background(), "DROP SCHEMA "+schema+" CASCADE")
		_ = admin.Close()
	})
	sep := "?"
	if strings.Contains(dsn, "?") {
		sep = "&"
	}
	return dsn + sep + "search_path=" + schema
}

// TestPostgresEvaluationReadsUseIndexes is TestEvaluationReadsUseIndexes
// on Postgres (#241): the "newest evaluation by id" reads walk an index of
// migration 0009 backwards, with no sort, and the fleet's latest snapshot
// heads are one index step per cluster. Sequential scans are switched off
// so the empty tables cannot make one cheaper than the index.
func TestPostgresEvaluationReadsUseIndexes(t *testing.T) {
	ctx := context.Background()
	p, err := OpenPostgres(pgTestSchema(t, "plans"))
	if err != nil {
		t.Fatalf("OpenPostgres: %v", err)
	}
	defer p.Close()
	conn, err := p.db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	for _, set := range []string{`SET enable_seqscan = off`, `SET enable_bitmapscan = off`} {
		if _, err := conn.ExecContext(ctx, set); err != nil {
			t.Fatal(err)
		}
	}
	plan := func(query string, args ...any) string {
		t.Helper()
		rows, err := conn.QueryContext(ctx, `EXPLAIN `+query, args...)
		if err != nil {
			t.Fatalf("EXPLAIN: %v", err)
		}
		defer rows.Close()
		var lines []string
		for rows.Next() {
			var line string
			if err := rows.Scan(&line); err != nil {
				t.Fatal(err)
			}
			lines = append(lines, strings.TrimSpace(line))
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		return strings.Join(lines, "; ")
	}
	for _, c := range []struct {
		name, query, index string
		args               []any
	}{
		{"LatestKnownEvaluation", pgLatestKnownQuery, "idx_evaluations_", []any{1, "1.36"}},
		{"LatestKnownEvaluation id (the commit's baseline check)", pgLatestKnownIDQuery, "idx_evaluations_", []any{1, "1.36"}},
		{"ScoreHistory", pgScoreHistoryQuery, "idx_evaluations_cluster_target_id", []any{1, "1.36", 100}},
		{"LatestSnapshotHeads", `SELECT s.id FROM clusters c CROSS JOIN LATERAL (
			SELECT id FROM snapshots WHERE cluster_id = c.id ORDER BY id DESC LIMIT 1) s`, "idx_snapshots_cluster_id", nil},
	} {
		got := plan(c.query, c.args...)
		if strings.Contains(got, "Sort") || !strings.Contains(got, c.index) || strings.Contains(got, "created") {
			t.Errorf("%s: plan = %q, want an index scan on %s* and no sort", c.name, got, c.index)
		}
	}
}

// TestMigration0009BackfillsPostgres is TestMigration0009Backfills on
// Postgres (hack/pg-test.sh runs it).
func TestMigration0009BackfillsPostgres(t *testing.T) {
	ctx := context.Background()
	dsn := pgTestSchema(t, "backfill9")
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	sub, err := fs.Sub(pgMigrationsFS, "pgmigrations")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := migratePostgres(ctx, db, migrationsBefore(t, sub, "0009")); err != nil {
		t.Fatalf("migrate to 0008: %v", err)
	}
	for _, q := range []string{
		`INSERT INTO clusters (name, first_seen, last_seen) VALUES ('a', now(), now()), ('b', now(), now()), ('c', now(), now()), ('d', now(), now()), ('e', now(), now())`,
		`INSERT INTO snapshots (cluster_id, hash, received_at, inventory) VALUES (1, 'a', now(), convert_to('{"serverVersion":"v1.34.2","nodes":[]}', 'UTF8'))`,
		`INSERT INTO snapshots (cluster_id, hash, received_at, inventory) VALUES (2, 'b', now(), convert_to('{"nodes":[]}', 'UTF8'))`,
		`INSERT INTO snapshots (cluster_id, hash, received_at, inventory) VALUES (3, 'c', now(), convert_to('{"serverVersion":', 'UTF8'))`,
		`INSERT INTO snapshots (cluster_id, hash, received_at, server_version, inventory) VALUES (4, 'd', now(), 'v1.30.0', convert_to('{"serverVersion":"v1.31.0"}', 'UTF8'))`,
		// \u0000, which encoding/json accepts and jsonb refuses: left as it was.
		`INSERT INTO snapshots (cluster_id, hash, received_at, inventory) VALUES (5, 'e', now(), convert_to('{"serverVersion":"v1.33.0","x":"\u0000"}', 'UTF8'))`,
		`INSERT INTO evaluations (cluster_id, snapshot_id, target, score, ready, blockers, warnings, report, created_at)
			VALUES (1, 1, '1.35', 60, false, 1, 0, convert_to('{"carriedForward":[{"key":"k","holdUntil":"2026-06-11T00:00:00Z"}]}', 'UTF8'), now()),
			       (1, 1, '1.36', 60, false, 1, 0, convert_to('{"findings":[]}', 'UTF8'), now()),
			       (1, 1, '1.37', 60, false, 1, 0, NULL, now())`,
	} {
		if _, err := db.ExecContext(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	p, err := OpenPostgres(dsn) // applies 0009
	if err != nil {
		t.Fatalf("OpenPostgres: %v", err)
	}
	defer p.Close()
	for cid, want := range map[int64]string{1: "v1.34.2", 2: "", 3: "", 4: "v1.30.0", 5: ""} {
		head, err := p.LatestSnapshotHead(ctx, cid)
		if err != nil || head.ServerVersion != want {
			t.Errorf("cluster %d: ServerVersion = (%q, %v), want %q", cid, head.ServerVersion, err, want)
		}
	}
	for target, want := range map[string]bool{"1.35": true, "1.36": false, "1.37": false} {
		e, err := p.CurrentEvaluationSummary(ctx, 1, target)
		if err != nil || e.CarriesHold != want {
			t.Errorf("%s: CarriesHold = (%v, %v), want %v", target, e.CarriesHold, err, want)
		}
	}
	var nulls int
	if err := p.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM evaluations WHERE carries_hold IS NULL`).Scan(&nulls); err != nil || nulls != 0 {
		t.Errorf("rows left NULL = (%d, %v), want 0", nulls, err)
	}
}

// TestPostgresPruneEvaluationDrainUsesCreatedAtIndex is
// TestPruneEvaluationDrainUsesCreatedAtIndex on Postgres (#297): the inner
// SELECT of retention's evaluation DELETE ordered by (created_at, id)
// walks idx_evaluations_created_at (migration 0010) bounded by the cutoff
// and needs no sort of the old rows, so a run with nothing to delete reads
// the oldest index entries and no report. Sequential and bitmap scans are
// switched off: on the test's near-empty tables the planner may otherwise
// prefer a bitmap scan of the same index plus a sort (which also reads only
// the rows the index matches), and the point here is that the index serves
// the drain's (created_at, id) order. The statement's other subqueries may
// plan as they like.
func TestPostgresPruneEvaluationDrainUsesCreatedAtIndex(t *testing.T) {
	ctx := context.Background()
	p, err := OpenPostgres(pgTestSchema(t, "drainplan"))
	if err != nil {
		t.Fatalf("OpenPostgres: %v", err)
	}
	defer p.Close()
	conn, err := p.db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	for _, set := range []string{`SET enable_seqscan = off`, `SET enable_bitmapscan = off`} {
		if _, err := conn.ExecContext(ctx, set); err != nil {
			t.Fatal(err)
		}
	}
	rows, err := conn.QueryContext(ctx, `EXPLAIN `+pgDialect.evaluationDrain(), tBase.UTC(), pruneBatchRows)
	if err != nil {
		t.Fatalf("EXPLAIN: %v", err)
	}
	defer rows.Close()
	var lines []string
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			t.Fatal(err)
		}
		lines = append(lines, strings.TrimSpace(line))
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	plan := strings.Join(lines, "; ")
	if !strings.Contains(plan, "Index Scan using idx_evaluations_created_at") && !strings.Contains(plan, "Index Only Scan using idx_evaluations_created_at") {
		t.Errorf("plan = %q, want an index scan on idx_evaluations_created_at", plan)
	}
	// No sort on the drain's order columns at all: a sort by created_at
	// means the index's order went unused, and a sort by id means the drain
	// is ordered by id again (#297's bug), which the index cannot serve. The
	// other subqueries group by cluster_id and target, which this ignores.
	sortsDrain := regexp.MustCompile(`created_at|\.id(,|$)`)
	for _, line := range lines {
		if strings.HasPrefix(line, "Sort Key:") && sortsDrain.MatchString(line) {
			t.Errorf("plan = %q: %q, want the drain read in idx_evaluations_created_at's order with no sort", plan, line)
		}
	}
}

// TestPostgresMigration0010AcceptsAnIndexBuiltConcurrently: an operator who
// built idx_evaluations_created_at with CREATE INDEX CONCURRENTLY before the
// upgrade (docs/operations/upgrade.md, so the build does not block writes
// to evaluations) gets a migration 0010 that succeeds, records itself and
// leaves the one index, not a failure of the replica's start.
func TestPostgresMigration0010AcceptsAnIndexBuiltConcurrently(t *testing.T) {
	ctx := context.Background()
	dsn := pgTestSchema(t, "mig10")
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	sub, err := fs.Sub(pgMigrationsFS, "pgmigrations")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := migratePostgres(ctx, db, migrationsBefore(t, sub, "0010")); err != nil {
		t.Fatalf("migrate to 0009: %v", err)
	}
	if _, err := db.ExecContext(ctx, `CREATE INDEX CONCURRENTLY idx_evaluations_created_at ON evaluations (created_at, id)`); err != nil {
		t.Fatalf("CREATE INDEX CONCURRENTLY: %v", err)
	}
	p, err := OpenPostgres(dsn) // applies 0010
	if err != nil {
		t.Fatalf("OpenPostgres with the index already built: %v", err)
	}
	defer p.Close()
	var recorded, indexes int
	if err := p.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM schema_migrations WHERE version = '0010_evaluation_created_at.sql'`).Scan(&recorded); err != nil || recorded != 1 {
		t.Errorf("migration 0010 recorded = (%d, %v), want 1", recorded, err)
	}
	if err := p.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM pg_indexes WHERE schemaname = current_schema() AND indexname = 'idx_evaluations_created_at'`).Scan(&indexes); err != nil || indexes != 1 {
		t.Errorf("idx_evaluations_created_at count = (%d, %v), want 1", indexes, err)
	}
}
