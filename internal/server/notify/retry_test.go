package notify

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// TestRetryAfterOn429: a receiver that rate-limits with 429 or 503 and a
// Retry-After header (delta-seconds or an HTTP-date) fails the delivery,
// and the error carries the delay it asked for. No header, an unparseable
// one, or another status carries none. A value too large to represent is
// the longest wait (MaxRetryAfter), not absent.
func TestRetryAfterOn429(t *testing.T) {
	inAnHour := time.Now().Add(time.Hour).UTC().Format(http.TimeFormat)
	tests := []struct {
		name       string
		status     int
		retryAfter string
		want       time.Duration // with a minute of slack for the HTTP-date
		wantOK     bool
	}{
		{"seconds", http.StatusTooManyRequests, "120", 2 * time.Minute, true},
		{"http-date", http.StatusTooManyRequests, inAnHour, time.Hour, true},
		{"date in the past", http.StatusTooManyRequests, "Mon, 01 Jan 2024 00:00:00 GMT", 0, true},
		{"no header", http.StatusTooManyRequests, "", 0, false},
		{"garbage", http.StatusTooManyRequests, "soon", 0, false},
		{"negative", http.StatusTooManyRequests, "-5", 0, false},
		{"503 is honoured too", http.StatusServiceUnavailable, "120", 2 * time.Minute, true},
		{"503 without header", http.StatusServiceUnavailable, "", 0, false},
		{"beyond 32 bits is the longest wait", http.StatusTooManyRequests, "4294967296", MaxRetryAfter, true},
		{"absurdly large is the longest wait", http.StatusTooManyRequests, "99999999999999999999999", MaxRetryAfter, true},
		{"another status", http.StatusInternalServerError, "120", 0, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if tt.retryAfter != "" {
					w.Header().Set("Retry-After", tt.retryAfter)
				}
				w.WriteHeader(tt.status)
			}))
			defer srv.Close()
			err := NewGenericWebhook(srv.URL).Notify(context.Background(), testNotification())
			if err == nil {
				t.Fatalf("status %d: want an error", tt.status)
			}
			got, ok := RetryAfter(err)
			if ok != tt.wantOK || got > tt.want || got < tt.want-time.Minute {
				t.Errorf("RetryAfter(%v) = %v, %v; want %v, %v", err, got, ok, tt.want, tt.wantOK)
			}
		})
	}
}
