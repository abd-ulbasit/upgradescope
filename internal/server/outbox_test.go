package server

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/abd-ulbasit/upgradescope/internal/server/notify"
	"github.com/abd-ulbasit/upgradescope/internal/server/store"
)

// blockingNotifier never returns until its context ends.
type blockingNotifier struct{ calls chan struct{} }

func (b *blockingNotifier) Notify(ctx context.Context, _ notify.Notification) error {
	b.calls <- struct{}{}
	<-ctx.Done()
	return ctx.Err()
}

// flakyNotifier fails its first n deliveries, then records.
type flakyNotifier struct {
	recordingNotifier
	mu    sync.Mutex
	fails int
}

func (f *flakyNotifier) Notify(ctx context.Context, n notify.Notification) error {
	f.mu.Lock()
	if f.fails > 0 {
		f.fails--
		f.mu.Unlock()
		return errors.New("unexpected status 503")
	}
	f.mu.Unlock()
	return f.recordingNotifier.Notify(ctx, n)
}

// blockedThenClean pushes a PSP inventory, then a clean one: the second
// push produces exactly one became-ready event (target 1.35).
func blockedThenClean(t *testing.T, ts *httptest.Server) {
	t.Helper()
	for _, body := range [][]byte{pushReqBody(t, testInventoryWithPSP()), pushReqBody(t, testInventory())} {
		if resp, out := postSnapshot(t, ts, "ingest-tok", body, false); resp.StatusCode != http.StatusAccepted {
			t.Fatalf("push = %d %v", resp.StatusCode, out)
		}
	}
}

// TestIngestDoesNotWaitForNotifiers: a notifier that hangs forever must not
// slow a push down — events are committed to the outbox with the
// evaluations and delivered by the background worker.
func TestIngestDoesNotWaitForNotifiers(t *testing.T) {
	st := newFakeStore()
	hang := &blockingNotifier{calls: make(chan struct{}, 10)}
	s := newTestServer(t, st, func(c *Config) { c.Notifier = hang })
	s.notifyTimeout = 200 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { s.runOutbox(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	start := time.Now()
	blockedThenClean(t, ts)
	if d := time.Since(start); d > time.Second {
		t.Fatalf("two pushes took %v with a hanging notifier, want < 1s", d)
	}
	select {
	case <-hang.calls: // the worker picked the event up after commit
	case <-time.After(5 * time.Second):
		t.Fatal("worker never attempted delivery")
	}
}

// TestIngestCommitFailureFailsWholeIngest: snapshot and evaluations are one
// write — when it fails the push is a 500 and nothing is stored, so the
// agent's retry is not swallowed as a duplicate.
func TestIngestCommitFailureFailsWholeIngest(t *testing.T) {
	st := newFakeStore()
	st.errs["CommitEvaluations"] = errors.New("disk full")
	ts := httptest.NewServer(newTestServer(t, st, func(c *Config) { c.ExtraTargets = []string{"1.37"} }).Handler())
	defer ts.Close()
	resp, _ := postSnapshot(t, ts, "ingest-tok", pushReqBody(t, testInventoryWithPSP()), false)
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", resp.StatusCode)
	}
	if len(st.snapshots) != 0 || len(st.evals) != 0 {
		t.Fatalf("failed ingest left snapshots %d evals %d, want none", len(st.snapshots), len(st.evals))
	}

	delete(st.errs, "CommitEvaluations")
	if resp, _ := postSnapshot(t, ts, "ingest-tok", pushReqBody(t, testInventoryWithPSP()), false); resp.StatusCode != http.StatusAccepted {
		t.Fatalf("retry = %d, want 202 (a real ingest, not a duplicate)", resp.StatusCode)
	}
	if len(st.evals) != 2 {
		t.Fatalf("retry stored %d evaluations, want 2 (1.35 and 1.37)", len(st.evals))
	}
}

// TestDuplicatePushBackfillsMissingTarget repairs databases hit by the old
// non-atomic ingest: a target with no evaluation for the latest snapshot
// is evaluated on the next duplicate push.
func TestDuplicatePushBackfillsMissingTarget(t *testing.T) {
	st := newFakeStore()
	ts := httptest.NewServer(newTestServer(t, st, func(c *Config) { c.ExtraTargets = []string{"1.37"} }).Handler())
	defer ts.Close()
	body := pushReqBody(t, testInventoryWithPSP())
	if resp, _ := postSnapshot(t, ts, "ingest-tok", body, false); resp.StatusCode != http.StatusAccepted {
		t.Fatalf("first push = %d", resp.StatusCode)
	}
	st.mu.Lock()
	st.evals = slices.DeleteFunc(st.evals, func(e store.Evaluation) bool { return e.Target == "1.37" })
	st.mu.Unlock()

	if resp, out := postSnapshot(t, ts, "ingest-tok", body, false); resp.StatusCode != http.StatusOK || out["duplicate"] != true {
		t.Fatalf("re-push = %d %v, want 200 duplicate", resp.StatusCode, out)
	}
	e, err := st.CurrentEvaluation(context.Background(), 1, "1.37")
	if err != nil {
		t.Fatalf("1.37 after backfill: %v", err)
	}
	if e.Ready || e.Blockers != 1 || e.Score != 75 {
		t.Errorf("backfilled 1.37 = score %d ready %v blockers %d, want 75 false 1 (PSP removed)", e.Score, e.Ready, e.Blockers)
	}
	if n := len(st.evals); n != 2 {
		t.Errorf("evaluations = %d, want 2 (1.35 untouched, 1.37 backfilled)", n)
	}
}

// TestOutboxRetriesThenDeliversOnce: a sink that fails once gets the event
// exactly once, after the backoff.
func TestOutboxRetriesThenDeliversOnce(t *testing.T) {
	st := newFakeStore()
	sink := &flakyNotifier{fails: 1}
	clock := &fakeClock{t: time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC)}
	s := newTestServer(t, st, func(c *Config) { c.Notifier = sink })
	s.now = clock.now
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()
	blockedThenClean(t, ts)

	ctx := context.Background()
	s.deliverOutbox(ctx)
	if got := sink.all(); len(got) != 0 {
		t.Fatalf("first attempt should fail, delivered %+v", got)
	}
	s.deliverOutbox(ctx) // not due yet: backoff
	if got := sink.all(); len(got) != 0 {
		t.Fatalf("retried before the backoff: %+v", got)
	}
	clock.set(clock.now().Add(outboxBackoff(1) + time.Second))
	s.deliverOutbox(ctx)
	s.deliverOutbox(ctx)
	got := sink.all()
	if len(got) != 1 || got[0].Kind != notify.KindBecameReady {
		t.Fatalf("delivered %+v, want exactly one became-ready", got)
	}
	if len(st.outbox) != 0 {
		t.Errorf("outbox still holds %d messages after delivery", len(st.outbox))
	}
}

// TestOutboxGivesUpAfterMaxAttempts: retries are bounded.
func TestOutboxGivesUpAfterMaxAttempts(t *testing.T) {
	st := newFakeStore()
	sink := &flakyNotifier{fails: 1000}
	clock := &fakeClock{t: time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC)}
	s := newTestServer(t, st, func(c *Config) { c.Notifier = sink })
	s.now = clock.now
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()
	blockedThenClean(t, ts)

	for range outboxMaxAttempts + 2 {
		s.deliverOutbox(context.Background())
		clock.set(clock.now().Add(2 * outboxMaxBackoff))
	}
	if n := len(st.outbox); n != 0 {
		t.Fatalf("outbox holds %d messages after %d failed attempts, want 0 (given up)", n, outboxMaxAttempts)
	}
	if left := sink.fails; left != 1000-outboxMaxAttempts {
		t.Errorf("attempts = %d, want %d", 1000-left, outboxMaxAttempts)
	}
}

// TestOutboxFansOutPerSink: each configured notifier gets its own outbox
// message, so one sink's retry never re-sends to another that succeeded.
func TestOutboxFansOutPerSink(t *testing.T) {
	st := newFakeStore()
	ok := &recordingNotifier{}
	flaky := &flakyNotifier{fails: 1}
	clock := &fakeClock{t: time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC)}
	s := newTestServer(t, st, func(c *Config) { c.Notifier = notify.Multi(ok, flaky) })
	s.now = clock.now
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()
	blockedThenClean(t, ts)

	s.deliverOutbox(context.Background())
	clock.set(clock.now().Add(outboxMaxBackoff))
	s.deliverOutbox(context.Background())
	if len(ok.all()) != 1 || len(flaky.all()) != 1 {
		t.Fatalf("deliveries: healthy sink %d, flaky sink %d — want 1 each", len(ok.all()), len(flaky.all()))
	}
}

// TestOutboxErrorIsStorable: a notifier error is stored as the message's
// last_error, and Postgres TEXT rejects NUL bytes and invalid UTF-8; a
// rejected reschedule would leave the retry to the lease. The stored text
// is valid UTF-8, NUL-free and bounded.
func TestOutboxErrorIsStorable(t *testing.T) {
	got := outboxError(errors.New("webhook said: a\x00b\xffc " + strings.Repeat("é", 2000)))
	if strings.ContainsRune(got, 0) || !utf8.ValidString(got) || len(got) > outboxMaxErrorLen {
		t.Errorf("outboxError = %d bytes, NUL %v, valid UTF-8 %v; want ≤ %d, no NUL, valid",
			len(got), strings.ContainsRune(got, 0), utf8.ValidString(got), outboxMaxErrorLen)
	}
	if !strings.HasPrefix(got, "webhook said: ab") {
		t.Errorf("outboxError = %.40q…, want the message kept", got)
	}
}
