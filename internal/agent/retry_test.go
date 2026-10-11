package agent

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/abd-ulbasit/upgradescope/internal/inventory"
)

// runLoop runs r.loop with a JSON logger and a fixed spacing between
// ticks, and returns the log, the observer and a stop that cancels the
// loop and requires it to return promptly.
func runLoop(t *testing.T, r *runner, spacing time.Duration) (logs *syncBuffer, obs *observer, stop func()) {
	t.Helper()
	logs = &syncBuffer{}
	log := slog.New(slog.NewJSONHandler(logs, nil))
	r.cfg.Logger = log
	r.pusher.log = log
	obs = newObserver(log, r.kb, r.cfg.Interval)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- r.loop(ctx, obs, log, func() time.Duration { return spacing }) }()
	stopped := false
	stop = func() {
		t.Helper()
		if stopped {
			return
		}
		stopped = true
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("loop: %v", err)
			}
		case <-time.After(2 * time.Second):
			t.Error("loop did not return within 2s of the stop")
		}
	}
	t.Cleanup(stop)
	return logs, obs, stop
}

// text is everything logged so far.
func (b *syncBuffer) text() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// waitUntil polls cond every 5ms for up to d.
func waitUntil(d time.Duration, cond func() bool) bool {
	deadline := time.Now().Add(d)
	for !cond() {
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(5 * time.Millisecond)
	}
	return true
}

// freeAddr is a loopback address nothing listens on: connecting to it is
// refused until a server is started on it.
func freeAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	return addr
}

// #386: the first push of a combined install races the server's start.
// The server refuses connections at the first tick and through the first
// retry, then listens: the buffered snapshot is delivered by the second
// retry, long before the next tick (an hour away here), and the tick line
// counts the failed push it reports.
func TestLoopRetriesTheBufferedSnapshotBeforeTheNextTick(t *testing.T) {
	addr := freeAddr(t)
	srv := newSnapServer(t)
	srv.srv.Close() // replaced by a server on addr, started by the retry hook below
	srv.srv = httptest.NewUnstartedServer(srv.srv.Config.Handler)
	srv.srv.Listener.Close() // its own listener is replaced by one on addr
	t.Cleanup(func() { srv.srv.Close() })

	r := testRunner(t, fakeDyn(), "http://"+addr)
	const token = "sekret-push-token-386"
	r.pusher.token = token
	var mu sync.Mutex
	var asked []int
	r.retryDelay = func(n int) time.Duration {
		mu.Lock()
		defer mu.Unlock()
		asked = append(asked, n)
		if n == 1 { // the first retry was refused too; the server is up now
			l, err := net.Listen("tcp", addr)
			if err != nil {
				t.Errorf("listen on %s: %v", addr, err)
				return time.Millisecond
			}
			srv.srv.Listener = l
			srv.srv.Start()
		}
		return time.Millisecond
	}
	logs, obs, stop := runLoop(t, r, time.Hour)

	if !waitUntil(5*time.Second, func() bool { return srv.count() == 1 }) {
		t.Fatalf("snapshot not delivered within 5s, with the next tick an hour away; log: %v", logs.lines(t))
	}
	if !waitUntil(time.Second, func() bool { return len(linesWithMsg(logs.lines(t), msgPushRetried)) == 1 }) {
		t.Fatalf("no %q line; log: %v", msgPushRetried, logs.lines(t))
	}
	stop()

	mu.Lock()
	if len(asked) != 2 || asked[0] != 0 || asked[1] != 1 {
		t.Errorf("retry delays asked = %v, want [0 1] (stop at the first delivery)", asked)
	}
	mu.Unlock()
	lines := logs.lines(t)
	ticks := linesWithMsg(lines, msgTickComplete)
	if len(ticks) != 1 || ticks[0]["push"] != pushFailed || ticks[0]["consecutiveFailures"] != float64(1) {
		t.Errorf("tick lines = %v, want one with push=failed and consecutiveFailures=1", ticks)
	}
	if failed := linesWithMsg(lines, msgPushRetryFailed); len(failed) != 1 || failed[0]["level"] != "WARN" || failed[0]["retry"] != float64(1) {
		t.Errorf("retry-failed lines = %v, want one WARN for retry 1", failed)
	}
	if ok := linesWithMsg(lines, msgPushRetried); ok[0]["level"] != "INFO" || ok[0]["retry"] != float64(2) {
		t.Errorf("retried line = %v, want INFO for retry 2", ok[0])
	}
	if text := logs.text(); strings.Contains(text, token) {
		t.Errorf("the log carries the push token:\n%s", text)
	}
	// The tick's push and the first retry failed; the second delivered.
	if _, body := serve(t, obs.handler(), "/metrics"); !strings.Contains(body, "upgradescope_agent_push_errors_total 2\n") {
		t.Errorf("/metrics does not count the tick's failed push and the failed retry:\n%s", body)
	}
	// The delivered snapshot advanced the hash gate: the same inventory is
	// not pushed again at the next tick.
	if r.lastHash == "" || r.lastPush.IsZero() {
		t.Errorf("hash gate not advanced by the delivered retry: lastHash %q, lastPush %v", r.lastHash, r.lastPush)
	}
	if r.retryPending {
		t.Error("retryPending still set after the delivery")
	}
}

// A newer tick's snapshot replaces the buffered one: the retry stops when
// the tick is due, and the older snapshot is never sent after the newer.
func TestLoopNextTickSupersedesTheBufferedSnapshot(t *testing.T) {
	var ticks atomic.Int32
	var mu sync.Mutex
	var sent []string // the server version of each request, in arrival order
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		rec := &pushRecorder{statuses: []int{http.StatusAccepted}}
		rec.handler(t)(httptest.NewRecorder(), req)
		inv := string(rec.payloads[0].Inventory)
		mu.Lock()
		switch {
		case strings.Contains(inv, `"serverVersion":"v1.35.3"`):
			sent = append(sent, "new")
		case strings.Contains(inv, `"serverVersion":"v1.35.2"`):
			sent = append(sent, "old")
		default:
			sent = append(sent, "?")
		}
		mu.Unlock()
		if ticks.Load() < 2 { // down until the second tick has begun
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusAccepted)
	}))
	t.Cleanup(srv.Close)

	r := testRunner(t, fakeDyn(), srv.URL)
	collect := r.collectFn
	r.collectFn = func(ctx context.Context) inventory.Inventory {
		inv := collect(ctx)
		if ticks.Add(1) >= 2 {
			inv.ServerVersion = "v1.35.3"
		}
		return inv
	}
	r.retryDelay = func(int) time.Duration { return 5 * time.Millisecond }
	_, _, stop := runLoop(t, r, 300*time.Millisecond)

	newDelivered := func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(sent) > 0 && sent[len(sent)-1] == "new"
	}
	if !waitUntil(5*time.Second, newDelivered) {
		t.Fatal("the second tick's snapshot was never delivered")
	}
	time.Sleep(100 * time.Millisecond) // a stray retry of the old one would land here
	stop()

	mu.Lock()
	defer mu.Unlock()
	oldRetries, seenNew := 0, false
	for i, s := range sent {
		switch {
		case s == "new":
			seenNew = true
		case s == "old" && seenNew:
			t.Fatalf("request %d sent the older snapshot after the newer one: %v", i, sent)
		case s == "old":
			oldRetries++
		}
	}
	if oldRetries <= pushRetries+1 {
		t.Errorf("old snapshot sent %d times, want more than the tick's own %d attempts (retried between ticks): %v", oldRetries, pushRetries+1, sent)
	}
	if ticks.Load() < 2 {
		t.Errorf("ticks = %d, want the second to have run", ticks.Load())
	}
}

// A stop ends the retry promptly, both while it waits out a backoff and
// while a retry's request hangs on the server.
func TestLoopStopEndsTheRetry(t *testing.T) {
	for _, tc := range []struct {
		name  string
		delay time.Duration
		hang  bool
	}{
		{"during the backoff", time.Hour, false},
		{"during a hung request", time.Millisecond, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var requests atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				// Read to the end: the server notices the client went away,
				// cancelling req's context, only once the body is read.
				_, _ = io.Copy(io.Discard, req.Body)
				if requests.Add(1) > int32(pushRetries+1) && tc.hang {
					<-req.Context().Done() // the retry's request hangs until the stop cancels it
					return
				}
				w.WriteHeader(http.StatusServiceUnavailable)
			}))
			t.Cleanup(srv.Close)
			r := testRunner(t, fakeDyn(), srv.URL)
			r.retryDelay = func(int) time.Duration { return tc.delay }
			_, _, stop := runLoop(t, r, time.Hour)
			want := int32(pushRetries + 1) // the tick's own attempts
			if tc.hang {
				want++ // and the hung retry
			}
			if !waitUntil(5*time.Second, func() bool { return requests.Load() >= want }) {
				t.Fatalf("requests = %d, want %d", requests.Load(), want)
			}
			time.Sleep(20 * time.Millisecond)
			start := time.Now()
			stop() // fails the test unless the loop returns within 2s
			if d := time.Since(start); d > time.Second {
				t.Errorf("stop took %v", d)
			}
		})
	}
}

// A push the server refuses for good (401) is dropped, not retried
// between ticks.
func TestLoopDoesNotRetryAPermanentlyRefusedPush(t *testing.T) {
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	t.Cleanup(srv.Close)
	r := testRunner(t, fakeDyn(), srv.URL)
	var asked atomic.Int32
	r.retryDelay = func(int) time.Duration { asked.Add(1); return time.Millisecond }
	logs, _, stop := runLoop(t, r, time.Hour)
	if !waitUntil(5*time.Second, func() bool { return len(linesWithMsg(logs.lines(t), msgTickComplete)) == 1 }) {
		t.Fatal("no tick line")
	}
	time.Sleep(50 * time.Millisecond)
	stop()
	if requests.Load() != 1 || asked.Load() != 0 {
		t.Errorf("requests = %d, retries = %d, want 1 and 0: a 401 is never retried", requests.Load(), asked.Load())
	}
}

// The buffered-snapshot backoff: 10s doubling to a 2m cap, within ±20%.
func TestBufferedRetryDelay(t *testing.T) {
	steps := []time.Duration{10 * time.Second, 20 * time.Second, 40 * time.Second, 80 * time.Second, 2 * time.Minute, 2 * time.Minute}
	for n, step := range steps {
		near := func(a, b time.Duration) bool { return (a - b).Abs() < time.Microsecond }
		lo, hi := bufferedRetryDelay(n, 0), bufferedRetryDelay(n, 0.999999)
		if !near(lo, step*8/10) || hi < step*119/100 || hi > step*12/10 {
			t.Errorf("retry %d: delay from %v to %v, want %v ±20%%", n, lo, hi, step)
		}
		if mid := bufferedRetryDelay(n, 0.5); !near(mid, step) {
			t.Errorf("retry %d: midpoint delay %v, want %v", n, mid, step)
		}
	}
	if d := bufferedRetryDelay(1000, 0.999999); d > 2*time.Minute*12/10 {
		t.Errorf("retry 1000: %v, over the cap", d)
	}
}

// consecutiveFailures counts the failure the line reports: failed pushes
// in a row on a line whose push failed (1 after the first), failed ticks
// in a row on a failed tick's line, 0 after a success. A retry that
// delivers the snapshot between ticks ends the push streak.
func TestObserverConsecutiveFailuresCountsTheReportedFailure(t *testing.T) {
	logs := &syncBuffer{}
	o := newObserver(slog.New(slog.NewJSONHandler(logs, nil)), mustKB(t), 10*time.Minute)
	pushErr := errors.New("push snapshot after 4 attempts (kept buffered): connection refused")
	o.record(tickReport{push: pushFailed, pushErr: pushErr})
	o.record(tickReport{push: pushFailed, pushErr: pushErr})
	o.record(tickReport{push: pushOK})
	o.record(tickReport{push: pushFailed, pushErr: pushErr})
	o.recordRetry(nil) // delivered between ticks
	o.record(tickReport{push: pushUnchanged})
	o.record(tickReport{push: pushFailed, pushErr: pushErr})
	o.record(tickReport{err: errors.New("boom"), push: pushOK})
	o.record(tickReport{push: pushFailed, pushErr: pushErr})

	want := []float64{1, 2, 0, 1, 0, 1, 1}
	lines := linesWithMsg(logs.lines(t), msgTickComplete)
	failed := linesWithMsg(logs.lines(t), msgTickFailed)
	all := logs.lines(t)
	if len(all) != len(want)+1 || len(lines) != len(want) || len(failed) != 1 {
		t.Fatalf("log lines = %v", all)
	}
	for i, l := range lines {
		if l["consecutiveFailures"] != want[i] {
			t.Errorf("tick line %d: consecutiveFailures = %v, want %v (%v)", i, l["consecutiveFailures"], want[i], l)
		}
	}
	if failed[0]["consecutiveFailures"] != float64(1) {
		t.Errorf("failed tick line: consecutiveFailures = %v, want 1", failed[0]["consecutiveFailures"])
	}
	// Every failed push is counted: five ticks' pushes; the delivered retry is not.
	if _, body := serve(t, o.handler(), "/metrics"); !strings.Contains(body, "upgradescope_agent_push_errors_total 5\n") {
		t.Errorf("/metrics push errors, want 5:\n%s", body)
	}
	o.recordRetry(pushErr)
	if _, body := serve(t, o.handler(), "/metrics"); !strings.Contains(body, "upgradescope_agent_push_errors_total 6\n") {
		t.Errorf("/metrics push errors after a failed retry, want 6:\n%s", body)
	}
}
