package agent

import (
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/abd-ulbasit/upgradescope/internal/engine"
	"github.com/abd-ulbasit/upgradescope/internal/inventory"
	"github.com/abd-ulbasit/upgradescope/internal/kb"
)

// Push results, as logged per tick.
const (
	pushOff       = "off"       // CRD-only mode
	pushUnchanged = "unchanged" // same inventory, force-sync not due
	pushOK        = "ok"
	pushFailed    = "failed"
)

// Log messages, fixed so log queries can match them.
const (
	msgStarting     = "agent starting"
	msgTickComplete = "tick complete"
	msgTickFailed   = "tick failed"
)

// tickReport is one tick's outcome. A tick fails when anything but the
// push fails: the ClusterReadiness status may then be missing or stale. A
// push failure is reported on its own and never fails the tick, because
// the agent's local value does not depend on the server (spec §3).
type tickReport struct {
	err      error
	push     string
	pushErr  error
	duration time.Duration
	caps     map[inventory.Capability]inventory.CapabilityStatus
	reports  []engine.Report // one per evaluated target, in target order
}

// tickTimeout bounds one tick: half the interval, at most 5m. client-go's
// HTTP/2 health checks and the apiserver's request timeout already bound
// most calls; this is the backstop that keeps the loop moving regardless.
func tickTimeout(interval time.Duration) time.Duration {
	return min(interval/2, 5*time.Minute)
}

// observer turns tick reports into one log line per tick, the /readyz
// verdict and Prometheus metrics. Gauges describe the last successful
// tick; a failed tick leaves them as they were (the last-success timestamp
// shows how old they are).
type observer struct {
	log      *slog.Logger
	kb       kb.KB
	interval time.Duration
	now      func() time.Time

	reg          *prometheus.Registry
	tickDuration prometheus.Histogram
	tickErrors   prometheus.Counter
	pushErrors   prometheus.Counter

	mu          sync.Mutex
	ticks       int       // completed ticks, failed or not
	failures    int       // consecutive failed ticks
	lastErr     error     // of the latest failed tick
	lastSuccess time.Time // zero until a tick succeeds
	good        tickReport
}

func newObserver(log *slog.Logger, k kb.KB, interval time.Duration) *observer {
	o := &observer{
		log:      log,
		kb:       k,
		interval: interval,
		now:      time.Now,
		reg:      prometheus.NewRegistry(),
		tickDuration: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name:    "upgradescope_agent_tick_duration_seconds",
			Help:    "Duration of agent ticks (collect, evaluate, write status, push).",
			Buckets: []float64{0.5, 1, 2.5, 5, 10, 30, 60, 120, 300},
		}),
		tickErrors: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "upgradescope_agent_tick_errors_total",
			Help: "Ticks that failed (the ClusterReadiness status may be missing or stale). Push failures are counted separately.",
		}),
		pushErrors: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "upgradescope_agent_push_errors_total",
			Help: "Snapshot pushes to the upgradescope server that failed after retries.",
		}),
	}
	o.reg.MustRegister(o.tickDuration, o.tickErrors, o.pushErrors, o,
		collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
	return o
}

// record folds one tick into the observer's state and logs its line.
func (o *observer) record(rep tickReport) {
	o.tickDuration.Observe(rep.duration.Seconds())
	if rep.pushErr != nil {
		o.pushErrors.Inc()
	}
	o.mu.Lock()
	o.ticks++
	if rep.err != nil {
		o.failures++
		o.lastErr = rep.err
		o.tickErrors.Inc()
	} else {
		o.failures = 0
		o.lastSuccess = o.now()
		o.good = rep
	}
	failures := o.failures
	o.mu.Unlock()

	attrs := []any{
		"duration", rep.duration.Round(time.Millisecond).String(),
		"push", rep.push,
		"consecutiveFailures", failures,
		capabilityAttr(rep.caps),
		targetsAttr(rep.reports),
	}
	switch {
	case rep.err != nil:
		o.log.Error(msgTickFailed, append([]any{"err", rep.err}, attrs...)...)
	case rep.pushErr != nil:
		o.log.Warn(msgTickComplete, append(attrs, "pushError", rep.pushErr.Error())...)
	default:
		o.log.Info(msgTickComplete, attrs...)
	}
}

// capabilityAttr renders capability availability as a group:
// capabilities.helm=true (text) or "capabilities":{"helm":true} (JSON).
func capabilityAttr(caps map[inventory.Capability]inventory.CapabilityStatus) slog.Attr {
	names := make([]string, 0, len(caps))
	for c := range caps {
		names = append(names, string(c))
	}
	sort.Strings(names)
	attrs := make([]any, 0, len(names))
	for _, n := range names {
		attrs = append(attrs, slog.Bool(n, caps[inventory.Capability(n)].Available))
	}
	return slog.Group("capabilities", attrs...)
}

// targetsAttr renders each evaluated target as a group keyed by target.
func targetsAttr(reports []engine.Report) slog.Attr {
	attrs := make([]any, 0, len(reports))
	for _, r := range reports {
		attrs = append(attrs, slog.Group(r.Target.String(),
			"verdict", string(r.Verdict), "score", r.Score, "blockers", blockers(r)))
	}
	return slog.Group("targets", attrs...)
}

func blockers(r engine.Report) int {
	n := 0
	for _, f := range r.Findings {
		if f.Severity == engine.SevBlocker {
			n++
		}
	}
	return n
}

// readyWindow is how old the last successful tick may be before /readyz
// fails: two intervals (one missed tick is tolerated; jitter adds at most
// 10%) plus the tick deadline.
func (o *observer) readyWindow() time.Duration {
	return 2*o.interval + tickTimeout(o.interval)
}

// readiness reports whether the agent is ready and, if not, why.
func (o *observer) readiness() (bool, string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	switch {
	case o.ticks == 0:
		return false, "no tick has completed yet"
	case o.lastSuccess.IsZero():
		return false, fmt.Sprintf("no successful tick yet; last error: %v", o.lastErr)
	}
	if age := o.now().Sub(o.lastSuccess); age > o.readyWindow() {
		return false, fmt.Sprintf("last successful tick was %s ago (allowed %s); last error: %v",
			age.Round(time.Second), o.readyWindow(), o.lastErr)
	}
	return true, "ok"
}

// handler serves /healthz (the process is up), /readyz (a tick succeeded
// recently) and /metrics.
func (o *observer) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprintln(w, "ok")
	})
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, _ *http.Request) {
		ok, why := o.readiness()
		if !ok {
			w.WriteHeader(http.StatusServiceUnavailable)
		}
		_, _ = fmt.Fprintln(w, why)
	})
	mux.Handle("GET /metrics", promhttp.HandlerFor(o.reg, promhttp.HandlerOpts{}))
	return mux
}

var (
	descLastSuccess = prometheus.NewDesc("upgradescope_agent_last_success_timestamp_seconds",
		"Unix time of the last successful tick (0 before the first).", nil, nil)
	descScore = prometheus.NewDesc("upgradescope_readiness_score",
		"Readiness score (0-100) per target, from the last successful tick.", []string{"target"}, nil)
	descVerdict = prometheus.NewDesc("upgradescope_readiness_verdict",
		"1 for the target's current verdict (ready, blocked or unknown), 0 for the others.", []string{"target", "verdict"}, nil)
	descFindings = prometheus.NewDesc("upgradescope_findings",
		"Findings per target, severity and category, from the last successful tick.", []string{"target", "severity", "category"}, nil)
	descCapability = prometheus.NewDesc("upgradescope_capability_available",
		"1 when the collector capability was available on the last successful tick.", []string{"capability"}, nil)
	descKBInfo = prometheus.NewDesc("upgradescope_kb_info",
		"Embedded knowledge base: dataset version and newest Kubernetes minor it covers.", []string{"kb_version", "max_known_k8s"}, nil)
)

var verdicts = []engine.Verdict{engine.VerdictReady, engine.VerdictBlocked, engine.VerdictUnknown}

// Describe implements prometheus.Collector for the state gauges.
func (o *observer) Describe(ch chan<- *prometheus.Desc) {
	for _, d := range []*prometheus.Desc{descLastSuccess, descScore, descVerdict, descFindings, descCapability, descKBInfo} {
		ch <- d
	}
}

// Collect emits the state gauges from the last successful tick. Built at
// scrape time, so a target or category that disappears drops its series
// instead of leaving a stale one behind. Labels are targets, verdicts,
// severities, categories and capabilities: all small fixed sets.
func (o *observer) Collect(ch chan<- prometheus.Metric) {
	o.mu.Lock()
	last, good := o.lastSuccess, o.good
	o.mu.Unlock()

	ts := 0.0
	if !last.IsZero() {
		ts = float64(last.UnixNano()) / 1e9
	}
	ch <- prometheus.MustNewConstMetric(descLastSuccess, prometheus.GaugeValue, ts)
	ch <- prometheus.MustNewConstMetric(descKBInfo, prometheus.GaugeValue, 1, o.kb.Version, o.kb.MaxKnownK8s.String())
	for c, st := range good.caps {
		ch <- prometheus.MustNewConstMetric(descCapability, prometheus.GaugeValue, boolValue(st.Available), string(c))
	}
	for _, r := range good.reports {
		target := r.Target.String()
		ch <- prometheus.MustNewConstMetric(descScore, prometheus.GaugeValue, float64(r.Score), target)
		for _, v := range verdicts {
			ch <- prometheus.MustNewConstMetric(descVerdict, prometheus.GaugeValue, boolValue(r.Verdict == v), target, string(v))
		}
		counts := map[[2]string]int{}
		for _, f := range r.Findings {
			counts[[2]string{string(f.Severity), string(f.Category)}]++
		}
		for k, n := range counts {
			ch <- prometheus.MustNewConstMetric(descFindings, prometheus.GaugeValue, float64(n), target, k[0], k[1])
		}
	}
}

func boolValue(b bool) float64 {
	if b {
		return 1
	}
	return 0
}
