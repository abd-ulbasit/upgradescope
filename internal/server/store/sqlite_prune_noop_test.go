package store

import (
	"context"
	"sync"
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
// few milliseconds. Two bounds, each under 50 ms: Prune itself, and a push
// of another cluster that starts 1 ms after Prune's first statement has been
// sent (beforeStmt starts it), so it contends for the write lock if that
// statement holds it, and commits only once the lock is free (SQLite's busy
// handler waits on its own schedule, so it commits no sooner). Before the
// index every run scanned the table, the reports' overflow pages included,
// 177-270 ms (five runs, commit 2f9de7cc, 10 October 2026;
// docs/operations/retention-and-backup.md) and growing with the history
// (#297); with the index dropped back to ORDER BY id, this test fails on
// both bounds (mutation-checked, see the commit). The plan test pins the
// index; this pins the effect.
// It writes 600 MB, so it does not run with -short, and it skips under the
// race detector, whose slowdown the bounds are not for. It is a timing
// proof, not a heap bound; it is in hack/test-heap.sh's set only because it
// reads raceEnabled.
func TestPruneWithNothingToDeleteIsCheap(t *testing.T) {
	if testing.Short() {
		t.Skip("writes 600 MB of reports")
	}
	if raceEnabled {
		t.Skip("timing bounds are for a run without the race detector")
	}
	ctx := context.Background()
	s := newTestStore(t)
	seedInWindowEvaluations(t, s, noopPruneEvaluations, noopPruneReportBytes)
	other := mustCluster(t, s, "other")

	// pushDelay is how long after Prune's first statement is sent the push
	// begins: long enough for the statement to hold the write lock if it is
	// going to, far shorter than the 177-270 ms it held it before the index.
	const pushDelay = time.Millisecond
	pushed := make(chan time.Duration, 1)
	var once sync.Once
	s.tuning.beforeStmt = func(string) {
		once.Do(func() {
			go func() {
				time.Sleep(pushDelay)
				began := time.Now()
				_, _, err := s.InsertSnapshot(ctx, Snapshot{ClusterID: other, Hash: "bbb", KBVersion: "kb-1", AgentVersion: "v0.2.0", ReceivedAt: tBase, Inventory: []byte(`{}`)})
				if err != nil {
					t.Error(err)
				}
				pushed <- time.Since(began)
			}()
		})
	}
	start := time.Now()
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
	t.Logf("database %d MiB; %d evaluations of %d KB, nothing to delete: Prune %v, push begun %v after Prune's first statement committed after %v", size>>20, noopPruneEvaluations, noopPruneReportBytes>>10, took, pushDelay, wait)
	if took > 50*time.Millisecond {
		t.Errorf("Prune took %v, want under 50ms", took)
	}
	if wait > 50*time.Millisecond {
		t.Errorf("a push begun %v after Prune's first statement took %v to commit, want under 50ms: Prune holds the write lock too long", pushDelay, wait)
	}
}
