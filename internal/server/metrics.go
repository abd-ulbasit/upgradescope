package server

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/abd-ulbasit/upgradescope/internal/engine"
	"github.com/abd-ulbasit/upgradescope/internal/server/store"
)

// scrapeTimeout bounds the store reads behind one /metrics scrape.
const scrapeTimeout = 10 * time.Second

// Route labels for requests no API pattern matched.
const (
	routeDashboard = "dashboard" // the embedded SPA and its assets
	routeUnmatched = "unmatched" // a reserved or /api/ path with no route (JSON 404)
)

// serverMetrics is the server's Prometheus registry: HTTP traffic by route
// pattern, ingest outcomes, and per-cluster readiness read from the store
// at scrape time. Labels are route patterns, status codes, ingest results,
// cluster names (bounded by the fleet) and targets.
type serverMetrics struct {
	reg      *prometheus.Registry
	requests *prometheus.CounterVec
	duration *prometheus.HistogramVec
	ingest   *prometheus.CounterVec
}

func newServerMetrics(s *Server) *serverMetrics {
	m := &serverMetrics{
		reg: prometheus.NewRegistry(),
		requests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "upgradescope_http_requests_total",
			Help: "HTTP requests by route pattern and status code.",
		}, []string{"route", "code"}),
		duration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "upgradescope_http_request_duration_seconds",
			Help:    "HTTP request latency by route pattern.",
			Buckets: prometheus.DefBuckets,
		}, []string{"route"}),
		ingest: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "upgradescope_ingest_total",
			Help: "Snapshot pushes by result: accepted, duplicate, unauthorized, forbidden, conflict, invalid, too_large, error.",
		}, []string{"result"}),
	}
	m.reg.MustRegister(m.requests, m.duration, m.ingest, clusterCollector{s},
		collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
	return m
}

func (m *serverMetrics) handler() http.Handler {
	return promhttp.HandlerFor(m.reg, promhttp.HandlerOpts{})
}

// statusRecorder captures the status code a handler writes.
type statusRecorder struct {
	http.ResponseWriter
	code int
}

func (r *statusRecorder) WriteHeader(code int) {
	if r.code == 0 {
		r.code = code
	}
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Write(b []byte) (int, error) {
	if r.code == 0 {
		r.code = http.StatusOK
	}
	return r.ResponseWriter.Write(b)
}

// Unwrap lets http.ResponseController reach the underlying writer.
func (r *statusRecorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

// instrument counts and times every request under its ServeMux pattern
// (r.Pattern, set by the mux as it routes), never the raw path, so label
// values stay a fixed set.
func (m *serverMetrics) instrument(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w}
		next.ServeHTTP(rec, r)
		code := rec.code
		if code == 0 {
			code = http.StatusOK
		}
		route := r.Pattern
		switch {
		case route != "":
		case isServerPath(r.URL.Path):
			route = routeUnmatched
		default:
			route = routeDashboard
		}
		m.requests.WithLabelValues(route, strconv.Itoa(code)).Inc()
		m.duration.WithLabelValues(route).Observe(time.Since(start).Seconds())
		if route == "POST /api/v1/snapshots" {
			m.ingest.WithLabelValues(ingestResult(code)).Inc()
		}
	})
}

// ingestResult names a snapshot push outcome from handleIngest's status.
func ingestResult(code int) string {
	switch {
	case code == http.StatusAccepted:
		return "accepted"
	case code == http.StatusOK:
		return "duplicate"
	case code == http.StatusUnauthorized:
		return "unauthorized"
	case code == http.StatusForbidden:
		return "forbidden"
	case code == http.StatusConflict:
		return "conflict"
	case code == http.StatusRequestEntityTooLarge:
		return "too_large"
	case code >= 500:
		return "error"
	default:
		return "invalid"
	}
}

var (
	descClusterScore = prometheus.NewDesc("upgradescope_cluster_score",
		"Readiness score (0-100) of the cluster's current evaluation per target.", []string{"cluster", "target"}, nil)
	descClusterVerdict = prometheus.NewDesc("upgradescope_cluster_verdict",
		"1 for the current verdict (ready, blocked or unknown) per cluster and target, 0 for the others.", []string{"cluster", "target", "verdict"}, nil)
	descClusterBlockers = prometheus.NewDesc("upgradescope_cluster_blockers",
		"Blocker findings in the cluster's current evaluation per target.", []string{"cluster", "target"}, nil)
	descClusterPushAge = prometheus.NewDesc("upgradescope_cluster_last_push_age_seconds",
		"Seconds since the cluster's agent last pushed a snapshot (duplicates included).", []string{"cluster"}, nil)
	descClusterStale = prometheus.NewDesc("upgradescope_cluster_stale",
		"1 when the cluster's agent has not pushed within the server's --stale-after, else 0.", []string{"cluster"}, nil)
)

var verdicts = []engine.Verdict{engine.VerdictReady, engine.VerdictBlocked, engine.VerdictUnknown}

// clusterCollector reads per-cluster gauges from the store at scrape time:
// for each cluster, its current evaluation (of its latest snapshot) for the
// default target and every applicable extra target, the same set the
// cluster detail API shows. A deleted cluster or a re-evaluation is
// reflected on the next scrape with no state kept here. A store error
// fails the scrape, which Prometheus reports as up == 0.
type clusterCollector struct{ s *Server }

func (c clusterCollector) Describe(ch chan<- *prometheus.Desc) {
	for _, d := range []*prometheus.Desc{descClusterScore, descClusterVerdict, descClusterBlockers, descClusterPushAge, descClusterStale} {
		ch <- d
	}
}

func (c clusterCollector) Collect(ch chan<- prometheus.Metric) {
	ctx, cancel := context.WithTimeout(context.Background(), scrapeTimeout)
	defer cancel()
	s := c.s
	clusters, err := s.cfg.Store.ListClusters(ctx)
	if err != nil {
		ch <- prometheus.NewInvalidMetric(descClusterScore, err)
		return
	}
	now := s.now()
	for _, cl := range clusters {
		ch <- prometheus.MustNewConstMetric(descClusterPushAge, prometheus.GaugeValue,
			max(0, now.Sub(cl.LastSeen).Seconds()), cl.Name)
		stale := 0.0
		if s.clusterStale(cl, now) {
			stale = 1
		}
		ch <- prometheus.MustNewConstMetric(descClusterStale, prometheus.GaugeValue, stale, cl.Name)
		snap, err := s.cfg.Store.LatestSnapshot(ctx, cl.ID)
		if errors.Is(err, store.ErrNotFound) {
			continue
		}
		if err != nil {
			ch <- prometheus.NewInvalidMetric(descClusterScore, err)
			return
		}
		// Only the server version decides the targets; skip decoding the
		// rest of the inventory. A corrupt one leaves the extra targets.
		for _, t := range s.evalTargets(judgedVersion(snap)) {
			e, err := s.cfg.Store.CurrentEvaluation(ctx, cl.ID, t.String())
			if errors.Is(err, store.ErrNotFound) {
				continue
			}
			if err != nil {
				ch <- prometheus.NewInvalidMetric(descClusterScore, err)
				return
			}
			target := t.String()
			ch <- prometheus.MustNewConstMetric(descClusterScore, prometheus.GaugeValue, float64(e.Score), cl.Name, target)
			ch <- prometheus.MustNewConstMetric(descClusterBlockers, prometheus.GaugeValue, float64(e.Blockers), cl.Name, target)
			v := verdictOf(e)
			for _, cand := range verdicts {
				val := 0.0
				if v == cand {
					val = 1
				}
				ch <- prometheus.MustNewConstMetric(descClusterVerdict, prometheus.GaugeValue, val, cl.Name, target, string(cand))
			}
		}
	}
}
