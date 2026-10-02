package server

import (
	"sync"
	"time"

	"github.com/abd-ulbasit/upgradescope/internal/server/notify"
)

// outboxMaxRetryAfter caps the delay a rate-limited sink may ask for, so
// a receiver answering "Retry-After: 86400" cannot park a message for a
// day: past it, the message is retried as if after the longest backoff.
const outboxMaxRetryAfter = outboxMaxBackoff

// sinkHolds remembers which sinks asked to be left alone (Retry-After) and
// until when. The zero value is ready to use. It lives in memory: a restart
// forgets it, and the first delivery to a still-limited sink meets the 429
// again and re-establishes the hold; each replica keeps its own.
type sinkHolds struct {
	mu    sync.Mutex
	until map[string]time.Time
}

// hold keeps sink untouched until t (a shorter hold never replaces a longer
// one).
func (h *sinkHolds) hold(sink string, t time.Time) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.until == nil {
		h.until = map[string]time.Time{}
	}
	if t.After(h.until[sink]) {
		h.until[sink] = t
	}
}

// heldUntil reports until when sink is held at now; ok is false when it is
// not (an expired hold is forgotten).
func (h *sinkHolds) heldUntil(sink string, now time.Time) (t time.Time, ok bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	t, ok = h.until[sink]
	if ok && !t.After(now) {
		delete(h.until, sink)
		return time.Time{}, false
	}
	return t, ok
}

// retryAfterHold is how long a sink is held after it answered err: the
// Retry-After it asked for, capped at outboxMaxRetryAfter. 0 when it asked
// for nothing.
func retryAfterHold(err error) time.Duration {
	after, ok := notify.RetryAfter(err)
	if !ok {
		return 0
	}
	return min(after, outboxMaxRetryAfter)
}

// retryDelay is the delay after the attempts-th failed delivery, failed
// with err: the backoff, or longer when the sink answered 429 with a
// Retry-After (notify.RetryAfter), capped at outboxMaxRetryAfter.
func retryDelay(attempts int, err error) time.Duration {
	d := outboxBackoff(attempts)
	if after, ok := notify.RetryAfter(err); ok {
		d = max(d, min(after, outboxMaxRetryAfter))
	}
	return d
}
