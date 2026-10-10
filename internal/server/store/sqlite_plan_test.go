package store

import (
	"context"
	"database/sql"
	"fmt"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// queryPlan is EXPLAIN QUERY PLAN of query, its details joined.
func queryPlan(t *testing.T, db *sql.DB, query string, args ...any) string {
	t.Helper()
	rows, err := db.Query(`EXPLAIN QUERY PLAN `+query, args...)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var plan []string
	for rows.Next() {
		var id, parent, notused int
		var detail string
		if err := rows.Scan(&id, &parent, &notused, &detail); err != nil {
			t.Fatal(err)
		}
		plan = append(plan, detail)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return strings.Join(plan, "; ")
}

// TestEvaluationReadsUseIndexes pins the plans of the "newest evaluation
// by id" reads (#241): each walks an index of migration 0009 backwards and
// stops at the row it keeps. With only (cluster_id, target, created_at)
// they sorted every history row of the pair in a temp B-tree, reports
// carried along, so LatestKnownEvaluation, the notification baseline every
// changed push reads, grew with the pair's history.
func TestEvaluationReadsUseIndexes(t *testing.T) {
	s := newTestStore(t)
	for _, c := range []struct {
		name, query, index string
		args               []any
	}{
		{"LatestKnownEvaluation", sqliteLatestKnownQuery, "idx_evaluations_decided", []any{1, "1.36"}},
		{"LatestKnownEvaluation id (the commit's baseline check)", sqliteLatestKnownIDQuery, "idx_evaluations_decided", []any{1, "1.36"}},
		{"LatestEvaluation", sqliteLatestEvaluationQuery, "idx_evaluations_cluster_target_id", []any{1, "1.36"}},
		{"ScoreHistory", sqliteScoreHistoryQuery, "idx_evaluations_cluster_target_id", []any{1, "1.36", 100}},
		{"CurrentEvaluation", sqliteCurrentEvaluationQuery, "idx_evaluations_snapshot_target_id", []any{1, "1.36"}},
		{"CurrentEvaluationSummary", sqliteCurrentSummaryQuery, "idx_evaluations_snapshot_target_id", []any{1, "1.36"}},
		{"the commit's Current check", sqliteSnapshotEvaluationIDQuery, "idx_evaluations_snapshot_target_id", []any{1, "1.36"}},
	} {
		plan := queryPlan(t, s.db, c.query, c.args...)
		if strings.Contains(plan, "TEMP B-TREE") || !strings.Contains(plan, c.index) {
			t.Errorf("%s: plan = %q, want %s and no temp B-tree", c.name, plan, c.index)
		}
	}
}

// TestLatestSnapshotHeadsLooksUpEachCluster: the fleet reads' one query
// for every cluster's latest snapshot head steps down
// idx_snapshots_cluster_id once per cluster instead of grouping every
// snapshot row.
func TestLatestSnapshotHeadsLooksUpEachCluster(t *testing.T) {
	s := newTestStore(t)
	plan := queryPlan(t, s.db, `
		SELECT s.id FROM clusters c
		JOIN snapshots s ON s.id = (SELECT MAX(id) FROM snapshots WHERE cluster_id = c.id)`)
	if strings.Contains(plan, "TEMP B-TREE") || !strings.Contains(plan, "idx_snapshots_cluster_id") ||
		!strings.Contains(plan, "INTEGER PRIMARY KEY") {
		t.Errorf("plan = %q, want a primary-key lookup per cluster through idx_snapshots_cluster_id", plan)
	}
}

// TestEvaluationReadsDoNotGrowWithHistory measures the reads the plans
// above pin on 4,000 history rows of one (cluster, target), each with a
// 30 KB report (#241: LatestKnownEvaluation took 189.5 ms there, through
// a temp B-tree): the best of five runs of each is under 2 ms
// (LatestKnownEvaluation) and 5 ms (ScoreHistory of 100), and the newest
// decided row behind 3,999 unknown ones is found as fast as one on top.
func TestEvaluationReadsDoNotGrowWithHistory(t *testing.T) {
	if testing.Short() {
		t.Skip("writes 120 MB of reports")
	}
	ctx := context.Background()
	s := newTestStore(t)
	cid := mustCluster(t, s, "prod")
	sid := mustSnapshot(t, s, cid, "aaa", tBase)
	report := []byte(`{"findings":[],"pad":"` + strings.Repeat("x", 30<<10) + `"}`)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	for i := range 4000 {
		// The first row is the only decided one: the baseline sits at the
		// far end of the history.
		e := Evaluation{ClusterID: cid, SnapshotID: sid, Target: "1.33", Score: 90, Blockers: 0, Report: report, CreatedAt: tBase.Add(time.Duration(i) * time.Minute)}
		if i == 0 {
			e.Blockers = 1
		}
		if _, err := insertEvaluationSQLite(ctx, tx, e); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	// Same pair, newest row decided: the other end.
	cid2 := mustCluster(t, s, "other")
	sid2 := mustSnapshot(t, s, cid2, "bbb", tBase)
	if _, err := s.InsertEvaluation(ctx, Evaluation{ClusterID: cid2, SnapshotID: sid2, Target: "1.33", Score: 90, Blockers: 1, Report: report, CreatedAt: tBase}); err != nil {
		t.Fatal(err)
	}
	best := func(f func() error) time.Duration {
		t.Helper()
		var min time.Duration
		for i := range 5 {
			start := time.Now()
			if err := f(); err != nil {
				t.Fatal(err)
			}
			if d := time.Since(start); i == 0 || d < min {
				min = d
			}
		}
		return min
	}
	known := best(func() error { _, err := s.LatestKnownEvaluation(ctx, cid, "1.33"); return err })
	hist := best(func() error {
		h, err := s.ScoreHistory(ctx, cid, "1.33", 100)
		if err == nil && len(h) != 100 {
			err = fmt.Errorf("ScoreHistory returned %d points", len(h))
		}
		return err
	})
	top := best(func() error { _, err := s.LatestKnownEvaluation(ctx, cid2, "1.33"); return err })
	t.Logf("4,000 rows of 30 KB: LatestKnownEvaluation %v (the baseline 3,999 rows back), %v (on top); ScoreHistory(100) %v", known, top, hist)
	if raceEnabled {
		// The race detector slows the reads several-fold: the plans above
		// pin the absence of a sort; the bounds are for a plain run.
		return
	}
	if known > 2*time.Millisecond || hist > 5*time.Millisecond {
		t.Errorf("LatestKnownEvaluation %v (want < 2ms), ScoreHistory(100) %v (want < 5ms)", known, hist)
	}
}

// TestMigration0009Backfills: a database from before 0009 gets the
// server version of each snapshot stored without one (before 0006) from
// its inventory, and each evaluation's carries_hold from its report; a
// row that does not parse, or names no version, is left as it was.
func TestMigration0009Backfills(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "db.sqlite")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	sub, err := fs.Sub(migrationsFS, "migrations")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Migrate(ctx, db, migrationsBefore(t, sub, "0009")); err != nil {
		t.Fatalf("migrate to 0008: %v", err)
	}
	const at = `'2026-06-10T12:00:00.000000000Z'`
	for _, q := range []string{
		`INSERT INTO clusters (name, first_seen, last_seen) VALUES ('a', ` + at + `, ` + at + `), ('b', ` + at + `, ` + at + `), ('c', ` + at + `, ` + at + `), ('d', ` + at + `, ` + at + `)`,
		`INSERT INTO snapshots (cluster_id, hash, received_at, inventory) VALUES (1, 'a', ` + at + `, CAST('{"serverVersion":"v1.34.2","nodes":[]}' AS BLOB))`,
		`INSERT INTO snapshots (cluster_id, hash, received_at, inventory) VALUES (2, 'b', ` + at + `, CAST('{"nodes":[]}' AS BLOB))`,
		`INSERT INTO snapshots (cluster_id, hash, received_at, inventory) VALUES (3, 'c', ` + at + `, CAST('{"serverVersion":' AS BLOB))`,
		`INSERT INTO snapshots (cluster_id, hash, received_at, server_version, inventory) VALUES (4, 'd', ` + at + `, 'v1.30.0', CAST('{"serverVersion":"v1.31.0"}' AS BLOB))`,
		`INSERT INTO evaluations (cluster_id, snapshot_id, target, score, ready, blockers, warnings, report, created_at, evaluated_at)
			VALUES (1, 1, '1.35', 60, 0, 1, 0, CAST('{"carriedForward":[{"key":"k","holdUntil":"2026-06-11T00:00:00Z"}]}' AS BLOB), ` + at + `, ` + at + `),
			       (1, 1, '1.36', 60, 0, 1, 0, CAST('{"findings":[]}' AS BLOB), ` + at + `, ` + at + `),
			       (1, 1, '1.37', 60, 0, 1, 0, NULL, ` + at + `, ` + at + `)`,
	} {
		if _, err := db.ExecContext(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	s, err := Open(path) // applies 0009
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()
	for cid, want := range map[int64]string{1: "v1.34.2", 2: "", 3: "", 4: "v1.30.0"} {
		head, err := s.LatestSnapshotHead(ctx, cid)
		if err != nil || head.ServerVersion != want {
			t.Errorf("cluster %d: ServerVersion = (%q, %v), want %q", cid, head.ServerVersion, err, want)
		}
	}
	for target, want := range map[string]bool{"1.35": true, "1.36": false, "1.37": false} {
		e, err := s.CurrentEvaluationSummary(ctx, 1, target)
		if err != nil || e.CarriesHold != want {
			t.Errorf("%s: CarriesHold = (%v, %v), want %v", target, e.CarriesHold, err, want)
		}
	}
	var nulls int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM evaluations WHERE carries_hold IS NULL`).Scan(&nulls); err != nil || nulls != 0 {
		t.Errorf("rows left NULL = (%d, %v), want 0", nulls, err)
	}
}

// TestEvaluationWrittenWithoutCarriesHoldMayCarryOne: a row inserted by a
// binary that predates 0009 (a rollback after it ran) has a NULL
// carries_hold, read as carrying a hold: the background pass then loads
// its inventory, never skips a hold.
func TestEvaluationWrittenWithoutCarriesHoldMayCarryOne(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	cid := mustCluster(t, s, "prod")
	sid := mustSnapshot(t, s, cid, "aaa", tBase)
	if _, err := s.db.ExecContext(ctx, `
		INSERT INTO evaluations (cluster_id, snapshot_id, target, score, ready, blockers, warnings, report, created_at, evaluated_at, team_map_hash, not_assessed, teams)
		VALUES (?, ?, '1.36', 60, 0, 1, 0, CAST('{}' AS BLOB), ?, ?, '', '', '[]')`, cid, sid, formatTime(tBase), formatTime(tBase)); err != nil {
		t.Fatal(err)
	}
	if e, err := s.CurrentEvaluationSummary(ctx, cid, "1.36"); err != nil || !e.CarriesHold {
		t.Errorf("CarriesHold = (%v, %v), want true for a NULL column", e.CarriesHold, err)
	}
}

// TestPruneEvaluationDrainUsesCreatedAtIndex pins the plan of retention's
// evaluation DELETE (#297): its inner SELECT finds the old rows through
// idx_evaluations_created_at (migration 0010) and does not scan the table.
// created_at is stored after the report BLOB, so a scan reads every
// report's overflow chain, under the write lock, on every run, even with
// nothing to delete. The index alone is not enough: with ORDER BY id
// SQLite still scans (measured in #297), so the statement orders by
// (created_at, id) as the index does.
func TestPruneEvaluationDrainUsesCreatedAtIndex(t *testing.T) {
	s := newTestStore(t)
	plan := queryPlan(t, s.db, sqliteDialect.evaluationDrain(), formatTime(tBase), pruneBatchRows)
	if !strings.Contains(plan, "idx_evaluations_created_at") || !strings.Contains(plan, "created_at<?") {
		t.Errorf("plan = %q, want a search of idx_evaluations_created_at bounded by created_at<?", plan)
	}
	// The outer DELETE looks the ids up by primary key, and the subquery of
	// each cluster's newest decided evaluation reads a covering index
	// (idx_evaluations_decided, no report); a bare "SCAN evaluations" is
	// the table, reports and all.
	for _, step := range strings.Split(plan, "; ") {
		if step == "SCAN evaluations" {
			t.Errorf("plan step %q scans every evaluation (plan = %q)", step, plan)
		}
	}
	if strings.Contains(plan, "TEMP B-TREE") {
		t.Errorf("plan = %q, want no sort: the index delivers the order", plan)
	}
}
