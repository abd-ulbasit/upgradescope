package mcp

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"sync"
	"time"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// The streamable HTTP transport's session limits, as `upgradescope mcp
// --http` serves it.
const (
	// DefaultSessionTimeout closes a session that has had no request for
	// this long. A client that exits without a DELETE (or never sends one)
	// leaves its session behind, and each holds a goroutine and some
	// memory; a client whose session expired gets 404 and starts another,
	// as the MCP specification says it must.
	DefaultSessionTimeout = 10 * time.Minute
	// DefaultMaxSessions is the most sessions kept at once. One person's
	// clients need a handful; past the bound a new session is refused.
	DefaultMaxSessions = 100
)

// HTTPOptions bound the sessions of the streamable HTTP transport.
type HTTPOptions struct {
	// SessionTimeout closes a session idle this long; it must be positive.
	SessionTimeout time.Duration
	// MaxSessions is the most sessions open at once; it must be positive.
	MaxSessions int
}

// callHeader carries, on each HTTP request NewHTTPHandler passes to the
// SDK, an id for the request's context, through to the tool call it
// carries (the SDK hands a call the request's header, not its context).
// A client's own value is replaced.
const callHeader = "Upgradescope-Mcp-Http-Request"

// errCallerGone is the cause of a call cancelled because the HTTP request
// that carried it ended before its answer: the client closed the
// connection, or went away.
var errCallerGone = errors.New("the HTTP request that carried the call ended (the client closed the connection)")

// NewHTTPHandler serves srv over MCP's streamable HTTP transport, the way
// `upgradescope mcp --http` does:
//
//   - a request from another origin in a browser is refused (403), and the
//     SDK refuses one whose Host is not loopback arriving on a loopback
//     address (DNS rebinding), and a body over 4 MiB;
//   - a session idle for opts.SessionTimeout is closed, and at most
//     opts.MaxSessions are open at once: a new one past that is refused
//     with 503;
//   - a tools/call whose HTTP request ends before it is answered (the client
//     closed the connection or went away) is cancelled, whatever the
//     protocol version. The SDK's own PropagateRequestCancellation does
//     that only for the 2026-07-28 protocol on a stateless handler, and
//     this one keeps sessions; with no event store the answer could never
//     be delivered, so a scan would hold the scan slot for nothing.
//
// It adds a receiving middleware to srv, so srv serves this handler alone.
func NewHTTPHandler(srv *mcpsdk.Server, opts HTTPOptions) http.Handler {
	if opts.SessionTimeout <= 0 || opts.MaxSessions <= 0 {
		panic("mcp: NewHTTPHandler needs a positive SessionTimeout and MaxSessions")
	}
	calls := &httpCalls{live: map[string]context.Context{}}
	srv.AddReceivingMiddleware(calls.cancelWithRequest)
	sdk := mcpsdk.NewStreamableHTTPHandler(func(*http.Request) *mcpsdk.Server { return srv }, &mcpsdk.StreamableHTTPOptions{
		SessionTimeout: opts.SessionTimeout,
		// For the requests the SDK applies it to (see above); calls does
		// it for the others.
		PropagateRequestCancellation: true,
	})
	capped := &sessionCap{srv: srv, opts: opts, next: calls.track(sdk)}
	return http.NewCrossOriginProtection().Handler(capped)
}

// httpCalls ties each tool call to the HTTP request that carries it.
type httpCalls struct {
	mu   sync.Mutex
	next uint64
	live map[string]context.Context // callHeader id → the request's context, while it is served
}

// track gives each request an id in callHeader and keeps its context under
// that id while the request is served.
func (c *httpCalls) track(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c.mu.Lock()
		c.next++
		id := strconv.FormatUint(c.next, 10)
		c.live[id] = r.Context()
		c.mu.Unlock()
		defer func() {
			c.mu.Lock()
			delete(c.live, id)
			c.mu.Unlock()
		}()
		r.Header.Set(callHeader, id)
		next.ServeHTTP(w, r)
	})
}

// cancelWithRequest runs a tools/call under a context that also ends with
// the HTTP request that carried it. A call that arrived on no HTTP request
// (stdio, a test's in-memory transport) is left as it is.
func (c *httpCalls) cancelWithRequest(next mcpsdk.MethodHandler) mcpsdk.MethodHandler {
	return func(ctx context.Context, method string, req mcpsdk.Request) (mcpsdk.Result, error) {
		extra := req.GetExtra()
		if method != "tools/call" || extra == nil || extra.Header == nil || extra.Header.Get(callHeader) == "" {
			return next(ctx, method, req)
		}
		c.mu.Lock()
		reqCtx, ok := c.live[extra.Header.Get(callHeader)]
		c.mu.Unlock()
		if !ok { // the request ended before the call was handled
			return nil, errCallerGone
		}
		ctx, cancel := context.WithCancelCause(ctx)
		defer cancel(nil)
		stop := context.AfterFunc(reqCtx, func() { cancel(errCallerGone) })
		defer stop()
		return next(ctx, method, req)
	}
}

// sessionCap refuses a request that would open a session past
// opts.MaxSessions.
type sessionCap struct {
	srv  *mcpsdk.Server
	opts HTTPOptions
	next http.Handler

	mu      sync.Mutex
	opening int // requests that may open a session, being served
}

func (s *sessionCap) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Only a POST without a session id opens a session.
	if r.Method != http.MethodPost || r.Header.Get("Mcp-Session-Id") != "" {
		s.next.ServeHTTP(w, r)
		return
	}
	s.mu.Lock()
	open := s.opening
	for range s.srv.Sessions() {
		open++
	}
	if open >= s.opts.MaxSessions {
		s.mu.Unlock()
		w.Header().Set("Retry-After", strconv.Itoa(int(min(s.opts.SessionTimeout, time.Minute)/time.Second)))
		http.Error(w, fmt.Sprintf("too many open MCP sessions (%d, the most this server keeps): a client that exits closes its own, and a session idle for %s is closed; retry later", s.opts.MaxSessions, s.opts.SessionTimeout), http.StatusServiceUnavailable)
		return
	}
	s.opening++
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.opening--
		s.mu.Unlock()
	}()
	s.next.ServeHTTP(w, r)
}
