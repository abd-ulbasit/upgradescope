package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"testing/fstest"
	"time"
	"unicode/utf8"
)

// migrationsBefore is the embedded migration set up to (not including)
// the file named first.
func migrationsBefore(t *testing.T, fsys fs.FS, first string) fstest.MapFS {
	t.Helper()
	names, err := fs.Glob(fsys, "*.sql")
	if err != nil {
		t.Fatal(err)
	}
	out := fstest.MapFS{}
	for _, name := range names {
		if name >= first {
			continue
		}
		data, err := fs.ReadFile(fsys, name)
		if err != nil {
			t.Fatal(err)
		}
		out[name] = &fstest.MapFile{Data: data}
	}
	return out
}

// notAssessedBackfillRows are evaluations stored before migration 0007,
// with the not_assessed each must have after it.
var notAssessedBackfillRows = []struct {
	target string
	report []byte
	want   any // decoded JSON; nil = none
}{
	{"1.31", []byte(`{"findings":[],"notAssessed":[{"capability":"apiUsage","reason":"forbidden"}]}`),
		[]any{map[string]any{"capability": "apiUsage", "reason": "forbidden"}}},
	{"1.32", []byte(`{"findings":[],"notAssessed":[]}`), nil},
	{"1.33", []byte(`{"findings":[]}`), nil},
	{"1.34", []byte(`{"notAssessed":`), nil}, // corrupt
	{"1.35", nil, nil},                       // NULL report
	{"1.36", []byte(`[1,2]`), nil},           // not an object
}

func decodedGaps(t *testing.T, raw []byte) any {
	t.Helper()
	if raw == nil {
		return nil
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatalf("not_assessed %q: %v", raw, err)
	}
	return v
}

// Migration 0007 backfills not_assessed from each stored report, so the
// summaries that read it instead of the report keep saying what an
// existing verdict could not cover; a report that does not parse fails
// nothing.
func TestMigration0007BackfillsNotAssessedSQLite(t *testing.T) {
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
	if _, err := Migrate(ctx, db, migrationsBefore(t, sub, "0007")); err != nil {
		t.Fatalf("migrate to 0006: %v", err)
	}
	for _, q := range []string{
		`INSERT INTO clusters (name, first_seen, last_seen) VALUES ('prod', '2026-06-10T12:00:00.000000000Z', '2026-06-10T12:00:00.000000000Z')`,
		`INSERT INTO snapshots (cluster_id, hash, received_at, inventory) VALUES (1, 'aaa', '2026-06-10T12:00:00.000000000Z', x'7b7d')`,
	} {
		if _, err := db.ExecContext(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	for _, r := range notAssessedBackfillRows {
		if _, err := db.ExecContext(ctx, `
			INSERT INTO evaluations (cluster_id, snapshot_id, target, score, ready, blockers, warnings, report, created_at, evaluated_at)
			VALUES (1, 1, ?, 70, 0, 1, 0, ?, '2026-06-10T12:00:00.000000000Z', '2026-06-10T12:00:00.000000000Z')`, r.target, r.report); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	s, err := Open(path) // applies 0007
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()
	for _, r := range notAssessedBackfillRows {
		got, err := s.CurrentEvaluationSummary(ctx, 1, r.target)
		if err != nil {
			t.Fatalf("%s: %v", r.target, err)
		}
		if g := decodedGaps(t, got.NotAssessed); !reflect.DeepEqual(g, r.want) {
			t.Errorf("%s (report %.40q): NotAssessed = %s, want %v", r.target, r.report, got.NotAssessed, r.want)
		}
	}
	var nulls int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM evaluations WHERE not_assessed IS NULL`).Scan(&nulls); err != nil || nulls != 0 {
		t.Errorf("rows left NULL = (%d, %v), want 0", nulls, err)
	}
}

// The Postgres 0007 backfill, on the same rows (hack/pg-test.sh runs it).
func TestMigration0007BackfillsNotAssessedPostgres(t *testing.T) {
	dsn := os.Getenv("UPGRADESCOPE_PG_TEST_DSN")
	if dsn == "" {
		t.Skip("UPGRADESCOPE_PG_TEST_DSN not set; run via hack/pg-test.sh")
	}
	ctx := context.Background()
	admin, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	schema := fmt.Sprintf("backfill_%d", time.Now().UnixNano())
	if _, err := admin.ExecContext(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	defer admin.ExecContext(context.WithoutCancel(ctx), "DROP SCHEMA "+schema+" CASCADE") //nolint:errcheck // cleanup
	sep := "?"
	if strings.Contains(dsn, "?") {
		sep = "&"
	}
	schemaDSN := dsn + sep + "search_path=" + schema

	db, err := sql.Open("pgx", schemaDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	sub, err := fs.Sub(pgMigrationsFS, "pgmigrations")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := migratePostgres(ctx, db, migrationsBefore(t, sub, "0007")); err != nil {
		t.Fatalf("migrate to 0006: %v", err)
	}
	for _, q := range []string{
		`INSERT INTO clusters (name, first_seen, last_seen) VALUES ('prod', now(), now())`,
		`INSERT INTO snapshots (cluster_id, hash, received_at, inventory) VALUES (1, 'aaa', now(), '\x7b7d'::bytea)`,
	} {
		if _, err := db.ExecContext(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	for _, r := range notAssessedBackfillRows {
		if _, err := db.ExecContext(ctx, `
			INSERT INTO evaluations (cluster_id, snapshot_id, target, score, ready, blockers, warnings, report, created_at)
			VALUES (1, 1, $1, 70, false, 1, 0, $2, now())`, r.target, r.report); err != nil {
			t.Fatal(err)
		}
	}

	s, err := OpenPostgres(schemaDSN) // applies 0007
	if err != nil {
		t.Fatalf("OpenPostgres: %v", err)
	}
	defer s.Close()
	for _, r := range notAssessedBackfillRows {
		got, err := s.CurrentEvaluationSummary(ctx, 1, r.target)
		if err != nil {
			t.Fatalf("%s: %v", r.target, err)
		}
		if g := decodedGaps(t, got.NotAssessed); !reflect.DeepEqual(g, r.want) {
			t.Errorf("%s (report %.40q): NotAssessed = %s, want %v", r.target, r.report, got.NotAssessed, r.want)
		}
	}
	var nulls int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM evaluations WHERE not_assessed IS NULL`).Scan(&nulls); err != nil || nulls != 0 {
		t.Errorf("rows left NULL = (%d, %v), want 0", nulls, err)
	}
}

// A row written by a binary that predates 0007 (a rollback after it ran)
// has a NULL not_assessed: it reads as none, and the next refresh fills it.
func TestEvaluationWrittenWithoutNotAssessedStaysReadable(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	cid := mustCluster(t, s, "prod")
	sid := mustSnapshot(t, s, cid, "aaa", tBase)
	gaps := `{"notAssessed":[{"capability":"apiUsage"}]}`
	res, err := s.db.ExecContext(ctx, `
		INSERT INTO evaluations (cluster_id, snapshot_id, target, kb_version, score, ready, blockers, warnings, report, created_at, evaluated_at, team_map_hash)
		VALUES (?, ?, '1.36', 'kb', 70, 0, 1, 0, ?, ?, ?, '')`, cid, sid, []byte(gaps), formatTime(tBase), formatTime(tBase))
	if err != nil {
		t.Fatal(err)
	}
	id, _ := res.LastInsertId()
	if got, err := s.CurrentEvaluationSummary(ctx, cid, "1.36"); err != nil || got.NotAssessed != nil {
		t.Fatalf("pre-0007 row = (%s, %v), want readable with no NotAssessed", got.NotAssessed, err)
	}
	if _, _, err := s.CommitEvaluations(ctx, EvaluationBatch{
		ClusterID: cid, SnapshotID: sid, Current: map[string]int64{"1.36": id},
		Refresh: []Evaluation{{ID: id, Blockers: 1, Report: []byte(gaps), EvaluatedAt: tPlus(30)}},
	}); err != nil {
		t.Fatal(err)
	}
	if got, err := s.CurrentEvaluationSummary(ctx, cid, "1.36"); err != nil || !strings.Contains(string(got.NotAssessed), "apiUsage") {
		t.Errorf("after refresh = (%s, %v), want the report's gaps", got.NotAssessed, err)
	}
}

// The column is a summary that the fleet-wide reads load for every
// cluster and target, so it keeps at most a bounded part of each gap: its
// capability name cut to SummaryCapabilityBytes, its reason to
// SummaryReasonBytes, the first SummarySkipped skipped entries, each cut
// to SummarySkippedBytes, and how many more there are; every other field
// as it was, and no field the gap did not have. It writes < > & and
// U+2028 as themselves: json.Marshal wrote each as six bytes, so a report
// of gaps whose reasons were mostly those stored a column four times its
// size.
func TestNotAssessedOfIsBounded(t *testing.T) {
	var skipped []string
	for i := range SummarySkipped + 5 {
		skipped = append(skipped, fmt.Sprintf("s%d", i))
	}
	skipped[0] = strings.Repeat("k", SummarySkippedBytes+10)
	gap := map[string]any{"capability": "api-usage", "partial": true, "required": true, "future": 1,
		"reason": strings.Repeat("é", SummaryReasonBytes) + "\u2028<&>", "skipped": skipped}
	small := map[string]any{"capability": "helm", "reason": "<forbidden> & 'denied'\u2028"}
	named := map[string]any{"capability": strings.Repeat("c", 16<<10)} // no reason, no skipped
	var report strings.Builder                                         // as the server writes reports: < > & as themselves (below)
	enc := json.NewEncoder(&report)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(map[string]any{"notAssessed": []any{gap, small, named}}); err != nil {
		t.Fatal(err)
	}
	// The server writes line separators as themselves too; Encoder does not.
	got := notAssessedOf([]byte(strings.ReplaceAll(report.String(), `\u2028`, "\u2028")))
	var gaps []map[string]any
	if err := json.Unmarshal([]byte(got), &gaps); err != nil || len(gaps) != 3 {
		t.Fatalf("notAssessedOf = %.300q (%v), want three gaps", got, err)
	}
	reason, _ := gaps[0]["reason"].(string)
	listed, _ := gaps[0]["skipped"].([]any)
	if len(reason) > SummaryReasonBytes+len("…") || !strings.HasSuffix(reason, "…") || !utf8.ValidString(reason) ||
		len(listed) != SummarySkipped || len(listed[0].(string)) > SummarySkippedBytes+len("…") ||
		gaps[0]["skippedOmitted"] != float64(5) || gaps[0]["capability"] != "api-usage" || gaps[0]["required"] != true || gaps[0]["future"] != float64(1) {
		t.Errorf("large gap = %.400v, want its reason and skipped cut, 5 more counted, the rest kept", gaps[0])
	}
	if !reflect.DeepEqual(gaps[1], small) {
		t.Errorf("small gap = %v, want it whole", gaps[1])
	}
	if c, _ := gaps[2]["capability"].(string); len(gaps[2]) != 1 || len(c) > SummaryCapabilityBytes+len("…") {
		t.Errorf("long-named gap = %.200v, want only its capability, cut", gaps[2])
	}
	if !strings.Contains(got, "\"<forbidden> & 'denied'\u2028\"") || strings.Contains(got, `\u2028`) || strings.Contains(got, `\u003c`) {
		t.Errorf("notAssessedOf = %.300q, want < > & and U+2028 written as themselves", got)
	}
}

func TestNotAssessedOf(t *testing.T) {
	for report, want := range map[string]string{
		`{"notAssessed":[{"capability":"x"}]}`: `[{"capability":"x"}]`,
		`{"notAssessed": [ ] }`:                ``,
		`{"notAssessed":null}`:                 ``,
		`{"findings":[]}`:                      ``,
		`{"notAssessed":`:                      ``,
		``:                                     ``,
	} {
		if got := notAssessedOf([]byte(report)); got != want {
			t.Errorf("notAssessedOf(%q) = %q, want %q", report, got, want)
		}
	}
}
