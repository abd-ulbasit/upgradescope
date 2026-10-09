package storetest

import (
	"context"
	"fmt"
	"testing"

	"github.com/abd-ulbasit/upgradescope/internal/server/store"
)

// Retention deletes in bounded batches (#263): a backlog larger than one
// batch drains over several transactions, each of at most the batch size,
// and a run cut short leaves what it committed deleted and the next run
// finishes. The stores take the batch size and a per-transaction hook
// through store.PruneTuner so the tests need not seed thousands of rows.

const (
	batchTestRows    = 5  // rows one retention transaction may delete in these tests
	batchTestBacklog = 23 // old snapshots, each with one undecided evaluation
)

// tuned returns s as a PruneTuner with the test batch size, recording the
// size of every transaction that deleted rows.
func tuned(t *testing.T, s store.Store, onBatch func(store.PruneBatch)) {
	t.Helper()
	pt, ok := s.(store.PruneTuner)
	if !ok {
		t.Fatalf("%T does not implement store.PruneTuner: the batch size cannot be set", s)
	}
	pt.TunePrune(batchTestRows, onBatch)
}

// seedBacklog stores a cluster with batchTestBacklog old snapshots, each
// with one undecided evaluation, an old decided baseline (kept, with its
// snapshot) and a current snapshot with its evaluation. It returns the
// cluster, the current snapshot and the baseline's snapshot.
func seedBacklog(t *testing.T, s store.Store) (cluster, current, baseline int64) {
	t.Helper()
	cluster = mustCluster(t, s, "backlog")
	baseline = mustSnapshot(t, s, cluster, "baseline", day(-400))
	mustEval(t, s, store.Evaluation{ClusterID: cluster, SnapshotID: baseline, Target: "1.36", Score: 70, Blockers: 1, CreatedAt: day(-400)})
	for i := 0; i < batchTestBacklog; i++ {
		sid := mustSnapshot(t, s, cluster, fmt.Sprintf("old-%d", i), day(-300+i))
		mustEval(t, s, store.Evaluation{ClusterID: cluster, SnapshotID: sid, Target: "1.37", Score: 80, CreatedAt: day(-300 + i)})
	}
	current = mustSnapshot(t, s, cluster, "current", day(-1))
	mustEval(t, s, store.Evaluation{ClusterID: cluster, SnapshotID: current, Target: "1.37", Score: 80, CreatedAt: day(-1)})
	return cluster, current, baseline
}

// testPruneDrainsABacklogInBatches: 23 old snapshots and 23 old evaluations
// are deleted in ceil(23/5) = 5 transactions each, none of more than 5
// rows, and what retention spares (the current snapshot and evaluation, the
// decided baseline and its snapshot) is untouched.
func testPruneDrainsABacklogInBatches(t *testing.T, s store.Store) {
	ctx := context.Background()
	var batches []store.PruneBatch
	tuned(t, s, func(b store.PruneBatch) { batches = append(batches, b) })
	cluster, current, baseline := seedBacklog(t, s)

	res, err := s.Prune(ctx, day(-90), nil)
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if res != (store.PruneResult{Snapshots: batchTestBacklog, Evaluations: batchTestBacklog}) {
		t.Errorf("Prune = %+v, want %d snapshots and %d evaluations", res, batchTestBacklog, batchTestBacklog)
	}
	perTable := map[string][]int64{}
	for _, b := range batches {
		if b.Rows < 1 || b.Rows > batchTestRows {
			t.Errorf("a %s transaction deleted %d rows, want 1 to %d", b.Table, b.Rows, batchTestRows)
		}
		perTable[b.Table] = append(perTable[b.Table], b.Rows)
	}
	want := []int64{5, 5, 5, 5, 3}
	for _, table := range []string{"evaluations", "snapshots"} {
		if fmt.Sprint(perTable[table]) != fmt.Sprint(want) {
			t.Errorf("%s transactions deleted %v rows, want %v", table, perTable[table], want)
		}
	}
	if latest, err := s.LatestSnapshot(ctx, cluster); err != nil || latest.ID != current {
		t.Errorf("latest snapshot = (%+v, %v), want %d kept", latest, err, current)
	}
	if e, err := s.LatestKnownEvaluation(ctx, cluster, "1.36"); err != nil || e.SnapshotID != baseline {
		t.Errorf("1.36 baseline = (%+v, %v), want it and snapshot %d kept", e, err, baseline)
	}
	if h, err := s.ScoreHistory(ctx, cluster, "1.37", 0); err != nil || len(h) != 1 || !h[0].At.Equal(day(-1)) {
		t.Errorf("1.37 history = (%+v, %v), want only the current pass", h, err)
	}
	batches = nil
	if again, err := s.Prune(ctx, day(-90), nil); err != nil || again != (store.PruneResult{}) || len(batches) != 0 {
		t.Errorf("second Prune = (%+v, %v) in %d transactions, want nothing", again, err, len(batches))
	}
}

// testPruneResumesAfterACutShortRun: a run that fails after two evaluation
// transactions (the context ends in the hook, standing in for a failed
// statement or a shutdown) returns the error and the rows it had deleted,
// those stay deleted, and the next run deletes the rest.
func testPruneResumesAfterACutShortRun(t *testing.T, s store.Store) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	evalTx := 0
	tuned(t, s, func(b store.PruneBatch) {
		if b.Table == "evaluations" {
			if evalTx++; evalTx == 2 {
				cancel()
			}
		}
	})
	cluster, _, _ := seedBacklog(t, s)

	res, err := s.Prune(ctx, day(-90), nil)
	if err == nil {
		t.Fatalf("Prune with its context ended after two transactions = %+v, nil; want an error", res)
	}
	if res.Evaluations != 2*batchTestRows || res.Snapshots != 0 {
		t.Errorf("cut-short Prune counted %+v, want the %d evaluations it deleted and no snapshot", res, 2*batchTestRows)
	}
	if h, _ := s.ScoreHistory(context.Background(), cluster, "1.37", 0); len(h) != batchTestBacklog+1-2*batchTestRows {
		t.Errorf("1.37 history after the cut-short run has %d points, want %d (the two committed batches stay deleted)", len(h), batchTestBacklog+1-2*batchTestRows)
	}

	tuned(t, s, nil)
	rest, err := s.Prune(context.Background(), day(-90), nil)
	if err != nil {
		t.Fatalf("resumed Prune: %v", err)
	}
	if rest.Evaluations != batchTestBacklog-2*batchTestRows || rest.Snapshots != batchTestBacklog {
		t.Errorf("resumed Prune = %+v, want %d evaluations and %d snapshots", rest, batchTestBacklog-2*batchTestRows, batchTestBacklog)
	}
}
