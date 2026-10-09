package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/abd-ulbasit/upgradescope/internal/engine"
	"github.com/abd-ulbasit/upgradescope/internal/mcp"
)

// setMCPHTTPLimits changes the HTTP transport's limits for one test, so it
// need not wait out the real ones.
func setMCPHTTPLimits(t *testing.T, edit func(*mcpHTTPConfig)) {
	t.Helper()
	orig := mcpHTTPLimits
	edit(&mcpHTTPLimits)
	t.Cleanup(func() { mcpHTTPLimits = orig })
}

// goroutinesBackTo waits until the process runs at most base goroutines
// (and a little slack for the runtime's own), failing with how many are
// left.
func goroutinesBackTo(t *testing.T, base int, within time.Duration) {
	t.Helper()
	deadline := time.Now().Add(within)
	for {
		n := runtime.NumGoroutine()
		if n <= base+3 {
			return
		}
		if time.Now().After(deadline) {
			buf := make([]byte, 1<<20)
			t.Fatalf("%d goroutines after %s, want about %d as before:\n%s", n, within, base, buf[:runtime.Stack(buf, true)])
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// closedWithin reads conn until the server closes it, and fails unless it
// does within d.
func closedWithin(t *testing.T, what string, conn net.Conn, d time.Duration) {
	t.Helper()
	_ = conn.SetReadDeadline(time.Now().Add(d))
	_, err := io.Copy(io.Discard, conn)
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		t.Errorf("%s: the server kept the connection open for %s", what, d)
	}
}

func dial(t *testing.T, addr string) net.Conn {
	t.Helper()
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

// rawPOST writes a POST to /mcp on conn with the given headers (each a
// "Name: value" line) and body, and declares length as its Content-Length.
func rawPOST(t *testing.T, conn net.Conn, addr string, length int, body string, headers ...string) {
	t.Helper()
	req := fmt.Sprintf("POST /mcp HTTP/1.1\r\nHost: %s\r\nContent-Type: application/json\r\nAccept: application/json, text/event-stream\r\nContent-Length: %d\r\n", addr, length)
	for _, h := range headers {
		req += h + "\r\n"
	}
	if _, err := io.WriteString(conn, req+"\r\n"+body); err != nil {
		t.Fatal(err)
	}
}

// TestMCPHTTPClosesIdleAndStalledConnections: a connection that sends no
// request, a keep-alive connection left idle after its request, and one
// that stalls in the middle of a body are each closed by the server once
// its timeout is up, so a peer cannot hold connections and goroutines,
// token or not; and a request whose headers are over the limit is refused.
func TestMCPHTTPClosesIdleAndStalledConnections(t *testing.T) {
	setMCPHTTPLimits(t, func(c *mcpHTTPConfig) {
		c.readHeaderTimeout, c.readTimeout, c.idleTimeout = 300*time.Millisecond, 300*time.Millisecond, 300*time.Millisecond
	})
	addr, _ := startMCPHTTP(t, "--http-token", "s3cret")
	base := runtime.NumGoroutine()

	const each = 10
	var idle, stalled, silent []net.Conn
	for range each {
		c := dial(t, addr)
		rawPOST(t, c, addr, 2, "{}", "Authorization: Bearer s3cret")
		resp, err := http.ReadResponse(bufio.NewReader(c), nil)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if resp.Close {
			t.Fatalf("an authenticated request's connection was not kept alive (%s), so idling is not tested", resp.Status)
		}
		idle = append(idle, c)

		c = dial(t, addr)
		rawPOST(t, c, addr, 1000, "ab", "Authorization: Bearer s3cret")
		stalled = append(stalled, c)

		silent = append(silent, dial(t, addr))
	}
	time.Sleep(50 * time.Millisecond)
	if n := runtime.NumGoroutine(); n < base+2*each {
		t.Fatalf("%d goroutines with %d connections open, %d before: the connections are not being served", n, 3*each, base)
	}
	for i := range each {
		closedWithin(t, "an idle keep-alive connection", idle[i], 5*time.Second)
		closedWithin(t, "a stalled body", stalled[i], 5*time.Second)
		closedWithin(t, "a connection that sends nothing", silent[i], 5*time.Second)
	}
	goroutinesBackTo(t, base, 5*time.Second)

	c := dial(t, addr)
	rawPOST(t, c, addr, 2, "{}", "Authorization: Bearer s3cret", "X-Big: "+strings.Repeat("x", 100<<10))
	resp, err := http.ReadResponse(bufio.NewReader(c), nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusRequestHeaderFieldsTooLarge {
		t.Errorf("a request with 100 KiB of headers = %s, want 431", resp.Status)
	}

	big := `{"jsonrpc":"2.0","id":1,"method":"ping","params":{"x":"` + strings.Repeat("x", mcpsdk.DefaultMaxRequestBodyBytes) + `"}}`
	req, _ := http.NewRequest(http.MethodPost, "http://"+addr+"/mcp", strings.NewReader(big))
	req.Header.Set("Authorization", "Bearer s3cret")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if resp, err := http.DefaultClient.Do(req); err != nil {
		t.Errorf("a body over %d bytes: %v", mcpsdk.DefaultMaxRequestBodyBytes, err)
	} else {
		resp.Body.Close()
		if resp.StatusCode != http.StatusRequestEntityTooLarge {
			t.Errorf("a body over %d bytes = %s, want 413", mcpsdk.DefaultMaxRequestBodyBytes, resp.Status)
		}
	}
}

// TestMCPHTTPRefusalClosesTheConnection: a request without the token is
// answered 401 and its connection closed, without waiting for (or
// reading) a body it declared and did not send, and without keeping the
// connection alive for another try; nothing is left running.
func TestMCPHTTPRefusalClosesTheConnection(t *testing.T) {
	addr, _ := startMCPHTTP(t, "--http-token", "s3cret") // the real limits: a minute to read a request
	base := runtime.NumGoroutine()
	for i := range 20 {
		c := dial(t, addr)
		if i%2 == 0 {
			rawPOST(t, c, addr, 0, "")
		} else {
			rawPOST(t, c, addr, 1000, "ab") // stalls in its body
		}
		// A deadline of its own, so a server that drains the stalled body
		// before it answers, or keeps the connection, fails in seconds
		// rather than at the go test timeout.
		_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
		br := bufio.NewReader(c)
		resp, err := http.ReadResponse(br, nil)
		if err != nil {
			t.Fatalf("no answer to an unauthenticated request within 5s: %v", err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized || !resp.Close {
			t.Errorf("an unauthenticated request = %s (close %v), want 401 closing the connection", resp.Status, resp.Close)
		}
		if _, err := io.Copy(io.Discard, br); err != nil {
			t.Errorf("after a 401 the connection stayed open: %v", err)
		}
	}
	goroutinesBackTo(t, base, 5*time.Second)
}

// TestMCPHTTPScanOutlastsTheReadTimeout: the read timeout bounds reading a
// request, not answering it; a scan that takes longer still returns its
// result on the same response.
func TestMCPHTTPScanOutlastsTheReadTimeout(t *testing.T) {
	setMCPHTTPLimits(t, func(c *mcpHTTPConfig) {
		c.readHeaderTimeout, c.readTimeout, c.idleTimeout = 200*time.Millisecond, 200*time.Millisecond, 200*time.Millisecond
	})
	evaluate := fixtureEvaluator(t)
	orig := runMCPScan
	runMCPScan = func(ctx context.Context, opts []scanOptions) ([]engine.Report, error) {
		select {
		case <-time.After(1500 * time.Millisecond):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		return []engine.Report{evaluate(opts[0].targetVersion)}, nil
	}
	t.Cleanup(func() { runMCPScan = orig })
	addr, _ := startMCPHTTP(t)
	cs := httpClient(t, addr)
	res := callMCPWithin(t, cs, 30*time.Second, mcp.ToolScan, map[string]any{"targets": []any{"1.38"}})
	if res.IsError {
		t.Fatalf("a scan longer than the read timeout: %s", mcpText(res))
	}
}

func httpClient(t *testing.T, addr string) *mcpsdk.ClientSession {
	t.Helper()
	cs, err := mcpsdk.NewClient(&mcpsdk.Implementation{Name: "test", Version: "0"}, nil).
		Connect(context.Background(), &mcpsdk.StreamableClientTransport{Endpoint: "http://" + addr + "/mcp"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}

const initializeBody = `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"x","version":"0"}}}`

// mcpPOST sends one JSON-RPC message to /mcp, in session when it is not
// empty, and returns the response with its body read.
func mcpPOST(ctx context.Context, client *http.Client, addr, session, body string) (*http.Response, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://"+addr+"/mcp", strings.NewReader(body))
	if err != nil {
		return nil, "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if session != "" {
		req.Header.Set("Mcp-Session-Id", session)
		req.Header.Set("Mcp-Protocol-Version", "2025-06-18")
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	return resp, string(b), err
}

// initializeSession opens a session as a client does (initialize, then
// notifications/initialized) and returns its id.
func initializeSession(t *testing.T, client *http.Client, addr string) string {
	t.Helper()
	resp, body, err := mcpPOST(context.Background(), client, addr, "", initializeBody)
	if err != nil {
		t.Fatal(err)
	}
	id := resp.Header.Get("Mcp-Session-Id")
	if resp.StatusCode != http.StatusOK || id == "" {
		t.Fatalf("initialize = %s %q", resp.Status, body)
	}
	if resp, body, err := mcpPOST(context.Background(), client, addr, id, `{"jsonrpc":"2.0","method":"notifications/initialized"}`); err != nil || resp.StatusCode != http.StatusAccepted {
		t.Fatalf("notifications/initialized = %v %q %v", resp, body, err)
	}
	return id
}

// TestMCPHTTPIdleSessionsExpire: sessions a client opens and never closes
// (2000 initializes without a DELETE) are closed once idle for the session
// timeout, so what they hold is given back.
func TestMCPHTTPIdleSessionsExpire(t *testing.T) {
	setMCPHTTPLimits(t, func(c *mcpHTTPConfig) {
		c.sessions.SessionTimeout = 2 * time.Second
		c.sessions.MaxSessions = 5000
	})
	addr, _ := startMCPHTTP(t)
	client := &http.Client{Transport: &http.Transport{}}
	defer client.CloseIdleConnections()
	if resp, _, err := mcpPOST(context.Background(), client, addr, "", `{}`); err != nil {
		t.Fatal(err)
	} else if resp.StatusCode == http.StatusOK {
		t.Fatal("a POST of {} opened a session")
	}
	client.CloseIdleConnections()
	time.Sleep(100 * time.Millisecond)
	base := runtime.NumGoroutine()

	const n = 2000
	// The first few hundred sessions open well within the timeout, so they
	// are all open when the goroutines are counted: an open session holds
	// one, which proves the count falling back below is the expiry.
	const early = 200
	ids := map[string]bool{}
	for i := range n {
		resp, body, err := mcpPOST(context.Background(), client, addr, "", initializeBody)
		if err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("initialize = %s %q", resp.Status, body)
		}
		ids[resp.Header.Get("Mcp-Session-Id")] = true
		if i == early-1 {
			if g := runtime.NumGoroutine(); g < base+early/2 {
				t.Fatalf("%d goroutines with %d sessions open, %d before: sessions hold none, so their expiry proves nothing", g, early, base)
			}
		}
	}
	if len(ids) != n {
		t.Fatalf("%d sessions for %d initializes", len(ids), n)
	}
	client.CloseIdleConnections()
	goroutinesBackTo(t, base, 15*time.Second)

	// An expired session is gone: the client is told to start a new one.
	for id := range ids {
		resp, _, err := mcpPOST(context.Background(), client, addr, id, `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)
		if err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("a call in an expired session = %s, want 404", resp.Status)
		}
		break
	}
}

// TestMCPHTTPSessionsAreCapped: past the most sessions the server keeps, a
// new one is refused with 503 saying why and when to retry, while the open
// ones go on working; a session closed (DELETE) makes room.
func TestMCPHTTPSessionsAreCapped(t *testing.T) {
	setMCPHTTPLimits(t, func(c *mcpHTTPConfig) { c.sessions.MaxSessions = 3 })
	addr, _ := startMCPHTTP(t)
	client := &http.Client{}
	var ids []string
	for range 3 {
		ids = append(ids, initializeSession(t, client, addr))
	}
	resp, body, err := mcpPOST(context.Background(), client, addr, "", initializeBody)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusServiceUnavailable || !strings.Contains(body, "too many open MCP sessions (3") || resp.Header.Get("Retry-After") == "" {
		t.Errorf("a fourth session = %s %q (Retry-After %q), want 503 saying there are too many", resp.Status, body, resp.Header.Get("Retry-After"))
	}
	if _, err := mcpsdk.NewClient(&mcpsdk.Implementation{Name: "test", Version: "0"}, nil).
		Connect(context.Background(), &mcpsdk.StreamableClientTransport{Endpoint: "http://" + addr + "/mcp"}, nil); err == nil || !strings.Contains(err.Error(), "Service Unavailable") {
		t.Errorf("an SDK client past the cap: %v, want it refused (503)", err)
	}
	if resp, body, err := mcpPOST(context.Background(), client, addr, ids[0], `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`); err != nil || resp.StatusCode != http.StatusOK {
		t.Errorf("an open session past the cap: %v %q %v", resp, body, err)
	}

	req, _ := http.NewRequest(http.MethodDelete, "http://"+addr+"/mcp", nil)
	req.Header.Set("Mcp-Session-Id", ids[1])
	if resp, err := client.Do(req); err != nil || resp.StatusCode != http.StatusNoContent {
		t.Fatalf("DELETE = %v %v", resp, err)
	}
	waitFor(t, func() bool {
		resp, _, err := mcpPOST(context.Background(), client, addr, "", initializeBody)
		return err == nil && resp.StatusCode == http.StatusOK
	})
}

// TestMCPHTTPDisconnectCancelsTheCall: over HTTP, a scan whose client
// closes the connection that carries the call (without sending
// notifications/cancelled) is cancelled, so it stops reading the cluster
// and gives up the scan slot: another session's scan then starts at once.
func TestMCPHTTPDisconnectCancelsTheCall(t *testing.T) {
	evaluate := fixtureEvaluator(t)
	started, ended := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	orig := runMCPScan
	runMCPScan = func(ctx context.Context, opts []scanOptions) ([]engine.Report, error) {
		if calls.Add(1) == 1 {
			close(started)
			<-ctx.Done()
			close(ended)
			return nil, ctx.Err()
		}
		return []engine.Report{evaluate(opts[0].targetVersion)}, nil
	}
	t.Cleanup(func() { runMCPScan = orig })
	addr, _ := startMCPHTTP(t)

	client := &http.Client{Transport: &http.Transport{}}
	defer client.CloseIdleConnections()
	id := initializeSession(t, client, addr)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, _, err := mcpPOST(ctx, client, addr, id, `{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"scan","arguments":{"targets":["1.38"]}}}`)
		done <- err
	}()
	select {
	case <-started:
	case <-time.After(10 * time.Second):
		t.Fatal("the scan did not start")
	}
	cancel() // the client goes away: its connection closes
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("the abandoned call returned %v", err)
	}
	select {
	case <-ended:
	case <-time.After(5 * time.Second):
		t.Fatal("the scan of a client that went away was not cancelled")
	}

	other := httpClient(t, addr)
	res := callMCPWithin(t, other, 5*time.Second, mcp.ToolScan, map[string]any{"targets": []any{"1.38"}})
	if res.IsError {
		t.Errorf("the next scan: %s", mcpText(res))
	}
}
