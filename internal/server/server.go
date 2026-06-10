// Package server is the upgradescope continuous-mode server: snapshot
// ingest, persisted evaluations, a read API, and on-demand what-if
// re-evaluation. Storage is behind store.Store; the CLI owns flag parsing
// and store construction (DB path never reaches this package).
package server

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/abd-ulbasit/upgradescope/internal/inventory"
	"github.com/abd-ulbasit/upgradescope/internal/kb"
	"github.com/abd-ulbasit/upgradescope/internal/server/notify"
	"github.com/abd-ulbasit/upgradescope/internal/server/store"
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
		Handler:           s.mux,
		ReadHeaderTimeout: 10 * time.Second,
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
}

// Handler exposes the full route table for httptest and embedding.
func (s *Server) Handler() http.Handler { return s.mux }

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

// Shutdown gracefully drains in-flight requests, then unblocks Start.
func (s *Server) Shutdown(ctx context.Context) error {
	return s.httpSrv.Shutdown(ctx)
}
