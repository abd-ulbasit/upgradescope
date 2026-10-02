package notify

import (
	"errors"
	"net/http"
	"strconv"
	"time"
)

// RetryAfterError is a delivery the receiver rate-limited: it answered
// 429 Too Many Requests with a Retry-After header, asking not to be
// retried before After. The server's outbox waits at least that long.
type RetryAfterError struct {
	Err   error
	After time.Duration
}

func (e *RetryAfterError) Error() string { return e.Err.Error() }
func (e *RetryAfterError) Unwrap() error { return e.Err }

// RetryAfter reports the delay a failed delivery's receiver asked for, if
// it asked (see RetryAfterError).
func RetryAfter(err error) (time.Duration, bool) {
	var ra *RetryAfterError
	if errors.As(err, &ra) {
		return ra.After, true
	}
	return 0, false
}

// parseRetryAfter reads a Retry-After value (RFC 9110 §10.2.3):
// delta-seconds or an HTTP-date, a date in the past being 0. ok is false
// when it is absent or neither.
func parseRetryAfter(v string, now time.Time) (d time.Duration, ok bool) {
	if v == "" {
		return 0, false
	}
	if secs, err := strconv.ParseUint(v, 10, 32); err == nil {
		return time.Duration(secs) * time.Second, true
	}
	if t, err := http.ParseTime(v); err == nil {
		return max(t.Sub(now), 0), true
	}
	return 0, false
}
