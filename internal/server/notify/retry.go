package notify

import (
	"errors"
	"math"
	"net/http"
	"strconv"
	"time"
)

// MaxRetryAfter is the longest Retry-After delay reported: a delta-seconds
// value that does not fit 32 bits (about 136 years) is read as this rather
// than as absent. The server's outbox caps what it honours far below it.
const MaxRetryAfter = time.Duration(math.MaxUint32) * time.Second

// RetryAfterError is a delivery the receiver pushed back on: it answered
// 429 Too Many Requests or 503 Service Unavailable with a Retry-After
// header, asking not to be retried before After. The server's outbox
// waits at least that long, and sends that sink nothing else meanwhile.
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
// delta-seconds or an HTTP-date, a date in the past being 0 and seconds
// beyond 32 bits MaxRetryAfter. ok is false when it is absent or neither.
func parseRetryAfter(v string, now time.Time) (d time.Duration, ok bool) {
	if v == "" {
		return 0, false
	}
	if secs, err := strconv.ParseUint(v, 10, 64); err == nil {
		return time.Duration(min(secs, math.MaxUint32)) * time.Second, true
	}
	if allDigits(v) { // a number too large even for 64 bits
		return MaxRetryAfter, true
	}
	if t, err := http.ParseTime(v); err == nil {
		return max(t.Sub(now), 0), true
	}
	return 0, false
}

// allDigits reports whether v is a non-empty run of ASCII digits.
func allDigits(v string) bool {
	for _, r := range v {
		if r < '0' || r > '9' {
			return false
		}
	}
	return v != ""
}
