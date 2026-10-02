package server

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/abd-ulbasit/upgradescope/internal/server/notify"
	"github.com/abd-ulbasit/upgradescope/internal/server/store"
)

// Notification delivery. Events are committed to the outbox in the same
// transaction as the evaluations that produced them, one message per
// (event, sink), and a background worker delivers them after commit. So a
// push never waits on a webhook, a restart loses nothing (but messages past
// outboxMaxAge, which are dropped unsent), and a failed delivery is retried
// with exponential backoff — per sink, so a retry never re-sends to a sink
// that already succeeded. Delivery is at-least-once: a
// crash between a successful send and the delete re-sends after the lease.
//
// A sink that answers 429 or 503 with Retry-After is held: until the delay
// has passed (capped at outboxMaxRetryAfter) it is not called for any of
// its messages, which are put back due at the end of the hold without
// consuming an attempt. The hold is in memory (sinkHolds), so a restart or
// another replica forgets it. Because only one message per hold window is
// called, a sink that stays limited would drain its queue one message per
// window, so a message also has a lifetime: it is given up (logged,
// deleted) once outboxMaxAge has passed since it was queued, whatever its
// attempts, shortly after that (within a few minutes: the worker polls every
// outboxPoll, a pass can run longer, and a crash leaves a lease).
const (
	outboxBatch       = 50
	outboxLease       = 2 * time.Minute // a claimed message is re-claimable after this
	outboxMaxAttempts = 8               // then the message is dropped (logged)
	// outboxMaxAge is the longest a message is kept: 8 attempts of the
	// longest wait (outboxMaxRetryAfter) each. It bounds a message held
	// behind a rate-limited sink, which the attempt count alone does not.
	outboxMaxAge      = outboxMaxAttempts * outboxMaxRetryAfter
	outboxBaseBackoff = 30 * time.Second
	outboxMaxBackoff  = time.Hour
	outboxPoll        = 30 * time.Second // retry pickup when nothing kicks the worker
	notifyTimeout     = 30 * time.Second // per delivery, over the notifier's own timeout
	outboxMaxErrorLen = 1024             // bytes of the last delivery error kept
)

// outboxError is err as stored in last_error: NUL bytes and invalid UTF-8
// (which Postgres TEXT rejects, failing the reschedule) dropped, and
// bounded, since a notifier error may quote a response body.
func outboxError(err error) string {
	s := strings.ToValidUTF8(strings.ReplaceAll(err.Error(), "\x00", ""), "")
	if len(s) > outboxMaxErrorLen {
		s = strings.ToValidUTF8(s[:outboxMaxErrorLen], "") // drops a cut rune
	}
	return s
}

// outboxBackoff is the delay after the attempts-th failed attempt:
// 30s, 1m, 2m, … capped at an hour. With outboxMaxAttempts = 8 the
// largest delay used is 32m (attempt 7), about 63m in all before giving up.
// A sink's Retry-After (retryDelay, at most outboxMaxRetryAfter) replaces a
// shorter backoff, so the delays can total more; outboxMaxAge bounds the
// whole, held messages included.
func outboxBackoff(attempts int) time.Duration {
	d := outboxBaseBackoff
	for i := 1; i < attempts && d < outboxMaxBackoff; i++ {
		d *= 2
	}
	return min(d, outboxMaxBackoff)
}

// sink is one configured notifier with the stable name outbox messages
// are routed by.
type sink struct {
	name string
	n    notify.Notifier
}

// sinksOf flattens the configured notifier into named sinks: Slack is
// "slack", the generic webhook "webhook", anything else its Go type; a
// repeated name gets a "#2", "#3" suffix in configuration order.
func sinksOf(n notify.Notifier) []sink {
	var out []sink
	seen := map[string]int{}
	for _, m := range notify.Members(n) {
		var name string
		switch m.(type) {
		case *notify.SlackNotifier:
			name = "slack"
		case *notify.GenericWebhook:
			name = "webhook"
		default:
			name = fmt.Sprintf("%T", m)
		}
		seen[name]++
		if k := seen[name]; k > 1 {
			name = fmt.Sprintf("%s#%d", name, k)
		}
		out = append(out, sink{name: name, n: m})
	}
	return out
}

// kickOutbox wakes the worker after a commit that queued messages.
func (s *Server) kickOutbox() {
	select {
	case s.outboxKick <- struct{}{}:
	default: // a wake-up is already pending
	}
}

// deliverOutbox delivers every message due now, batch by batch, until none
// is left. Returns when the outbox has nothing due or ctx ends.
func (s *Server) deliverOutbox(ctx context.Context) {
	for ctx.Err() == nil {
		msgs, err := s.cfg.Store.ClaimOutbox(ctx, s.now(), outboxLease, outboxBatch)
		if err != nil {
			log.Printf("server: claiming notifications: %v", err)
			return
		}
		for _, m := range msgs {
			s.deliver(ctx, m)
		}
		if len(msgs) < outboxBatch {
			return
		}
	}
}

// deliver sends one claimed message and settles it: deleted when sent (or
// undeliverable), rescheduled with backoff when the sink failed.
func (s *Server) deliver(ctx context.Context, m store.OutboxMessage) {
	settle := func(err error) {
		if err != nil {
			log.Printf("server: settling notification %d: %v", m.ID, err)
		}
	}
	var target notify.Notifier
	for _, sk := range s.sinks {
		if sk.name == m.Sink {
			target = sk.n
		}
	}
	if target == nil {
		log.Printf("server: dropping notification %d: sink %q is no longer configured", m.ID, m.Sink)
		settle(s.cfg.Store.DeleteOutbox(ctx, m.ID))
		return
	}
	n, err := notificationOf(m)
	if err != nil {
		log.Printf("server: dropping notification %d: corrupt payload: %v", m.ID, err)
		settle(s.cfg.Store.DeleteOutbox(ctx, m.ID))
		return
	}
	// A message that has waited out its lifetime is stale: dropped unsent,
	// so a sink that stays limited cannot keep a queue growing.
	if age := s.now().Sub(m.CreatedAt); !m.CreatedAt.IsZero() && age >= outboxMaxAge {
		log.Printf("server: giving up on notification %s (cluster %s, sink %s): queued %s ago, past the %s limit",
			n.DeliveryID, n.Cluster.Name, m.Sink, age.Round(time.Minute), outboxMaxAge)
		settle(s.cfg.Store.DeleteOutbox(ctx, m.ID))
		return
	}
	// A sink that asked to be left alone (Retry-After) is not called for any
	// of its messages: they wait out the hold, and the claim that brought
	// this one here is not an attempt.
	if until, held := s.holds.heldUntil(m.Sink, s.now()); held {
		settle(s.cfg.Store.DeferOutbox(ctx, m.ID, expiryCap(m, until)))
		return
	}
	nctx, cancel := context.WithTimeout(ctx, s.notifyTimeout)
	err = target.Notify(nctx, n)
	cancel()
	if err == nil {
		settle(s.cfg.Store.DeleteOutbox(ctx, m.ID))
		return
	}
	if hold := retryAfterHold(err); hold > 0 {
		s.holds.hold(m.Sink, s.now().Add(hold))
	}
	if m.Attempts >= outboxMaxAttempts {
		log.Printf("server: giving up on notification %s (cluster %s, sink %s) after %d attempts: %v",
			n.DeliveryID, n.Cluster.Name, m.Sink, m.Attempts, err)
		settle(s.cfg.Store.DeleteOutbox(ctx, m.ID))
		return
	}
	next := expiryCap(m, s.now().Add(retryDelay(m.Attempts, err)))
	log.Printf("server: notification %s failed (cluster %s, sink %s, attempt %d), retrying at %s: %v",
		n.DeliveryID, n.Cluster.Name, m.Sink, m.Attempts, next.UTC().Format(time.RFC3339), err)
	settle(s.cfg.Store.RescheduleOutbox(ctx, m.ID, next, outboxError(err)))
}

// expiryCap is t, or the moment m outlives outboxMaxAge if that comes
// first, so a message is picked up again to be given up at its expiry
// rather than up to an hour later.
func expiryCap(m store.OutboxMessage, t time.Time) time.Time {
	if m.CreatedAt.IsZero() {
		return t
	}
	if exp := m.CreatedAt.Add(outboxMaxAge); exp.Before(t) {
		return exp
	}
	return t
}

// runOutbox is the delivery worker: it drains the outbox when kicked after
// a commit and every outboxPoll (retries, and messages left by a previous
// run), until ctx ends.
func (s *Server) runOutbox(ctx context.Context) {
	poll := time.NewTicker(outboxPoll)
	defer poll.Stop()
	for {
		s.deliverOutbox(ctx)
		select {
		case <-ctx.Done():
			return
		case <-s.outboxKick:
		case <-poll.C:
		}
	}
}
