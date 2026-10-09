package store

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
)

// TestLatestSnapshotUsesIndex pins the plan of the "latest snapshot" read
// every fleet and report request makes: with only (cluster_id,
// received_at) indexed it sorted every snapshot of the cluster in a temp
// B-tree, reading each inventory blob, so latency grew with history.
func TestLatestSnapshotUsesIndex(t *testing.T) {
	s := newTestStore(t)
	rows, err := s.db.Query(`EXPLAIN QUERY PLAN
		SELECT id, cluster_id, hash, kb_version, agent_version, received_at, inventory
		FROM snapshots WHERE cluster_id = ? ORDER BY id DESC LIMIT 1`, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var plan []string
	for rows.Next() {
		var id, parent, notused int
		var detail string
		if err := rows.Scan(&id, &parent, &notused, &detail); err != nil {
			t.Fatal(err)
		}
		plan = append(plan, detail)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(plan, "; ")
	if strings.Contains(joined, "TEMP B-TREE") || !strings.Contains(joined, "idx_snapshots_cluster_id") {
		t.Errorf("plan = %q, want idx_snapshots_cluster_id and no temp B-tree", joined)
	}
}

// TestPruneRowCounts measures retention on a year of history: one cluster
// whose inventory changed every day (one snapshot and two target
// evaluations a day), plus a cluster silent for the whole year. A 90-day
// window keeps 90 days of rows and the silent cluster's current state.
func TestPruneRowCounts(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	now := tBase
	daysAgo := func(d int) time.Time { return now.Add(-time.Duration(d) * 24 * time.Hour) }
	busy := mustCluster(t, s, "busy")
	for d := 364; d >= 0; d-- {
		sid := mustSnapshot(t, s, busy, fmt.Sprintf("day-%d", d), daysAgo(d))
		for _, target := range []string{"1.35", "1.36"} {
			if _, err := s.InsertEvaluation(ctx, Evaluation{ClusterID: busy, SnapshotID: sid, Target: target, Score: 90, CreatedAt: daysAgo(d)}); err != nil {
				t.Fatal(err)
			}
		}
	}
	silent := mustCluster(t, s, "silent")
	sid := mustSnapshot(t, s, silent, "once", daysAgo(365))
	if _, err := s.InsertEvaluation(ctx, Evaluation{ClusterID: silent, SnapshotID: sid, Target: "1.35", Score: 90, CreatedAt: daysAgo(365)}); err != nil {
		t.Fatal(err)
	}
	count := func(table string) int {
		t.Helper()
		var n int
		if err := s.db.QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if got := [2]int{count("snapshots"), count("evaluations")}; got != [2]int{366, 731} {
		t.Fatalf("seeded rows (snapshots, evaluations) = %v, want [366 731]", got)
	}

	res, err := s.Prune(ctx, daysAgo(90), nil)
	if err != nil {
		t.Fatal(err)
	}
	// Days 364..91 ago go: 274 snapshots, 548 evaluations. Day 90 sits on
	// the cutoff and stays.
	if res != (PruneResult{Snapshots: 274, Evaluations: 548}) {
		t.Errorf("Prune = %+v, want {274 548}", res)
	}
	if got := [2]int{count("snapshots"), count("evaluations")}; got != [2]int{92, 183} {
		t.Errorf("rows after prune = %v, want [92 183] (91 days of busy, plus silent's latest)", got)
	}
}
