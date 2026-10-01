package server

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/abd-ulbasit/upgradescope/internal/inventory"
	"github.com/abd-ulbasit/upgradescope/internal/kb"
)

// testKB is a tiny deterministic KB: one lifecycle entry (PodSecurityPolicy,
// deprecated 1.30, removed 1.35) and a MaxKnownK8s high enough that kb-stale
// never fires in tests.
func testKB() kb.KB {
	deprecated := inventory.Version{Major: 1, Minor: 30}
	removed := inventory.Version{Major: 1, Minor: 35}
	return kb.KB{
		Version: "test-kb",
		APILifecycle: []kb.APILifecycleEntry{{
			Group:      "policy",
			Version:    "v1beta1",
			Kind:       "PodSecurityPolicy",
			Introduced: inventory.Version{Major: 1, Minor: 10},
			Deprecated: &deprecated,
			Removed:    &removed,
		}},
		Skew:        kb.DefaultSkewPolicy(),
		MaxKnownK8s: inventory.Version{Major: 1, Minor: 99},
	}
}

func TestNewValidation(t *testing.T) {
	st := newFakeStore()
	if _, err := New(Config{Store: nil, KB: testKB(), IngestToken: "tok"}); err == nil {
		t.Fatal("New with nil Store: want error, got nil")
	}
	if _, err := New(Config{Store: st, KB: testKB(), IngestToken: ""}); err == nil {
		t.Fatal("New with empty IngestToken: want error, got nil")
	}
	if _, err := New(Config{Store: st, KB: testKB(), IngestToken: "tok", ExtraTargets: []string{"bogus"}}); err == nil {
		t.Fatal("New with unparseable extra target: want error, got nil")
	}
	s, err := New(Config{Store: st, KB: testKB(), IngestToken: "tok", ExtraTargets: []string{"1.37", "v1.38"}})
	if err != nil {
		t.Fatalf("New with valid config: %v", err)
	}
	if s.Handler() == nil {
		t.Fatal("Handler() returned nil")
	}
}

func TestStartShutdown(t *testing.T) {
	s, err := New(Config{
		Listen:      "127.0.0.1:0",
		Store:       newFakeStore(),
		KB:          testKB(),
		IngestToken: "tok",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	errc := make(chan error, 1)
	go func() { errc <- s.Start() }()
	select {
	case <-s.Ready():
	case err := <-errc:
		t.Fatalf("server exited before becoming ready: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("server never became ready")
	}
	// /nope.txt: a path with an extension never hits the SPA index.html
	// fallback, so this 404s whether or not `make web` has staged a real
	// dashboard bundle into webdist/ (extensionless /nope would be 200
	// index.html in a post-`make web` tree).
	resp, err := http.Get(fmt.Sprintf("http://%s/nope.txt", s.Addr()))
	if err != nil {
		t.Fatalf("GET while serving: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("GET /nope.txt status = %d, want 404 (unknown file, no SPA fallback)", resp.StatusCode)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := s.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if err := <-errc; err != nil {
		t.Fatalf("Start returned %v after graceful Shutdown, want nil", err)
	}
}

// startServer runs s.Start in the background and waits for Ready. The
// returned stop func shuts down gracefully and returns Start's result.
func startServer(t *testing.T, s *Server) (stop func(context.Context) error) {
	t.Helper()
	errc := make(chan error, 1)
	go func() { errc <- s.Start() }()
	select {
	case <-s.Ready():
	case err := <-errc:
		t.Fatalf("server exited before becoming ready: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("server never became ready")
	}
	var once sync.Once
	var startErr error
	stop = func(ctx context.Context) error {
		if err := s.Shutdown(ctx); err != nil {
			return err
		}
		once.Do(func() { startErr = <-errc })
		return startErr
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = stop(ctx)
	})
	return stop
}

// stallGate opens a raw connection, sends complete headers for a large
// POST /api/v1/gate plus a few body bytes, then stalls.
func stallGate(t *testing.T, addr string) net.Conn {
	t.Helper()
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	fmt.Fprintf(conn, "POST /api/v1/gate?target=1.35 HTTP/1.1\r\nHost: x\r\n"+
		"Content-Type: application/x-yaml\r\nContent-Length: 100000\r\n\r\nabc")
	return conn
}

// waitClosed reads conn until the server closes it, failing if that takes
// longer than within.
func waitClosed(t *testing.T, conn net.Conn, within time.Duration) {
	t.Helper()
	_ = conn.SetReadDeadline(time.Now().Add(within))
	if _, err := io.Copy(io.Discard, conn); err != nil {
		var ne net.Error
		if errors.As(err, &ne) && ne.Timeout() {
			t.Fatalf("connection still open after %s", within)
		}
	}
}

// A client that sends headers plus a partial body and then stalls is cut
// off by ReadTimeout instead of pinning a goroutine forever.
func TestStalledBodyDisconnectedByReadTimeout(t *testing.T) {
	s := newTestServer(t, newFakeStore(), func(c *Config) { c.Listen = "127.0.0.1:0" })
	if s.httpSrv.ReadTimeout == 0 || s.httpSrv.WriteTimeout == 0 || s.httpSrv.IdleTimeout == 0 {
		t.Fatalf("timeouts must be non-zero: read %s write %s idle %s",
			s.httpSrv.ReadTimeout, s.httpSrv.WriteTimeout, s.httpSrv.IdleTimeout)
	}
	if s.httpSrv.MaxHeaderBytes == 0 {
		t.Error("MaxHeaderBytes must be set")
	}
	s.httpSrv.ReadTimeout = 300 * time.Millisecond
	startServer(t, s)
	waitClosed(t, stallGate(t, s.Addr()), 5*time.Second)
}

// An idle keep-alive connection is closed after IdleTimeout.
func TestIdleKeepAliveClosed(t *testing.T) {
	s := newTestServer(t, newFakeStore(), func(c *Config) { c.Listen = "127.0.0.1:0" })
	s.httpSrv.IdleTimeout = 300 * time.Millisecond
	startServer(t, s)
	conn, err := net.Dial("tcp", s.Addr())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	fmt.Fprintf(conn, "GET /healthz HTTP/1.1\r\nHost: x\r\n\r\n")
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatalf("reading /healthz response: %v", err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/healthz status = %d", resp.StatusCode)
	}
	waitClosed(t, conn, 5*time.Second)
}

// A stalled client must not turn a graceful shutdown into an error: once
// the drain window expires, the remaining connections are closed and
// Shutdown/Start still report success.
func TestShutdownWithStalledClient(t *testing.T) {
	s := newTestServer(t, newFakeStore(), func(c *Config) { c.Listen = "127.0.0.1:0" })
	stop := startServer(t, s)
	conn := stallGate(t, s.Addr())
	time.Sleep(100 * time.Millisecond) // let the server start reading the body

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	began := time.Now()
	if err := stop(ctx); err != nil {
		t.Fatalf("shutdown with a stalled client = %v, want nil", err)
	}
	if took := time.Since(began); took > 2*time.Second {
		t.Fatalf("shutdown took %s, want it bounded by the drain window", took)
	}
	waitClosed(t, conn, 2*time.Second)
}

// The connection limits must not break the largest legitimate request: a
// snapshot just under the 20 MiB cap over a real listener with the default
// timeouts is still accepted.
func TestLargeSnapshotWithinDefaultTimeouts(t *testing.T) {
	s := newTestServer(t, newFakeStore(), func(c *Config) { c.Listen = "127.0.0.1:0" })
	startServer(t, s)

	var body map[string]any
	if err := json.Unmarshal(pushReqBody(t, testInventory()), &body); err != nil {
		t.Fatal(err)
	}
	body["padding"] = strings.Repeat("x", maxSnapshotBody-(1<<20)) // unknown field: ignored, but on the wire
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest(http.MethodPost, "http://"+s.Addr()+"/api/v1/snapshots", bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer ingest-tok")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST %d-byte snapshot: %v", len(raw), err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		msg, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d (%s), want 202", resp.StatusCode, msg)
	}
}
