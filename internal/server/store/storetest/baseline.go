package storetest

import (
	"context"
	"errors"
	"testing"

	"github.com/abd-ulbasit/upgradescope/internal/server/store"
)

// The notification baseline (#241): what a pass computes its delta
// against is the newest decided evaluation of each target it reads, and
// the commit refuses a pass whose baseline another writer replaced in
// between, so no became-ready or new-blocker is lost or sent twice; and
// retention never prunes a baseline.

// decided is a decided (blocked) evaluation of target for a batch.
func decided(target string, blockers int) store.Evaluation {
	return store.Evaluation{Target: target, Score: 60, Blockers: blockers, Report: []byte(`{"findings":[]}`), CreatedAt: base}
}

// testCommitRefusesStaleBaseline: a push reads the cluster's latest
// snapshot and its targets' baselines, another writer commits in between
// (the hook), and the push's commit is refused with ErrConflict, writing
// nothing; computed again against what is there now, it commits.
func testCommitRefusesStaleBaseline(t *testing.T, s store.Store) {
	ctx := context.Background()
	cid := mustCluster(t, s, "prod")
	s1, _, err := s.CommitEvaluations(ctx, store.EvaluationBatch{ClusterID: cid,
		Snapshot: &store.Snapshot{ClusterID: cid, Hash: "s1", ReceivedAt: base, Inventory: []byte(`{}`)},
		Insert:   []store.Evaluation{{Target: "1.36", Score: 100, Ready: true, Report: []byte(`{}`), CreatedAt: base}}})
	if err != nil {
		t.Fatal(err)
	}
	// What the push reads before computing its deltas.
	head, err := s.LatestSnapshotHead(ctx, cid)
	if err != nil || head.ID != s1 {
		t.Fatalf("LatestSnapshotHead = (%+v, %v), want %d", head, err, s1)
	}
	e1, err := s.LatestKnownEvaluation(ctx, cid, "1.36")
	if err != nil {
		t.Fatal(err)
	}
	expect := &store.Expectation{LatestSnapshotID: s1, Baselines: map[string]int64{"1.36": e1.ID, "1.35": 0}}

	// The hook: another writer pushes a blocked snapshot in between.
	s2, _, err := s.CommitEvaluations(ctx, store.EvaluationBatch{ClusterID: cid,
		Snapshot: &store.Snapshot{ClusterID: cid, Hash: "s2", ReceivedAt: at(1), Inventory: []byte(`{}`)},
		Insert:   []store.Evaluation{decided("1.36", 1)}})
	if err != nil {
		t.Fatal(err)
	}
	push := func(expect *store.Expectation) (int64, bool, error) {
		return s.CommitEvaluations(ctx, store.EvaluationBatch{ClusterID: cid,
			Snapshot: &store.Snapshot{ClusterID: cid, Hash: "s3", ReceivedAt: at(2), Inventory: []byte(`{}`)},
			Insert:   []store.Evaluation{{Target: "1.36", Score: 100, Ready: true, Report: []byte(`{}`), CreatedAt: at(2)}},
			Outbox:   []store.OutboxMessage{outboxMsg("slack", `{"kind":"became-ready"}`, at(2))},
			Expect:   expect})
	}
	if _, _, err := push(expect); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("push against a replaced baseline: err = %v, want ErrConflict", err)
	}
	if head, _ := s.LatestSnapshotHead(ctx, cid); head.ID != s2 {
		t.Errorf("latest snapshot after the refused push = %d, want %d (nothing written)", head.ID, s2)
	}
	if msgs, _ := s.ClaimOutbox(ctx, at(10), 0, 10); len(msgs) != 0 {
		t.Errorf("outbox after the refused push = %+v, want empty", msgs)
	}

	// Only the snapshot moved on (an unknown pass is no baseline): refused.
	e2, err := s.LatestKnownEvaluation(ctx, cid, "1.36")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := push(&store.Expectation{LatestSnapshotID: s1, Baselines: map[string]int64{"1.36": e2.ID}}); !errors.Is(err, store.ErrConflict) {
		t.Errorf("push expecting an older latest snapshot: err = %v, want ErrConflict", err)
	}
	// A baseline expected absent that now exists: refused.
	if _, _, err := push(&store.Expectation{LatestSnapshotID: s2, Baselines: map[string]int64{"1.36": 0}}); !errors.Is(err, store.ErrConflict) {
		t.Errorf("push expecting no baseline: err = %v, want ErrConflict", err)
	}

	// Recomputed against what is there now, it commits.
	s3, dup, err := push(&store.Expectation{LatestSnapshotID: s2, Baselines: map[string]int64{"1.36": e2.ID, "1.35": 0}})
	if err != nil || dup {
		t.Fatalf("push with a current expectation = (%d, %v, %v), want committed", s3, dup, err)
	}
	if e, err := s.CurrentEvaluation(ctx, cid, "1.36"); err != nil || e.SnapshotID != s3 || !e.Ready {
		t.Errorf("current 1.36 = (%+v, %v), want s3's ready evaluation", e, err)
	}

	// A duplicate writes no evaluation, so it is never refused for one.
	if id, dup, err := s.CommitEvaluations(ctx, store.EvaluationBatch{ClusterID: cid,
		Snapshot: &store.Snapshot{ClusterID: cid, Hash: "s3", ReceivedAt: at(3), Inventory: []byte(`{}`)},
		Expect:   &store.Expectation{LatestSnapshotID: s1}}); err != nil || !dup || id != s3 {
		t.Errorf("duplicate with a stale expectation = (%d, %v, %v), want (%d, true, nil)", id, dup, err, s3)
	}
}

// testReevaluateRefusesStaleBaseline: a pass over the stored snapshot that
// read a target's baseline is refused when another pass added a decided
// evaluation of that target meanwhile, even one of another target (an
// upgrade baseline) the Current check does not cover.
func testReevaluateRefusesStaleBaseline(t *testing.T, s store.Store) {
	ctx := context.Background()
	cid := mustCluster(t, s, "prod")
	sid := mustSnapshot(t, s, cid, "aaa", base)
	mustEval(t, s, store.Evaluation{ClusterID: cid, SnapshotID: sid, Target: "1.35", Score: 60, Blockers: 1, CreatedAt: base})
	known, err := s.LatestKnownEvaluation(ctx, cid, "1.35")
	if err != nil {
		t.Fatal(err)
	}
	mustEval(t, s, store.Evaluation{ClusterID: cid, SnapshotID: sid, Target: "1.35", Score: 100, Ready: true, CreatedAt: at(1)})
	_, _, err = s.CommitEvaluations(ctx, store.EvaluationBatch{ClusterID: cid, SnapshotID: sid,
		Current: map[string]int64{"1.36": 0},
		Insert:  []store.Evaluation{decided("1.36", 1)},
		Expect:  &store.Expectation{Baselines: map[string]int64{"1.36": 0, "1.35": known.ID}}})
	if !errors.Is(err, store.ErrConflict) {
		t.Fatalf("pass against a replaced upgrade baseline: err = %v, want ErrConflict", err)
	}
	if _, err := s.CurrentEvaluation(ctx, cid, "1.36"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("1.36 after the refused pass: err = %v, want ErrNotFound (nothing written)", err)
	}
}

// testPruneKeepsNotificationBaseline: retention keeps each (cluster,
// target)'s newest decided evaluation, and so its snapshot, however old:
// after a stretch of unknown verdicts longer than the window, and for a
// target no longer evaluated (the previous default target, an upgrade's
// baseline), the next decided pass still has its baseline.
func testPruneKeepsNotificationBaseline(t *testing.T, s store.Store) {
	ctx := context.Background()
	ev := func(cid, sid int64, target string, blockers int, created int) int64 {
		t.Helper()
		return mustEval(t, s, store.Evaluation{ClusterID: cid, SnapshotID: sid, Target: target, Score: 80, Blockers: blockers, CreatedAt: day(created)})
	}
	a := mustCluster(t, s, "a")
	s0 := mustSnapshot(t, s, a, "a0", day(-300))
	ev(a, s0, "1.36", 1, -300)          // superseded by s1's: pruned
	old35 := ev(a, s0, "1.35", 1, -300) // the old default target's last decided: kept
	s1 := mustSnapshot(t, s, a, "a1", day(-200))
	base36 := ev(a, s1, "1.36", 1, -200) // the baseline: kept
	ev(a, s1, "1.36", 0, -199)           // unknown: pruned
	s2 := mustSnapshot(t, s, a, "a2", day(-150))
	ev(a, s2, "1.36", 0, -150) // the cluster's current state, unknown for 150 days

	res, err := s.Prune(ctx, day(-90), nil)
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if res != (store.PruneResult{Snapshots: 0, Evaluations: 2}) {
		t.Errorf("Prune = %+v, want {0 2}: s0's superseded 1.36 and s1's unknown pass", res)
	}
	if e, err := s.LatestKnownEvaluation(ctx, a, "1.36"); err != nil || e.ID != base36 || e.SnapshotID != s1 {
		t.Errorf("1.36 baseline after Prune = (%+v, %v), want evaluation %d of snapshot %d", e, err, base36, s1)
	}
	if e, err := s.LatestKnownEvaluation(ctx, a, "1.35"); err != nil || e.ID != old35 || e.SnapshotID != s0 {
		t.Errorf("1.35 baseline after Prune = (%+v, %v), want evaluation %d of snapshot %d", e, err, old35, s0)
	}
	if h, _ := s.ScoreHistory(ctx, a, "1.36", 0); len(h) != 2 || !h[0].At.Equal(day(-200)) {
		t.Errorf("1.36 history = %+v, want the kept baseline (day -200) and the current pass", h)
	}

	// A decided pass on a new snapshot moves the baseline: the old one goes.
	s3 := mustSnapshot(t, s, a, "a3", day(-1))
	ev(a, s3, "1.36", 2, -1)
	res, err = s.Prune(ctx, day(-90), nil)
	if err != nil {
		t.Fatalf("second Prune: %v", err)
	}
	// s1's baseline and s2's unknown pass, then snapshots s1 and s2.
	if res != (store.PruneResult{Snapshots: 2, Evaluations: 2}) {
		t.Errorf("second Prune = %+v, want {2 2}", res)
	}
	if e, err := s.LatestKnownEvaluation(ctx, a, "1.35"); err != nil || e.ID != old35 {
		t.Errorf("1.35 baseline after the second Prune = (%+v, %v), want %d kept", e, err, old35)
	}
}

// testPruneLimitsBaselinesToTargetsInUse: with PruneBaselines naming a
// cluster's targets in use, the baselines of its other targets age out
// like any evaluation, a snapshot only they kept with them; the targets
// named, a cluster not named, and the cluster's current state stay.
func testPruneLimitsBaselinesToTargetsInUse(t *testing.T, s store.Store) {
	ctx := context.Background()
	ev := func(cid, sid int64, target string, blockers int, created int) int64 {
		t.Helper()
		return mustEval(t, s, store.Evaluation{ClusterID: cid, SnapshotID: sid, Target: target, Score: 80, Blockers: blockers, CreatedAt: day(created)})
	}
	// a: three old default targets, each last decided in s0, and 1.36 in use.
	a := mustCluster(t, s, "a")
	a0 := mustSnapshot(t, s, a, "a0", day(-300))
	ev(a, a0, "1.34", 1, -300)
	ev(a, a0, "1.35", 1, -300)
	base36 := ev(a, a0, "1.36", 1, -300)
	a1 := mustSnapshot(t, s, a, "a1", day(-200))
	cur36 := ev(a, a1, "1.36", 0, -200) // unknown: the current state
	// b: the same history, not named: every baseline stays.
	b := mustCluster(t, s, "b")
	b0 := mustSnapshot(t, s, b, "b0", day(-300))
	ev(b, b0, "1.34", 1, -300)
	ev(b, b0, "1.35", 1, -300)
	b1 := mustSnapshot(t, s, b, "b1", day(-200))
	ev(b, b1, "1.35", 0, -200)
	// c: a snapshot kept only by a baseline of a target not in use.
	c := mustCluster(t, s, "c")
	c0 := mustSnapshot(t, s, c, "c0", day(-300))
	ev(c, c0, "1.35", 1, -300)
	c1 := mustSnapshot(t, s, c, "c1", day(-200))
	ev(c, c1, "1.36", 1, -200)

	limit := store.PruneBaselines{a: {"1.36"}, c: {"1.36"}}
	res, err := s.Prune(ctx, day(-90), limit)
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}
	// a's 1.34 and 1.35 baselines and c's 1.35, then snapshot c0.
	if res != (store.PruneResult{Snapshots: 1, Evaluations: 3}) {
		t.Errorf("Prune = %+v, want {1 3}: a's 1.34 and 1.35, c's 1.35, and c's snapshot they alone kept", res)
	}
	for _, tc := range []struct {
		cluster int64
		target  string
		want    int64 // the baseline's evaluation id; 0: none
	}{{a, "1.34", 0}, {a, "1.35", 0}, {a, "1.36", base36}, {c, "1.35", 0}} {
		e, err := s.LatestKnownEvaluation(ctx, tc.cluster, tc.target)
		switch {
		case tc.want == 0 && !errors.Is(err, store.ErrNotFound):
			t.Errorf("baseline of cluster %d target %s = (%+v, %v), want ErrNotFound: not in use", tc.cluster, tc.target, e, err)
		case tc.want != 0 && (err != nil || e.ID != tc.want):
			t.Errorf("baseline of cluster %d target %s = (%+v, %v), want evaluation %d", tc.cluster, tc.target, e, err, tc.want)
		}
	}
	for _, target := range []string{"1.34", "1.35"} {
		if _, err := s.LatestKnownEvaluation(ctx, b, target); err != nil {
			t.Errorf("baseline of cluster b (not named) target %s: %v, want kept", target, err)
		}
	}
	if e, err := s.CurrentEvaluation(ctx, a, "1.36"); err != nil || e.ID != cur36 {
		t.Errorf("a's current 1.36 = (%+v, %v), want evaluation %d", e, err, cur36)
	}
	if _, err := s.LatestSnapshot(ctx, c); err != nil {
		t.Errorf("c's latest snapshot: %v", err)
	}
	// Idempotent.
	if again, err := s.Prune(ctx, day(-90), limit); err != nil || again != (store.PruneResult{}) {
		t.Errorf("second Prune = (%+v, %v), want nothing", again, err)
	}
}

// testCarriesHold: every evaluation read says whether its report carries
// a held deprecated caller ("holdUntil"), the summary included, written by
// the store from the report on insert and on refresh.
func testCarriesHold(t *testing.T, s store.Store) {
	ctx := context.Background()
	cid := mustCluster(t, s, "prod")
	sid := mustSnapshot(t, s, cid, "aaa", base)
	held := []byte(`{"findings":[],"carriedForward":[{"category":"deprecated-api-in-use","severity":"blocker","key":"k","holdUntil":"2026-06-11T00:00:00Z"}]}`)
	mustEval(t, s, store.Evaluation{ClusterID: cid, SnapshotID: sid, Target: "1.35", Score: 60, Blockers: 1, Report: held, CreatedAt: base})
	id36 := mustEval(t, s, store.Evaluation{ClusterID: cid, SnapshotID: sid, Target: "1.36", Score: 60, Blockers: 1, Report: []byte(`{"findings":[]}`), CreatedAt: base})
	for _, c := range []struct {
		target string
		want   bool
	}{{"1.35", true}, {"1.36", false}} {
		sum, err := s.CurrentEvaluationSummary(ctx, cid, c.target)
		if err != nil || sum.CarriesHold != c.want {
			t.Errorf("summary %s CarriesHold = (%v, %v), want %v", c.target, sum.CarriesHold, err, c.want)
		}
		full, err := s.CurrentEvaluation(ctx, cid, c.target)
		if err != nil || full.CarriesHold != c.want {
			t.Errorf("current %s CarriesHold = (%v, %v), want %v", c.target, full.CarriesHold, err, c.want)
		}
	}
	if _, _, err := s.CommitEvaluations(ctx, store.EvaluationBatch{ClusterID: cid, SnapshotID: sid,
		Current: map[string]int64{"1.36": id36},
		Refresh: []store.Evaluation{{ID: id36, Target: "1.36", Blockers: 1, Report: held, EvaluatedAt: at(1)}}}); err != nil {
		t.Fatal(err)
	}
	if sum, err := s.CurrentEvaluationSummary(ctx, cid, "1.36"); err != nil || !sum.CarriesHold {
		t.Errorf("1.36 after a refresh with a held caller: CarriesHold = (%v, %v), want true", sum.CarriesHold, err)
	}
}

// testDuplicateFillsServerVersion: a duplicate push records the version
// it is judged at on a latest snapshot stored without one (before
// migration 0006), so reads stop decoding its inventory for it; a version
// already stored is never overwritten.
func testDuplicateFillsServerVersion(t *testing.T, s store.Store) {
	ctx := context.Background()
	cid := mustCluster(t, s, "prod")
	sid := mustSnapshot(t, s, cid, "aaa", base) // no server version
	dup := func(version string) {
		t.Helper()
		id, isDup, err := s.CommitEvaluations(ctx, store.EvaluationBatch{ClusterID: cid,
			Snapshot: &store.Snapshot{ClusterID: cid, Hash: "aaa", KBVersion: "kb-1", AgentVersion: "v0.2.0", ServerVersion: version, Inventory: []byte(`{}`)}})
		if err != nil || !isDup || id != sid {
			t.Fatalf("duplicate push = (%d, %v, %v), want (%d, true, nil)", id, isDup, err, sid)
		}
	}
	dup("")
	if head, _ := s.LatestSnapshotHead(ctx, cid); head.ServerVersion != "" {
		t.Errorf("after a degraded duplicate: ServerVersion = %q, want empty", head.ServerVersion)
	}
	dup("v1.34.2")
	if head, _ := s.LatestSnapshotHead(ctx, cid); head.ServerVersion != "v1.34.2" {
		t.Errorf("after a duplicate with a version: ServerVersion = %q, want v1.34.2", head.ServerVersion)
	}
	dup("v1.34.9")
	if head, _ := s.LatestSnapshotHead(ctx, cid); head.ServerVersion != "v1.34.2" {
		t.Errorf("a later duplicate overwrote the stored version: %q, want v1.34.2", head.ServerVersion)
	}
}
