package store

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

var tBase = time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC)

func tPlus(h int) time.Time { return tBase.Add(time.Duration(h) * time.Hour) }

func newTestStore(t *testing.T) *SQLite {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func mustCluster(t *testing.T, s *SQLite, name string) int64 {
	t.Helper()
	id, err := s.UpsertCluster(context.Background(),
		Cluster{Name: name, ClusterUID: "uid-" + name, FirstSeen: tBase, LastSeen: tBase})
	if err != nil {
		t.Fatalf("UpsertCluster(%s): %v", name, err)
	}
	return id
}

func mustSnapshot(t *testing.T, s *SQLite, clusterID int64, hash string, at time.Time) int64 {
	t.Helper()
	id, dup, err := s.InsertSnapshot(context.Background(), Snapshot{
		ClusterID: clusterID, Hash: hash, KBVersion: "kb-1", AgentVersion: "v0.2.0",
		ReceivedAt: at, Inventory: []byte(`{"hash":"` + hash + `"}`),
	})
	if err != nil {
		t.Fatalf("InsertSnapshot(%s): %v", hash, err)
	}
	if dup {
		t.Fatalf("InsertSnapshot(%s): unexpected duplicate", hash)
	}
	return id
}

func TestOpenSetsPragmas(t *testing.T) {
	s := newTestStore(t)
	var mode string
	if err := s.db.QueryRow(`PRAGMA journal_mode`).Scan(&mode); err != nil {
		t.Fatalf("journal_mode: %v", err)
	}
	if mode != "wal" {
		t.Errorf("journal_mode = %q, want wal", mode)
	}
	var fk int
	if err := s.db.QueryRow(`PRAGMA foreign_keys`).Scan(&fk); err != nil {
		t.Fatalf("foreign_keys: %v", err)
	}
	if fk != 1 {
		t.Errorf("foreign_keys = %d, want 1", fk)
	}
	var busy int
	if err := s.db.QueryRow(`PRAGMA busy_timeout`).Scan(&busy); err != nil {
		t.Fatalf("busy_timeout: %v", err)
	}
	if busy != 5000 {
		t.Errorf("busy_timeout = %d, want 5000", busy)
	}
}

func TestOpenIdempotentAcrossReopen(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "db.sqlite")
	s1, err := Open(path)
	if err != nil {
		t.Fatalf("first Open: %v", err)
	}
	id, err := s1.UpsertCluster(ctx, Cluster{Name: "prod", FirstSeen: tBase, LastSeen: tBase})
	if err != nil {
		t.Fatalf("UpsertCluster: %v", err)
	}
	if err := s1.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	s2, err := Open(path) // migrations must be idempotent on an existing file
	if err != nil {
		t.Fatalf("re-Open: %v", err)
	}
	defer s2.Close()
	got, err := s2.GetCluster(ctx, id)
	if err != nil {
		t.Fatalf("GetCluster after reopen: %v", err)
	}
	if got.Name != "prod" {
		t.Errorf("Name = %q, want prod", got.Name)
	}
	var n int
	if err := s2.db.QueryRow(`SELECT COUNT(*) FROM schema_migrations`).Scan(&n); err != nil {
		t.Fatalf("count schema_migrations: %v", err)
	}
	if n != 1 {
		t.Errorf("schema_migrations rows = %d, want 1 (0001 only, applied once)", n)
	}
	for _, table := range []string{"clusters", "snapshots", "evaluations"} {
		var name string
		if err := s2.db.QueryRow(
			`SELECT name FROM sqlite_master WHERE type='table' AND name=?`, table).Scan(&name); err != nil {
			t.Errorf("table %s missing: %v", table, err)
		}
	}
}

func TestUpsertClusterInsertThenUpdate(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	id, err := s.UpsertCluster(ctx, Cluster{Name: "prod-eu-1", ClusterUID: "uid-1", FirstSeen: tBase, LastSeen: tBase})
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	if id <= 0 {
		t.Fatalf("insert returned id %d, want > 0", id)
	}

	id2, err := s.UpsertCluster(ctx, Cluster{Name: "prod-eu-1", ClusterUID: "uid-1b", FirstSeen: tPlus(1), LastSeen: tPlus(1)})
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
	if !got.FirstSeen.Equal(tBase) {
		t.Errorf("FirstSeen = %v, want %v (must not move on update)", got.FirstSeen, tBase)
	}
	if !got.LastSeen.Equal(tPlus(1)) {
		t.Errorf("LastSeen = %v, want %v (must bump on update)", got.LastSeen, tPlus(1))
	}
	if got.ClusterUID != "uid-1b" {
		t.Errorf("ClusterUID = %q, want uid-1b", got.ClusterUID)
	}

	if _, err := s.UpsertCluster(ctx, Cluster{Name: "dev-1", FirstSeen: tBase, LastSeen: tBase}); err != nil {
		t.Fatalf("second cluster: %v", err)
	}
	list, err := s.ListClusters(ctx)
	if err != nil {
		t.Fatalf("ListClusters: %v", err)
	}
	if len(list) != 2 || list[0].Name != "dev-1" || list[1].Name != "prod-eu-1" {
		t.Errorf("ListClusters = %+v, want [dev-1 prod-eu-1] sorted by name", list)
	}

	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM clusters WHERE name = 'prod-eu-1'`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 1 {
		t.Errorf("rows for prod-eu-1 = %d, want 1 (name UNIQUE upsert)", n)
	}
}

func TestUpsertClusterZeroTimesDefaultToNow(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	before := time.Now().UTC().Add(-time.Minute)
	id, err := s.UpsertCluster(ctx, Cluster{Name: "zero-times"})
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
			t.Errorf("%s = %v, want within [%v, %v]", name, ts, before, after)
		}
	}
}

func TestInsertSnapshotDedup(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	cid := mustCluster(t, s, "prod")

	idA := mustSnapshot(t, s, cid, "aaa", tBase)

	// Same hash as latest → duplicate, no insert, existing id returned.
	dupID, dup, err := s.InsertSnapshot(ctx, Snapshot{
		ClusterID: cid, Hash: "aaa", ReceivedAt: tPlus(1), Inventory: []byte(`{"hash":"aaa"}`),
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

	// Different hash → new row.
	idB := mustSnapshot(t, s, cid, "bbb", tPlus(2))
	if idB == idA {
		t.Errorf("new hash reused id %d", idB)
	}

	// Hash "aaa" again: it is no longer the LATEST (superseded by "bbb"),
	// so this is NOT a duplicate — the cluster changed back.
	idA2, dup, err := s.InsertSnapshot(ctx, Snapshot{
		ClusterID: cid, Hash: "aaa", ReceivedAt: tPlus(3), Inventory: []byte(`{"hash":"aaa"}`),
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

	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM snapshots WHERE cluster_id = ?`, cid).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 3 {
		t.Errorf("snapshot rows = %d, want 3 (aaa, bbb, aaa-again; dup not stored)", n)
	}
}

func TestLatestSnapshotRoundTrip(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	cid := mustCluster(t, s, "prod")
	mustSnapshot(t, s, cid, "aaa", tBase)
	idB := mustSnapshot(t, s, cid, "bbb", tPlus(1))

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
	if !got.ReceivedAt.Equal(tPlus(1)) || got.ReceivedAt.Location() != time.UTC {
		t.Errorf("ReceivedAt = %v (%v), want %v UTC", got.ReceivedAt, got.ReceivedAt.Location(), tPlus(1))
	}
	if !bytes.Equal(got.Inventory, []byte(`{"hash":"bbb"}`)) {
		t.Errorf("Inventory = %s, want raw bytes back", got.Inventory)
	}
}

func TestForeignKeysEnforced(t *testing.T) {
	s := newTestStore(t)
	_, _, err := s.InsertSnapshot(context.Background(), Snapshot{
		ClusterID: 12345, Hash: "x", ReceivedAt: tBase, Inventory: []byte("{}"),
	})
	if err == nil {
		t.Fatal("insert with bogus cluster_id succeeded — foreign_keys pragma not effective")
	}
}

func TestEvaluationsLatestPerTarget(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	cid := mustCluster(t, s, "prod")
	sid := mustSnapshot(t, s, cid, "aaa", tBase)

	ins := func(target string, score int, ready bool, at time.Time, report []byte) {
		t.Helper()
		_, err := s.InsertEvaluation(ctx, Evaluation{
			ClusterID: cid, SnapshotID: sid, Target: target, KBVersion: "kb-1",
			Score: score, Ready: ready, Blockers: 2, Warnings: 3,
			Report: report, CreatedAt: at,
		})
		if err != nil {
			t.Fatalf("InsertEvaluation(%s,%d): %v", target, score, err)
		}
	}
	ins("1.36", 70, false, tBase, []byte(`{"score":70}`))
	ins("1.36", 80, true, tPlus(1), []byte(`{"score":80}`))
	ins("1.37", 55, false, tBase, []byte(`{"score":55}`))

	got, err := s.LatestEvaluation(ctx, cid, "1.36")
	if err != nil {
		t.Fatalf("LatestEvaluation(1.36): %v", err)
	}
	if got.Score != 80 || !got.Ready || !got.CreatedAt.Equal(tPlus(1)) {
		t.Errorf("latest 1.36 = score %d ready %v at %v, want 80/true/%v", got.Score, got.Ready, got.CreatedAt, tPlus(1))
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

	got37, err := s.LatestEvaluation(ctx, cid, "1.37")
	if err != nil {
		t.Fatalf("LatestEvaluation(1.37): %v", err)
	}
	if got37.Score != 55 {
		t.Errorf("latest 1.37 score = %d, want 55", got37.Score)
	}

	if _, err := s.LatestEvaluation(ctx, cid, "1.38"); !errors.Is(err, ErrNotFound) {
		t.Errorf("LatestEvaluation(1.38) err = %v, want ErrNotFound", err)
	}
}

func TestScoreHistoryOrderingAndLimit(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	cid := mustCluster(t, s, "prod")
	sid := mustSnapshot(t, s, cid, "aaa", tBase)

	scores := []struct {
		score int
		ready bool
		at    time.Time
	}{
		{70, false, tBase}, {75, false, tPlus(1)}, {80, false, tPlus(2)}, {92, true, tPlus(3)},
	}
	for _, e := range scores {
		if _, err := s.InsertEvaluation(ctx, Evaluation{
			ClusterID: cid, SnapshotID: sid, Target: "1.36",
			Score: e.score, Ready: e.ready, CreatedAt: e.at,
		}); err != nil {
			t.Fatalf("InsertEvaluation: %v", err)
		}
	}
	// Different target must not leak into 1.36 history.
	if _, err := s.InsertEvaluation(ctx, Evaluation{
		ClusterID: cid, SnapshotID: sid, Target: "1.37", Score: 10, CreatedAt: tBase,
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
		{"limit larger than rows", 10, []int{70, 75, 80, 92}},
		{"most recent 2, returned oldest-first", 2, []int{80, 92}},
		{"most recent 1", 1, []int{92}},
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
			if last := got[len(got)-1]; last.Score == 92 && !last.Ready {
				t.Error("ready flag lost on final point")
			}
		})
	}

	// Unknown cluster: empty history, nil error — NOT ErrNotFound.
	empty, err := s.ScoreHistory(ctx, 999, "1.36", 5)
	if err != nil {
		t.Errorf("ScoreHistory(unknown) err = %v, want nil", err)
	}
	if len(empty) != 0 {
		t.Errorf("ScoreHistory(unknown) = %+v, want empty", empty)
	}
}

func TestNotFoundSentinels(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
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
			if err := tt.call(); !errors.Is(err, ErrNotFound) {
				t.Errorf("err = %v, want errors.Is(err, ErrNotFound)", err)
			}
		})
	}
}

func TestTimesStoredUTCFixedWidth(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	pkt := time.FixedZone("PKT", 5*3600)
	zoned := time.Date(2026, 6, 10, 17, 0, 0, 123456789, pkt) // == 12:00:00.123456789Z

	id, err := s.UpsertCluster(ctx, Cluster{Name: "tz", FirstSeen: zoned, LastSeen: zoned})
	if err != nil {
		t.Fatalf("UpsertCluster: %v", err)
	}
	got, err := s.GetCluster(ctx, id)
	if err != nil {
		t.Fatalf("GetCluster: %v", err)
	}
	if !got.FirstSeen.Equal(zoned) {
		t.Errorf("FirstSeen = %v, want same instant as %v", got.FirstSeen, zoned)
	}
	if got.FirstSeen.Location() != time.UTC {
		t.Errorf("FirstSeen location = %v, want UTC", got.FirstSeen.Location())
	}

	var raw string
	if err := s.db.QueryRow(`SELECT first_seen FROM clusters WHERE id = ?`, id).Scan(&raw); err != nil {
		t.Fatalf("raw select: %v", err)
	}
	const want = "2026-06-10T12:00:00.123456789Z"
	if raw != want {
		t.Errorf("stored text = %q, want %q (UTC, RFC 3339, fixed 9-digit nanos)", raw, want)
	}
}

func TestCloseThenOperationsFail(t *testing.T) {
	s := newTestStore(t)
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := s.GetCluster(context.Background(), 1); err == nil {
		t.Error("GetCluster after Close succeeded, want error")
	}
}
