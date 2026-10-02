// Package server is the upgradescope continuous-mode server: snapshot
// ingest, persisted evaluations, a read API, and on-demand what-if
// re-evaluation. Storage is behind store.Store; the CLI owns flag parsing
// and store construction (DB path never reaches this package).
package server

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
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

// Config wires a Server.
type Config struct {
	Listen       string          // listen address for Start, e.g. ":8080"
	Store        store.Store     // required
	KB           kb.KB           // evaluation knowledge base
	ExtraTargets []string        // minors evaluated for every snapshot, e.g. ["1.37"]
	Notifier     notify.Notifier // nil = notifications disabled
	IngestToken  string          // optional shared bearer for POST /api/v1/snapshots (any cluster); "" = per-cluster tokens only
	ReadToken    string          // optional bearer for the read API; "" = open (document loudly)
	AdminToken   string          // bearer for cluster delete/rename (also accepted for reads); "" = both refused
	TeamMap      TeamMap         // optional namespace→team override, applied before every Evaluate
	Version      string          // build version stamped into SARIF tool metadata ("" = omitted)

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
// memory: the worst document that passes maxManifestDocBytes peaks the
// process at ~260 MB RSS, and two at once at ~480 MB — too close to the
// chart's 512Mi limit. So evaluations run one at a time (a normal one
// takes milliseconds). A request asks for the slot only once its whole
// body is in, so a slow uploader cannot hold it; it waits up to
// gateQueueTimeout for the slot, then gets 503 + Retry-After.
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
const (
	maxConcurrentGates    = 1
	gateQueueTimeout      = 30 * time.Second
	maxBufferedGateBodies = 3
)

// Server serves the ingest + read API. Construct with New; a Server is
// single-use (one Start/Shutdown cycle).
type Server struct {
	cfg          Config
	extraTargets []inventory.Version
	mux          *http.ServeMux
	httpSrv      *http.Server
	now          func() time.Time // injected clock: EOL math + timestamps stay testable

	gateSlots        chan struct{} // semaphore: one token per running /gate evaluation
	gateQueueTimeout time.Duration // how long a /gate request waits for a slot
	gateBuffered     *byteBudget   // /gate body bytes held across requests

	teamMapHash        string        // fingerprint of cfg.TeamMap stored with evaluations
	sinks              []sink        // cfg.Notifier flattened; outbox messages are per sink
	outboxKick         chan struct{} // wakes the delivery worker after a commit
	notifyTimeout      time.Duration // bounds one delivery attempt
	reevaluateInterval time.Duration // background re-evaluation period
	stopBackground     context.CancelFunc
	backgroundDone     sync.WaitGroup

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
		mux:              http.NewServeMux(),
		gateSlots:        make(chan struct{}, maxConcurrentGates),
		gateQueueTimeout: gateQueueTimeout,
		ready:            make(chan struct{}),
	}
	s.gateBuffered = newByteBudget(maxBufferedGateBodies * s.maxGateBytes())
	s.teamMapHash = hashTeamMap(cfg.TeamMap)
	s.sinks = sinksOf(cfg.Notifier)
	s.outboxKick = make(chan struct{}, 1)
	s.notifyTimeout = notifyTimeout
	s.reevaluateInterval = reevaluateInterval
	for _, t := range cfg.ExtraTargets {
		v, err := inventory.ParseTarget(t)
		if err != nil {
			return nil, fmt.Errorf("server: bad extra target %q: %w", t, err)
		}
		s.extraTargets = append(s.extraTargets, v)
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
	s.mux.HandleFunc("GET /metrics", s.readAuth(s.metrics.handler().ServeHTTP))
	s.mux.HandleFunc("POST /api/v1/snapshots", s.handleIngest)
	s.mux.HandleFunc("GET /api/v1/clusters", s.readAuth(s.handleListClusters))
	s.mux.HandleFunc("GET /api/v1/clusters/{id}", s.readAuth(s.handleGetCluster))
	s.mux.HandleFunc("DELETE /api/v1/clusters/{id}", s.adminAuth(s.handleDeleteCluster))
	s.mux.HandleFunc("PATCH /api/v1/clusters/{id}", s.adminAuth(s.handleRenameCluster))
	s.mux.HandleFunc("GET /api/v1/clusters/{id}/report", s.readAuth(s.handleReport))
	s.mux.HandleFunc("GET /api/v1/clusters/{id}/findings", s.readAuth(s.handleFindings))
	s.mux.HandleFunc("GET /api/v1/clusters/{id}/history", s.readAuth(s.handleHistory))
	s.mux.HandleFunc("GET /api/v1/clusters/{id}/teams", s.readAuth(s.handleTeams))
	s.mux.HandleFunc("GET /api/v1/fleet", s.readAuth(s.handleFleet))
	s.mux.HandleFunc("GET /api/v1/fleet/teams", s.readAuth(s.handleFleetTeams))
	s.mux.HandleFunc("POST /api/v1/gate", s.readAuth(s.handleGate))
	s.mux.HandleFunc("GET /api/v1/clusters/{id}/export", s.readAuth(s.handleExport))
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

// securityHeaders sets defense-in-depth headers on every response:
// dashboard, assets, API and exports alike. X-Frame-Options backs up
// frame-ancestors for browsers without CSP level 2.
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

// acquireGateSlot waits for a /gate evaluation slot. On success the caller
// must call release (idempotent); otherwise the 503 (or nothing, for a
// client that went away) has been written.
func (s *Server) acquireGateSlot(w http.ResponseWriter, r *http.Request) (release func(), ok bool) {
	timer := time.NewTimer(s.gateQueueTimeout)
	defer timer.Stop()
	select {
	case s.gateSlots <- struct{}{}:
		return sync.OnceFunc(func() { <-s.gateSlots }), true
	case <-r.Context().Done():
		return nil, false
	case <-timer.C:
		w.Header().Set("Retry-After", "10")
		errJSON(w, http.StatusServiceUnavailable, "too many concurrent gate evaluations; retry shortly")
		return nil, false
	}
}

// Start binds Config.Listen and serves until Shutdown. It returns nil after
// a clean Shutdown, otherwise the listen/serve error. Once Ready() is
// closed, Addr() reports the bound address (Listen ":0" works in tests).
func (s *Server) Start() error {
	ln, err := net.Listen("tcp", s.cfg.Listen)
	if err != nil {
		return fmt.Errorf("server: listen %s: %w", s.cfg.Listen, err)
	}
	// Background work: the notification worker and the re-evaluation
	// ticker (whose first pass runs now). Stopped by Shutdown.
	bg, stop := context.WithCancel(context.Background())
	s.mu.Lock()
	s.addr = ln.Addr().String()
	s.stopBackground = stop
	s.mu.Unlock()
	s.backgroundDone.Add(2)
	go func() { defer s.backgroundDone.Done(); s.runOutbox(bg) }()
	go func() { defer s.backgroundDone.Done(); s.runReevaluation(bg) }()
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
	stop := s.stopBackground
	s.mu.Unlock()
	if stop != nil {
		stop()
		s.backgroundDone.Wait()
	}
	return err
}
