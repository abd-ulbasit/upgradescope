// Package server is the upgradescope continuous-mode server: snapshot
// ingest, persisted evaluations, a read API, and on-demand what-if
// re-evaluation. Storage is behind store.Store; the CLI owns flag parsing
// and store construction (DB path never reaches this package).
package server

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/abd-ulbasit/upgradescope/internal/inventory"
	"github.com/abd-ulbasit/upgradescope/internal/kb"
	"github.com/abd-ulbasit/upgradescope/internal/server/notify"
	"github.com/abd-ulbasit/upgradescope/internal/server/store"
)

// Connection limits. Without them a client that sends headers and then
// stalls the body (or just idles a keep-alive) holds a goroutine and its
// buffers forever — no token needed, since the server drains up to 256 KiB
// of body before it even writes a 401. ReadTimeout covers the whole
// request: 60s carries the default 20 MiB snapshot cap at ~350 KiB/s.
// WriteTimeout runs from the end of the headers, so it also bounds the
// handler (ingest evaluates synchronously; notifications are delivered
// after commit by a background worker).
const (
	readHeaderTimeout = 10 * time.Second
	readTimeout       = 60 * time.Second
	writeTimeout      = 120 * time.Second
	idleTimeout       = 120 * time.Second
	maxHeaderBytes    = 64 << 10 // a bearer token needs <1 KiB; Go's default is 1 MiB
)

// MaxExtraTargets is how many distinct Config.ExtraTargets (serve
// --targets) New accepts. Each one adds a report of up to
// --max-snapshot-bytes to every ingest and to the re-evaluation pass, so
// the server's memory bound (docs/operations.md, make test-heap) is
// measured at this many.
const MaxExtraTargets = 4

// ExtraTargetsCost says why MaxExtraTargets exists, for its refusals.
const ExtraTargetsCost = "each one adds a report of up to --max-snapshot-bytes to every push and to the re-evaluation pass, " +
	"and the server's memory bound is measured at that many (see Memory and request limits in the operations guide)"

// Config wires a Server.
type Config struct {
	Listen       string          // listen address for Start, e.g. ":8080"
	Store        store.Store     // required
	KB           kb.KB           // evaluation knowledge base
	ExtraTargets []string        // minors evaluated for every snapshot, e.g. ["1.37"]
	Notifier     notify.Notifier // nil = notifications disabled
	IngestToken  string          // optional shared bearer for POST /api/v1/snapshots (any cluster); "" = per-cluster tokens only
	ReadToken    string          // optional bearer for the read API; "" = open on loopback only, unless AllowAnonymousRead
	// AllowAnonymousRead serves an open read API (ReadToken "") on an
	// address that is not loopback. Without it Start refuses one.
	AllowAnonymousRead bool
	AdminToken         string  // bearer for cluster delete/rename (also accepted for reads); "" = both refused
	TeamMap            TeamMap // optional namespace→team override, applied before every Evaluate
	Version            string  // build version: SARIF tool metadata ("" = omitted there) and toolVersion in report responses ("" when unset)

	// StaleAfter marks a cluster stale when its agent has not pushed
	// (duplicates included) for longer; 0 = DefaultStaleAfter.
	StaleAfter time.Duration
	// Retention prunes snapshots and evaluations older than this, except
	// each cluster's latest snapshot and its evaluations, at startup and
	// daily; 0 = keep everything.
	Retention time.Duration

	MaxSnapshotBytes int64 // POST /api/v1/snapshots body cap; 0 = DefaultMaxSnapshotBytes
	MaxGateBytes     int64 // POST /api/v1/gate body cap; 0 = DefaultMaxGateBytes

	// TLSCertFile/TLSKeyFile (PEM) make Start serve HTTPS; both or neither.
	// Loaded once in New, so a rotated certificate needs a restart.
	TLSCertFile string
	TLSKeyFile  string
}

// /gate concurrency and memory. Each evaluation decodes its manifests in
// memory, at a cost bounded by the node budget (maxManifestUnits): at most
// ~165 MiB of heap for the worst stream that passes it, measured
// (TestGateDecodeHeapIsBounded). Two at once would not fit the chart's
// 1Gi limit, so evaluations run one at a time (a normal one takes
// milliseconds), and the chart sets GOMEMLIMIT so the garbage one leaves
// is collected before the next one's decode piles on top. A request asks
// for the slot only once its whole body is in, so a slow uploader cannot
// hold it; it waits up to gateQueueTimeout for the slot, then gets 503 +
// Retry-After. Its answer is encoded in the slot too, and a client that
// does not read it holds only those bytes (maxHeldResponses).
//
// Bodies sit in memory while they arrive and while they wait for the
// slot, so the bytes held across all /gate requests are capped at
// maxBufferedGateBodies × --max-gate-bytes (30 MiB by default). A request
// is charged for its body bytes as each read returns them — a declared
// Content-Length reserves nothing — and gives them all back once its
// manifests are decoded or it fails. A charge that does not fit is never
// waited for: the request gives back everything it holds, in the same
// step, and gets 503 + Retry-After at once. No request waits for budget,
// so none can deadlock on it or queue behind a stalled upload; filling the
// cap takes really sending that many bytes. A stalled uploader holds the
// bytes it has sent, plus the unfilled rest of one read chunk (at most
// 64 KiB, uncharged), until ReadTimeout ends its request — and never the
// evaluation slot. Without the cap, 30 concurrent 9.5 MiB streams of small
// documents — each under every per-request cap — buffered ~800 MB.
//
// The residual cost is availability, not memory: a client that really
// sends 3 × --max-gate-bytes (30 MiB) and then stalls makes every other
// /gate request 503 until ReadTimeout (60s) cuts it off, for about 0.5
// MiB/s of its bandwidth and, when reads are anonymous, no credentials.
// SECURITY.md documents it; a read token closes it to outsiders.
const (
	maxConcurrentGates    = 1
	gateQueueTimeout      = 30 * time.Second
	maxBufferedGateBodies = 3
)

// Snapshot ingest concurrency and memory, on the same model as /gate.
// Decoding, evaluating and storing one push costs up to ~216 MiB of heap
// on SQLite at the size and node caps (maxSnapshotUnits) and at
// MaxExtraTargets, for a cluster's later push with notifications
// configured (its five reports are at most --max-snapshot-bytes each,
// maxReportBytes, and the evaluation stops there; each extra target adds
// one; each previous report is read only for its findings' heads), so
// pushes are ingested one at a time (a normal one takes milliseconds; an agent's whole fleet
// pushing on one tick queues). A push asks for the slot once its body is
// in and waits up to ingestQueueTimeout, under the agent's 30s request
// timeout, then gets 503 + Retry-After, which the agent retries. Bodies
// waiting are capped at maxBufferedSnapshotBodies × --max-snapshot-bytes
// (40 MiB by default) of decompressed bytes, charged as they arrive.
// Before this, 30 concurrent 20 MiB pushes from any agent token held
// 1.1 GB and 60 gzip bombs 1.9 GB.
const (
	maxConcurrentIngests      = 1
	ingestQueueTimeout        = 10 * time.Second
	maxBufferedSnapshotBodies = 2
)

// Read concurrency and memory. A read of one cluster (its detail, report,
// findings, teams, history or export) loads the cluster's latest snapshot,
// up to --max-snapshot-bytes as stored, which the SQLite driver holds
// twice; one that computes a report on request (a what-if: a target with
// no stored evaluation, here or in the fleet teams rollup) also decodes
// and evaluates the whole inventory, which a snapshot at its node budget
// takes ~45 MiB of heap for (~95 MiB on SQLite for one whose report is
// about the report limit, the response included). Unbounded, 10 such
// reads at once grew the heap ~400 MiB, so these reads run one at a
// time, on the /gate model (a normal one takes milliseconds): a read
// waits up to readQueueTimeout for the slot, then gets 503 + Retry-After.
// One read in the slot costs up to ~130 MiB on SQLite, the HTML export of
// a report at the report limit (TestReadHeapIsBounded).
//
// Reads of the whole fleet (/clusters, /fleet, /metrics) load no inventory
// and no report: they read each cluster's snapshot head from one store
// query and each evaluation's summary columns, so one costs about its
// response (TestFleetReadsLoadNoReport). They run up to
// maxConcurrentFleetReads at a time in fleet slots of their own, apart
// from the per-cluster reads (503 + Retry-After after readQueueTimeout),
// and their responses are held like the per-cluster reads' (see
// maxHeldResponses). Written straight to their clients, with nothing
// capping how many, 100 clients that never read held 201 MiB (/clusters)
// and 251 MiB (/fleet) of a 2000-cluster fleet's, and 30 held 433 MiB
// of /metrics, whose handler keeps the gathered metric families until
// its write returns (TestUnreadFleetResponsesAreBounded).
//
// A Prometheus scrape waits for a fleet slot only metricsQueueTimeout,
// within Prometheus' default 10s scrape_timeout, so a busy server answers
// it 503 (an up of 0 for that scrape) rather than leaving it to time out.
const (
	maxConcurrentReads      = 1
	maxConcurrentFleetReads = 2
	readQueueTimeout        = 30 * time.Second
	metricsQueueTimeout     = 5 * time.Second
)

// Responses held for their clients. Every read (per-cluster or of the
// whole fleet) and /gate write their response to memory in their slot
// (heldResponse), so what a
// response is built from (a stored report, the gate's evaluations) is
// garbage before the slot is released, and send gives the slot back
// before a slow client reads a byte. Those held bytes are charged to one
// budget, maxHeldResponses × --max-snapshot-bytes (40 MiB by default), for
// as long as their client takes, up to the 120s write timeout. A response
// that does not fit what is left of it gets 503 + Retry-After, and the
// slot goes to the next request; one larger than the whole budget, which
// could never fit, is sent in the slot under slotWriteTimeout. So clients
// that never read hold at most the budget, plus one response in each
// slot, and they never keep a slot past slotWriteTimeout. Unbounded,
// 20 that asked for a 17.5 MB report and did not read it held 366 MiB,
// and 10 that sent /gate?cluster= against it 320 MiB; a 120s window
// holds ~100 of either.
const (
	maxHeldResponses = 2
	slotWriteTimeout = 20 * time.Second
)

// Server serves the ingest + read API. Construct with New; a Server is
// single-use (one Start/Shutdown cycle).
type Server struct {
	cfg          Config
	extraTargets []inventory.Version
	mux          *http.ServeMux
	httpSrv      *http.Server
	now          func() time.Time                                    // injected clock: EOL math + timestamps stay testable
	listen       func(network, address string) (net.Listener, error) // net.Listen; injected in tests

	gateSlots        chan struct{} // semaphore: one token per running /gate evaluation
	gateQueueTimeout time.Duration // how long a /gate request waits for a slot
	gateBuffered     *byteBudget   // /gate body bytes held across requests

	ingestSlots        chan struct{} // semaphore: one token per snapshot push being ingested
	ingestQueueTimeout time.Duration // how long a push waits for a slot
	ingestBuffered     *byteBudget   // snapshot body bytes (decompressed) held across pushes

	readSlots        chan struct{} // semaphore: one token per read that loads a snapshot
	readQueueTimeout time.Duration // how long such a read waits for a slot

	fleetSlots          chan struct{} // semaphore: one token per read of the whole fleet being built
	fleetQueueTimeout   time.Duration // how long such a read waits for a slot
	metricsQueueTimeout time.Duration // how long a /metrics scrape waits for one

	heldResponses    *byteBudget   // read, fleet read and /gate response bytes held for clients after their slot
	slotWriteTimeout time.Duration // how long a response sent in its slot may take

	observeGateBound func(bound int64) // test hook: each /gate answer's gateAnswerBound
	maxGateAnswer    int64             // test override of gateAnswerLimit; 0 = --max-gate-bytes

	teamMapHash        string        // fingerprint of cfg.TeamMap stored with evaluations
	sinks              []sink        // cfg.Notifier flattened; outbox messages are per sink
	outboxKick         chan struct{} // wakes the delivery worker after a commit
	holds              sinkHolds     // sinks that asked to be left alone (Retry-After), in memory
	notifyTimeout      time.Duration // bounds one delivery attempt
	reevaluateInterval time.Duration // background re-evaluation period
	reevaluateKick     chan struct{} // starts the next re-evaluation pass early
	retentionInterval  time.Duration // pruning period after the startup pass
	stopBackground     context.CancelFunc
	backgroundDone     sync.WaitGroup
	shutDown           bool // set by Shutdown, under mu: a later Start serves nothing

	metrics *serverMetrics

	ready chan struct{} // closed once the listener is bound
	mu    sync.Mutex
	addr  string
}

// New validates cfg and builds the route table.
func New(cfg Config) (*Server, error) {
	if cfg.Store == nil {
		return nil, errors.New("server: Config.Store is required")
	}
	s := &Server{
		cfg:              cfg,
		now:              time.Now,
		listen:           net.Listen,
		mux:              http.NewServeMux(),
		gateSlots:        make(chan struct{}, maxConcurrentGates),
		gateQueueTimeout: gateQueueTimeout,
		ready:            make(chan struct{}),
	}
	s.gateBuffered = newByteBudget(maxBufferedGateBodies * s.maxGateBytes())
	s.ingestSlots = make(chan struct{}, maxConcurrentIngests)
	s.ingestQueueTimeout = ingestQueueTimeout
	s.ingestBuffered = newByteBudget(maxBufferedSnapshotBodies * s.maxSnapshotBytes())
	s.readSlots = make(chan struct{}, maxConcurrentReads)
	s.readQueueTimeout = readQueueTimeout
	s.fleetSlots = make(chan struct{}, maxConcurrentFleetReads)
	s.fleetQueueTimeout = readQueueTimeout
	s.metricsQueueTimeout = metricsQueueTimeout
	s.heldResponses = newByteBudget(maxHeldResponses * s.maxSnapshotBytes())
	s.slotWriteTimeout = slotWriteTimeout
	s.teamMapHash = hashTeamMap(cfg.TeamMap)
	s.sinks = sinksOf(cfg.Notifier)
	s.outboxKick = make(chan struct{}, 1)
	s.reevaluateKick = make(chan struct{}, 1)
	s.notifyTimeout = notifyTimeout
	s.reevaluateInterval = reevaluateInterval
	s.retentionInterval = retentionInterval
	for _, t := range cfg.ExtraTargets {
		v, err := inventory.ParseTarget(t)
		if err != nil {
			return nil, fmt.Errorf("server: bad extra target %q: %w", t, err)
		}
		if !slices.Contains(s.extraTargets, v) {
			s.extraTargets = append(s.extraTargets, v)
		}
	}
	if len(s.extraTargets) > MaxExtraTargets {
		return nil, fmt.Errorf("server: %d distinct extra targets, want at most %d: %s", len(s.extraTargets), MaxExtraTargets, ExtraTargetsCost)
	}
	s.metrics = newServerMetrics(s)
	s.routes()
	s.httpSrv = &http.Server{
		Addr:              cfg.Listen,
		Handler:           s.handler(),
		ReadHeaderTimeout: readHeaderTimeout,
		ReadTimeout:       readTimeout,
		WriteTimeout:      writeTimeout,
		IdleTimeout:       idleTimeout,
		MaxHeaderBytes:    maxHeaderBytes,
	}
	if (cfg.TLSCertFile == "") != (cfg.TLSKeyFile == "") {
		return nil, errors.New("server: Config.TLSCertFile and Config.TLSKeyFile must be set together")
	}
	if cfg.TLSCertFile != "" {
		// Load now so a bad pair fails New, not the first handshake.
		cert, err := tls.LoadX509KeyPair(cfg.TLSCertFile, cfg.TLSKeyFile)
		if err != nil {
			return nil, fmt.Errorf("server: load TLS key pair: %w", err)
		}
		s.httpSrv.TLSConfig = &tls.Config{
			Certificates: []tls.Certificate{cert},
			MinVersion:   tls.VersionTLS12,
		}
	}
	return s, nil
}

// routes registers all endpoints (Go 1.22 method+path patterns — ServeMux
// emits 405 + Allow for wrong methods on registered paths).
func (s *Server) routes() {
	s.mux.HandleFunc("GET /healthz", s.handleHealthz)
	s.mux.HandleFunc("GET /readyz", s.handleReadyz)
	// Per-cluster scores are what the read token protects, so /metrics
	// takes it too (the chart's ServiceMonitor sends it).
	s.mux.HandleFunc("GET /metrics", s.readAuth(s.inMetricsSlot(s.metrics.handler().ServeHTTP)))
	s.mux.HandleFunc("POST /api/v1/snapshots", s.handleIngest)
	s.mux.HandleFunc("GET /api/v1/clusters", s.readAuth(s.inFleetSlot(s.handleListClusters)))
	s.mux.HandleFunc("GET /api/v1/clusters/{id}", s.readAuth(s.inReadSlot(s.handleGetCluster)))
	s.mux.HandleFunc("DELETE /api/v1/clusters/{id}", s.adminAuth(s.handleDeleteCluster))
	s.mux.HandleFunc("PATCH /api/v1/clusters/{id}", s.adminAuth(s.handleRenameCluster))
	s.mux.HandleFunc("GET /api/v1/clusters/{id}/report", s.readAuth(s.inReadSlot(s.handleReport)))
	s.mux.HandleFunc("GET /api/v1/clusters/{id}/findings", s.readAuth(s.inReadSlot(s.handleFindings)))
	s.mux.HandleFunc("GET /api/v1/clusters/{id}/history", s.readAuth(s.inReadSlot(s.handleHistory)))
	s.mux.HandleFunc("GET /api/v1/clusters/{id}/teams", s.readAuth(s.inReadSlot(s.handleTeams)))
	s.mux.HandleFunc("GET /api/v1/fleet", s.readAuth(s.inFleetSlot(s.handleFleet)))
	s.mux.HandleFunc("GET /api/v1/fleet/teams", s.readAuth(s.inReadSlot(s.handleFleetTeams)))
	s.mux.HandleFunc("POST /api/v1/gate", s.readAuth(s.handleGate))
	s.mux.HandleFunc("GET /api/v1/clusters/{id}/export", s.readAuth(s.inReadSlot(s.handleExport)))
	s.mux.HandleFunc("GET /api/v1/registry", s.readAuth(s.handleRegistry))
}

// reservedPaths are the operational endpoints. They, any path below them
// and everything under /api/ belong to the mux and never reach the
// dashboard: a probe or scrape aimed at one the server lacks (/livez) gets
// a JSON 404, not index.html with 200.
var reservedPaths = []string{"/healthz", "/readyz", "/livez", "/metrics"}

// isServerPath reports whether p is routed to the mux.
func isServerPath(p string) bool {
	if p == "/api" || strings.HasPrefix(p, "/api/") {
		return true
	}
	for _, rp := range reservedPaths {
		if p == rp || strings.HasPrefix(p, rp+"/") {
			return true
		}
	}
	return false
}

// handler is the served root: reserved paths and /api/* go to the mux,
// everything else is the embedded dashboard. Routing by prefix — not a
// "GET /" catch-all route — keeps the mux's wrong-method semantics intact:
// a DELETE on a GET-only API path must stay 405 + Allow, and a catch-all
// would match it instead. Static assets are unauthenticated — the SPA
// itself sends the read token with every API call.
func (s *Server) handler() http.Handler {
	spa := spaHandler(distFS())
	return securityHeaders(s.metrics.instrument(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if isServerPath(r.URL.Path) {
			s.serveMux(w, r)
			return
		}
		spa.ServeHTTP(w, r)
	})))
}

// serveMux routes to the mux, answering an unregistered path with the
// API's JSON 404 instead of ServeMux's plain-text one. mux.Handler returns
// an empty pattern for both a missing route and a wrong method, so only
// the 404 is rewritten; a 405 passes through with its Allow header.
func (s *Server) serveMux(w http.ResponseWriter, r *http.Request) {
	if _, pattern := s.mux.Handler(r); pattern == "" {
		w = &jsonNotFound{ResponseWriter: w}
	}
	s.mux.ServeHTTP(w, r)
}

// jsonNotFound replaces a 404 written by ServeMux's NotFound handler with
// errJSON's body and drops the plain-text one.
type jsonNotFound struct {
	http.ResponseWriter
	replaced bool
}

func (w *jsonNotFound) WriteHeader(code int) {
	if code == http.StatusNotFound {
		w.replaced = true
		errJSON(w.ResponseWriter, http.StatusNotFound, "unknown path")
		return
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *jsonNotFound) Write(b []byte) (int, error) {
	if w.replaced {
		return len(b), nil
	}
	return w.ResponseWriter.Write(b)
}

// handleReadyz: GET /readyz — 200 when the store answers a ping within 2s,
// else 503. The chart's readinessProbe uses it, so a server whose database
// is unreachable leaves the Service; /healthz (liveness) never touches the
// store, so the outage does not restart it.
func (s *Server) handleReadyz(w http.ResponseWriter, r *http.Request) {
	p, ok := s.cfg.Store.(interface{ Ping(context.Context) error })
	if !ok {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	if err := p.Ping(ctx); err != nil {
		log.Printf("server: readyz: store ping: %v", err)
		errJSON(w, http.StatusServiceUnavailable, "store unreachable")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// contentSecurityPolicy fits the built dashboard: one module script and one
// stylesheet from /assets, fetch() to the same origin, a data: favicon. No
// inline or eval'd script is allowed — the SPA keeps the read token in
// localStorage, so script injection is what this guards. Styles allow
// 'unsafe-inline' for index.html's pre-paint <style> block and the HTML
// export's inline stylesheet; injected CSS cannot read localStorage.
const contentSecurityPolicy = "default-src 'self'; script-src 'self'; " +
	"style-src 'self' 'unsafe-inline'; img-src 'self' data:; connect-src 'self'; " +
	"object-src 'none'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'"

// securityHeaders sets defense-in-depth headers on every response the
// handler writes: dashboard, assets, API, exports and their errors alike.
// Responses net/http writes before any handler runs (431, a 400 for a
// malformed request, 501 for an unknown Transfer-Encoding) carry none;
// their bodies are fixed text. X-Frame-Options backs up frame-ancestors
// for browsers without CSP level 2.
func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", contentSecurityPolicy)
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("X-Frame-Options", "DENY")
		next.ServeHTTP(w, r)
	})
}

// Handler exposes the full route table (API + dashboard fallback) for
// httptest and embedding.
func (s *Server) Handler() http.Handler { return s.handler() }

// acquireSlot waits up to timeout for one of slots (a /gate evaluation, a
// snapshot ingest), or until gone is closed (the client went away; nil
// waits regardless, for work that outlives its client). On success the
// caller must call release (idempotent); otherwise the 503 + Retry-After
// with busy (or nothing, once gone is closed) has been written.
func acquireSlot(w http.ResponseWriter, gone <-chan struct{}, slots chan struct{}, timeout time.Duration, busy string) (release func(), ok bool) {
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case slots <- struct{}{}:
		return sync.OnceFunc(func() { <-slots }), true
	case <-gone:
		return nil, false
	case <-timer.C:
		w.Header().Set("Retry-After", "10")
		errJSON(w, http.StatusServiceUnavailable, busy)
		return nil, false
	}
}

// inReadSlot runs h in the read slot (maxConcurrentReads) with its
// response written to memory, and sends it as send does: the slot is held
// for loading, decoding and evaluating, not for a client's reading.
func (s *Server) inReadSlot(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		s.heldIn(w, r, h, s.readSlots, s.readQueueTimeout, "too many concurrent reads; retry shortly")
	}
}

// inFleetSlot runs h, a read of the whole fleet, in a fleet slot
// (maxConcurrentFleetReads) with its response written to memory, and
// sends it as send does.
func (s *Server) inFleetSlot(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		s.heldIn(w, r, h, s.fleetSlots, s.fleetQueueTimeout, "too many concurrent fleet reads; retry shortly")
	}
}

// inMetricsSlot is inFleetSlot for a Prometheus scrape, which waits for
// the slot only metricsQueueTimeout.
func (s *Server) inMetricsSlot(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		s.heldIn(w, r, h, s.fleetSlots, s.metricsQueueTimeout, "too many concurrent fleet reads; retry shortly")
	}
}

// heldIn runs h in one of slots (waiting up to timeout, then 503 with
// busy) with its response written to memory, and sends it.
func (s *Server) heldIn(w http.ResponseWriter, r *http.Request, h http.HandlerFunc, slots chan struct{}, timeout time.Duration, busy string) {
	release, ok := acquireSlot(w, r.Context().Done(), slots, timeout, busy)
	if !ok {
		return
	}
	defer release()
	resp := newHeldResponse()
	h(resp, r)
	s.send(w, resp, release)
}

// send sends resp, written in a slot that release gives back (see
// maxHeldResponses). A response that fits the held-response budget is
// charged to it until its write returns, and the slot is released first.
// One that does not fit what is left of the budget is answered 503 +
// Retry-After instead, and one larger than the whole budget is sent in
// the slot, under slotWriteTimeout. Its Content-Length lets a client tell
// a response cut off by a write deadline from a whole one.
func (s *Server) send(w http.ResponseWriter, resp *heldResponse, release func()) {
	size := int64(resp.body.Cap())
	switch {
	case s.heldResponses.charge(0, size):
		defer s.heldResponses.give(size)
		release()
	case size <= s.heldResponses.max:
		release()
		w.Header().Set("Retry-After", "10")
		errJSON(w, http.StatusServiceUnavailable, "too many responses waiting for their clients; retry shortly")
		return
	default:
		// The error is for a writer with no connection (a test
		// recorder), which has nothing to wait for.
		_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(s.slotWriteTimeout))
	}
	h := w.Header()
	for k, v := range resp.header {
		h[k] = v
	}
	if resp.body.Len() > 0 {
		h.Set("Content-Length", strconv.Itoa(resp.body.Len()))
	}
	w.WriteHeader(resp.status)
	_, _ = w.Write(resp.body.Bytes())
}

// heldResponse is an http.ResponseWriter that keeps the response, headers
// included, in memory until send copies it to the real writer: a 503 from
// send carries none of the handler's headers.
type heldResponse struct {
	header  http.Header
	status  int
	written bool
	body    bytes.Buffer
}

func newHeldResponse() *heldResponse {
	return &heldResponse{header: http.Header{}, status: http.StatusOK}
}

func (h *heldResponse) Header() http.Header { return h.header }

func (h *heldResponse) WriteHeader(code int) {
	if !h.written {
		h.status, h.written = code, true
	}
}

func (h *heldResponse) Write(p []byte) (int, error) {
	h.written = true
	return h.body.Write(p)
}

// reset drops what was written, headers and status included, so the
// handler can answer afresh; the body's memory goes with it.
func (h *heldResponse) reset() {
	*h = *newHeldResponse()
}

// Start binds Config.Listen and serves until Shutdown. It returns nil after
// a clean Shutdown, otherwise the listen/serve error. Once Ready() is
// closed, Addr() reports the bound address (Listen ":0" works in tests).
//
// Without a read token the read API is open, which Start allows only on
// a loopback address unless AllowAnonymousRead says otherwise. It checks
// the address it actually bound, after binding, so no name resolution
// decides it: "localhost" mapped to a routable address in /etc/hosts, a
// hostname, or ":8080" (every interface) is refused, and nothing is
// served on it in between.
func (s *Server) Start() error {
	ln, err := s.listen("tcp", s.cfg.Listen)
	if err != nil {
		return fmt.Errorf("server: listen %s: %w", s.cfg.Listen, err)
	}
	if s.cfg.ReadToken == "" && !s.cfg.AllowAnonymousRead && !isLoopbackAddr(ln.Addr()) {
		ln.Close()
		return fmt.Errorf("server: refusing to serve the read API and /api/v1/gate without a read token on %s (from %q), "+
			"which is not a loopback address: set a read token, listen on loopback, or allow anonymous reads", ln.Addr(), s.cfg.Listen)
	}
	// Background work: the notification worker, the re-evaluation ticker
	// and, with a retention window, the pruner (the first passes of both
	// run now). Stopped by Shutdown.
	bg, stop := context.WithCancel(context.Background())
	s.mu.Lock()
	if s.shutDown {
		// Shutdown came first (a signal during startup) and found no
		// background work to stop: start none, and serve nothing.
		s.mu.Unlock()
		stop()
		ln.Close()
		return nil
	}
	s.addr = ln.Addr().String()
	s.stopBackground = stop
	s.mu.Unlock()
	s.backgroundDone.Add(2)
	go func() { defer s.backgroundDone.Done(); s.runOutbox(bg) }()
	go func() { defer s.backgroundDone.Done(); s.runReevaluation(bg) }()
	if s.cfg.Retention > 0 {
		s.backgroundDone.Add(1)
		go func() { defer s.backgroundDone.Done(); s.runRetention(bg) }()
	}
	s.logStartup()
	close(s.ready)
	if s.httpSrv.TLSConfig != nil {
		err = s.httpSrv.ServeTLS(ln, "", "") // certificate already in TLSConfig
	} else {
		err = s.httpSrv.Serve(ln)
	}
	if !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// isLoopbackAddr reports whether a bound address is a loopback IP (any of
// 127.0.0.0/8, or ::1).
func isLoopbackAddr(a net.Addr) bool {
	tcp, ok := a.(*net.TCPAddr)
	return ok && tcp.IP.IsLoopback()
}

// logStartup reports the bound address and warns about configurations an
// operator should know they are running: a silent start used to hide that
// every read endpoint was open.
func (s *Server) logStartup() {
	scheme := "http"
	if s.httpSrv.TLSConfig != nil {
		scheme = "https"
	}
	log.Printf("server: listening on %s://%s", scheme, s.Addr())
	if s.cfg.ReadToken == "" {
		log.Printf("WARN server: no read token: the read API, dashboard data and /api/v1/gate are open to anyone who can reach %s", s.Addr())
	}
	if s.cfg.AdminToken == "" {
		log.Printf("server: no admin token: cluster delete and rename (DELETE/PATCH /api/v1/clusters/{id}) are refused")
	}
	if s.cfg.IngestToken == "" {
		log.Printf("server: no shared ingest token: snapshot pushes need a per-cluster token ('upgradescope tokens create')")
		return
	}
	// The shared token is not bound to a cluster: whoever holds it can push
	// as any cluster, including ones that moved to per-cluster tokens.
	// Checked once here: a token minted later with `tokens create` does not
	// re-trigger the warning until the next start (the flag help says so).
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	toks, err := s.cfg.Store.ListTokens(ctx, "")
	if err != nil {
		log.Printf("server: listing per-cluster tokens: %v", err)
		return
	}
	for _, tk := range toks {
		if tk.RevokedAt == nil {
			log.Printf("WARN server: a shared ingest token is set alongside per-cluster tokens; it can push as any cluster — drop it once every agent has its own token")
			return
		}
	}
}

// Ready is closed once the listener is bound. It is NEVER closed when
// Listen fails — Start just returns the error — so callers must not block
// on Ready alone: run Start in a goroutine and select on Ready() AND the
// goroutine's error channel, or a bad Listen address hangs the caller.
func (s *Server) Ready() <-chan struct{} { return s.ready }

// Addr returns the bound listen address ("" before Ready is closed).
func (s *Server) Addr() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.addr
}

// Shutdown gracefully drains in-flight requests until ctx ends, then
// closes whatever is still open, and unblocks Start. A connection still
// active at the deadline is a stalled or abusive client (ReadTimeout and
// WriteTimeout bound every legitimate request), so cutting it off is the
// expected end of the drain, not a shutdown failure.
//
// A Shutdown that comes before Start has started serving makes Start return
// nil without serving or starting any background work.
//
// Background work stops after the drain: a notification mid-delivery is
// cancelled and stays in the outbox (its lease expires and the next start
// delivers it); a re-evaluation mid-commit rolls back.
func (s *Server) Shutdown(ctx context.Context) error {
	err := s.httpSrv.Shutdown(ctx)
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		log.Printf("server: drain window ended with connections still open; closing them")
		err = s.httpSrv.Close()
	}
	s.mu.Lock()
	s.shutDown = true
	stop := s.stopBackground
	s.mu.Unlock()
	if stop != nil {
		stop()
		s.backgroundDone.Wait()
	}
	return err
}
