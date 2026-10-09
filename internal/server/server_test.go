package server

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log"
	"maps"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
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
	// The shared ingest token is optional: per-cluster tokens alone suffice.
	if _, err := New(Config{Store: st, KB: testKB(), IngestToken: ""}); err != nil {
		t.Fatalf("New without a shared IngestToken: %v", err)
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
	fmt.Fprintf(conn, "POST /api/v1/gate?target=1.35 HTTP/1.1\r\nHost: localhost\r\n"+
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
	fmt.Fprintf(conn, "GET /healthz HTTP/1.1\r\nHost: localhost\r\n\r\n")
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

// captureLog redirects the standard logger for the rest of the test.
func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf syncBuffer
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	return &buf.Buffer
}

// syncBuffer is a bytes.Buffer safe for the logger's concurrent writes.
type syncBuffer struct {
	mu sync.Mutex
	bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.Buffer.Write(p)
}

// Start announces where it listens, and warns loudly when the read API is
// open because no read token is configured.
func TestStartLogsListenAndOpenReadWarning(t *testing.T) {
	out := captureLog(t)
	s := newTestServer(t, newFakeStore(), func(c *Config) { c.Listen = "127.0.0.1:0" })
	startServer(t, s)
	got := out.String()
	if !strings.Contains(got, "listening on http://"+s.Addr()) {
		t.Errorf("log %q lacks the listening line", got)
	}
	if !strings.Contains(got, "WARN") || !strings.Contains(got, "no read token") {
		t.Errorf("log %q lacks the open-read WARN", got)
	}
}

func TestStartNoOpenReadWarningWithReadToken(t *testing.T) {
	out := captureLog(t)
	s := newTestServer(t, newFakeStore(), func(c *Config) {
		c.Listen = "127.0.0.1:0"
		c.ReadToken = "read-tok"
	})
	startServer(t, s)
	if got := out.String(); !strings.Contains(got, "listening on") || strings.Contains(got, "no read token") {
		t.Errorf("log %q: want the listening line and no open-read WARN", got)
	}
}

// A shared ingest token can push as ANY cluster, so running it next to
// per-cluster tokens deserves a startup WARN.
func TestStartWarnsSharedTokenAlongsidePerClusterTokens(t *testing.T) {
	const warn = "WARN server: a shared ingest token is set alongside per-cluster tokens"
	for _, tc := range []struct {
		name     string
		shared   string
		tokens   map[string]*fakeToken
		wantWarn bool
	}{
		{"shared only", "ingest-tok", nil, false},
		{"per-cluster only", "", map[string]*fakeToken{"p": {id: 1, cluster: "prod"}}, false},
		{"both", "ingest-tok", map[string]*fakeToken{"p": {id: 1, cluster: "prod"}}, true},
		{"both, per-cluster revoked", "ingest-tok", map[string]*fakeToken{"p": {id: 1, cluster: "prod", revoked: true}}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := captureLog(t)
			st := newFakeStore()
			maps.Copy(st.tokens, tc.tokens)
			s := newTestServer(t, st, func(c *Config) {
				c.Listen = "127.0.0.1:0"
				c.IngestToken = tc.shared
				c.ReadToken = "read-tok"
			})
			startServer(t, s)
			if got := strings.Contains(out.String(), warn); got != tc.wantWarn {
				t.Fatalf("WARN %q logged = %v, want %v; log:\n%s", warn, got, tc.wantWarn, out)
			}
		})
	}
}

// writeSelfSignedCert writes a throwaway ECDSA certificate for 127.0.0.1
// and its key as PEM files, returning their paths and the certificate.
func writeSelfSignedCert(t *testing.T) (certFile, keyFile string, cert *x509.Certificate) {
	t.Helper()
	certPEM, keyPEM, cert := selfSignedPEM(t, 1)
	dir := t.TempDir()
	certFile, keyFile = filepath.Join(dir, "tls.crt"), filepath.Join(dir, "tls.key")
	if err := os.WriteFile(certFile, certPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyFile, keyPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	return certFile, keyFile, cert
}

// selfSignedPEM is a throwaway ECDSA certificate for 127.0.0.1 with the
// given serial, and its key, PEM-encoded.
func selfSignedPEM(t *testing.T, serial int64) (certPEM, keyPEM []byte, cert *x509.Certificate) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(serial),
		Subject:      pkix.Name{CommonName: "upgradescope-test"},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	if cert, err = x509.ParseCertificate(der); err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), cert
}

// With a certificate and key the server speaks HTTPS directly — agents in
// other clusters need no TLS-terminating proxy to keep tokens off the wire.
func TestServeTLS(t *testing.T) {
	certFile, keyFile, cert := writeSelfSignedCert(t)
	out := captureLog(t)
	s := newTestServer(t, newFakeStore(), func(c *Config) {
		c.Listen = "127.0.0.1:0"
		c.TLSCertFile = certFile
		c.TLSKeyFile = keyFile
	})
	startServer(t, s)

	pool := x509.NewCertPool()
	pool.AddCert(cert)
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool}}}
	resp, err := client.Get("https://" + s.Addr() + "/healthz")
	if err != nil {
		t.Fatalf("GET https /healthz: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || resp.TLS == nil {
		t.Fatalf("status = %d, TLS = %v; want 200 over TLS", resp.StatusCode, resp.TLS != nil)
	}
	if !strings.Contains(out.String(), "listening on https://"+s.Addr()) {
		t.Errorf("log %q lacks the https listening line", out)
	}

	// Plain HTTP to the TLS port is refused, not served.
	if resp, err := http.Get("http://" + s.Addr() + "/healthz"); err == nil {
		resp.Body.Close()
		if resp.StatusCode == http.StatusOK {
			t.Fatal("plain HTTP request to the TLS listener got 200")
		}
	}
}

func TestNewTLSValidation(t *testing.T) {
	certFile, keyFile, _ := writeSelfSignedCert(t)
	for name, mod := range map[string]func(*Config){
		"cert without key": func(c *Config) { c.TLSCertFile = certFile },
		"key without cert": func(c *Config) { c.TLSKeyFile = keyFile },
		"missing files": func(c *Config) {
			c.TLSCertFile, c.TLSKeyFile = filepath.Join(t.TempDir(), "nope.crt"), keyFile
		},
		"key does not match": func(c *Config) { c.TLSCertFile, c.TLSKeyFile = certFile, certFile },
	} {
		cfg := Config{Store: newFakeStore(), KB: testKB()}
		mod(&cfg)
		if _, err := New(cfg); err == nil {
			t.Errorf("%s: New succeeded, want error", name)
		}
	}
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
	body["padding"] = strings.Repeat("x", DefaultMaxSnapshotBytes-(1<<20)) // unknown field: ignored, but on the wire
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

// A Shutdown that comes before Start has started its background work (a
// signal during startup, or runServe given a cancelled context) must still
// stop it: Start then serves nothing and starts no worker that outlives
// it. It used to start the outbox and re-evaluation workers after
// Shutdown had found nothing to stop, and they kept the store busy after
// the caller closed it (TestRunServeCreatesDBParentDir's flaky TempDir
// cleanup).
func TestShutdownBeforeStartStopsTheBackground(t *testing.T) {
	s := newTestServer(t, newFakeStore(), func(c *Config) { c.Listen = "127.0.0.1:0" })
	if err := s.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown before Start = %v, want nil", err)
	}
	errCh := make(chan error, 1)
	go func() { errCh <- s.Start() }()
	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("Start after Shutdown = %v, want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Start after Shutdown did not return")
	}
	done := make(chan struct{})
	go func() { s.backgroundDone.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("background workers still running after Shutdown and Start returned")
	}
}

// Start and a Shutdown that races it (runServe on a context cancelled
// while Start runs): whichever goes first, every background worker Start
// starts is one Shutdown waits for. Start used to count its workers in
// backgroundDone after releasing the lock Shutdown reads stopBackground
// under, so Shutdown's Wait could run concurrently with that Add (a data
// race under -race) and return before the workers it was to stop had
// been counted.
func TestShutdownRacingStartStopsTheBackground(t *testing.T) {
	for i := 0; i < 50; i++ {
		s := newTestServer(t, newFakeStore(), func(c *Config) {
			c.Listen = "127.0.0.1:0"
			c.Retention = time.Hour
		})
		errCh := make(chan error, 1)
		go func() { errCh <- s.Start() }()
		if err := s.Shutdown(context.Background()); err != nil {
			t.Fatalf("Shutdown racing Start = %v, want nil", err)
		}
		select {
		case err := <-errCh:
			if err != nil {
				t.Fatalf("Start = %v, want nil", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("Start did not return after Shutdown")
		}
		done := make(chan struct{})
		go func() { s.backgroundDone.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatalf("iteration %d: background workers still running after Shutdown and Start returned", i)
		}
	}
}
