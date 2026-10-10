package store

import (
	"context"
	"testing"
	"time"
)

// noopPruneEvaluations and noopPruneReportBytes are what
// TestPruneWithNothingToDeleteIsCheap seeds: about what a fleet of a few
// dozen clusters holds after weeks of changing reports (#297 measured
// the scan on 20,000 and 40,000 rows of 30 KB).
const (
	noopPruneEvaluations = 20000
	noopPruneReportBytes = 30 << 10
)

// seedInWindowEvaluations stores n evaluations of reportBytes bytes of
// one cluster, each created within the 14 days before tBase, so none is
// older than a 90-day cutoff. It returns the cluster's id.
func seedInWindowEvaluations(t *testing.T, s *SQLite, n, reportBytes int) int64 {
	t.Helper()
	cid := mustCluster(t, s, "prod")
	sid := mustSnapshot(t, s, cid, "aaa", tBase)
	if _, err := s.db.Exec(`
		WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM n WHERE i < ?)
		INSERT INTO evaluations (cluster_id, snapshot_id, target, kb_version, score, ready, blockers, warnings, report, created_at, evaluated_at, team_map_hash, not_assessed)
		SELECT ?, ?, '1.36', 'kb', 80, 0, 0, 0, zeroblob(?),
			strftime('%Y-%m-%dT%H:%M:%S.000000000Z', '2026-06-10 12:00:00', '-' || i || ' minutes'),
			strftime('%Y-%m-%dT%H:%M:%S.000000000Z', '2026-06-10 12:00:00', '-' || i || ' minutes'), '', ''
		FROM n`, n, cid, sid, reportBytes); err != nil {
		t.Fatal(err)
	}
	return cid
}

// TestPruneWithNothingToDeleteIsCheap: a Prune that deletes nothing,
// on 20,000 evaluations of 30 KB reports all inside the window, finds that
// out from idx_evaluations_created_at (migration 0010) without reading
// a report, and so does not hold SQLite's one write lock for longer than a
// few milliseconds: a push of another cluster started with it commits in
// well under 50 ms. Before the index every run scanned the table, the
// reports' overflow pages included, 184-220 ms here on a warm cache and
// growing with the history (#297). The plan test pins the index; this
// pins the effect. It writes 600 MB, so it does not run with -short, and
// the bounds are for a plain run, not the race detector's.
func TestPruneWithNothingToDeleteIsCheap(t *testing.T) {
	if testing.Short() {
		t.Skip("writes 600 MB of reports")
	}
	ctx := context.Background()
	s := newTestStore(t)
	seedInWindowEvaluations(t, s, noopPruneEvaluations, noopPruneReportBytes)
	other := mustCluster(t, s, "other")

	pushed := make(chan time.Duration, 1)
	start := time.Now()
	go func() {
		// A push of another cluster contending for the write lock.
		_, _, err := s.InsertSnapshot(ctx, Snapshot{ClusterID: other, Hash: "bbb", KBVersion: "kb-1", AgentVersion: "v0.2.0", ReceivedAt: tBase, Inventory: []byte(`{}`)})
		if err != nil {
			t.Error(err)
		}
		pushed <- time.Since(start)
	}()
	res, err := s.Prune(ctx, tBase.Add(-90*24*time.Hour), nil)
	took := time.Since(start)
	if err != nil {
		t.Fatal(err)
	}
	if res != (PruneResult{}) {
		t.Fatalf("PruneResult = %+v, want nothing deleted", res)
	}
	wait := <-pushed
	var size int64
	if err := s.db.QueryRow(`SELECT (SELECT page_count FROM pragma_page_count) * (SELECT page_size FROM pragma_page_size)`).Scan(&size); err != nil {
		t.Fatal(err)
	}
	t.Logf("database %d MB; %d evaluations of %d KB, nothing to delete: Prune %v, concurrent push committed after %v", size>>20, noopPruneEvaluations, noopPruneReportBytes>>10, took, wait)
	if raceEnabled {
		return
	}
	if took > 50*time.Millisecond || wait > 50*time.Millisecond {
		t.Errorf("Prune took %v and the concurrent push %v, want both under 50ms", took, wait)
	}
}
