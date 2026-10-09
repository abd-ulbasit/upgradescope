// Package storetest pins the behavioral contract of store.Store.
//
// Any implementation (SQLite in P2, Postgres in P3) must pass
// RunStoreConformance. The suite touches only the exported interface —
// nothing driver-specific (pragmas, raw SQL, file layout) is asserted here;
// implementation packages pin their own storage representation.
package storetest

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/abd-ulbasit/upgradescope/internal/server/store"
)

// NewStoreFunc returns a fresh, empty Store for one subtest. Implementations
// must register cleanup on t (t.Cleanup / t.TempDir); the suite never calls
// Close except where Close semantics are themselves under test.
type NewStoreFunc func(t *testing.T) store.Store

var base = time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC)

func at(h int) time.Time { return base.Add(time.Duration(h) * time.Hour) }

// RunStoreConformance runs the full behavioral suite against implementations
// produced by newStore. Each subtest gets its own fresh store.
func RunStoreConformance(t *testing.T, newStore NewStoreFunc) {
	t.Run("UpsertClusterInsertThenUpdate", func(t *testing.T) { testUpsertCluster(t, newStore(t)) })
	t.Run("UpsertClusterZeroTimesDefault", func(t *testing.T) { testZeroTimes(t, newStore(t)) })
	t.Run("UpsertClusterRejectsUIDChange", func(t *testing.T) { testClusterUIDConflict(t, newStore(t)) })
	t.Run("ClusterUIDAlternatingPushesNeverInterleave", func(t *testing.T) { testClusterUIDAlternating(t, newStore(t)) })
	t.Run("EmptyUIDPushNeverInterleaves", func(t *testing.T) { testEmptyUIDNeverInterleaves(t, newStore(t)) })
	t.Run("DeleteCluster", func(t *testing.T) { testDeleteCluster(t, newStore(t)) })
	t.Run("ClusterByName", func(t *testing.T) { testClusterByName(t, newStore(t)) })
	t.Run("RenameCluster", func(t *testing.T) { testRenameCluster(t, newStore(t)) })
	t.Run("CommitEvaluationsRegistersCluster", func(t *testing.T) { testCommitRegistersCluster(t, newStore(t)) })
	t.Run("CommitFailureLeavesNoCluster", func(t *testing.T) { testCommitFailureLeavesNoCluster(t, newStore(t)) })
	t.Run("CommitRechecksIngestToken", func(t *testing.T) { testCommitRechecksIngestToken(t, newStore(t)) })
	t.Run("CommitRefusesChangedCluster", func(t *testing.T) { testCommitRefusesChangedCluster(t, newStore(t)) })
	t.Run("DuplicateRecordsEnvelope", func(t *testing.T) { testDuplicateRecordsEnvelope(t, newStore(t)) })
	t.Run("PruneKeepsLatestSnapshot", func(t *testing.T) { testPrune(t, newStore(t)) })
	t.Run("SnapshotDedup", func(t *testing.T) { testSnapshotDedup(t, newStore(t)) })
	t.Run("SnapshotDedupClusterScoped", func(t *testing.T) { testSnapshotDedupClusterScoped(t, newStore(t)) })
	t.Run("ConcurrentIngestSerializes", func(t *testing.T) { testConcurrentIngest(t, newStore(t)) })
	t.Run("LatestSnapshotRoundTrip", func(t *testing.T) { testLatestSnapshot(t, newStore(t)) })
	t.Run("SnapshotServerVersionAndBytesRoundTrip", func(t *testing.T) { testSnapshotServerVersion(t, newStore(t)) })
	t.Run("LatestSnapshotHeads", func(t *testing.T) { testLatestSnapshotHeads(t, newStore(t)) })
	t.Run("EvaluationsLatestPerTarget", func(t *testing.T) { testEvaluations(t, newStore(t)) })
	t.Run("LatestEvaluationTieBreakHigherID", func(t *testing.T) { testLatestEvaluationTieBreak(t, newStore(t)) })
	t.Run("ScoreHistoryOldestFirstLimitNewest", func(t *testing.T) { testScoreHistory(t, newStore(t)) })
	t.Run("EvaluationFreshnessFields", func(t *testing.T) { testEvaluationFreshnessFields(t, newStore(t)) })
	t.Run("CurrentEvaluationIsLatestSnapshotOnly", func(t *testing.T) { testCurrentEvaluation(t, newStore(t)) })
	t.Run("CurrentEvaluationSummary", func(t *testing.T) { testEvaluationSummary(t, newStore(t)) })
	t.Run("CurrentEvaluationIgnoresCreatedAt", func(t *testing.T) { testCurrentEvaluationIgnoresCreatedAt(t, newStore(t)) })
	t.Run("LatestKnownEvaluationSkipsUnknown", func(t *testing.T) { testLatestKnownEvaluation(t, newStore(t)) })
	t.Run("CommitEvaluationsNewSnapshot", func(t *testing.T) { testCommitNewSnapshot(t, newStore(t)) })
	t.Run("CommitEvaluationsIsAtomic", func(t *testing.T) { testCommitIsAtomic(t, newStore(t)) })
	t.Run("CommitEvaluationsExistingSnapshot", func(t *testing.T) { testCommitExistingSnapshot(t, newStore(t)) })
	t.Run("Outbox", func(t *testing.T) { testOutbox(t, newStore(t)) })
	t.Run("OutboxDefer", func(t *testing.T) { testOutboxDefer(t, newStore(t)) })
	t.Run("NotFound", func(t *testing.T) { testNotFound(t, newStore(t)) })
	t.Run("TokensCreateValidateRevoke", func(t *testing.T) { testTokens(t, newStore(t)) })
	t.Run("TokensMultiplePerCluster", func(t *testing.T) { testTokensMultiplePerCluster(t, newStore(t)) })
	t.Run("TokensDuplicateRejected", func(t *testing.T) { testTokensDuplicate(t, newStore(t)) })
	t.Run("TokensRevokeByID", func(t *testing.T) { testTokensRevokeByID(t, newStore(t)) })
	t.Run("TokensList", func(t *testing.T) { testTokensList(t, newStore(t)) })
	t.Run("ReadTokens", func(t *testing.T) { testReadTokens(t, newStore(t)) })
	t.Run("ClustersOfTeams", func(t *testing.T) { testClustersOfTeams(t, newStore(t)) })
	t.Run("Close", func(t *testing.T) { testClose(t, newStore(t)) })
}

func mustCluster(t *testing.T, s store.Store, name string) int64 {
	t.Helper()
	id, err := s.UpsertCluster(context.Background(),
		store.Cluster{Name: name, ClusterUID: "uid-" + name, FirstSeen: base, LastSeen: base})
	if err != nil {
		t.Fatalf("UpsertCluster(%s): %v", name, err)
	}
	return id
}

func mustSnapshot(t *testing.T, s store.Store, clusterID int64, hash string, received time.Time) int64 {
	t.Helper()
	id, dup, err := s.InsertSnapshot(context.Background(), store.Snapshot{
		ClusterID: clusterID, Hash: hash, KBVersion: "kb-1", AgentVersion: "v0.2.0",
		ReceivedAt: received, Inventory: []byte(`{"hash":"` + hash + `"}`),
	})
	if err != nil {
		t.Fatalf("InsertSnapshot(%s): %v", hash, err)
	}
	if dup {
		t.Fatalf("InsertSnapshot(%s): unexpected duplicate", hash)
	}
	return id
}

func testUpsertCluster(t *testing.T, s store.Store) {
	ctx := context.Background()
	id, err := s.UpsertCluster(ctx, store.Cluster{Name: "prod-eu-1", ClusterUID: "uid-1", FirstSeen: base, LastSeen: base})
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	if id <= 0 {
		t.Fatalf("insert returned id %d, want > 0", id)
	}
	id2, err := s.UpsertCluster(ctx, store.Cluster{Name: "prod-eu-1", ClusterUID: "uid-1", FirstSeen: at(1), LastSeen: at(1)})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if id2 != id {
		t.Errorf("upsert by existing name returned id %d, want %d", id2, id)
	}
	got, err := s.GetCluster(ctx, id)
	if err != nil {
		t.Fatalf("GetCluster: %v", err)
	}
	if !got.FirstSeen.Equal(base) {
		t.Errorf("FirstSeen = %v, want %v (must not move on update)", got.FirstSeen, base)
	}
	if !got.LastSeen.Equal(at(1)) {
		t.Errorf("LastSeen = %v, want %v (must bump on update)", got.LastSeen, at(1))
	}
	if got.ClusterUID != "uid-1" {
		t.Errorf("ClusterUID = %q, want uid-1", got.ClusterUID)
	}
	if got.FirstSeen.Location() != time.UTC || got.LastSeen.Location() != time.UTC {
		t.Errorf("times must come back UTC, got %v / %v", got.FirstSeen.Location(), got.LastSeen.Location())
	}
	if _, err := s.UpsertCluster(ctx, store.Cluster{Name: "dev-1", FirstSeen: base, LastSeen: base}); err != nil {
		t.Fatalf("second cluster: %v", err)
	}
	list, err := s.ListClusters(ctx)
	if err != nil {
		t.Fatalf("ListClusters: %v", err)
	}
	if len(list) != 2 || list[0].Name != "dev-1" || list[1].Name != "prod-eu-1" {
		t.Errorf("ListClusters = %+v, want [dev-1 prod-eu-1] ascending by name", list)
	}
}

func testZeroTimes(t *testing.T, s store.Store) {
	ctx := context.Background()
	before := time.Now().UTC().Add(-time.Minute)
	id, err := s.UpsertCluster(ctx, store.Cluster{Name: "zero-times"})
	if err != nil {
		t.Fatalf("UpsertCluster: %v", err)
	}
	after := time.Now().UTC().Add(time.Minute)
	got, err := s.GetCluster(ctx, id)
	if err != nil {
		t.Fatalf("GetCluster: %v", err)
	}
	for name, ts := range map[string]time.Time{"FirstSeen": got.FirstSeen, "LastSeen": got.LastSeen} {
		if ts.Before(before) || ts.After(after) {
			t.Errorf("%s = %v, want defaulted to now (within [%v, %v])", name, ts, before, after)
		}
	}
}

// testClusterUIDConflict pins cluster identity: a name stays bound to the
// first non-empty cluster UID pushed under it. A different UID is refused
// (and changes nothing), an empty stored UID is adopted, and an empty
// incoming UID keeps the stored one.
func testClusterUIDConflict(t *testing.T, s store.Store) {
	ctx := context.Background()
	id, err := s.UpsertCluster(ctx, store.Cluster{Name: "prod", ClusterUID: "uid-a", FirstSeen: base, LastSeen: base})
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	_, err = s.UpsertCluster(ctx, store.Cluster{Name: "prod", ClusterUID: "uid-b", LastSeen: at(1)})
	if !errors.Is(err, store.ErrClusterUIDConflict) {
		t.Fatalf("upsert with another UID: err = %v, want ErrClusterUIDConflict", err)
	}
	var conflict *store.ClusterUIDConflictError
	if !errors.As(err, &conflict) || conflict.Name != "prod" || conflict.StoredUID != "uid-a" || conflict.PushedUID != "uid-b" {
		t.Errorf("conflict error = %#v, want {prod uid-a uid-b}", conflict)
	}
	got, err := s.GetCluster(ctx, id)
	if err != nil {
		t.Fatalf("GetCluster: %v", err)
	}
	if got.ClusterUID != "uid-a" || !got.LastSeen.Equal(base) {
		t.Errorf("after refused upsert: uid %q last seen %v, want uid-a / %v (unchanged)", got.ClusterUID, got.LastSeen, base)
	}

	// An empty incoming UID is no wildcard: a push that cannot say which
	// cluster it is may come from another cluster entirely, so it is
	// refused too (and changes nothing).
	_, err = s.UpsertCluster(ctx, store.Cluster{Name: "prod", LastSeen: at(2)})
	if !errors.As(err, &conflict) || conflict.StoredUID != "uid-a" || conflict.PushedUID != "" {
		t.Fatalf("upsert with empty UID: err = %v, want a conflict naming uid-a and an empty pushed UID", err)
	}
	if got, _ := s.GetCluster(ctx, id); got.ClusterUID != "uid-a" || !got.LastSeen.Equal(base) {
		t.Errorf("after empty-UID upsert: uid %q last seen %v, want uid-a / %v (unchanged)", got.ClusterUID, got.LastSeen, base)
	}

	// A cluster first registered without a UID adopts the first one pushed.
	anon, err := s.UpsertCluster(ctx, store.Cluster{Name: "anon", FirstSeen: base, LastSeen: base})
	if err != nil {
		t.Fatalf("insert anon: %v", err)
	}
	if _, err := s.UpsertCluster(ctx, store.Cluster{Name: "anon", ClusterUID: "uid-c", LastSeen: at(1)}); err != nil {
		t.Fatalf("adopt UID: %v", err)
	}
	if got, _ := s.GetCluster(ctx, anon); got.ClusterUID != "uid-c" {
		t.Errorf("anon uid = %q, want uid-c (adopted)", got.ClusterUID)
	}
	// Until a UID is bound, UID-less pushes of one name are one cluster:
	// nothing tells them apart.
	if _, err := s.UpsertCluster(ctx, store.Cluster{Name: "nouid", LastSeen: base}); err != nil {
		t.Fatalf("insert nouid: %v", err)
	}
	if _, err := s.UpsertCluster(ctx, store.Cluster{Name: "nouid", LastSeen: at(1)}); err != nil {
		t.Errorf("second UID-less upsert of an unbound name: %v", err)
	}
}

// testEmptyUIDNeverInterleaves replays a cluster that registered its name
// with a UID and a second, UID-less pusher of the same name (an agent that
// cannot read kube-system, or a misconfigured one). The UID-less pushes
// are refused, so they never land in the bound cluster's history.
func testEmptyUIDNeverInterleaves(t *testing.T, s store.Store) {
	ctx := context.Background()
	var accepted, refused int
	for i := range 6 {
		uid, hash := "uid-x", fmt.Sprintf("x-%d", i)
		if i%2 == 1 {
			uid, hash = "", fmt.Sprintf("anon-%d", i)
		}
		cid, err := s.UpsertCluster(ctx, store.Cluster{Name: "prod", ClusterUID: uid, LastSeen: at(i)})
		if errors.Is(err, store.ErrClusterUIDConflict) {
			refused++
			continue
		}
		if err != nil {
			t.Fatalf("push %d: %v", i, err)
		}
		mustSnapshot(t, s, cid, hash, at(i))
		accepted++
	}
	if accepted != 3 || refused != 3 {
		t.Fatalf("accepted %d / refused %d pushes, want 3 / 3 (every UID-less push refused)", accepted, refused)
	}
	clusters, err := s.ListClusters(ctx)
	if err != nil || len(clusters) != 1 {
		t.Fatalf("ListClusters = (%+v, %v), want one cluster", clusters, err)
	}
	latest, err := s.LatestSnapshot(ctx, clusters[0].ID)
	if err != nil || latest.Hash != "x-4" {
		t.Errorf("latest snapshot = (%q, %v), want x-4 (UID-less pushes never write)", latest.Hash, err)
	}
}

// testClusterUIDAlternating replays two clusters that share one name,
// pushing alternately the way two agents would. Only the cluster that
// registered the name may write: the other's snapshots never land in the
// shared history.
func testClusterUIDAlternating(t *testing.T, s store.Store) {
	ctx := context.Background()
	var accepted, refused int
	var lastAccepted string
	for i := range 6 {
		uid, hash := "uid-a", fmt.Sprintf("a-%d", i)
		if i%2 == 1 {
			uid, hash = "uid-b", fmt.Sprintf("b-%d", i)
		}
		cid, err := s.UpsertCluster(ctx, store.Cluster{Name: "prod", ClusterUID: uid, LastSeen: at(i)})
		if errors.Is(err, store.ErrClusterUIDConflict) {
			refused++
			continue
		}
		if err != nil {
			t.Fatalf("push %d: %v", i, err)
		}
		mustSnapshot(t, s, cid, hash, at(i))
		accepted++
		lastAccepted = hash
	}
	if accepted != 3 || refused != 3 {
		t.Fatalf("accepted %d / refused %d pushes, want 3 / 3", accepted, refused)
	}
	clusters, err := s.ListClusters(ctx)
	if err != nil {
		t.Fatalf("ListClusters: %v", err)
	}
	if len(clusters) != 1 || clusters[0].ClusterUID != "uid-a" {
		t.Fatalf("clusters = %+v, want one cluster bound to uid-a", clusters)
	}
	latest, err := s.LatestSnapshot(ctx, clusters[0].ID)
	if err != nil {
		t.Fatalf("LatestSnapshot: %v", err)
	}
	if latest.Hash != lastAccepted || !strings.HasPrefix(latest.Hash, "a-") {
		t.Errorf("latest snapshot hash = %q, want %q (uid-b must never write)", latest.Hash, lastAccepted)
	}
}

// testDeleteCluster pins decommissioning, also the escape hatch for a
// rebuilt cluster: DeleteCluster drops the cluster with its snapshots,
// evaluations, ingest tokens and queued notifications, and the name is
// then free for a new UID. Another cluster's rows are untouched.
func testDeleteCluster(t *testing.T, s store.Store) {
	ctx := context.Background()
	cid := mustCluster(t, s, "prod")
	sid, _, err := s.CommitEvaluations(ctx, store.EvaluationBatch{
		ClusterID: cid,
		Snapshot:  &store.Snapshot{ClusterID: cid, Hash: "aaa", ReceivedAt: base, Inventory: []byte(`{}`)},
		Outbox:    []store.OutboxMessage{outboxMsg("slack", `{"c":"prod"}`, base)},
	})
	if err != nil {
		t.Fatalf("CommitEvaluations(prod): %v", err)
	}
	if _, err := s.InsertEvaluation(ctx, store.Evaluation{ClusterID: cid, SnapshotID: sid, Target: "1.36", Score: 90, CreatedAt: base}); err != nil {
		t.Fatalf("InsertEvaluation: %v", err)
	}
	if _, err := s.CreateToken(ctx, "prod", "tok-prod-delete"); err != nil {
		t.Fatalf("CreateToken: %v", err)
	}
	other := mustCluster(t, s, "dev")
	if _, _, err := s.CommitEvaluations(ctx, store.EvaluationBatch{
		ClusterID: other,
		Snapshot:  &store.Snapshot{ClusterID: other, Hash: "ddd", ReceivedAt: base, Inventory: []byte(`{}`)},
		Outbox:    []store.OutboxMessage{outboxMsg("slack", `{"c":"dev"}`, base)},
	}); err != nil {
		t.Fatalf("CommitEvaluations(dev): %v", err)
	}
	if _, err := s.CreateToken(ctx, "dev", "tok-dev-keep"); err != nil {
		t.Fatalf("CreateToken(dev): %v", err)
	}

	if err := s.DeleteCluster(ctx, "prod"); err != nil {
		t.Fatalf("DeleteCluster: %v", err)
	}
	if _, err := s.GetCluster(ctx, cid); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("GetCluster after delete: err = %v, want ErrNotFound", err)
	}
	if _, err := s.LatestSnapshot(ctx, cid); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("LatestSnapshot after delete: err = %v, want ErrNotFound", err)
	}
	if _, err := s.LatestEvaluation(ctx, cid, "1.36"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("LatestEvaluation after delete: err = %v, want ErrNotFound", err)
	}
	if _, err := s.LatestSnapshot(ctx, other); err != nil {
		t.Errorf("other cluster's snapshot: %v (must survive)", err)
	}
	if name, ok, err := s.ValidToken(ctx, "tok-prod-delete"); err != nil || ok {
		t.Errorf("ValidToken after delete = (%q, %v, %v), want the token gone", name, ok, err)
	}
	if toks, err := s.ListTokens(ctx, "prod"); err != nil || len(toks) != 0 {
		t.Errorf("ListTokens(prod) after delete = (%+v, %v), want none", toks, err)
	}
	if name, ok, err := s.ValidToken(ctx, "tok-dev-keep"); err != nil || !ok || name != "dev" {
		t.Errorf("dev token after deleting prod = (%q, %v, %v), want it kept", name, ok, err)
	}
	msgs, err := s.ClaimOutbox(ctx, at(1), time.Minute, 10)
	if err != nil || len(msgs) != 1 || string(msgs[0].Payload) != `{"c":"dev"}` || msgs[0].ClusterID != other {
		t.Errorf("outbox after delete = (%+v, %v), want only dev's message", msgs, err)
	}
	newID, err := s.UpsertCluster(ctx, store.Cluster{Name: "prod", ClusterUID: "uid-rebuilt"})
	if err != nil {
		t.Fatalf("re-register prod with a new UID: %v", err)
	}
	if newID == cid {
		t.Errorf("re-registered prod reused id %d, want a fresh row", newID)
	}
	if err := s.DeleteCluster(ctx, "never-existed"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("DeleteCluster(unknown) err = %v, want ErrNotFound", err)
	}
}

func testSnapshotDedup(t *testing.T, s store.Store) {
	ctx := context.Background()
	cid := mustCluster(t, s, "prod")
	idA := mustSnapshot(t, s, cid, "aaa", base)

	dupID, dup, err := s.InsertSnapshot(ctx, store.Snapshot{
		ClusterID: cid, Hash: "aaa", ReceivedAt: at(1), Inventory: []byte(`{"hash":"aaa"}`),
	})
	if err != nil {
		t.Fatalf("duplicate push: %v", err)
	}
	if !dup {
		t.Error("duplicate = false, want true (same hash as latest)")
	}
	if dupID != idA {
		t.Errorf("duplicate id = %d, want existing %d", dupID, idA)
	}
	if latest, err := s.LatestSnapshot(ctx, cid); err != nil || latest.ID != idA {
		t.Errorf("latest after dup = (%+v, %v), want id %d", latest, err, idA)
	}

	idB := mustSnapshot(t, s, cid, "bbb", at(2))
	if idB == idA {
		t.Errorf("new hash reused id %d", idB)
	}

	// "aaa" again: superseded by "bbb", so NOT a duplicate — new row.
	idA2, dup, err := s.InsertSnapshot(ctx, store.Snapshot{
		ClusterID: cid, Hash: "aaa", ReceivedAt: at(3), Inventory: []byte(`{"hash":"aaa"}`),
	})
	if err != nil {
		t.Fatalf("superseded re-push: %v", err)
	}
	if dup {
		t.Error("duplicate = true for superseded hash, want false")
	}
	if idA2 == idA || idA2 == idB {
		t.Errorf("superseded re-push id = %d, want a fresh row", idA2)
	}
	latest, err := s.LatestSnapshot(ctx, cid)
	if err != nil {
		t.Fatalf("LatestSnapshot: %v", err)
	}
	if latest.ID != idA2 || latest.Hash != "aaa" {
		t.Errorf("latest = id %d hash %q, want id %d hash aaa", latest.ID, latest.Hash, idA2)
	}
}

// testSnapshotDedupClusterScoped pins that dedup compares against the same
// cluster's latest snapshot only: the same hash pushed to a different
// cluster is a fresh, non-duplicate row.
func testSnapshotDedupClusterScoped(t *testing.T, s store.Store) {
	ctx := context.Background()
	cidA := mustCluster(t, s, "cluster-a")
	cidB := mustCluster(t, s, "cluster-b")
	idA := mustSnapshot(t, s, cidA, "aaa", base)

	idB, dup, err := s.InsertSnapshot(ctx, store.Snapshot{
		ClusterID: cidB, Hash: "aaa", ReceivedAt: at(1), Inventory: []byte(`{"hash":"aaa"}`),
	})
	if err != nil {
		t.Fatalf("push same hash to other cluster: %v", err)
	}
	if dup {
		t.Error("duplicate = true across clusters, want false (dedup is cluster-scoped)")
	}
	if idB == idA {
		t.Errorf("cross-cluster push reused id %d, want a fresh row", idB)
	}
	latestA, err := s.LatestSnapshot(ctx, cidA)
	if err != nil {
		t.Fatalf("LatestSnapshot(A): %v", err)
	}
	if latestA.ID != idA {
		t.Errorf("cluster A latest = id %d, want %d (B's push must not affect A)", latestA.ID, idA)
	}
	latestB, err := s.LatestSnapshot(ctx, cidB)
	if err != nil {
		t.Fatalf("LatestSnapshot(B): %v", err)
	}
	if latestB.ID != idB || latestB.ClusterID != cidB {
		t.Errorf("cluster B latest = id %d cluster %d, want id %d cluster %d", latestB.ID, latestB.ClusterID, idB, cidB)
	}
}

// testConcurrentIngest pins that any backend serializes concurrent
// InsertSnapshot calls correctly: no errors, and dedup-vs-latest holds
// exactly once when all writers push the same hash.
func testConcurrentIngest(t *testing.T, s store.Store) {
	const writers = 16
	cid := mustCluster(t, s, "prod")

	type pushResult struct {
		id  int64
		dup bool
		err error
	}
	run := func(hash func(i int) string) []pushResult {
		results := make([]pushResult, writers)
		start := make(chan struct{})
		var wg sync.WaitGroup
		for i := range writers {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				h := hash(i)
				id, dup, err := s.InsertSnapshot(context.Background(), store.Snapshot{
					ClusterID: cid, Hash: h, ReceivedAt: base, Inventory: []byte(`{"hash":"` + h + `"}`),
				})
				results[i] = pushResult{id, dup, err}
			}()
		}
		close(start)
		wg.Wait()
		return results
	}

	// Phase 1: same hash from every writer — exactly one insert wins, the
	// rest dedup against it.
	same := run(func(int) string { return "same" })
	var winnerID int64
	var nonDup int
	for i, r := range same {
		if r.err != nil {
			t.Fatalf("phase 1 writer %d: %v", i, r.err)
		}
		if !r.dup {
			nonDup++
			winnerID = r.id
		}
	}
	if nonDup != 1 {
		t.Fatalf("phase 1: %d non-duplicate inserts, want exactly 1", nonDup)
	}
	for i, r := range same {
		if r.dup && r.id != winnerID {
			t.Errorf("phase 1 writer %d: duplicate id = %d, want winner %d", i, r.id, winnerID)
		}
	}
	latest, err := s.LatestSnapshot(context.Background(), cid)
	if err != nil {
		t.Fatalf("LatestSnapshot after phase 1: %v", err)
	}
	if latest.ID != winnerID || latest.Hash != "same" {
		t.Errorf("latest = id %d hash %q, want id %d hash same", latest.ID, latest.Hash, winnerID)
	}

	// Phase 2: distinct hashes — every push is a fresh row, distinct ids.
	distinct := run(func(i int) string { return fmt.Sprintf("h-%d", i) })
	seen := map[int64]bool{winnerID: true}
	for i, r := range distinct {
		if r.err != nil {
			t.Fatalf("phase 2 writer %d: %v", i, r.err)
		}
		if r.dup {
			t.Errorf("phase 2 writer %d: duplicate = true for unique hash", i)
		}
		if seen[r.id] {
			t.Errorf("phase 2 writer %d: id %d reused", i, r.id)
		}
		seen[r.id] = true
	}
	if latest, err := s.LatestSnapshot(context.Background(), cid); err != nil {
		t.Fatalf("LatestSnapshot after phase 2: %v", err)
	} else if !seen[latest.ID] || latest.ID == winnerID {
		t.Errorf("latest id %d is not one of phase 2's inserts", latest.ID)
	}
}

func testLatestSnapshot(t *testing.T, s store.Store) {
	ctx := context.Background()
	cid := mustCluster(t, s, "prod")
	mustSnapshot(t, s, cid, "aaa", base)
	idB := mustSnapshot(t, s, cid, "bbb", at(1))

	got, err := s.LatestSnapshot(ctx, cid)
	if err != nil {
		t.Fatalf("LatestSnapshot: %v", err)
	}
	if got.ID != idB || got.Hash != "bbb" || got.ClusterID != cid {
		t.Errorf("got id=%d hash=%q cluster=%d, want id=%d hash=bbb cluster=%d",
			got.ID, got.Hash, got.ClusterID, idB, cid)
	}
	if got.KBVersion != "kb-1" || got.AgentVersion != "v0.2.0" {
		t.Errorf("versions = %q/%q, want kb-1/v0.2.0", got.KBVersion, got.AgentVersion)
	}
	if !got.ReceivedAt.Equal(at(1)) || got.ReceivedAt.Location() != time.UTC {
		t.Errorf("ReceivedAt = %v (%v), want %v UTC", got.ReceivedAt, got.ReceivedAt.Location(), at(1))
	}
	if !bytes.Equal(got.Inventory, []byte(`{"hash":"bbb"}`)) {
		t.Errorf("Inventory = %s, want raw bytes back", got.Inventory)
	}
}

// testLatestSnapshotHeads: one call returns every cluster's latest
// snapshot — the same row LatestSnapshot returns — without the inventory
// bytes, and nothing for a cluster with no snapshot.
func testLatestSnapshotHeads(t *testing.T, s store.Store) {
	ctx := context.Background()
	if heads, err := s.LatestSnapshotHeads(ctx); err != nil || len(heads) != 0 {
		t.Fatalf("empty store heads = (%v, %v), want none", heads, err)
	}
	prod, dev := mustCluster(t, s, "prod"), mustCluster(t, s, "dev")
	mustCluster(t, s, "empty")
	mustSnapshot(t, s, prod, "aaa", base)
	if _, _, err := s.InsertSnapshot(ctx, store.Snapshot{ClusterID: dev, Hash: "ddd", ServerVersion: "v1.33.1", ReceivedAt: at(1), Inventory: []byte(`{}`)}); err != nil {
		t.Fatal(err)
	}
	mustSnapshot(t, s, prod, "bbb", at(2))

	heads, err := s.LatestSnapshotHeads(ctx)
	if err != nil {
		t.Fatalf("LatestSnapshotHeads: %v", err)
	}
	if len(heads) != 2 {
		t.Fatalf("heads = %+v, want prod and dev only", heads)
	}
	for _, cid := range []int64{prod, dev} {
		want, err := s.LatestSnapshot(ctx, cid)
		if err != nil {
			t.Fatal(err)
		}
		want.Inventory = nil
		got := heads[cid]
		if got.ID != want.ID || got.ClusterID != want.ClusterID || got.Hash != want.Hash || got.KBVersion != want.KBVersion ||
			got.AgentVersion != want.AgentVersion || got.ServerVersion != want.ServerVersion || !got.ReceivedAt.Equal(want.ReceivedAt) || got.Inventory != nil {
			t.Errorf("head of cluster %d = %+v, want %+v without inventory", cid, got, want)
		}
		one, err := s.LatestSnapshotHead(ctx, cid)
		if err != nil || one.ID != want.ID || one.Hash != want.Hash || one.ServerVersion != want.ServerVersion ||
			one.KBVersion != want.KBVersion || one.AgentVersion != want.AgentVersion || !one.ReceivedAt.Equal(want.ReceivedAt) || one.Inventory != nil {
			t.Errorf("LatestSnapshotHead(%d) = (%+v, %v), want %+v without inventory", cid, one, err, want)
		}
	}
	if heads[prod].Hash != "bbb" || heads[dev].ServerVersion != "v1.33.1" {
		t.Errorf("heads = %+v, want prod at bbb and dev judged at v1.33.1", heads)
	}
}

// testSnapshotServerVersion: a snapshot's ServerVersion (the version it is
// judged at) and its inventory bytes come back exactly as stored —
// whitespace, key order and fields the server does not know included —
// whether written by InsertSnapshot or by CommitEvaluations.
func testSnapshotServerVersion(t *testing.T, s store.Store) {
	ctx := context.Background()
	cid := mustCluster(t, s, "prod")
	raw := []byte(`{ "serverVersion":"v1.34.2", "futureField": {"b":1,"a":[2]}, "schemaVersion":1 }`)
	if _, _, err := s.InsertSnapshot(ctx, store.Snapshot{ClusterID: cid, Hash: "aaa", ServerVersion: "v1.34.2", ReceivedAt: base, Inventory: raw}); err != nil {
		t.Fatal(err)
	}
	got, err := s.LatestSnapshot(ctx, cid)
	if err != nil {
		t.Fatal(err)
	}
	if got.ServerVersion != "v1.34.2" || !bytes.Equal(got.Inventory, raw) {
		t.Errorf("snapshot = version %q inventory %s, want v1.34.2 and %s", got.ServerVersion, got.Inventory, raw)
	}
	if _, _, err := s.CommitEvaluations(ctx, store.EvaluationBatch{ClusterID: cid,
		Snapshot: &store.Snapshot{Hash: "bbb", ServerVersion: "v1.33.0", ReceivedAt: at(1), Inventory: []byte(`{}`)}}); err != nil {
		t.Fatal(err)
	}
	if got, err := s.LatestSnapshot(ctx, cid); err != nil || got.ServerVersion != "v1.33.0" {
		t.Errorf("committed snapshot = (%+v, %v), want server version v1.33.0", got, err)
	}
	if _, _, err := s.InsertSnapshot(ctx, store.Snapshot{ClusterID: cid, Hash: "ccc", ReceivedAt: at(2), Inventory: []byte(`{}`)}); err != nil {
		t.Fatal(err)
	}
	if got, err := s.LatestSnapshot(ctx, cid); err != nil || got.ServerVersion != "" {
		t.Errorf("snapshot without a version = (%+v, %v), want none", got, err)
	}
}

func testEvaluations(t *testing.T, s store.Store) {
	ctx := context.Background()
	cid := mustCluster(t, s, "prod")
	sid := mustSnapshot(t, s, cid, "aaa", base)

	ins := func(target string, score int, ready bool, created time.Time, report []byte) {
		t.Helper()
		if _, err := s.InsertEvaluation(ctx, store.Evaluation{
			ClusterID: cid, SnapshotID: sid, Target: target, KBVersion: "kb-1",
			Score: score, Ready: ready, Blockers: 2, Warnings: 3,
			Report: report, CreatedAt: created,
		}); err != nil {
			t.Fatalf("InsertEvaluation(%s,%d): %v", target, score, err)
		}
	}
	ins("1.36", 70, false, base, []byte(`{"score":70}`))
	ins("1.36", 80, true, at(1), []byte(`{"score":80}`))
	ins("1.37", 55, false, base, []byte(`{"score":55}`))

	got, err := s.LatestEvaluation(ctx, cid, "1.36")
	if err != nil {
		t.Fatalf("LatestEvaluation(1.36): %v", err)
	}
	if got.Score != 80 || !got.Ready || !got.CreatedAt.Equal(at(1)) {
		t.Errorf("latest 1.36 = score %d ready %v at %v, want 80/true/%v", got.Score, got.Ready, got.CreatedAt, at(1))
	}
	if got.ClusterID != cid || got.SnapshotID != sid || got.Target != "1.36" || got.KBVersion != "kb-1" {
		t.Errorf("identity fields wrong: %+v", got)
	}
	if got.Blockers != 2 || got.Warnings != 3 {
		t.Errorf("Blockers/Warnings = %d/%d, want 2/3", got.Blockers, got.Warnings)
	}
	if !bytes.Equal(got.Report, []byte(`{"score":80}`)) {
		t.Errorf("Report = %s, want {\"score\":80}", got.Report)
	}
	if got.CreatedAt.Location() != time.UTC {
		t.Errorf("CreatedAt location = %v, want UTC", got.CreatedAt.Location())
	}

	got37, err := s.LatestEvaluation(ctx, cid, "1.37")
	if err != nil {
		t.Fatalf("LatestEvaluation(1.37): %v", err)
	}
	if got37.Score != 55 {
		t.Errorf("latest 1.37 score = %d, want 55", got37.Score)
	}
	if _, err := s.LatestEvaluation(ctx, cid, "1.38"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("LatestEvaluation(1.38) err = %v, want ErrNotFound", err)
	}
}

// testLatestEvaluationTieBreak pins the tie-break on equal created_at: the
// later insert (higher id) wins.
func testLatestEvaluationTieBreak(t *testing.T, s store.Store) {
	ctx := context.Background()
	cid := mustCluster(t, s, "prod")
	sid := mustSnapshot(t, s, cid, "aaa", base)

	if _, err := s.InsertEvaluation(ctx, store.Evaluation{
		ClusterID: cid, SnapshotID: sid, Target: "1.36", Score: 60, CreatedAt: base,
	}); err != nil {
		t.Fatalf("first InsertEvaluation: %v", err)
	}
	if _, err := s.InsertEvaluation(ctx, store.Evaluation{
		ClusterID: cid, SnapshotID: sid, Target: "1.36", Score: 65, CreatedAt: base, // same created_at
	}); err != nil {
		t.Fatalf("second InsertEvaluation: %v", err)
	}

	got, err := s.LatestEvaluation(ctx, cid, "1.36")
	if err != nil {
		t.Fatalf("LatestEvaluation: %v", err)
	}
	if got.Score != 65 {
		t.Errorf("latest score = %d, want 65 (equal created_at: later insert / higher id wins)", got.Score)
	}
}

func testScoreHistory(t *testing.T, s store.Store) {
	ctx := context.Background()
	cid := mustCluster(t, s, "prod")
	sid := mustSnapshot(t, s, cid, "aaa", base)

	points := []struct {
		score int
		ready bool
		when  time.Time
	}{
		{70, false, base}, {75, false, at(1)}, {80, false, at(2)}, {92, true, at(3)},
	}
	for _, p := range points {
		if _, err := s.InsertEvaluation(ctx, store.Evaluation{
			ClusterID: cid, SnapshotID: sid, Target: "1.36",
			Score: p.score, Ready: p.ready, CreatedAt: p.when,
		}); err != nil {
			t.Fatalf("InsertEvaluation: %v", err)
		}
	}
	if _, err := s.InsertEvaluation(ctx, store.Evaluation{
		ClusterID: cid, SnapshotID: sid, Target: "1.37", Score: 10, CreatedAt: base,
	}); err != nil {
		t.Fatalf("InsertEvaluation(1.37): %v", err)
	}

	tests := []struct {
		name       string
		limit      int
		wantScores []int
	}{
		{"zero limit means all", 0, []int{70, 75, 80, 92}},
		{"negative limit means all", -1, []int{70, 75, 80, 92}},
		{"limit selects most recent N, returned oldest-first", 2, []int{80, 92}},
		{"limit one", 1, []int{92}},
		{"limit beyond rows", 10, []int{70, 75, 80, 92}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := s.ScoreHistory(ctx, cid, "1.36", tt.limit)
			if err != nil {
				t.Fatalf("ScoreHistory: %v", err)
			}
			if len(got) != len(tt.wantScores) {
				t.Fatalf("got %d points, want %d: %+v", len(got), len(tt.wantScores), got)
			}
			for i, p := range got {
				if p.Score != tt.wantScores[i] {
					t.Errorf("point %d score = %d, want %d", i, p.Score, tt.wantScores[i])
				}
				if i > 0 && !got[i-1].At.Before(p.At) {
					t.Errorf("points not ascending by At: %v then %v", got[i-1].At, p.At)
				}
			}
		})
	}

	empty, err := s.ScoreHistory(ctx, 999, "1.36", 5)
	if err != nil {
		t.Errorf("ScoreHistory(unknown cluster) err = %v, want nil", err)
	}
	if len(empty) != 0 {
		t.Errorf("ScoreHistory(unknown cluster) = %+v, want empty", empty)
	}
}

func testNotFound(t *testing.T, s store.Store) {
	ctx := context.Background()
	tests := []struct {
		name string
		call func() error
	}{
		{"GetCluster", func() error { _, err := s.GetCluster(ctx, 999); return err }},
		{"LatestSnapshot", func() error { _, err := s.LatestSnapshot(ctx, 999); return err }},
		{"LatestSnapshotHead", func() error { _, err := s.LatestSnapshotHead(ctx, 999); return err }},
		{"LatestEvaluation", func() error { _, err := s.LatestEvaluation(ctx, 999, "1.36"); return err }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := tt.call(); !errors.Is(err, store.ErrNotFound) {
				t.Errorf("err = %v, want errors.Is(err, store.ErrNotFound)", err)
			}
		})
	}
}

// testTokens pins the per-cluster ingest-token lifecycle. Note tokens are
// keyed by cluster NAME, not id: a token may be minted before the cluster's
// first push registers it.
func testTokens(t *testing.T, s store.Store) {
	ctx := context.Background()

	if _, err := s.CreateToken(ctx, "prod", "tok-prod"); err != nil {
		t.Fatalf("CreateToken(prod): %v", err)
	}
	if _, err := s.CreateToken(ctx, "dev", "tok-dev"); err != nil {
		t.Fatalf("CreateToken(dev): %v", err)
	}

	name, ok, err := s.ValidToken(ctx, "tok-prod")
	if err != nil || !ok || name != "prod" {
		t.Errorf("ValidToken(tok-prod) = (%q, %v, %v), want (prod, true, nil)", name, ok, err)
	}
	name, ok, err = s.ValidToken(ctx, "tok-dev")
	if err != nil || !ok || name != "dev" {
		t.Errorf("ValidToken(tok-dev) = (%q, %v, %v), want (dev, true, nil)", name, ok, err)
	}
	// Unknown token: not an error — (\"\", false, nil).
	name, ok, err = s.ValidToken(ctx, "no-such-token")
	if err != nil || ok || name != "" {
		t.Errorf("ValidToken(unknown) = (%q, %v, %v), want (\"\", false, nil)", name, ok, err)
	}

	if err := s.RevokeToken(ctx, "prod"); err != nil {
		t.Fatalf("RevokeToken(prod): %v", err)
	}
	if _, ok, err := s.ValidToken(ctx, "tok-prod"); err != nil || ok {
		t.Errorf("ValidToken after revoke = (ok %v, err %v), want (false, nil)", ok, err)
	}
	if name, ok, _ := s.ValidToken(ctx, "tok-dev"); !ok || name != "dev" {
		t.Errorf("dev token must survive prod revoke, got (%q, %v)", name, ok)
	}

	// Revoking a cluster with no active tokens (already revoked, or never
	// had any) reports ErrNotFound.
	if err := s.RevokeToken(ctx, "prod"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("second RevokeToken(prod) err = %v, want ErrNotFound", err)
	}
	if err := s.RevokeToken(ctx, "never-existed"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("RevokeToken(unknown cluster) err = %v, want ErrNotFound", err)
	}

	// A fresh token after revocation is independent of the revoked one.
	if _, err := s.CreateToken(ctx, "prod", "tok-prod-2"); err != nil {
		t.Fatalf("CreateToken(prod, second): %v", err)
	}
	if name, ok, _ := s.ValidToken(ctx, "tok-prod-2"); !ok || name != "prod" {
		t.Errorf("re-issued prod token = (%q, %v), want (prod, true)", name, ok)
	}
}

// testTokensMultiplePerCluster pins that a cluster may hold several active
// tokens (rotation) and RevokeToken revokes them all at once.
func testTokensMultiplePerCluster(t *testing.T, s store.Store) {
	ctx := context.Background()
	for _, tok := range []string{"rot-a", "rot-b"} {
		if _, err := s.CreateToken(ctx, "prod", tok); err != nil {
			t.Fatalf("CreateToken(%s): %v", tok, err)
		}
	}
	for _, tok := range []string{"rot-a", "rot-b"} {
		if name, ok, err := s.ValidToken(ctx, tok); err != nil || !ok || name != "prod" {
			t.Errorf("ValidToken(%s) = (%q, %v, %v), want (prod, true, nil)", tok, name, ok, err)
		}
	}
	if err := s.RevokeToken(ctx, "prod"); err != nil {
		t.Fatalf("RevokeToken: %v", err)
	}
	for _, tok := range []string{"rot-a", "rot-b"} {
		if _, ok, err := s.ValidToken(ctx, tok); err != nil || ok {
			t.Errorf("ValidToken(%s) after revoke = (ok %v, err %v), want (false, nil)", tok, ok, err)
		}
	}
}

// testTokensDuplicate pins that the same plaintext token cannot exist twice
// — even for different clusters, since ValidToken could not disambiguate.
func testTokensDuplicate(t *testing.T, s store.Store) {
	ctx := context.Background()
	if _, err := s.CreateToken(ctx, "prod", "dup-tok"); err != nil {
		t.Fatalf("CreateToken: %v", err)
	}
	if _, err := s.CreateToken(ctx, "dev", "dup-tok"); err == nil {
		t.Error("CreateToken with an already-issued token succeeded, want error")
	}
	// The original binding must be untouched.
	if name, ok, _ := s.ValidToken(ctx, "dup-tok"); !ok || name != "prod" {
		t.Errorf("ValidToken(dup-tok) = (%q, %v), want (prod, true)", name, ok)
	}
}

// testTokensRevokeByID pins zero-downtime rotation: with two active tokens
// for one cluster, revoking one by id leaves the other authenticating.
func testTokensRevokeByID(t *testing.T, s store.Store) {
	ctx := context.Background()
	oldID, err := s.CreateToken(ctx, "prod", "rot-old")
	if err != nil {
		t.Fatalf("CreateToken(rot-old): %v", err)
	}
	newID, err := s.CreateToken(ctx, "prod", "rot-new")
	if err != nil {
		t.Fatalf("CreateToken(rot-new): %v", err)
	}
	if oldID <= 0 || newID <= 0 || oldID == newID {
		t.Fatalf("CreateToken ids = %d, %d; want distinct positive ids", oldID, newID)
	}

	if err := s.RevokeTokenID(ctx, "prod", oldID); err != nil {
		t.Fatalf("RevokeTokenID(prod, %d): %v", oldID, err)
	}
	if _, ok, err := s.ValidToken(ctx, "rot-old"); err != nil || ok {
		t.Errorf("revoked token: ValidToken = (ok %v, err %v), want (false, nil)", ok, err)
	}
	if name, ok, err := s.ValidToken(ctx, "rot-new"); err != nil || !ok || name != "prod" {
		t.Errorf("other token after revoke-by-id = (%q, %v, %v), want (prod, true, nil)", name, ok, err)
	}

	// Already revoked, wrong cluster, or unknown id: ErrNotFound, and the
	// surviving token is untouched.
	for _, tc := range []struct {
		cluster string
		id      int64
	}{{"prod", oldID}, {"dev", newID}, {"prod", newID + 1000}} {
		if err := s.RevokeTokenID(ctx, tc.cluster, tc.id); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("RevokeTokenID(%s, %d) err = %v, want ErrNotFound", tc.cluster, tc.id, err)
		}
	}
	if _, ok, _ := s.ValidToken(ctx, "rot-new"); !ok {
		t.Error("a failed RevokeTokenID must not revoke anything")
	}
}

// testTokensList pins ListTokens: every token (active and revoked) in
// creation order, optionally filtered by cluster, with metadata only.
func testTokensList(t *testing.T, s store.Store) {
	ctx := context.Background()
	if toks, err := s.ListTokens(ctx, ""); err != nil || len(toks) != 0 {
		t.Fatalf("ListTokens on empty store = (%v, %v), want none", toks, err)
	}
	long := "0123abcd" + strings.Repeat("f", 56) // 64 chars, like `tokens create`
	prodA, err := s.CreateToken(ctx, "prod", long)
	if err != nil {
		t.Fatal(err)
	}
	dev, err := s.CreateToken(ctx, "dev", "short-dev")
	if err != nil {
		t.Fatal(err)
	}
	prodB, err := s.CreateToken(ctx, "prod", "short-prod")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.RevokeTokenID(ctx, "prod", prodA); err != nil {
		t.Fatal(err)
	}

	all, err := s.ListTokens(ctx, "")
	if err != nil {
		t.Fatalf("ListTokens: %v", err)
	}
	if len(all) != 3 || all[0].ID != prodA || all[1].ID != dev || all[2].ID != prodB {
		t.Fatalf("ListTokens = %+v, want ids %d, %d, %d in creation order", all, prodA, dev, prodB)
	}
	a := all[0]
	if a.ClusterName != "prod" || a.CreatedAt.IsZero() || a.RevokedAt == nil {
		t.Errorf("revoked token row = %+v, want prod, created, revoked", a)
	}
	if a.Prefix != "0123abcd" {
		t.Errorf("prefix = %q, want the first 8 chars of a long token", a.Prefix)
	}
	if all[1].RevokedAt != nil || all[1].ClusterName != "dev" {
		t.Errorf("active dev row = %+v, want unrevoked dev", all[1])
	}
	if all[1].Prefix != "" {
		t.Errorf("short token prefix = %q, want \"\" (a prefix would give away most of it)", all[1].Prefix)
	}

	prod, err := s.ListTokens(ctx, "prod")
	if err != nil {
		t.Fatal(err)
	}
	if len(prod) != 2 || prod[0].ID != prodA || prod[1].ID != prodB {
		t.Fatalf("ListTokens(prod) = %+v, want ids %d, %d", prod, prodA, prodB)
	}
	if none, err := s.ListTokens(ctx, "never-existed"); err != nil || len(none) != 0 {
		t.Fatalf("ListTokens(unknown) = (%v, %v), want none", none, err)
	}
}

func testClose(t *testing.T, s store.Store) {
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := s.UpsertCluster(context.Background(), store.Cluster{Name: "after-close"}); err == nil {
		t.Error("UpsertCluster after Close succeeded, want error")
	}
}
