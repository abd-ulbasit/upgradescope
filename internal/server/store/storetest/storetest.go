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
	t.Run("SnapshotDedup", func(t *testing.T) { testSnapshotDedup(t, newStore(t)) })
	t.Run("SnapshotDedupClusterScoped", func(t *testing.T) { testSnapshotDedupClusterScoped(t, newStore(t)) })
	t.Run("ConcurrentIngestSerializes", func(t *testing.T) { testConcurrentIngest(t, newStore(t)) })
	t.Run("LatestSnapshotRoundTrip", func(t *testing.T) { testLatestSnapshot(t, newStore(t)) })
	t.Run("EvaluationsLatestPerTarget", func(t *testing.T) { testEvaluations(t, newStore(t)) })
	t.Run("LatestEvaluationTieBreakHigherID", func(t *testing.T) { testLatestEvaluationTieBreak(t, newStore(t)) })
	t.Run("ScoreHistoryOldestFirstLimitNewest", func(t *testing.T) { testScoreHistory(t, newStore(t)) })
	t.Run("NotFound", func(t *testing.T) { testNotFound(t, newStore(t)) })
	t.Run("TokensCreateValidateRevoke", func(t *testing.T) { testTokens(t, newStore(t)) })
	t.Run("TokensMultiplePerCluster", func(t *testing.T) { testTokensMultiplePerCluster(t, newStore(t)) })
	t.Run("TokensDuplicateRejected", func(t *testing.T) { testTokensDuplicate(t, newStore(t)) })
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
	id2, err := s.UpsertCluster(ctx, store.Cluster{Name: "prod-eu-1", ClusterUID: "uid-1b", FirstSeen: at(1), LastSeen: at(1)})
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
	if got.ClusterUID != "uid-1b" {
		t.Errorf("ClusterUID = %q, want uid-1b", got.ClusterUID)
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

	if err := s.CreateToken(ctx, "prod", "tok-prod"); err != nil {
		t.Fatalf("CreateToken(prod): %v", err)
	}
	if err := s.CreateToken(ctx, "dev", "tok-dev"); err != nil {
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
	if err := s.CreateToken(ctx, "prod", "tok-prod-2"); err != nil {
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
		if err := s.CreateToken(ctx, "prod", tok); err != nil {
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
	if err := s.CreateToken(ctx, "prod", "dup-tok"); err != nil {
		t.Fatalf("CreateToken: %v", err)
	}
	if err := s.CreateToken(ctx, "dev", "dup-tok"); err == nil {
		t.Error("CreateToken with an already-issued token succeeded, want error")
	}
	// The original binding must be untouched.
	if name, ok, _ := s.ValidToken(ctx, "dup-tok"); !ok || name != "prod" {
		t.Errorf("ValidToken(dup-tok) = (%q, %v), want (prod, true)", name, ok)
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
