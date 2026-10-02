package server

import (
	"context"
	"errors"
	"net/http/httptest"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/abd-ulbasit/upgradescope/internal/server/notify"
	"github.com/abd-ulbasit/upgradescope/internal/server/store"
)

// rateLimitedNotifier answers every delivery with a Retry-After error and
// counts the calls.
type rateLimitedNotifier struct {
	mu    sync.Mutex
	calls int
	after time.Duration
}

func (r *rateLimitedNotifier) Notify(context.Context, notify.Notification) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls++
	return &notify.RetryAfterError{Err: errors.New("unexpected status 429"), After: r.after}
}

func (r *rateLimitedNotifier) count() int { r.mu.Lock(); defer r.mu.Unlock(); return r.calls }

// queueCopies adds n copies of every queued outbox message, so a sink has
// a burst waiting (as after a fleet-wide pass).
func queueCopies(st *fakeStore, n int) {
	st.mu.Lock()
	defer st.mu.Unlock()
	for _, m := range slices.Clone(st.outbox) {
		for range n {
			m.ID = st.id()
			st.outbox = append(st.outbox, m)
		}
	}
}

// outboxFor returns the queued messages for sink.
func outboxFor(st *fakeStore, sinkName string) []store.OutboxMessage {
	st.mu.Lock()
	defer st.mu.Unlock()
	var out []store.OutboxMessage
	for _, m := range st.outbox {
		if m.Sink == sinkName {
			out = append(out, m)
		}
	}
	return out
}

// TestOutboxHoldsRateLimitedSink: Retry-After holds the sink, not just the
// message that met it. With a burst queued for a sink that always answers
// 429, one pass calls it once, a later pass inside the delay not at all,
// and the held messages keep their attempts; another sink is untouched.
func TestOutboxHoldsRateLimitedSink(t *testing.T) {
	st := newFakeStore()
	limited := &rateLimitedNotifier{after: 10 * time.Minute}
	healthy := &recordingNotifier{}
	clock := &fakeClock{t: time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC)}
	s := newTestServer(t, st, func(c *Config) { c.Notifier = notify.Multi(limited, healthy) })
	s.now = clock.now
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()
	blockedThenClean(t, ts)
	queueCopies(st, 3) // 4 messages per sink

	limitedSink := s.sinks[0].name
	ctx := context.Background()
	start := clock.now()
	s.deliverOutbox(ctx)
	if n := limited.count(); n != 1 {
		t.Fatalf("rate-limited sink called %d times in one pass, want 1 (the hold covers the rest)", n)
	}
	if n := len(healthy.notifications()); n != 4 {
		t.Errorf("healthy sink delivered %d, want all 4 (a hold on another sink must not touch it)", n)
	}
	held := outboxFor(st, limitedSink)
	if len(held) != 4 {
		t.Fatalf("%d messages held for the limited sink, want 4", len(held))
	}
	attempts := 0
	for _, m := range held {
		attempts += m.Attempts
		if m.NextAttemptAt.Before(start.Add(10 * time.Minute)) {
			t.Errorf("message %d due %v after the 429, want at least the 10m asked for", m.ID, m.NextAttemptAt.Sub(start))
		}
	}
	if attempts != 1 {
		t.Errorf("held messages carry %d attempts in all, want 1 (only the call that met the 429 counts)", attempts)
	}

	// A message that falls due inside the hold (new work for the sink) is
	// deferred without a call and without costing an attempt.
	clock.set(start.Add(time.Minute))
	st.mu.Lock()
	st.outbox = append(st.outbox, store.OutboxMessage{ID: st.id(), Sink: limitedSink, Payload: held[0].Payload,
		CreatedAt: clock.now(), NextAttemptAt: clock.now()})
	st.mu.Unlock()
	s.deliverOutbox(ctx)
	if n := limited.count(); n != 1 {
		t.Fatalf("rate-limited sink called again inside the hold: %d calls", n)
	}
	for _, m := range outboxFor(st, limitedSink) {
		if m.NextAttemptAt.Before(start.Add(10 * time.Minute)) {
			t.Errorf("message %d due %v after the 429, want held to the end of the delay", m.ID, m.NextAttemptAt.Sub(start))
		}
		if m.Attempts > 1 {
			t.Errorf("message %d has %d attempts, want at most 1", m.ID, m.Attempts)
		}
	}

	// Once the hold ends the sink is tried again, once per pass.
	clock.set(start.Add(10*time.Minute + time.Second))
	s.deliverOutbox(ctx)
	if n := limited.count(); n != 2 {
		t.Errorf("rate-limited sink called %d times after the hold, want 2 in all", n)
	}
}

// TestOutboxHoldDoesNotConsumeAttempts: a sink that stays rate-limited for
// more than outboxMaxAttempts passes does not lose its queued messages;
// only the call that met each 429 counts, and it counts against one message.
func TestOutboxHoldDoesNotConsumeAttempts(t *testing.T) {
	st := newFakeStore()
	limited := &rateLimitedNotifier{after: 10 * time.Minute}
	clock := &fakeClock{t: time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC)}
	s := newTestServer(t, st, func(c *Config) { c.Notifier = limited })
	s.now = clock.now
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()
	blockedThenClean(t, ts)
	queueCopies(st, 4) // 5 messages

	for range outboxMaxAttempts - 1 {
		s.deliverOutbox(context.Background())
		clock.set(clock.now().Add(11 * time.Minute))
	}
	if n := len(st.outbox); n != 5 {
		t.Fatalf("outbox holds %d messages, want all 5 still queued", n)
	}
	if n := limited.count(); n != outboxMaxAttempts-1 {
		t.Errorf("sink called %d times in %d passes, want one per pass", n, outboxMaxAttempts-1)
	}
}
