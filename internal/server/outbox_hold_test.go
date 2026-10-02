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
// more than outboxMaxAttempts passes does not lose its queued messages
// within their lifetime (outboxMaxAge); only the call that met each 429
// counts, and it counts against one message.
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

// TestOutboxHoldBoundsMessageLifetime: against a sink that is rate-limited
// for good, every queued message is given up shortly after outboxMaxAge
// since that message was queued (not since the first or the last one), however
// many are queued ahead of it. Without the bound the hold lets one message
// per window be called and the queue drains one message per hold; without
// expiryCap a message is picked up only when its hold ends, which for a
// Retry-After that does not divide outboxMaxAge is well after its expiry.
// CreatedAt is staggered, and not in ID order, so a message queued behind
// older ones with a later expiry is covered.
func TestOutboxHoldBoundsMessageLifetime(t *testing.T) {
	// Minutes each message was already queued when the passes start.
	ages := []time.Duration{0, 300, 20, 180, 90, 420, 45, 240, 7, 150}
	for _, tc := range []struct {
		name  string
		after time.Duration
	}{
		{"hold divides the lifetime", outboxMaxRetryAfter},
		{"hold does not divide the lifetime", 50 * time.Minute},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := newFakeStore()
			limited := &rateLimitedNotifier{after: tc.after}
			clock := &fakeClock{t: time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC)}
			s := newTestServer(t, st, func(c *Config) { c.Notifier = limited })
			s.now = clock.now
			ts := httptest.NewServer(s.Handler())
			defer ts.Close()
			blockedThenClean(t, ts)
			queueCopies(st, len(ages)-1)
			start := clock.now()
			created := map[int64]time.Time{}
			queued := map[int64]time.Duration{} // how long before the start each was queued
			st.mu.Lock()
			for i := range st.outbox {
				st.outbox[i].CreatedAt = start.Add(-ages[i] * time.Minute)
				created[st.outbox[i].ID] = st.outbox[i].CreatedAt
				queued[st.outbox[i].ID] = ages[i] * time.Minute
			}
			st.mu.Unlock()

			// A pass every minute, so each bound is measured to within a minute.
			step := time.Minute
			for clock.now().Sub(start) <= outboxMaxAge+3*time.Hour && len(st.outbox) > 0 {
				before := map[int64]int{}
				for _, m := range st.outbox {
					before[m.ID] = m.Attempts
				}
				s.deliverOutbox(context.Background())
				for id, attempts := range before {
					if slices.ContainsFunc(st.outbox, func(m store.OutboxMessage) bool { return m.ID == id }) {
						continue
					}
					expiry := created[id].Add(outboxMaxAge)
					now := clock.now()
					if now.Before(expiry) && attempts != outboxMaxAttempts-1 {
						// Only the attempt cap may end a message earlier.
						t.Errorf("message %d gone %v before its expiry without reaching %d attempts", id, expiry.Sub(now), outboxMaxAttempts)
					}
					if late := now.Sub(expiry); late > outboxPoll+step {
						t.Errorf("message %d gone %v after its expiry (queued %v before the start), want within %v",
							id, late, queued[id], outboxPoll+step)
					}
				}
				clock.set(clock.now().Add(step))
			}
			if n := len(st.outbox); n != 0 {
				t.Fatalf("%d messages still queued %v after the start, want none past their outboxMaxAge (%v)",
					n, clock.now().Sub(start), outboxMaxAge)
			}
		})
	}
}

// TestOutboxExpiredMessageNotSent: a message older than outboxMaxAge is
// dropped without a call, even to a healthy sink (it would arrive stale).
func TestOutboxExpiredMessageNotSent(t *testing.T) {
	st := newFakeStore()
	sink := &recordingNotifier{}
	clock := &fakeClock{t: time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC)}
	s := newTestServer(t, st, func(c *Config) { c.Notifier = sink })
	s.now = clock.now
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()
	blockedThenClean(t, ts)
	clock.set(clock.now().Add(outboxMaxAge + time.Minute))
	s.deliverOutbox(context.Background())
	if n := len(st.outbox); n != 0 {
		t.Errorf("%d expired messages still queued, want 0", n)
	}
	if n := len(sink.notifications()); n != 0 {
		t.Errorf("sink received %d expired notifications, want 0", n)
	}
}
