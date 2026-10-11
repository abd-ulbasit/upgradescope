package storetest

import (
	"context"
	"errors"
	"testing"

	"github.com/abd-ulbasit/upgradescope/internal/server/store"
)

// An unknown verdict the server marks a baseline (Evaluation.Baseline,
// #380: its required gaps hide no finding) is a notification baseline like
// a decided one, in both stores: LatestKnownEvaluation returns it, the
// commit checks it (NT-05) and retention keeps it (NT-06). An unknown
// verdict without the mark stays skipped.

// unknownEval is an unknown-verdict evaluation (not ready, no blockers),
// marked a baseline or not.
func unknownEval(cid, sid int64, target string, baseline bool, created int) store.Evaluation {
	return store.Evaluation{ClusterID: cid, SnapshotID: sid, Target: target, Score: 95, Baseline: baseline,
		Report: []byte(`{"findings":[]}`), CreatedAt: day(created)}
}

// testLatestKnownEvaluationTakesUnknownBaseline: the newest baseline is
// the marked unknown row on top of a decided one, and an unmarked unknown
// row on top of it is skipped; the mark reads back, and a refresh writes
// it.
func testLatestKnownEvaluationTakesUnknownBaseline(t *testing.T, s store.Store) {
	ctx := context.Background()
	cid := mustCluster(t, s, "prod")
	sid := mustSnapshot(t, s, cid, "aaa", base)

	// Unmarked unknown alone: no baseline.
	mustEval(t, s, unknownEval(cid, sid, "1.38", false, -3))
	if e, err := s.LatestKnownEvaluation(ctx, cid, "1.38"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("unmarked unknown alone = (%+v, %v), want ErrNotFound", e, err)
	}
	blocked := mustEval(t, s, store.Evaluation{ClusterID: cid, SnapshotID: sid, Target: "1.38", Score: 70, Blockers: 1, CreatedAt: day(-2)})
	marked := mustEval(t, s, unknownEval(cid, sid, "1.38", true, -1))
	mustEval(t, s, unknownEval(cid, sid, "1.38", false, 0)) // a collector failure on top
	got, err := s.LatestKnownEvaluation(ctx, cid, "1.38")
	if err != nil || got.ID != marked || !got.Baseline || got.Ready || got.Blockers != 0 {
		t.Fatalf("baseline = (%+v, %v), want the marked unknown evaluation %d (not the blocked %d below it, nor the unmarked one above)", got, err, marked, blocked)
	}
	if cur, err := s.CurrentEvaluation(ctx, cid, "1.38"); err != nil || cur.Baseline {
		t.Errorf("current (unmarked) = (%+v, %v), want Baseline false on read", cur, err)
	}

	// A refresh writes the mark: cleared, the blocked row is the baseline again.
	if _, _, err := s.CommitEvaluations(ctx, store.EvaluationBatch{ClusterID: cid, SnapshotID: sid,
		Refresh: []store.Evaluation{{ID: marked, Target: "1.38", Score: 95, Report: []byte(`{"findings":[]}`), EvaluatedAt: day(0)}}}); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if got, err := s.LatestKnownEvaluation(ctx, cid, "1.38"); err != nil || got.ID != blocked {
		t.Errorf("after a refresh clearing the mark, baseline = (%+v, %v), want the blocked evaluation %d", got, err, blocked)
	}
	if _, _, err := s.CommitEvaluations(ctx, store.EvaluationBatch{ClusterID: cid, SnapshotID: sid,
		Refresh: []store.Evaluation{{ID: marked, Target: "1.38", Score: 95, Baseline: true, Report: []byte(`{"findings":[]}`), EvaluatedAt: day(0)}}}); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if got, err := s.LatestKnownEvaluation(ctx, cid, "1.38"); err != nil || got.ID != marked {
		t.Errorf("after a refresh setting the mark, baseline = (%+v, %v), want %d", got, err, marked)
	}
}

// testCommitRefusesReplacedUnknownBaseline (NT-05): a push that read the
// blocked baseline is refused once another writer commits a marked
// unknown evaluation of the target, which is the newest baseline now; an
// unmarked one replaces nothing.
func testCommitRefusesReplacedUnknownBaseline(t *testing.T, s store.Store) {
	ctx := context.Background()
	cid := mustCluster(t, s, "prod")
	commit := func(hash string, h int, e store.Evaluation, expect *store.Expectation) (int64, error) {
		t.Helper()
		e.CreatedAt = at(h)
		id, _, err := s.CommitEvaluations(ctx, store.EvaluationBatch{ClusterID: cid,
			Snapshot: &store.Snapshot{ClusterID: cid, Hash: hash, ReceivedAt: at(h), Inventory: []byte(`{}`)},
			Insert:   []store.Evaluation{e}, Expect: expect})
		return id, err
	}
	if _, err := commit("s1", 0, decided("1.38", 1), nil); err != nil {
		t.Fatal(err)
	}
	known, err := s.LatestKnownEvaluation(ctx, cid, "1.38")
	if err != nil {
		t.Fatal(err)
	}
	// Another writer: an unmarked unknown pass. The baseline did not move.
	s2, err := commit("s2", 1, store.Evaluation{Target: "1.38", Score: 95, Report: []byte(`{}`)}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if e, err := s.LatestKnownEvaluation(ctx, cid, "1.38"); err != nil || e.ID != known.ID {
		t.Fatalf("baseline after an unmarked unknown pass = (%+v, %v), want %d", e, err, known.ID)
	}
	// Then a marked one: the baseline moved.
	s3, err := commit("s3", 2, store.Evaluation{Target: "1.38", Score: 95, Baseline: true, Report: []byte(`{}`)},
		&store.Expectation{LatestSnapshotID: s2, Baselines: map[string]int64{"1.38": known.ID}})
	if err != nil {
		t.Fatalf("marked unknown pass against the current baseline: %v", err)
	}
	_, err = commit("s4", 3, decided("1.38", 1), &store.Expectation{LatestSnapshotID: s3, Baselines: map[string]int64{"1.38": known.ID}})
	if !errors.Is(err, store.ErrConflict) {
		t.Fatalf("push against the blocked baseline a marked unknown pass replaced: err = %v, want ErrConflict", err)
	}
	marked, err := s.LatestKnownEvaluation(ctx, cid, "1.38")
	if err != nil || marked.SnapshotID != s3 {
		t.Fatalf("baseline = (%+v, %v), want s3's marked unknown evaluation", marked, err)
	}
	if _, err := commit("s4", 3, decided("1.38", 1), &store.Expectation{LatestSnapshotID: s3, Baselines: map[string]int64{"1.38": marked.ID}}); err != nil {
		t.Errorf("push against the marked unknown baseline: %v", err)
	}
}

// testPruneKeepsUnknownBaseline (NT-06): retention keeps a pair's newest
// baseline when it is a marked unknown evaluation, with its snapshot,
// however old, and prunes the blocked one below it and the unmarked
// unknown ones as any history.
func testPruneKeepsUnknownBaseline(t *testing.T, s store.Store) {
	ctx := context.Background()
	a := mustCluster(t, s, "a")
	s0 := mustSnapshot(t, s, a, "a0", day(-300))
	mustEval(t, s, store.Evaluation{ClusterID: a, SnapshotID: s0, Target: "1.38", Score: 70, Blockers: 1, CreatedAt: day(-300)})
	s1 := mustSnapshot(t, s, a, "a1", day(-200))
	kept := mustEval(t, s, unknownEval(a, s1, "1.38", true, -200))
	mustEval(t, s, unknownEval(a, s1, "1.38", false, -199)) // pruned
	s2 := mustSnapshot(t, s, a, "a2", day(-150))
	mustEval(t, s, unknownEval(a, s2, "1.38", false, -150)) // the current state

	res, err := s.Prune(ctx, day(-90), store.PruneBaselines{a: {"1.38"}})
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}
	// s0's blocked evaluation and s1's unmarked one, then snapshot s0.
	if res != (store.PruneResult{Snapshots: 1, Evaluations: 2}) {
		t.Errorf("Prune = %+v, want {1 2}", res)
	}
	if e, err := s.LatestKnownEvaluation(ctx, a, "1.38"); err != nil || e.ID != kept || e.SnapshotID != s1 {
		t.Errorf("baseline after Prune = (%+v, %v), want the marked unknown evaluation %d of snapshot %d", e, err, kept, s1)
	}
}
