// Package server is the upgradescope continuous-mode server: snapshot
// ingest, persisted evaluations, a read API, and on-demand what-if
// re-evaluation. Storage is behind store.Store; the CLI owns flag parsing
// and store construction (DB path never reaches this package).
package server

import (
	"context"
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
// handler (ingest evaluates and notifies synchronously).
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
	IngestToken  string          // required bearer for POST /api/v1/snapshots
	ReadToken    string          // optional bearer for the read API; "" = open (document loudly)
	TeamMap      TeamMap         // optional namespace→team override, applied before every Evaluate
	Version      string          // build version stamped into SARIF tool metadata ("" = omitted)
}

// Server serves the ingest + read API. Construct with New; a Server is
// single-use (one Start/Shutdown cycle).
type Server struct {
	cfg          Config
	extraTargets []inventory.Version
	mux          *http.ServeMux
	httpSrv      *http.Server
	now          func() time.Time // injected clock: EOL math + timestamps stay testable

	ready chan struct{} // closed once the listener is bound
	mu    sync.Mutex
	addr  string
}

// New validates cfg and builds the route table.
func New(cfg Config) (*Server, error) {
	if cfg.Store == nil {
		return nil, errors.New("server: Config.Store is required")
	}
	if cfg.IngestToken == "" {
		return nil, errors.New("server: Config.IngestToken is required")
	}
	s := &Server{
		cfg:   cfg,
		now:   time.Now,
		mux:   http.NewServeMux(),
		ready: make(chan struct{}),
	}
	for _, t := range cfg.ExtraTargets {
		v, err := inventory.ParseVersion(t)
		if err != nil {
			return nil, fmt.Errorf("server: bad extra target %q: %w", t, err)
		}
		s.extraTargets = append(s.extraTargets, v)
	}
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
	return s, nil
}

// routes registers all endpoints (Go 1.22 method+path patterns — ServeMux
// emits 405 + Allow for wrong methods on registered paths).
func (s *Server) routes() {
	s.mux.HandleFunc("GET /healthz", s.handleHealthz)
	s.mux.HandleFunc("POST /api/v1/snapshots", s.handleIngest)
	s.mux.HandleFunc("GET /api/v1/clusters", s.readAuth(s.handleListClusters))
	s.mux.HandleFunc("GET /api/v1/clusters/{id}", s.readAuth(s.handleGetCluster))
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

// handler is the served root: /healthz and /api/* go to the mux, everything
// else is the embedded dashboard. Routing by prefix — not a "GET /"
// catch-all route, and not mux.Handler() no-match detection — keeps the
// mux's wrong-method semantics intact: a DELETE on a GET-only API path must
// stay 405 + Allow, and both alternatives turn it into the SPA (mux.Handler
// returns an empty pattern on method mismatch). Static assets are
// unauthenticated — the SPA itself sends the read token with every API call.
func (s *Server) handler() http.Handler {
	spa := spaHandler(distFS())
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" || strings.HasPrefix(r.URL.Path, "/api/") {
			s.mux.ServeHTTP(w, r)
			return
		}
		spa.ServeHTTP(w, r)
	})
}

// Handler exposes the full route table (API + dashboard fallback) for
// httptest and embedding.
func (s *Server) Handler() http.Handler { return s.handler() }

// Start binds Config.Listen and serves until Shutdown. It returns nil after
// a clean Shutdown, otherwise the listen/serve error. Once Ready() is
// closed, Addr() reports the bound address (Listen ":0" works in tests).
func (s *Server) Start() error {
	ln, err := net.Listen("tcp", s.cfg.Listen)
	if err != nil {
		return fmt.Errorf("server: listen %s: %w", s.cfg.Listen, err)
	}
	s.mu.Lock()
	s.addr = ln.Addr().String()
	s.mu.Unlock()
	close(s.ready)
	if err := s.httpSrv.Serve(ln); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
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
func (s *Server) Shutdown(ctx context.Context) error {
	err := s.httpSrv.Shutdown(ctx)
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		log.Printf("server: drain window ended with connections still open; closing them")
		return s.httpSrv.Close()
	}
	return err
}
