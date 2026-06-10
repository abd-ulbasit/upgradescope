package agent

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
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
