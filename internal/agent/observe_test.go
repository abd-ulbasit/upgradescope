package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/abd-ulbasit/upgradescope/internal/engine"
	"github.com/abd-ulbasit/upgradescope/internal/inventory"
)

// syncBuffer is a goroutine-safe log sink: Run logs from its own goroutine
// while the test reads.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

// lines decodes every JSON log line, failing on any that is not JSON.
func (b *syncBuffer) lines(t *testing.T) []map[string]any {
	t.Helper()
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []map[string]any
	for _, l := range strings.Split(strings.TrimSpace(b.buf.String()), "\n") {
		if l == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(l), &m); err != nil {
			t.Fatalf("log line is not JSON: %q: %v", l, err)
		}
		out = append(out, m)
	}
	return out
}

func linesWithMsg(lines []map[string]any, msg string) []map[string]any {
	var out []map[string]any
	for _, l := range lines {
		if l["msg"] == msg {
			out = append(out, l)
		}
	}
	return out
}

// startRun runs the agent loop with a JSON logger and the health listener
// on a loopback port, waits for the first tick's log line, and returns the
// log and the listener's base URL. stop cancels Run and requires a nil
// return.
func startRun(t *testing.T, cfg Config) (logs *syncBuffer, baseURL string, stop func()) {
	t.Helper()
	logs = &syncBuffer{}
	cfg.Logger = slog.New(slog.NewJSONHandler(logs, nil))
	cfg.HealthAddr = "127.0.0.1:0"
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Run(ctx, fakeClients(t, "v1.35.2"), fakeDyn(), fakeAPIExt(), mustKB(t), cfg) }()
	deadline := time.Now().Add(5 * time.Second)
	for len(linesWithMsg(logs.lines(t), msgTickComplete)) == 0 {
		if time.Now().After(deadline) {
			cancel()
			t.Fatalf("no tick line logged; log so far: %v", logs.lines(t))
		}
		time.Sleep(10 * time.Millisecond)
	}
	start := linesWithMsg(logs.lines(t), msgStarting)
	if len(start) != 1 {
		cancel()
		t.Fatalf("startup lines = %d, want 1", len(start))
	}
	addr, _ := start[0]["healthAddr"].(string)
	stopped := false
	stop = func() {
		if stopped {
			return
		}
		stopped = true
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("Run: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Error("Run did not return after cancel")
		}
	}
	t.Cleanup(stop)
	return logs, "http://" + addr, stop
}

func get(t *testing.T, url string) (*http.Response, string) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp, string(body)
}

// A healthy agent says what it is at startup and logs one structured line
// per tick: duration, capabilities, per-target verdict/score/blockers, the
// push result and the failure streak.
func TestRunLogsStartupAndOneLinePerTick(t *testing.T) {
	logs, _, stop := startRun(t, Config{})
	stop()

	lines := logs.lines(t)
	start := linesWithMsg(lines, msgStarting)[0]
	for _, k := range []string{"version", "kbVersion", "maxKnownK8s", "interval", "server"} {
		if _, ok := start[k]; !ok {
			t.Errorf("startup line has no %q: %v", k, start)
		}
	}
	ticks := linesWithMsg(lines, msgTickComplete)
	if len(ticks) != 1 {
		t.Fatalf("tick lines = %d, want exactly 1 (one tick ran)", len(ticks))
	}
	tick := ticks[0]
	if tick["level"] != "INFO" || tick["push"] != pushOff {
		t.Errorf("tick line level/push = %v/%v, want INFO/%s", tick["level"], tick["push"], pushOff)
	}
	if _, ok := tick["duration"]; !ok {
		t.Errorf("tick line has no duration: %v", tick)
	}
	if tick["consecutiveFailures"] != float64(0) {
		t.Errorf("consecutiveFailures = %v, want 0", tick["consecutiveFailures"])
	}
	caps, _ := tick["capabilities"].(map[string]any)
	if caps["versions"] != true || caps["api-usage"] != false {
		t.Errorf("capabilities = %v, want versions=true api-usage=false (nil metadata client)", caps)
	}
	targets, _ := tick["targets"].(map[string]any)
	t136, _ := targets["1.36"].(map[string]any)
	if t136["verdict"] != "unknown" || t136["score"] == nil || t136["blockers"] == nil {
		t.Errorf("targets = %v, want 1.36 with verdict unknown, score and blockers", targets)
	}
}

func TestRunServesHealthAndMetrics(t *testing.T) {
	_, base, _ := startRun(t, Config{})

	if resp, _ := get(t, base+"/healthz"); resp.StatusCode != http.StatusOK {
		t.Errorf("/healthz = %d, want 200", resp.StatusCode)
	}
	if resp, body := get(t, base+"/readyz"); resp.StatusCode != http.StatusOK {
		t.Errorf("/readyz after a successful tick = %d %s, want 200", resp.StatusCode, body)
	}
	resp, body := get(t, base+"/metrics")
	if resp.StatusCode != http.StatusOK || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/plain") {
		t.Fatalf("/metrics = %d %q", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	for _, want := range []string{
		`upgradescope_readiness_score{target="1.36"}`,
		`upgradescope_readiness_verdict{target="1.36",verdict="unknown"} 1`,
		`upgradescope_readiness_verdict{target="1.36",verdict="ready"} 0`,
		`upgradescope_capability_available{capability="api-usage"} 0`,
		`upgradescope_capability_available{capability="versions"} 1`,
		`upgradescope_kb_info{kb_version="`,
		`upgradescope_agent_tick_duration_seconds_count 1`,
		`upgradescope_agent_tick_errors_total 0`,
		`upgradescope_agent_last_success_timestamp_seconds `,
		// Alert rules compare the last-success age with the interval.
		`upgradescope_agent_interval_seconds 600`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("/metrics has no %s", want)
		}
	}
}

func TestTickTimeout(t *testing.T) {
	for _, tc := range []struct{ interval, want time.Duration }{
		{time.Minute, 30 * time.Second},
		{10 * time.Minute, 5 * time.Minute},
		{time.Hour, 5 * time.Minute},
	} {
		if got := tickTimeout(tc.interval); got != tc.want {
			t.Errorf("tickTimeout(%v) = %v, want %v", tc.interval, got, tc.want)
		}
	}
}

// Every tick runs under a deadline, so a wedged apiserver call cannot stall
// the loop past the next interval.
func TestRunTickHasDeadline(t *testing.T) {
	r := testRunner(t, fakeDyn(), "")
	var deadline time.Time
	var ok bool
	collect := r.collectFn
	r.collectFn = func(ctx context.Context) inventory.Inventory {
		deadline, ok = ctx.Deadline()
		return collect(ctx)
	}
	before := time.Now()
	r.runTick(context.Background())
	if !ok {
		t.Fatal("tick context has no deadline")
	}
	if d := deadline.Sub(before); d <= 0 || d > tickTimeout(r.cfg.Interval)+time.Second {
		t.Errorf("tick deadline in %v, want about %v", d, tickTimeout(r.cfg.Interval))
	}
}

// newTestObserver returns an observer on a settable clock.
func newTestObserver(t *testing.T, now *time.Time) *observer {
	t.Helper()
	o := newObserver(slog.New(slog.NewTextHandler(io.Discard, nil)), mustKB(t), 10*time.Minute)
	o.now = func() time.Time { return *now }
	return o
}

func serve(t *testing.T, h http.Handler, path string) (int, string) {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	return rec.Code, rec.Body.String()
}

// /readyz: 503 with a reason until a tick succeeds, 200 while the last
// success is within 2 intervals plus the tick deadline, 503 once older.
// /healthz only says the process serves.
func TestObserverReadiness(t *testing.T) {
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	o := newTestObserver(t, &now)
	h := o.handler()

	if code, body := serve(t, h, "/readyz"); code != http.StatusServiceUnavailable || !strings.Contains(body, "no tick has completed") {
		t.Errorf("/readyz before any tick = %d %q, want 503 naming the reason", code, body)
	}
	if code, _ := serve(t, h, "/healthz"); code != http.StatusOK {
		t.Errorf("/healthz before any tick = %d, want 200", code)
	}

	o.record(tickReport{err: errors.New("apiserver unreachable"), push: pushOff})
	if code, body := serve(t, h, "/readyz"); code != http.StatusServiceUnavailable || !strings.Contains(body, "apiserver unreachable") {
		t.Errorf("/readyz after a failed first tick = %d %q, want 503 with the error", code, body)
	}

	o.record(tickReport{push: pushOff})
	if code, body := serve(t, h, "/readyz"); code != http.StatusOK {
		t.Errorf("/readyz after a successful tick = %d %q, want 200", code, body)
	}

	// A failing push does not fail the tick: the CR status was written.
	o.record(tickReport{push: pushFailed, pushErr: errors.New("server returned 503")})
	if code, _ := serve(t, h, "/readyz"); code != http.StatusOK {
		t.Errorf("/readyz after a push failure = %d, want 200", code)
	}

	now = now.Add(2*10*time.Minute + tickTimeout(10*time.Minute) - time.Second)
	if code, _ := serve(t, h, "/readyz"); code != http.StatusOK {
		t.Errorf("/readyz just inside the window = %d, want 200", code)
	}
	now = now.Add(2 * time.Second)
	if code, body := serve(t, h, "/readyz"); code != http.StatusServiceUnavailable || !strings.Contains(body, "last successful tick") {
		t.Errorf("/readyz past the window = %d %q, want 503 naming the stale tick", code, body)
	}
	if code, _ := serve(t, h, "/healthz"); code != http.StatusOK {
		t.Errorf("/healthz on a stale agent = %d, want 200 (liveness is the process)", code)
	}
}

func TestObserverMetricsFromReports(t *testing.T) {
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	o := newTestObserver(t, &now)
	o.record(tickReport{
		push:     pushOK,
		duration: 2 * time.Second,
		caps: map[inventory.Capability]inventory.CapabilityStatus{
			inventory.CapHelm:     {Available: false, Reason: "forbidden"},
			inventory.CapAPIUsage: {Available: true, Partial: true, Reason: "list networking.k8s.io/v1 ingresses: forbidden"},
		},
		reports: []engine.Report{{
			Target: inventory.Version{Major: 1, Minor: 37}, Score: 55, Verdict: engine.VerdictBlocked,
			Findings: []engine.Finding{
				{Category: engine.CatRemovedAPI, Severity: engine.SevBlocker},
				{Category: engine.CatRemovedAPI, Severity: engine.SevBlocker},
				{Category: engine.CatEOLAddon, Severity: engine.SevWarning},
			},
		}},
	})
	o.record(tickReport{err: errors.New("boom"), push: pushOff})

	_, body := serve(t, o.handler(), "/metrics")
	for _, want := range []string{
		`upgradescope_readiness_score{target="1.37"} 55`,
		`upgradescope_readiness_verdict{target="1.37",verdict="blocked"} 1`,
		`upgradescope_findings{category="removed-api",severity="blocker",target="1.37"} 2`,
		`upgradescope_findings{category="eol-addon",severity="warning",target="1.37"} 1`,
		`upgradescope_capability_available{capability="helm"} 0`,
		// Available but partial: some of what it covers went unread.
		`upgradescope_capability_available{capability="api-usage"} 1`,
		`upgradescope_capability_partial{capability="api-usage"} 1`,
		`upgradescope_capability_partial{capability="helm"} 0`,
		`upgradescope_agent_tick_errors_total 1`,
		`upgradescope_agent_last_success_timestamp_seconds 1.790856e+09`,
		`upgradescope_agent_tick_duration_seconds_count 2`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("/metrics has no %s\n%s", want, body)
		}
	}
}

// Two reports for one target must not reach the registry as duplicate
// series: that fails the whole scrape, tick metrics included. The first
// report wins.
func TestObserverMetricsSkipRepeatedTarget(t *testing.T) {
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	o := newTestObserver(t, &now)
	v := inventory.Version{Major: 1, Minor: 36}
	o.record(tickReport{push: pushOff, reports: []engine.Report{
		{Target: v, Score: 90, Verdict: engine.VerdictReady},
		{Target: v, Score: 40, Verdict: engine.VerdictBlocked},
	}})

	code, body := serve(t, o.handler(), "/metrics")
	if code != http.StatusOK {
		t.Fatalf("/metrics = %d, want 200\n%s", code, body)
	}
	if want := `upgradescope_readiness_score{target="1.36"} 90`; !strings.Contains(body, want) {
		t.Errorf("/metrics has no %s\n%s", want, body)
	}
}

// A push failure is a WARN line on a completed tick; a tick failure is an
// ERROR line carrying the failure streak.
func TestObserverTickLogLevels(t *testing.T) {
	logs := &syncBuffer{}
	o := newObserver(slog.New(slog.NewJSONHandler(logs, nil)), mustKB(t), 10*time.Minute)
	o.record(tickReport{push: pushFailed, pushErr: errors.New("server returned 503")})
	o.record(tickReport{err: errors.New("boom"), push: pushOff})
	o.record(tickReport{err: errors.New("boom again"), push: pushOff})

	lines := logs.lines(t)
	if len(lines) != 3 {
		t.Fatalf("log lines = %d, want 3", len(lines))
	}
	if lines[0]["level"] != "WARN" || lines[0]["msg"] != msgTickComplete || lines[0]["pushError"] != "server returned 503" {
		t.Errorf("push-failure line = %v, want WARN tick complete with pushError", lines[0])
	}
	if lines[2]["level"] != "ERROR" || lines[2]["msg"] != msgTickFailed || lines[2]["consecutiveFailures"] != float64(2) {
		t.Errorf("second failure line = %v, want ERROR tick failed with consecutiveFailures 2", lines[2])
	}
}

// tick records its outcome for the observer: the push result separately
// from the tick's own errors, since the CR status never waits on the server.
func TestTickReportsPushResult(t *testing.T) {
	ctx := context.Background()
	srv := newSnapServer(t)
	r := testRunner(t, fakeDyn(), srv.srv.URL)
	if err := r.tick(ctx); err != nil {
		t.Fatal(err)
	}
	if r.last.push != pushOK || r.last.err != nil || len(r.last.reports) != 1 || len(r.last.caps) == 0 {
		t.Errorf("first tick report = %+v, want push ok, no error, one report, capabilities", r.last)
	}
	if err := r.tick(ctx); err != nil {
		t.Fatal(err)
	}
	if r.last.push != pushUnchanged {
		t.Errorf("second tick push = %q, want %q", r.last.push, pushUnchanged)
	}

	down := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	t.Cleanup(down.Close)
	r = testRunner(t, fakeDyn(), down.URL)
	if err := r.tick(ctx); err == nil {
		t.Fatal("tick with a rejected push: want the push error returned")
	}
	if r.last.push != pushFailed || r.last.pushErr == nil || r.last.err != nil {
		t.Errorf("tick report = %+v, want push failed with pushErr and no tick error", r.last)
	}
}

// A redirect on the push is a failed push every tick, never push=ok then
// push=unchanged (#190): the redirected GET a login page answers 200 must
// not advance the hash gate, and each failure feeds the push-error metric.
func TestTickRedirectedPushFailsEveryTick(t *testing.T) {
	ctx := context.Background()
	dest := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(dest.Close)
	src := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, dest.URL+"/login", http.StatusFound)
	}))
	t.Cleanup(src.Close)
	r := testRunner(t, fakeDyn(), src.URL)
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	o := newTestObserver(t, &now)
	for i := 1; i <= 2; i++ {
		if err := r.tick(ctx); err == nil {
			t.Fatalf("tick %d: want the redirected push reported as an error", i)
		}
		if r.last.push != pushFailed || r.last.pushErr == nil {
			t.Fatalf("tick %d report = %+v, want push failed", i, r.last)
		}
		o.record(r.last)
	}
	if _, body := serve(t, o.handler(), "/metrics"); !strings.Contains(body, "upgradescope_agent_push_errors_total 2\n") {
		t.Errorf("/metrics does not show two push errors:\n%s", body)
	}
}
