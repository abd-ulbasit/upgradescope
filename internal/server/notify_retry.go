package server

import (
	"time"

	"github.com/abd-ulbasit/upgradescope/internal/server/notify"
)

// outboxMaxRetryAfter caps the delay a rate-limited sink may ask for, so
// a receiver answering "Retry-After: 86400" cannot park a message for a
// day: past it, the message is retried as if after the longest backoff.
const outboxMaxRetryAfter = outboxMaxBackoff

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
