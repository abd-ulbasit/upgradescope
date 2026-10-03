package agent

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// pushRecorder is an httptest handler that decodes gzipped push payloads and
// replies per script: one status code per request, last repeats.
type pushRecorder struct {
	mu       sync.Mutex
	statuses []int
	payloads []pushPayload
	headers  []http.Header
}

func (rec *pushRecorder) handler(t *testing.T) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		rec.mu.Lock()
		defer rec.mu.Unlock()
		rec.headers = append(rec.headers, r.Header.Clone())
		if r.Header.Get("Content-Encoding") == "gzip" {
			zr, err := gzip.NewReader(r.Body)
			if err != nil {
				t.Errorf("bad gzip body: %v", err)
			} else {
				var pl pushPayload
				if err := json.NewDecoder(zr).Decode(&pl); err != nil {
					t.Errorf("bad payload JSON: %v", err)
				}
				rec.payloads = append(rec.payloads, pl)
			}
		}
		idx := len(rec.headers) - 1
		if idx >= len(rec.statuses) {
			idx = len(rec.statuses) - 1
		}
		code := rec.statuses[idx]
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		switch code {
		case http.StatusAccepted:
			io.WriteString(w, `{"snapshotId": 123}`)
		case http.StatusOK:
			io.WriteString(w, `{"snapshotId": 122, "duplicate": true}`)
		default:
			io.WriteString(w, `{"error": "nope"}`)
		}
	}
}

func (rec *pushRecorder) requests() int {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	return len(rec.headers)
}

func testPayload(name string) pushPayload {
	return pushPayload{
		SchemaVersion: 1,
		ClusterName:   name,
		AgentVersion:  "v0.2.0",
		KBVersion:     "kb-v",
		Inventory:     json.RawMessage(`{"schemaVersion":1,"clusterId":"uid-123"}`),
	}
}

func newTestPusher(t *testing.T, rec *pushRecorder, statuses ...int) (*pusher, *[]time.Duration) {
	t.Helper()
	rec.statuses = statuses
	srv := httptest.NewServer(rec.handler(t))
	t.Cleanup(srv.Close)
	p := newPusher(srv.URL+"/", "sekret") // trailing slash must be trimmed
	var slept []time.Duration
	p.wait = func(_ context.Context, d time.Duration) error {
		slept = append(slept, d)
		return nil
	}
	return p, &slept
}

func TestFlushSendsGzipBearerJSON(t *testing.T) {
	rec := &pushRecorder{}
	p, _ := newTestPusher(t, rec, http.StatusAccepted)
	p.offer(testPayload("prod-eu-1"))
	if err := p.flush(context.Background()); err != nil {
		t.Fatalf("flush: %v", err)
	}
	if rec.requests() != 1 {
		t.Fatalf("requests = %d, want 1", rec.requests())
	}
	h := rec.headers[0]
	if got := h.Get("Authorization"); got != "Bearer sekret" {
		t.Errorf("Authorization = %q", got)
	}
	if got := h.Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q", got)
	}
	if got := h.Get("Content-Encoding"); got != "gzip" {
		t.Errorf("Content-Encoding = %q", got)
	}
	pl := rec.payloads[0]
	if pl.SchemaVersion != 1 || pl.ClusterName != "prod-eu-1" || pl.KBVersion != "kb-v" {
		t.Errorf("payload = %+v", pl)
	}
	// Pending cleared on success: a second flush is a no-op.
	if err := p.flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	if rec.requests() != 1 {
		t.Errorf("flush after success re-sent: %d requests", rec.requests())
	}
}

func TestFlushDuplicate200IsSuccess(t *testing.T) {
	rec := &pushRecorder{}
	p, _ := newTestPusher(t, rec, http.StatusOK)
	p.offer(testPayload("c"))
	if err := p.flush(context.Background()); err != nil {
		t.Fatalf("200 duplicate must be success, got %v", err)
	}
}

func TestFlushLatestOnlyBuffer(t *testing.T) {
	rec := &pushRecorder{}
	p, _ := newTestPusher(t, rec, http.StatusAccepted)
	p.offer(testPayload("older"))
	p.offer(testPayload("newer")) // replaces, never queues
	if err := p.flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	if rec.requests() != 1 || rec.payloads[0].ClusterName != "newer" {
		t.Errorf("got %d requests, first cluster %q; want 1 request of newer", rec.requests(), rec.payloads[0].ClusterName)
	}
}

func TestFlushRetriesTransientWithBackoff(t *testing.T) {
	rec := &pushRecorder{}
	p, slept := newTestPusher(t, rec, http.StatusInternalServerError, http.StatusBadGateway, http.StatusAccepted)
	p.offer(testPayload("c"))
	if err := p.flush(context.Background()); err != nil {
		t.Fatalf("flush should succeed on third attempt: %v", err)
	}
	if rec.requests() != 3 {
		t.Errorf("requests = %d, want 3", rec.requests())
	}
	if want := []time.Duration{time.Second, 2 * time.Second}; len(*slept) != 2 || (*slept)[0] != want[0] || (*slept)[1] != want[1] {
		t.Errorf("backoff sleeps = %v, want %v", *slept, want)
	}
}

func TestFlushExhaustsRetriesKeepsPending(t *testing.T) {
	rec := &pushRecorder{}
	p, slept := newTestPusher(t, rec, http.StatusInternalServerError)
	p.offer(testPayload("c"))
	err := p.flush(context.Background())
	if err == nil {
		t.Fatal("want error after exhausting retries")
	}
	if rec.requests() != 4 { // initial + 3 retries
		t.Errorf("requests = %d, want 4", rec.requests())
	}
	if want := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second}; len(*slept) != 3 {
		t.Errorf("sleeps = %v, want %v", *slept, want)
	}
	// Payload kept buffered: next flush retries it.
	if err := p.flush(context.Background()); err == nil && rec.requests() == 4 {
		t.Error("pending was dropped after transient exhaustion")
	}
}

func TestFlush401DropsPayloadNoRetry(t *testing.T) {
	rec := &pushRecorder{}
	p, slept := newTestPusher(t, rec, http.StatusUnauthorized)
	p.offer(testPayload("c"))
	err := p.flush(context.Background())
	if err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("err = %v, want 401 mention", err)
	}
	if rec.requests() != 1 || len(*slept) != 0 {
		t.Errorf("401 retried: %d requests, sleeps %v", rec.requests(), *slept)
	}
	if err := p.flush(context.Background()); err != nil || rec.requests() != 1 {
		t.Error("payload not dropped after 401")
	}
}

func TestFlush422DropsPayloadWithServerError(t *testing.T) {
	rec := &pushRecorder{}
	p, _ := newTestPusher(t, rec, http.StatusUnprocessableEntity)
	p.offer(testPayload("c"))
	err := p.flush(context.Background())
	if err == nil || !strings.Contains(err.Error(), "nope") {
		t.Fatalf("err = %v, want server error body surfaced", err)
	}
	if rec.requests() != 1 {
		t.Errorf("422 retried: %d requests", rec.requests())
	}
}

func TestFlushOther4xxPermanentDropsPayload(t *testing.T) {
	rec := &pushRecorder{}
	p, slept := newTestPusher(t, rec, http.StatusForbidden)
	p.offer(testPayload("c"))
	err := p.flush(context.Background())
	if err == nil || !strings.Contains(err.Error(), "403") {
		t.Fatalf("err = %v, want 403 mention (permanent)", err)
	}
	if rec.requests() != 1 || len(*slept) != 0 {
		t.Errorf("403 retried: %d requests, waits %v", rec.requests(), *slept)
	}
	if err := p.flush(context.Background()); err != nil || rec.requests() != 1 {
		t.Error("payload not dropped after 403")
	}
}

func TestFlush429IsTransientRetried(t *testing.T) {
	rec := &pushRecorder{}
	p, slept := newTestPusher(t, rec, http.StatusTooManyRequests, http.StatusAccepted)
	p.offer(testPayload("c"))
	if err := p.flush(context.Background()); err != nil {
		t.Fatalf("429 then 202 should succeed: %v", err)
	}
	if rec.requests() != 2 || len(*slept) != 1 {
		t.Errorf("requests/waits = %d/%d, want 2/1 (429 retried once)", rec.requests(), len(*slept))
	}
}

// A redirect is never followed: Go would turn a 301/302/303 POST into a
// body-less GET that a login page or SPA answers 200, so the push would be
// reported delivered when nothing arrived (#190). Any 3xx is a permanent
// misconfiguration: loud, counted as a failure, payload dropped, never
// resent to the Location.
func TestFlushRedirectIsPermanentFailureNeverFollowed(t *testing.T) {
	for _, code := range []int{
		http.StatusMovedPermanently, http.StatusFound, http.StatusSeeOther,
		http.StatusTemporaryRedirect, http.StatusPermanentRedirect,
	} {
		t.Run(strconv.Itoa(code), func(t *testing.T) {
			var target, origin atomic.Int32
			var targetMethod atomic.Value
			dest := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				target.Add(1)
				targetMethod.Store(r.Method)
				w.WriteHeader(http.StatusOK) // what a login page or SPA answers a GET with
			}))
			defer dest.Close()
			src := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				origin.Add(1)
				http.Redirect(w, r, dest.URL+"/api/v1/snapshots", code)
			}))
			defer src.Close()

			var logs strings.Builder
			p := newPusher(src.URL, "sekret")
			p.log = slog.New(slog.NewTextHandler(&logs, nil))
			var slept []time.Duration
			p.wait = func(_ context.Context, d time.Duration) error {
				slept = append(slept, d)
				return nil
			}
			p.offer(testPayload("c"))
			err := p.flush(context.Background())
			if err == nil {
				t.Fatal("redirect reported as a successful push")
			}
			for _, want := range []string{strconv.Itoa(code), dest.URL + "/api/v1/snapshots", "--server-url"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("err = %q, want it to mention %q", err, want)
				}
			}
			if target.Load() != 0 {
				t.Errorf("Location received a %s: the redirect was followed", targetMethod.Load())
			}
			if origin.Load() != 1 || len(slept) != 0 {
				t.Errorf("origin requests/waits = %d/%d, want 1/0 (permanent, no retry)", origin.Load(), len(slept))
			}
			if !strings.Contains(logs.String(), "level=ERROR") || !strings.Contains(logs.String(), dest.URL) {
				t.Errorf("log = %q, want an error line naming the Location", logs.String())
			}
			// Dropped like a permanent 4xx: nothing is resent.
			if err := p.flush(context.Background()); err != nil || origin.Load() != 1 {
				t.Errorf("payload kept after a redirect: err %v, %d origin requests", err, origin.Load())
			}
		})
	}
}

// A redirect with no Location still fails loudly.
func TestFlushRedirectWithoutLocation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusFound)
	}))
	defer srv.Close()
	p := newPusher(srv.URL, "sekret")
	p.offer(testPayload("c"))
	if err := p.flush(context.Background()); err == nil || !strings.Contains(err.Error(), "302") {
		t.Fatalf("err = %v, want a 302 failure", err)
	}
}

// Retry-After on a 429 or 503 is honoured when it asks for longer than the
// backoff step, capped at the backoff maximum; a shorter one changes nothing.
func TestFlushHonoursRetryAfter(t *testing.T) {
	// A fixed clock: the HTTP-date case used to be built from the wall
	// clock before any subtest ran, so a loaded machine that spent over a
	// second on the earlier subtests failed it.
	now := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	future := now.Add(30 * time.Second).Format(http.TimeFormat)
	for _, tc := range []struct {
		name   string
		status int
		header string
		want   time.Duration
	}{
		{"429 seconds", http.StatusTooManyRequests, "20", 20 * time.Second},
		{"503 seconds", http.StatusServiceUnavailable, "10", 10 * time.Second},
		{"capped at the backoff maximum", http.StatusServiceUnavailable, "3600", maxPushBackoff},
		{"shorter than the backoff step", http.StatusServiceUnavailable, "0", time.Second},
		{"http-date", http.StatusTooManyRequests, future, 30 * time.Second},
		{"unparseable", http.StatusTooManyRequests, "soon", time.Second},
		{"negative", http.StatusTooManyRequests, "-5", time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls int
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				calls++
				if calls == 1 {
					w.Header().Set("Retry-After", tc.header)
					w.WriteHeader(tc.status)
					return
				}
				w.WriteHeader(http.StatusAccepted)
			}))
			defer srv.Close()
			p := newPusher(srv.URL, "sekret")
			var slept []time.Duration
			p.wait = func(_ context.Context, d time.Duration) error {
				slept = append(slept, d)
				return nil
			}
			p.now = func() time.Time { return now }
			p.offer(testPayload("c"))
			if err := p.flush(context.Background()); err != nil {
				t.Fatalf("flush: %v", err)
			}
			if len(slept) != 1 {
				t.Fatalf("sleeps = %v, want 1", slept)
			}
			if slept[0] != tc.want {
				t.Errorf("slept %v, want %v", slept[0], tc.want)
			}
		})
	}
}

// Retry-After repeated on every retry: each wait is the larger of the backoff
// step and the server's ask (3s, 3s, then the 4s step), and a huge ask is
// capped at the backoff maximum on every attempt.
func TestFlushRetryAfterOnEveryAttempt(t *testing.T) {
	for _, tc := range []struct {
		name   string
		header string
		want   []time.Duration
	}{
		{"mixed with the backoff steps", "3", []time.Duration{3 * time.Second, 3 * time.Second, 4 * time.Second}},
		{"capped every time", "3600", []time.Duration{maxPushBackoff, maxPushBackoff, maxPushBackoff}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls int
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				calls++
				w.Header().Set("Retry-After", tc.header)
				w.WriteHeader(http.StatusServiceUnavailable)
			}))
			defer srv.Close()
			p := newPusher(srv.URL, "sekret")
			var slept []time.Duration
			p.wait = func(_ context.Context, d time.Duration) error {
				slept = append(slept, d)
				return nil
			}
			p.offer(testPayload("c"))
			err := p.flush(context.Background())
			if err == nil || !strings.Contains(err.Error(), "503") {
				t.Fatalf("err = %v, want the 503 after exhausting retries", err)
			}
			if calls != pushRetries+1 {
				t.Errorf("requests = %d, want %d", calls, pushRetries+1)
			}
			if !slices.Equal(slept, tc.want) {
				t.Errorf("waits = %v, want %v", slept, tc.want)
			}
		})
	}
}

// A Retry-After longer than the time the tick has left must not hide why the
// push was failing: the deadline ends the wait, and the error still names the
// server's status (and wraps the context error).
func TestFlushDeadlineDuringRetryAfterKeepsStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "30")
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	p := newPusher(srv.URL, "sekret")
	p.offer(testPayload("c"))
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	err := p.flush(ctx)
	if err == nil || !strings.Contains(err.Error(), "503") {
		t.Fatalf("err = %v, want it to name the 503", err)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v, want it to wrap context.DeadlineExceeded", err)
	}
	p.mu.Lock()
	kept := p.pending != nil
	p.mu.Unlock()
	if !kept {
		t.Error("payload dropped on a deadline during backoff, want it kept buffered")
	}
}

func TestWaitForReturnsPromptlyOnCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()
	start := time.Now()
	if err := waitFor(ctx, 10*time.Second); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("waitFor blocked %v after cancel, want prompt return", elapsed)
	}
}

func TestFlushCancelDuringBackoffKeepsPayload(t *testing.T) {
	rec := &pushRecorder{}
	p, _ := newTestPusher(t, rec, http.StatusInternalServerError)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// Real ctx-aware wait; cancel fires as the backoff starts.
	p.wait = func(c context.Context, d time.Duration) error {
		cancel()
		return waitFor(c, d)
	}
	p.offer(testPayload("c"))
	start := time.Now()
	err := p.flush(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if rec.requests() != 1 {
		t.Errorf("requests = %d, want 1 (no retry after cancel)", rec.requests())
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("flush blocked %v during canceled backoff", elapsed)
	}
	p.mu.Lock()
	kept := p.pending != nil
	p.mu.Unlock()
	if !kept {
		t.Error("payload dropped on ctx cancel; must stay buffered")
	}
}

func TestBackoffCapped(t *testing.T) {
	if got := backoff(10); got != time.Minute {
		t.Errorf("backoff(10) = %v, want 1m cap", got)
	}
	if got := backoff(0); got != time.Second {
		t.Errorf("backoff(0) = %v, want 1s", got)
	}
}

// A bearer token sent over plain http:// to another host crosses the
// network in the clear; the chart's default in-cluster push did exactly
// that with the any-cluster ingest token (#126 SE-09). The agent says so.
func TestCleartextPushWarning(t *testing.T) {
	for _, tc := range []struct {
		url, token string
		warn       bool
	}{
		{"http://upgradescope-server.upgradescope.svc:8080", "tok", true},
		{"http://10.0.0.5:8080/", "tok", true},
		{"HTTP://hub.example.com", "tok", true},
		{"https://hub.example.com", "tok", false},
		{"http://127.0.0.1:8080", "tok", false},
		{"http://127.1.2.3:8080", "tok", false},
		{"http://[::1]:8080", "tok", false},
		{"http://localhost:8080", "tok", false},
		{"http://LocalHost:8080", "tok", false},
		{"http://hub.example.com", "", false},
		{"", "tok", false},
	} {
		msg := CleartextPushWarning(tc.url, tc.token)
		if (msg != "") != tc.warn {
			t.Errorf("CleartextPushWarning(%q, %q) = %q, want a warning: %v", tc.url, tc.token, msg, tc.warn)
		}
		if tc.warn && !strings.Contains(msg, "https") {
			t.Errorf("warning %q does not say to use https", msg)
		}
	}
}
