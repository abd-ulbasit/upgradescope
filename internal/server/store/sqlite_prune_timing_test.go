package store

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

// TestPruneBacklogTiming times one Prune of a backlog on SQLite and logs
// it; it asserts nothing, so it is opt-in (UPGRADESCOPE_PRUNE_TIMING=1,
// go test -run TestPruneBacklogTiming -v ./internal/server/store/). The
// numbers in docs/operations/retention-and-backup.md come from it: 60,000
// old snapshots of 4 KB, each with one 1 KB evaluation, plus a current
// snapshot and evaluation.
func TestPruneBacklogTiming(t *testing.T) {
	if os.Getenv("UPGRADESCOPE_PRUNE_TIMING") != "1" {
		t.Skip("set UPGRADESCOPE_PRUNE_TIMING=1 to time a 60,000-row prune")
	}
	ctx := context.Background()
	s := newTestStore(t)
	const n = 60000
	cid := mustCluster(t, s, "c")
	report := []byte(`{"pad":"` + strings.Repeat("x", 1000) + `"}`)
	inv := []byte(`{"pad":"` + strings.Repeat("y", 4000) + `"}`)
	tx, err := s.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < n; i++ {
		at := formatTime(tBase.Add(-400*24*time.Hour + time.Duration(i)*time.Second))
		r, err := tx.Exec(`INSERT INTO snapshots (cluster_id, hash, kb_version, agent_version, received_at, inventory) VALUES (?, ?, 'kb', 'v', ?, ?)`,
			cid, fmt.Sprintf("h%d", i), at, inv)
		if err != nil {
			t.Fatal(err)
		}
		sid, _ := r.LastInsertId()
		if _, err := tx.Exec(`INSERT INTO evaluations (cluster_id, snapshot_id, target, kb_version, score, ready, blockers, warnings, report, created_at, evaluated_at, team_map_hash, not_assessed)
			VALUES (?, ?, '1.36', 'kb', 80, 0, 0, 0, ?, ?, ?, '', '')`, cid, sid, report, at, at); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	cur := mustSnapshot(t, s, cid, "cur", tBase)
	if _, err := s.InsertEvaluation(ctx, Evaluation{ClusterID: cid, SnapshotID: cur, Target: "1.36", Score: 80, CreatedAt: tBase}); err != nil {
		t.Fatal(err)
	}
	txs := 0
	s.SetPruneTestHook(0, func(PruneBatch) { txs++ })
	start := time.Now()
	res, err := s.Prune(ctx, tBase.Add(-90*24*time.Hour), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("pruned %+v in %d transactions in %v", res, txs, time.Since(start))
}
