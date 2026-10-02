package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/abd-ulbasit/upgradescope/internal/server/notify"
)

// TestOutboxHonoursRetryAfter: a webhook that answers 429 with
// Retry-After is not retried before that delay, which overrides a
// shorter backoff, and a delay past outboxMaxRetryAfter is capped there.
func TestOutboxHonoursRetryAfter(t *testing.T) {
	tests := []struct {
		name       string
		retryAfter string
		due        time.Duration // when the retry is due after the 429
	}{
		{"longer than the backoff", "600", 10 * time.Minute},
		{"shorter than the backoff", "1", outboxBackoff(1)},
		{"capped", "172800", outboxMaxRetryAfter},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var mu sync.Mutex
			calls := 0
			recv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				mu.Lock()
				defer mu.Unlock()
				calls++
				if calls == 1 {
					w.Header().Set("Retry-After", tt.retryAfter)
					w.WriteHeader(http.StatusTooManyRequests)
				}
			}))
			defer recv.Close()
			callCount := func() int { mu.Lock(); defer mu.Unlock(); return calls }

			st := newFakeStore()
			clock := &fakeClock{t: time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC)}
			s := newTestServer(t, st, func(c *Config) { c.Notifier = notify.NewGenericWebhook(recv.URL) })
			s.now = clock.now
			ts := httptest.NewServer(s.Handler())
			defer ts.Close()
			blockedThenClean(t, ts)

			ctx := context.Background()
			start := clock.now()
			s.deliverOutbox(ctx)
			if n := callCount(); n != 1 {
				t.Fatalf("attempts = %d, want 1", n)
			}
			clock.set(start.Add(tt.due - time.Second))
			s.deliverOutbox(ctx)
			if n := callCount(); n != 1 {
				t.Fatalf("retried before %v had passed", tt.due)
			}
			clock.set(start.Add(tt.due + time.Second))
			s.deliverOutbox(ctx)
			if n := callCount(); n != 2 || len(st.outbox) != 0 {
				t.Fatalf("attempts = %d, outbox = %d; want delivered on the second attempt", n, len(st.outbox))
			}
		})
	}
}
