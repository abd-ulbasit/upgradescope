package storetest

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/abd-ulbasit/upgradescope/internal/server/store"
)

// Conformance for evaluation passes: current-state reads, the atomic
// CommitEvaluations write, and the notification outbox.

func mustEval(t *testing.T, s store.Store, e store.Evaluation) int64 {
	t.Helper()
	id, err := s.InsertEvaluation(context.Background(), e)
	if err != nil {
		t.Fatalf("InsertEvaluation(%s): %v", e.Target, err)
	}
	return id
}

// testEvaluationFreshnessFields pins EvaluatedAt (defaults to CreatedAt)
// and TeamMapHash round-tripping through every evaluation read.
func testEvaluationFreshnessFields(t *testing.T, s store.Store) {
	ctx := context.Background()
	cid := mustCluster(t, s, "prod")
	sid := mustSnapshot(t, s, cid, "aaa", base)
	mustEval(t, s, store.Evaluation{ClusterID: cid, SnapshotID: sid, Target: "1.36", Score: 90, Ready: true, CreatedAt: base, TeamMapHash: "tm-1"})
	mustEval(t, s, store.Evaluation{ClusterID: cid, SnapshotID: sid, Target: "1.37", Score: 90, Ready: true, CreatedAt: base, EvaluatedAt: at(2)})

	for name, read := range map[string]func(string) (store.Evaluation, error){
		"LatestEvaluation":      func(tg string) (store.Evaluation, error) { return s.LatestEvaluation(ctx, cid, tg) },
		"CurrentEvaluation":     func(tg string) (store.Evaluation, error) { return s.CurrentEvaluation(ctx, cid, tg) },
		"LatestKnownEvaluation": func(tg string) (store.Evaluation, error) { return s.LatestKnownEvaluation(ctx, cid, tg) },
	} {
		got, err := read("1.36")
		if err != nil {
			t.Fatalf("%s(1.36): %v", name, err)
		}
		if !got.EvaluatedAt.Equal(base) || got.EvaluatedAt.Location() != time.UTC {
			t.Errorf("%s: EvaluatedAt = %v, want CreatedAt %v (UTC) by default", name, got.EvaluatedAt, base)
		}
		if got.TeamMapHash != "tm-1" {
			t.Errorf("%s: TeamMapHash = %q, want tm-1", name, got.TeamMapHash)
		}
		got37, err := read("1.37")
		if err != nil {
			t.Fatalf("%s(1.37): %v", name, err)
		}
		if !got37.EvaluatedAt.Equal(at(2)) || !got37.CreatedAt.Equal(base) {
			t.Errorf("%s: 1.37 created %v evaluated %v, want %v / %v", name, got37.CreatedAt, got37.EvaluatedAt, base, at(2))
		}
	}
}

// testCurrentEvaluation pins the read-path rule: only evaluations of the
// cluster's latest snapshot are current. After a new snapshot, the old
// snapshot's verdicts are history, not the cluster's state.
func testCurrentEvaluation(t *testing.T, s store.Store) {
	ctx := context.Background()
	cid := mustCluster(t, s, "prod")
	if _, err := s.CurrentEvaluation(ctx, cid, "1.35"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("no snapshot: err = %v, want ErrNotFound", err)
	}
	old := mustSnapshot(t, s, cid, "v134-with-psp", base)
	mustEval(t, s, store.Evaluation{ClusterID: cid, SnapshotID: old, Target: "1.35", Score: 75, Blockers: 1, CreatedAt: base})
	mustEval(t, s, store.Evaluation{ClusterID: cid, SnapshotID: old, Target: "1.36", Score: 75, Blockers: 1, CreatedAt: base})
	if got, err := s.CurrentEvaluation(ctx, cid, "1.35"); err != nil || got.SnapshotID != old {
		t.Fatalf("CurrentEvaluation before upgrade = (%+v, %v), want the only snapshot's row", got, err)
	}

	cur := mustSnapshot(t, s, cid, "v135-no-psp", at(1))
	if _, err := s.CurrentEvaluation(ctx, cid, "1.35"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("1.35 after upgrade: err = %v, want ErrNotFound (old snapshot's row is not current)", err)
	}
	if got, err := s.LatestEvaluation(ctx, cid, "1.35"); err != nil || got.SnapshotID != old {
		t.Errorf("LatestEvaluation still returns history = (%+v, %v)", got, err)
	}
	mustEval(t, s, store.Evaluation{ClusterID: cid, SnapshotID: cur, Target: "1.36", Score: 100, Ready: true, CreatedAt: at(1)})
	got, err := s.CurrentEvaluation(ctx, cid, "1.36")
	if err != nil {
		t.Fatalf("CurrentEvaluation(1.36): %v", err)
	}
	if got.SnapshotID != cur || got.Score != 100 || got.Blockers != 0 {
		t.Errorf("current 1.36 = snapshot %d score %d blockers %d, want snapshot %d score 100 blockers 0", got.SnapshotID, got.Score, got.Blockers, cur)
	}
}

// testCurrentEvaluationIgnoresCreatedAt pins that "current", "latest" and
// history order are insertion order (id), never the stored timestamp. A
// server whose clock stepped backwards stamps a newer row with an older
// time; ordering by created_at kept serving the future-dated row as
// current forever, and every pass inserted a row that never became
// current. Times written in another zone or at sub-microsecond precision
// (Postgres keeps microseconds) cannot reorder anything either.
func testCurrentEvaluationIgnoresCreatedAt(t *testing.T, s store.Store) {
	ctx := context.Background()
	cid := mustCluster(t, s, "prod")
	sid := mustSnapshot(t, s, cid, "aaa", base)
	future := mustEval(t, s, store.Evaluation{ClusterID: cid, SnapshotID: sid, Target: "1.36", Score: 75, Blockers: 1, CreatedAt: at(48)})
	// The clock stepped back two days: the newer row is stamped earlier,
	// in another zone, with a sub-microsecond remainder.
	pkt := time.FixedZone("PKT", 5*3600)
	newer := mustEval(t, s, store.Evaluation{ClusterID: cid, SnapshotID: sid, Target: "1.36", Score: 50, Blockers: 2,
		CreatedAt: at(1).In(pkt).Add(500 * time.Nanosecond)})

	for name, read := range map[string]func() (store.Evaluation, error){
		"CurrentEvaluation":     func() (store.Evaluation, error) { return s.CurrentEvaluation(ctx, cid, "1.36") },
		"LatestEvaluation":      func() (store.Evaluation, error) { return s.LatestEvaluation(ctx, cid, "1.36") },
		"LatestKnownEvaluation": func() (store.Evaluation, error) { return s.LatestKnownEvaluation(ctx, cid, "1.36") },
	} {
		got, err := read()
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if got.ID != newer || got.Score != 50 {
			t.Errorf("%s = evaluation %d (score %d), want %d (inserted last), not the future-dated %d", name, got.ID, got.Score, newer, future)
		}
		if got.CreatedAt.Location() != time.UTC || got.CreatedAt.Sub(at(1)) < 0 || got.CreatedAt.Sub(at(1)) >= time.Microsecond {
			t.Errorf("%s: CreatedAt = %v, want %v as UTC (to the microsecond)", name, got.CreatedAt, at(1))
		}
	}
	hist, err := s.ScoreHistory(ctx, cid, "1.36", 0)
	if err != nil || len(hist) != 2 || hist[0].Score != 75 || hist[1].Score != 50 {
		t.Errorf("ScoreHistory = (%+v, %v), want [75 50] in insertion order", hist, err)
	}
	if last, err := s.ScoreHistory(ctx, cid, "1.36", 1); err != nil || len(last) != 1 || last[0].Score != 50 {
		t.Errorf("ScoreHistory(limit 1) = (%+v, %v), want the last inserted point (50)", last, err)
	}
	// A pass that saw the newer row as current commits; one that saw the
	// future-dated row is stale.
	if _, _, err := s.CommitEvaluations(ctx, store.EvaluationBatch{ClusterID: cid, SnapshotID: sid,
		Current: map[string]int64{"1.36": future}}); !errors.Is(err, store.ErrConflict) {
		t.Errorf("commit against the future-dated row: err = %v, want ErrConflict", err)
	}
	if _, _, err := s.CommitEvaluations(ctx, store.EvaluationBatch{ClusterID: cid, SnapshotID: sid,
		Current: map[string]int64{"1.36": newer}}); err != nil {
		t.Errorf("commit against the current row: %v", err)
	}
}

// testLatestKnownEvaluation pins the notification baseline: an unknown
// verdict (not ready, no blockers) is skipped.
func testLatestKnownEvaluation(t *testing.T, s store.Store) {
	ctx := context.Background()
	cid := mustCluster(t, s, "prod")
	sid := mustSnapshot(t, s, cid, "aaa", base)
	if _, err := s.LatestKnownEvaluation(ctx, cid, "1.36"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("no rows: err = %v, want ErrNotFound", err)
	}
	mustEval(t, s, store.Evaluation{ClusterID: cid, SnapshotID: sid, Target: "1.36", Score: 75, Blockers: 1, CreatedAt: base})
	mustEval(t, s, store.Evaluation{ClusterID: cid, SnapshotID: sid, Target: "1.36", Score: 100, CreatedAt: at(1)}) // unknown
	got, err := s.LatestKnownEvaluation(ctx, cid, "1.36")
	if err != nil {
		t.Fatalf("LatestKnownEvaluation: %v", err)
	}
	if got.Score != 75 || got.Blockers != 1 {
		t.Errorf("baseline = score %d blockers %d, want the blocked row (75/1), skipping unknown", got.Score, got.Blockers)
	}
	mustEval(t, s, store.Evaluation{ClusterID: cid, SnapshotID: sid, Target: "1.36", Score: 100, Ready: true, CreatedAt: at(2)})
	if got, err := s.LatestKnownEvaluation(ctx, cid, "1.36"); err != nil || !got.Ready {
		t.Errorf("after a ready row: (%+v, %v), want the ready row", got, err)
	}
}

func outboxMsg(sink, payload string, created time.Time) store.OutboxMessage {
	return store.OutboxMessage{Sink: sink, Payload: []byte(payload), CreatedAt: created}
}

// testCommitNewSnapshot pins the atomic ingest write: snapshot, its
// evaluations and outbox messages land together; a duplicate writes none.
func testCommitNewSnapshot(t *testing.T, s store.Store) {
	ctx := context.Background()
	cid := mustCluster(t, s, "prod")
	batch := store.EvaluationBatch{
		ClusterID: cid,
		Snapshot:  &store.Snapshot{ClusterID: cid, Hash: "aaa", ReceivedAt: base, Inventory: []byte(`{"hash":"aaa"}`)},
		Insert: []store.Evaluation{
			{ClusterID: cid, Target: "1.36", Score: 75, Blockers: 1, Report: []byte(`{"t":"1.36"}`), CreatedAt: base},
			{ClusterID: cid, Target: "1.37", Score: 75, Blockers: 1, Report: []byte(`{"t":"1.37"}`), CreatedAt: base},
		},
		Outbox: []store.OutboxMessage{outboxMsg("slack", `{"n":1}`, base), outboxMsg("webhook", `{"n":1}`, base)},
	}
	sid, dup, err := s.CommitEvaluations(ctx, batch)
	if err != nil || dup {
		t.Fatalf("CommitEvaluations = (%d, %v, %v), want a new snapshot", sid, dup, err)
	}
	if latest, err := s.LatestSnapshot(ctx, cid); err != nil || latest.ID != sid || latest.Hash != "aaa" {
		t.Fatalf("LatestSnapshot = (%+v, %v), want id %d", latest, err, sid)
	}
	for _, tg := range []string{"1.36", "1.37"} {
		got, err := s.CurrentEvaluation(ctx, cid, tg)
		if err != nil {
			t.Fatalf("CurrentEvaluation(%s): %v", tg, err)
		}
		if got.SnapshotID != sid || !bytes.Equal(got.Report, []byte(`{"t":"`+tg+`"}`)) {
			t.Errorf("%s = snapshot %d report %s, want snapshot %d and its own report", tg, got.SnapshotID, got.Report, sid)
		}
	}
	msgs, err := s.ClaimOutbox(ctx, at(1), time.Minute, 10)
	if err != nil {
		t.Fatalf("ClaimOutbox: %v", err)
	}
	if len(msgs) != 2 || msgs[0].Sink != "slack" || msgs[1].Sink != "webhook" || string(msgs[0].Payload) != `{"n":1}` {
		t.Fatalf("outbox = %+v, want the two committed messages in order", msgs)
	}

	batch.Snapshot = &store.Snapshot{ClusterID: cid, Hash: "aaa", ReceivedAt: at(1), Inventory: []byte(`{"hash":"aaa"}`)}
	dupID, dup, err := s.CommitEvaluations(ctx, batch)
	if err != nil || !dup || dupID != sid {
		t.Fatalf("duplicate commit = (%d, %v, %v), want (%d, true, nil)", dupID, dup, err, sid)
	}
	if hist, _ := s.ScoreHistory(ctx, cid, "1.36", 0); len(hist) != 1 {
		t.Errorf("duplicate commit wrote evaluations: history has %d points, want 1", len(hist))
	}
	if more, _ := s.ClaimOutbox(ctx, at(5), time.Minute, 10); len(more) != 2 {
		t.Errorf("duplicate commit changed the outbox: %d due messages, want the original 2", len(more))
	}
}

// testCommitIsAtomic forces a failure after the snapshot and an insert are
// written (a refresh of a row that does not exist): nothing may remain.
func testCommitIsAtomic(t *testing.T, s store.Store) {
	ctx := context.Background()
	cid := mustCluster(t, s, "prod")
	_, _, err := s.CommitEvaluations(ctx, store.EvaluationBatch{
		ClusterID: cid,
		Snapshot:  &store.Snapshot{ClusterID: cid, Hash: "aaa", ReceivedAt: base, Inventory: []byte(`{}`)},
		Insert:    []store.Evaluation{{ClusterID: cid, Target: "1.36", Score: 90, Ready: true, CreatedAt: base}},
		Refresh:   []store.Evaluation{{ID: 999999, ClusterID: cid, Target: "1.37", EvaluatedAt: base}},
		Outbox:    []store.OutboxMessage{outboxMsg("slack", `{}`, base)},
	})
	if err == nil {
		t.Fatal("commit with a refresh of a missing row succeeded, want error")
	}
	if _, err := s.LatestSnapshot(ctx, cid); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("failed commit left a snapshot (err %v), want none", err)
	}
	if _, err := s.LatestEvaluation(ctx, cid, "1.36"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("failed commit left an evaluation (err %v), want none", err)
	}
	if msgs, _ := s.ClaimOutbox(ctx, at(1), time.Minute, 10); len(msgs) != 0 {
		t.Errorf("failed commit left %d outbox messages, want 0", len(msgs))
	}
}

// testCommitExistingSnapshot pins re-evaluation of a stored snapshot:
// refresh keeps the history point and updates freshness, inserts add rows,
// and a stale view of the snapshot (Current, SnapshotID) is ErrConflict.
func testCommitExistingSnapshot(t *testing.T, s store.Store) {
	ctx := context.Background()
	cid := mustCluster(t, s, "prod")
	sid := mustSnapshot(t, s, cid, "aaa", base)
	e36 := mustEval(t, s, store.Evaluation{ClusterID: cid, SnapshotID: sid, Target: "1.36", KBVersion: "kb-1",
		Score: 95, Ready: true, Warnings: 1, Report: []byte(`{"v":1}`), CreatedAt: base})

	refreshed := store.Evaluation{ID: e36, ClusterID: cid, SnapshotID: sid, Target: "1.36", KBVersion: "kb-2",
		Score: 95, Ready: true, Warnings: 2, Report: []byte(`{"v":2}`), TeamMapHash: "tm", EvaluatedAt: at(24)}
	got, dup, err := s.CommitEvaluations(ctx, store.EvaluationBatch{
		ClusterID: cid, SnapshotID: sid,
		Current: map[string]int64{"1.36": e36, "1.37": 0},
		Refresh: []store.Evaluation{refreshed},
		Insert:  []store.Evaluation{{ClusterID: cid, Target: "1.37", Score: 75, Blockers: 1, CreatedAt: at(24)}},
	})
	if err != nil || dup || got != sid {
		t.Fatalf("CommitEvaluations = (%d, %v, %v), want (%d, false, nil)", got, dup, err, sid)
	}
	cur, err := s.CurrentEvaluation(ctx, cid, "1.36")
	if err != nil {
		t.Fatal(err)
	}
	if cur.ID != e36 || cur.KBVersion != "kb-2" || cur.Warnings != 2 || cur.TeamMapHash != "tm" ||
		!bytes.Equal(cur.Report, []byte(`{"v":2}`)) || !cur.EvaluatedAt.Equal(at(24)) || !cur.CreatedAt.Equal(base) {
		t.Errorf("refreshed 1.36 = %+v (report %s), want same row with kb-2, 2 warnings, tm, new report, evaluated %v, created %v", cur, cur.Report, at(24), base)
	}
	if hist, _ := s.ScoreHistory(ctx, cid, "1.36", 0); len(hist) != 1 {
		t.Errorf("refresh added history: %d points, want 1", len(hist))
	}
	e37, err := s.CurrentEvaluation(ctx, cid, "1.37")
	if err != nil || e37.SnapshotID != sid {
		t.Fatalf("inserted 1.37 = (%+v, %v), want a row on snapshot %d", e37, err, sid)
	}

	// Stale Current: the pass thought 1.37 had no evaluation.
	_, _, err = s.CommitEvaluations(ctx, store.EvaluationBatch{
		ClusterID: cid, SnapshotID: sid,
		Current: map[string]int64{"1.37": 0},
		Insert:  []store.Evaluation{{ClusterID: cid, Target: "1.37", Score: 75, Blockers: 1, CreatedAt: at(25)}},
		Outbox:  []store.OutboxMessage{outboxMsg("slack", `{}`, at(25))},
	})
	if !errors.Is(err, store.ErrConflict) {
		t.Fatalf("stale Current: err = %v, want ErrConflict", err)
	}
	if hist, _ := s.ScoreHistory(ctx, cid, "1.37", 0); len(hist) != 1 {
		t.Errorf("conflicting commit wrote: 1.37 history has %d points, want 1", len(hist))
	}
	if msgs, _ := s.ClaimOutbox(ctx, at(30), time.Minute, 10); len(msgs) != 0 {
		t.Errorf("conflicting commit wrote %d outbox messages, want 0", len(msgs))
	}

	// Stale SnapshotID: a newer snapshot arrived meanwhile.
	mustSnapshot(t, s, cid, "bbb", at(26))
	_, _, err = s.CommitEvaluations(ctx, store.EvaluationBatch{
		ClusterID: cid, SnapshotID: sid,
		Current: map[string]int64{"1.38": 0},
		Insert:  []store.Evaluation{{ClusterID: cid, Target: "1.38", Score: 100, Ready: true, CreatedAt: at(27)}},
	})
	if !errors.Is(err, store.ErrConflict) {
		t.Fatalf("superseded snapshot: err = %v, want ErrConflict", err)
	}
}

// testOutbox pins the delivery queue: due messages are claimed oldest
// first and leased; a leased message is not re-claimed until the lease
// ends; Reschedule and Delete act by id.
func testOutbox(t *testing.T, s store.Store) {
	ctx := context.Background()
	cid := mustCluster(t, s, "prod")
	if _, _, err := s.CommitEvaluations(ctx, store.EvaluationBatch{
		ClusterID: cid,
		Snapshot:  &store.Snapshot{ClusterID: cid, Hash: "aaa", ReceivedAt: base, Inventory: []byte(`{}`)},
		Outbox: []store.OutboxMessage{
			outboxMsg("slack", `{"n":1}`, base),
			outboxMsg("slack", `{"n":2}`, base),
			{Sink: "slack", Payload: []byte(`{"n":3}`), CreatedAt: base, NextAttemptAt: at(10)}, // not due yet
		},
	}); err != nil {
		t.Fatalf("CommitEvaluations: %v", err)
	}
	if none, err := s.ClaimOutbox(ctx, base.Add(-time.Second), time.Minute, 10); err != nil || len(none) != 0 {
		t.Fatalf("claim before anything is due = (%+v, %v), want none", none, err)
	}
	first, err := s.ClaimOutbox(ctx, base, time.Minute, 1)
	if err != nil || len(first) != 1 || string(first[0].Payload) != `{"n":1}` || first[0].Attempts != 1 {
		t.Fatalf("claim limit 1 = (%+v, %v), want n=1 with attempts 1", first, err)
	}
	if !first[0].CreatedAt.Equal(base) || first[0].CreatedAt.Location() != time.UTC {
		t.Errorf("CreatedAt = %v, want %v UTC", first[0].CreatedAt, base)
	}
	second, err := s.ClaimOutbox(ctx, base, time.Minute, 10)
	if err != nil || len(second) != 1 || string(second[0].Payload) != `{"n":2}` {
		t.Fatalf("second claim = (%+v, %v), want only n=2 (n=1 leased, n=3 not due)", second, err)
	}

	if err := s.DeleteOutbox(ctx, first[0].ID); err != nil {
		t.Fatalf("DeleteOutbox: %v", err)
	}
	if err := s.RescheduleOutbox(ctx, second[0].ID, at(2), "status 503"); err != nil {
		t.Fatalf("RescheduleOutbox: %v", err)
	}
	if got, _ := s.ClaimOutbox(ctx, at(1), time.Minute, 10); len(got) != 0 {
		t.Errorf("claim at +1h = %+v, want none (n=2 rescheduled to +2h)", got)
	}
	again, err := s.ClaimOutbox(ctx, at(2), time.Minute, 10)
	if err != nil || len(again) != 1 || again[0].ID != second[0].ID || again[0].Attempts != 2 {
		t.Fatalf("claim at +2h = (%+v, %v), want n=2 with attempts 2", again, err)
	}
	// A crashed delivery: the lease runs out and the message is due again.
	if got, _ := s.ClaimOutbox(ctx, at(2).Add(2*time.Minute), time.Minute, 10); len(got) != 1 || got[0].Attempts != 3 {
		t.Errorf("claim after the lease ran out = %+v, want n=2 again with attempts 3", got)
	}
	if err := s.DeleteOutbox(ctx, first[0].ID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("DeleteOutbox(gone) err = %v, want ErrNotFound", err)
	}
	if err := s.RescheduleOutbox(ctx, first[0].ID, at(3), "x"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("RescheduleOutbox(gone) err = %v, want ErrNotFound", err)
	}
}
