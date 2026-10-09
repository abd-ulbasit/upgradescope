package storetest

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/abd-ulbasit/upgradescope/internal/server/store"
)

// Conformance for the cluster lifecycle: lookup and rename, registration
// inside the ingest transaction, and retention.

func day(n int) time.Time { return base.Add(time.Duration(n) * 24 * time.Hour) }

// testClusterByName pins the read-only lookup ingest uses before it
// decides anything: no row is created for an unknown name.
func testClusterByName(t *testing.T, s store.Store) {
	ctx := context.Background()
	cid := mustCluster(t, s, "prod")
	got, err := s.ClusterByName(ctx, "prod")
	if err != nil || got.ID != cid || got.ClusterUID != "uid-prod" || !got.LastSeen.Equal(base) {
		t.Errorf("ClusterByName(prod) = (%+v, %v), want id %d uid-prod", got, err, cid)
	}
	if _, err := s.ClusterByName(ctx, "nope"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("ClusterByName(unknown) err = %v, want ErrNotFound", err)
	}
	if list, _ := s.ListClusters(ctx); len(list) != 1 {
		t.Errorf("lookup of an unknown name created a cluster: %+v", list)
	}
}

// testRenameCluster pins rename: the row, its history and its ingest
// tokens move to the new name together; a taken name is refused.
func testRenameCluster(t *testing.T, s store.Store) {
	ctx := context.Background()
	cid := mustCluster(t, s, "prod")
	sid := mustSnapshot(t, s, cid, "aaa", base)
	if _, err := s.CreateToken(ctx, "prod", "tok-prod-rename"); err != nil {
		t.Fatal(err)
	}
	mustCluster(t, s, "dev")

	if err := s.RenameCluster(ctx, "prod", "prod-eu"); err != nil {
		t.Fatalf("RenameCluster: %v", err)
	}
	got, err := s.GetCluster(ctx, cid)
	if err != nil || got.Name != "prod-eu" || got.ClusterUID != "uid-prod" {
		t.Errorf("after rename = (%+v, %v), want prod-eu keeping uid-prod", got, err)
	}
	if latest, err := s.LatestSnapshot(ctx, cid); err != nil || latest.ID != sid {
		t.Errorf("history after rename = (%+v, %v), want snapshot %d kept", latest, err, sid)
	}
	if name, ok, err := s.ValidToken(ctx, "tok-prod-rename"); err != nil || !ok || name != "prod-eu" {
		t.Errorf("token after rename = (%q, %v, %v), want it bound to prod-eu", name, ok, err)
	}
	if _, err := s.ClusterByName(ctx, "prod"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("old name still resolves: %v", err)
	}

	if err := s.RenameCluster(ctx, "prod-eu", "dev"); !errors.Is(err, store.ErrClusterNameTaken) {
		t.Errorf("rename onto an existing name: err = %v, want ErrClusterNameTaken", err)
	}
	if got, _ := s.GetCluster(ctx, cid); got.Name != "prod-eu" {
		t.Errorf("refused rename changed the name to %q", got.Name)
	}
	// Renaming a cluster onto its own name is a no-op in every store, not
	// a conflict with itself.
	if err := s.RenameCluster(ctx, "prod-eu", "prod-eu"); err != nil {
		t.Errorf("rename onto its own name: err = %v, want nil", err)
	}
	if got, _ := s.GetCluster(ctx, cid); got.Name != "prod-eu" {
		t.Errorf("no-op rename changed the name to %q", got.Name)
	}
	if err := s.RenameCluster(ctx, "nope", "nope"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("no-op rename of an unknown name: err = %v, want ErrNotFound", err)
	}
	if err := s.RenameCluster(ctx, "nope", "x"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("rename unknown: err = %v, want ErrNotFound", err)
	}
	if err := s.RenameCluster(ctx, "prod-eu", ""); err == nil {
		t.Error("rename to an empty name succeeded")
	}
}

// testCommitRegistersCluster pins cluster registration inside the ingest
// transaction: a new name becomes a cluster together with its first
// snapshot, and every row of the batch gets its id.
func testCommitRegistersCluster(t *testing.T, s store.Store) {
	ctx := context.Background()
	sid, dup, err := s.CommitEvaluations(ctx, store.EvaluationBatch{
		Cluster:  &store.Cluster{Name: "new", ClusterUID: "uid-new", LastSeen: base},
		Snapshot: &store.Snapshot{Hash: "aaa", ReceivedAt: base, Inventory: []byte(`{}`)},
		Insert:   []store.Evaluation{{Target: "1.36", Score: 90, Ready: true, CreatedAt: base}},
		Outbox:   []store.OutboxMessage{outboxMsg("slack", `{}`, base)},
	})
	if err != nil || dup {
		t.Fatalf("CommitEvaluations = (%d, %v, %v), want a new snapshot", sid, dup, err)
	}
	c, err := s.ClusterByName(ctx, "new")
	if err != nil || c.ClusterUID != "uid-new" || !c.FirstSeen.Equal(base) || !c.LastSeen.Equal(base) {
		t.Fatalf("registered cluster = (%+v, %v), want uid-new first/last seen %v", c, err, base)
	}
	if latest, err := s.LatestSnapshot(ctx, c.ID); err != nil || latest.ID != sid || latest.ClusterID != c.ID {
		t.Errorf("snapshot = (%+v, %v), want %d on cluster %d", latest, err, sid, c.ID)
	}
	if e, err := s.CurrentEvaluation(ctx, c.ID, "1.36"); err != nil || e.ClusterID != c.ID {
		t.Errorf("evaluation = (%+v, %v), want it on cluster %d", e, err, c.ID)
	}
	if msgs, _ := s.ClaimOutbox(ctx, at(1), time.Minute, 10); len(msgs) != 1 || msgs[0].ClusterID != c.ID {
		t.Errorf("outbox = %+v, want one message on cluster %d", msgs, c.ID)
	}

	// The same cluster pushing again is touched, not duplicated.
	if _, _, err := s.CommitEvaluations(ctx, store.EvaluationBatch{
		Cluster:  &store.Cluster{Name: "new", ClusterUID: "uid-new", LastSeen: at(2)},
		Snapshot: &store.Snapshot{Hash: "bbb", ReceivedAt: at(2), Inventory: []byte(`{}`)},
	}); err != nil {
		t.Fatalf("second commit: %v", err)
	}
	if list, _ := s.ListClusters(ctx); len(list) != 1 || !list[0].LastSeen.Equal(at(2)) || !list[0].FirstSeen.Equal(base) {
		t.Errorf("clusters after second push = %+v, want one, last seen %v, first seen %v", list, at(2), base)
	}

	// Another UID under the name: a conflict that writes nothing.
	_, _, err = s.CommitEvaluations(ctx, store.EvaluationBatch{
		Cluster:  &store.Cluster{Name: "new", ClusterUID: "uid-other", LastSeen: at(3)},
		Snapshot: &store.Snapshot{Hash: "ccc", ReceivedAt: at(3), Inventory: []byte(`{}`)},
	})
	var conflict *store.ClusterUIDConflictError
	if !errors.As(err, &conflict) || conflict.StoredUID != "uid-new" || conflict.PushedUID != "uid-other" {
		t.Fatalf("conflicting commit: err = %v, want *ClusterUIDConflictError", err)
	}
	if latest, _ := s.LatestSnapshot(ctx, c.ID); latest.Hash != "bbb" {
		t.Errorf("conflicting commit wrote snapshot %q", latest.Hash)
	}
	if got, _ := s.GetCluster(ctx, c.ID); !got.LastSeen.Equal(at(2)) {
		t.Errorf("conflicting commit moved last seen to %v", got.LastSeen)
	}
}

// testCommitFailureLeavesNoCluster: a failed ingest commit leaves no
// cluster row behind and does not move an existing cluster's last seen.
func testCommitFailureLeavesNoCluster(t *testing.T, s store.Store) {
	ctx := context.Background()
	failing := func(c store.Cluster) store.EvaluationBatch {
		return store.EvaluationBatch{
			Cluster:  &c,
			Snapshot: &store.Snapshot{Hash: "aaa", ReceivedAt: at(1), Inventory: []byte(`{}`)},
			Refresh:  []store.Evaluation{{ID: 999999, Target: "1.37", EvaluatedAt: at(1)}}, // no such row
		}
	}
	if _, _, err := s.CommitEvaluations(ctx, failing(store.Cluster{Name: "orphan", ClusterUID: "u", LastSeen: at(1)})); err == nil {
		t.Fatal("commit with a refresh of a missing row succeeded, want error")
	}
	if c, err := s.ClusterByName(ctx, "orphan"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("failed commit left cluster %+v (err %v), want none", c, err)
	}
	cid := mustCluster(t, s, "prod")
	if _, _, err := s.CommitEvaluations(ctx, failing(store.Cluster{Name: "prod", ClusterUID: "uid-prod", LastSeen: at(1)})); err == nil {
		t.Fatal("second failing commit succeeded")
	}
	if got, _ := s.GetCluster(ctx, cid); !got.LastSeen.Equal(base) {
		t.Errorf("failed commit moved last seen to %v, want %v", got.LastSeen, base)
	}
}

// testDuplicateRecordsEnvelope: a push whose inventory is unchanged still
// says something — the agent is alive, and may have been upgraded. It
// bumps last seen and records the agent and KB versions on the latest
// snapshot, without adding a snapshot or evaluations.
func testDuplicateRecordsEnvelope(t *testing.T, s store.Store) {
	ctx := context.Background()
	cid := mustCluster(t, s, "prod")
	sid := mustSnapshot(t, s, cid, "aaa", base)
	got, dup, err := s.CommitEvaluations(ctx, store.EvaluationBatch{
		Cluster:  &store.Cluster{Name: "prod", ClusterUID: "uid-prod", LastSeen: at(5)},
		Snapshot: &store.Snapshot{Hash: "aaa", KBVersion: "kb-2", AgentVersion: "v0.3.0", ReceivedAt: at(5), Inventory: []byte(`{}`)},
	})
	if err != nil || !dup || got != sid {
		t.Fatalf("duplicate commit = (%d, %v, %v), want (%d, true, nil)", got, dup, err, sid)
	}
	latest, err := s.LatestSnapshot(ctx, cid)
	if err != nil {
		t.Fatal(err)
	}
	if latest.ID != sid || latest.AgentVersion != "v0.3.0" || latest.KBVersion != "kb-2" {
		t.Errorf("latest = id %d agent %q kb %q, want %d v0.3.0 kb-2", latest.ID, latest.AgentVersion, latest.KBVersion, sid)
	}
	if !latest.ReceivedAt.Equal(base) {
		t.Errorf("ReceivedAt = %v, want %v (when the inventory first arrived)", latest.ReceivedAt, base)
	}
	if c, _ := s.GetCluster(ctx, cid); !c.LastSeen.Equal(at(5)) {
		t.Errorf("last seen = %v, want %v", c.LastSeen, at(5))
	}
}

// testPrune pins retention: rows older than the cutoff go, except each
// cluster's latest snapshot and its evaluations (the cluster's current
// state, however old), and an older snapshot stays while any of its
// evaluations is inside the window.
func testPrune(t *testing.T, s store.Store) {
	ctx := context.Background()
	ev := func(cid, sid int64, target string, created time.Time) int64 {
		t.Helper()
		return mustEval(t, s, store.Evaluation{ClusterID: cid, SnapshotID: sid, Target: target, Score: 80, Blockers: 1, CreatedAt: created})
	}
	a := mustCluster(t, s, "a")
	s1 := mustSnapshot(t, s, a, "a1", day(-200))
	ev(a, s1, "1.36", day(-200))
	ev(a, s1, "1.37", day(-200))
	s2 := mustSnapshot(t, s, a, "a2", day(-150))
	ev(a, s2, "1.36", day(-150))
	ev(a, s2, "1.36", day(-10)) // re-evaluated inside the window: s2 stays
	s3 := mustSnapshot(t, s, a, "a3", day(-5))
	ev(a, s3, "1.36", day(-5))

	b := mustCluster(t, s, "b") // silent for a year: its state is old but current
	b1 := mustSnapshot(t, s, b, "b1", day(-365))
	ev(b, b1, "1.36", day(-365))

	res, err := s.Prune(ctx, day(-90), nil)
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}
	// a1's 1.37 evaluation is that target's newest decided one, a
	// notification baseline (testPruneKeepsNotificationBaseline): it stays,
	// and a1 with it.
	if res.Snapshots != 0 || res.Evaluations != 2 {
		t.Errorf("Prune = %+v, want no snapshot and 2 evaluations (a1's 1.36, a2's day -150)", res)
	}
	hist, _ := s.ScoreHistory(ctx, a, "1.36", 0)
	if len(hist) != 2 || !hist[0].At.Equal(day(-10)) || !hist[1].At.Equal(day(-5)) {
		t.Errorf("a 1.36 history = %+v, want days -10 and -5", hist)
	}
	if h37, _ := s.ScoreHistory(ctx, a, "1.37", 0); len(h37) != 1 || !h37[0].At.Equal(day(-200)) {
		t.Errorf("a 1.37 history = %+v, want its last decided evaluation (day -200) kept", h37)
	}
	if latest, err := s.LatestSnapshot(ctx, a); err != nil || latest.ID != s3 {
		t.Errorf("a latest = (%+v, %v), want %d", latest, err, s3)
	}
	if e, err := s.CurrentEvaluation(ctx, b, "1.36"); err != nil || e.SnapshotID != b1 {
		t.Errorf("b current = (%+v, %v), want its year-old evaluation kept", e, err)
	}
	again, err := s.Prune(ctx, day(-90), nil)
	if err != nil || again.Snapshots != 0 || again.Evaluations != 0 {
		t.Errorf("second Prune = (%+v, %v), want nothing left to prune", again, err)
	}
}
