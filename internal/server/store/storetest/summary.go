package storetest

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"

	"github.com/abd-ulbasit/upgradescope/internal/server/store"
)

// notAssessed decodes an evaluation's NotAssessed for comparison: the
// store may re-encode the JSON (Postgres jsonb does), so bytes are not
// compared.
func notAssessed(t *testing.T, raw []byte) any {
	t.Helper()
	if raw == nil {
		return nil
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatalf("NotAssessed %q is not JSON: %v", raw, err)
	}
	return v
}

// testEvaluationSummary pins CurrentEvaluationSummary: the current
// evaluation without its report, carrying the report's notAssessed, which
// the store keeps beside the report on every write (insert, commit insert,
// refresh). The read API's fleet-wide summaries use it so that they never
// load a report.
func testEvaluationSummary(t *testing.T, s store.Store) {
	ctx := context.Background()
	cid := mustCluster(t, s, "prod")
	if _, err := s.CurrentEvaluationSummary(ctx, cid, "1.36"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("no snapshot: err = %v, want ErrNotFound", err)
	}
	sid := mustSnapshot(t, s, cid, "aaa", base)
	gaps := `{"findings":[],"notAssessed":[{"capability":"apiUsage","reason":"forbidden"}]}`
	want := []any{map[string]any{"capability": "apiUsage", "reason": "forbidden"}}
	e36 := mustEval(t, s, store.Evaluation{ClusterID: cid, SnapshotID: sid, Target: "1.36", KBVersion: "kb-1",
		Score: 70, Blockers: 1, Warnings: 2, Report: []byte(gaps), CreatedAt: base, EvaluatedAt: at(1), TeamMapHash: "tm"})
	mustEval(t, s, store.Evaluation{ClusterID: cid, SnapshotID: sid, Target: "1.37", Score: 100, Ready: true,
		Report: []byte(`{"notAssessed":[]}`), CreatedAt: base})

	got, err := s.CurrentEvaluationSummary(ctx, cid, "1.36")
	if err != nil {
		t.Fatalf("CurrentEvaluationSummary(1.36): %v", err)
	}
	if got.Report != nil {
		t.Errorf("summary carries a %d-byte report, want none", len(got.Report))
	}
	if got.ID != e36 || got.ClusterID != cid || got.SnapshotID != sid || got.Target != "1.36" || got.KBVersion != "kb-1" ||
		got.Score != 70 || got.Ready || got.Blockers != 1 || got.Warnings != 2 || got.TeamMapHash != "tm" ||
		!got.CreatedAt.Equal(base) || !got.EvaluatedAt.Equal(at(1)) {
		t.Errorf("summary = %+v, want the 1.36 row's columns", got)
	}
	if na := notAssessed(t, got.NotAssessed); !reflect.DeepEqual(na, want) {
		t.Errorf("summary NotAssessed = %s, want the report's %v", got.NotAssessed, want)
	}
	full, err := s.CurrentEvaluation(ctx, cid, "1.36")
	if err != nil {
		t.Fatal(err)
	}
	if na := notAssessed(t, full.NotAssessed); !reflect.DeepEqual(na, want) || string(full.Report) != gaps {
		t.Errorf("CurrentEvaluation NotAssessed = %s, report %s; want the report's gaps and the report", full.NotAssessed, full.Report)
	}
	if got37, err := s.CurrentEvaluationSummary(ctx, cid, "1.37"); err != nil || got37.NotAssessed != nil {
		t.Errorf("1.37 (empty notAssessed) = (%s, %v), want nil NotAssessed", got37.NotAssessed, err)
	}

	// A refresh replaces the report, and with it what was not assessed.
	if _, _, err := s.CommitEvaluations(ctx, store.EvaluationBatch{
		ClusterID: cid, SnapshotID: sid, Current: map[string]int64{"1.36": e36, "1.38": 0},
		Refresh: []store.Evaluation{{ID: e36, KBVersion: "kb-2", Blockers: 1, Report: []byte(`{"findings":[]}`), EvaluatedAt: at(2)}},
		Insert:  []store.Evaluation{{Target: "1.38", Score: 70, Blockers: 1, Report: []byte(gaps), CreatedAt: at(2)}},
	}); err != nil {
		t.Fatalf("CommitEvaluations: %v", err)
	}
	if got, err := s.CurrentEvaluationSummary(ctx, cid, "1.36"); err != nil || got.NotAssessed != nil || got.KBVersion != "kb-2" {
		t.Errorf("refreshed 1.36 = (%+v, %v), want kb-2 and no NotAssessed", got, err)
	}
	if got, err := s.CurrentEvaluationSummary(ctx, cid, "1.38"); err != nil || !reflect.DeepEqual(notAssessed(t, got.NotAssessed), want) {
		t.Errorf("committed 1.38 = (%s, %v), want the report's gaps", got.NotAssessed, err)
	}

	// Only the latest snapshot's evaluations are current.
	mustSnapshot(t, s, cid, "bbb", at(3))
	if _, err := s.CurrentEvaluationSummary(ctx, cid, "1.36"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("after a new snapshot: err = %v, want ErrNotFound", err)
	}
}
