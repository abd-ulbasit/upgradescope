package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// callWithin calls a tool and fails the test if the call does not come back
// within d: a tool that blocks on its input must not hold the call.
func callWithin(t *testing.T, cs *mcpsdk.ClientSession, d time.Duration, name string, args map[string]any) *mcpsdk.CallToolResult {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), d)
	defer cancel()
	res, err := cs.CallTool(ctx, &mcpsdk.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("%s %v: %v (want a tool error within %s)", name, args, err, d)
	}
	return res
}

func TestReportFileMustBeARegularFile(t *testing.T) {
	cs := connect(t, localConfig())
	res := callWithin(t, cs, 5*time.Second, ToolGetReport, map[string]any{"report_file": t.TempDir()})
	if !res.IsError || !strings.Contains(text(res), "is not a regular file (a directory)") {
		t.Errorf("a directory: isError=%v %q", res.IsError, text(res))
	}
}

// TestErrorsDoNotQuoteTheFile: a path can name any file the user can read,
// so the reason a file is refused carries none of its contents.
func TestErrorsDoNotQuoteTheFile(t *testing.T) {
	dir := t.TempDir()
	for name, content := range map[string]string{
		"passwd":   "# s3cret-comment\nroot:x:0:0:root:/root:/bin/sh\n",
		"json":     `{"schemaVersion":1,"target":"s3cret-value"}`,
		"constant": `{"schemaVersion":"s3cret-value"}`,
	} {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		res := call(t, connect(t, localConfig()), ToolGetReport, map[string]any{"report_file": path})
		if !res.IsError || strings.Contains(text(res), "s3cret") || strings.Contains(text(res), "'#'") {
			t.Errorf("%s: isError=%v %q, want a refusal that quotes nothing of the file", name, res.IsError, text(res))
		}
		if !strings.Contains(text(res), "not an upgradescope JSON report") {
			t.Errorf("%s: %q gives no reason", name, text(res))
		}
	}
}

// bigReport is a valid report of n findings, each the golden report's first
// finding.
func bigReport(t *testing.T, n int) []byte {
	t.Helper()
	_, doc := goldenReport(t, "mixed-everything")
	var m map[string]any
	if err := json.Unmarshal(doc, &m); err != nil {
		t.Fatal(err)
	}
	first := m["findings"].([]any)[0]
	findings := make([]any, n)
	for i := range findings {
		findings[i] = first
	}
	m["findings"] = findings
	out, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// TestAResultTooLargeToReceiveIsRefusedWithAWayOut: a report within the
// bound a tool reads, whose result would still be more than a client takes
// in one message, is refused with a pointer to list_findings, whose filters
// then return part of it.
func TestAResultTooLargeToReceiveIsRefusedWithAWayOut(t *testing.T) {
	doc := bigReport(t, 1)
	per := len(bigReport(t, 2)) - len(doc)
	doc = bigReport(t, (MaxReportBytes-len(doc))/per) // just under the bound
	if len(doc) > MaxReportBytes || len(doc) < MaxReportBytes*9/10 {
		t.Fatalf("report of %d bytes, want just under %d", len(doc), MaxReportBytes)
	}
	path := filepath.Join(t.TempDir(), "big.json")
	if err := os.WriteFile(path, doc, 0o644); err != nil {
		t.Fatal(err)
	}
	cs := connect(t, localConfig())
	res := call(t, cs, ToolGetReport, map[string]any{"report_file": path})
	if !res.IsError || !strings.Contains(text(res), "more than an MCP client takes in one message") || !strings.Contains(text(res), "list_findings") {
		t.Errorf("get_report of a %d-byte report: isError=%v %.300q", len(doc), res.IsError, text(res))
	}
	out := structured(t, call(t, cs, ToolListFindings, map[string]any{"report_file": path, "limit": 5}))
	var got struct {
		Findings  []json.RawMessage `json:"findings"`
		Truncated bool              `json:"truncated"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Findings) != 5 || !got.Truncated {
		t.Errorf("list_findings limit=5 of the large report: %d findings, truncated %v", len(got.Findings), got.Truncated)
	}
	// The most list_findings returns of these findings (about 0.5 KiB each)
	// is well within one message, so it is sent.
	res = call(t, cs, ToolListFindings, map[string]any{"report_file": path, "limit": maxLimit})
	if res.IsError {
		t.Fatalf("list_findings limit=%d of the large report: %q", maxLimit, text(res))
	}
	if err := json.Unmarshal(structured(t, res), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Findings) != maxLimit || !got.Truncated {
		t.Errorf("list_findings limit=%d: %d findings, truncated %v", maxLimit, len(got.Findings), got.Truncated)
	}
}

// TestFindingsTooLargeToReceiveAreRefusedWithAWayOut: findings that are
// small in the file can still be too large on the wire (each '<' is sent as
// \u003c, and again escaped in the text block), so the most list_findings
// returns is refused with how to narrow it, and a smaller limit is sent.
func TestFindingsTooLargeToReceiveAreRefusedWithAWayOut(t *testing.T) {
	_, golden := goldenReport(t, "mixed-everything")
	var m map[string]any
	if err := json.Unmarshal(golden, &m); err != nil {
		t.Fatal(err)
	}
	first := m["findings"].([]any)[0].(map[string]any)
	// Each under the cut of an outside string, so the size is the escaping.
	for _, k := range []string{"title", "detail", "remediation"} {
		first[k] = strings.Repeat("<", MaxClusterTextBytes-48)
	}
	findings := make([]any, maxLimit+100)
	for i := range findings {
		findings[i] = first
	}
	m["findings"] = findings
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false) // '<' takes one byte in the file
	if err := enc.Encode(m); err != nil {
		t.Fatal(err)
	}
	doc := buf.Bytes()
	if len(doc) > MaxReportBytes/2 {
		t.Fatalf("report of %d bytes, want well within %d", len(doc), MaxReportBytes)
	}
	path := filepath.Join(t.TempDir(), "escaped.json")
	if err := os.WriteFile(path, doc, 0o644); err != nil {
		t.Fatal(err)
	}
	cs := connect(t, localConfig())
	res := call(t, cs, ToolListFindings, map[string]any{"report_file": path, "limit": maxLimit})
	if !res.IsError || !strings.Contains(text(res), "more than an MCP client takes in one message") || !strings.Contains(text(res), "smaller limit") {
		t.Errorf("list_findings limit=%d of a %d-byte report: isError=%v %.300q", maxLimit, len(doc), res.IsError, text(res))
	}
	res = call(t, cs, ToolListFindings, map[string]any{"report_file": path, "limit": 50})
	if res.IsError {
		t.Fatalf("list_findings limit=50: %q", text(res))
	}
	var got struct {
		Findings []json.RawMessage `json:"findings"`
	}
	if err := json.Unmarshal(structured(t, res), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Findings) != 50 {
		t.Errorf("list_findings limit=50: %d findings", len(got.Findings))
	}
}

// TestFleetResponsesAreBounded: a fleet server's answer larger than a tool
// reads is refused, whether or not it says its length up front.
func TestFleetResponsesAreBounded(t *testing.T) {
	big := bytes.Repeat([]byte(" "), maxFleetResponseBytes+1)
	big[0] = '{'
	big[len(big)-1] = '}'
	for _, chunked := range []bool{false, true} {
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/api/v1/clusters" {
				_, _ = w.Write([]byte(`[{"id":1,"name":"prod"}]`))
				return
			}
			if chunked {
				w.(http.Flusher).Flush() // no Content-Length
			}
			_, _ = w.Write(big)
		}))
		f, _ := NewFleet(ts.URL, "")
		cfg := localConfig()
		cfg.Fleet = f
		cs := connect(t, cfg)
		for _, c := range []struct {
			tool string
			args map[string]any
		}{{ToolGetReport, map[string]any{"cluster": "prod"}}, {ToolFleetSummary, nil}} {
			res := call(t, cs, c.tool, c.args)
			if !res.IsError || !strings.Contains(text(res), "larger than 8 MiB") {
				t.Errorf("%s, chunked=%v: isError=%v %.200q", c.tool, chunked, res.IsError, text(res))
			}
		}
		ts.Close()
	}
}

// TestFleetReportMustFollowTheSchema: a server answer that carries
// schemaVersion 1 but is not a report is a tool error with the reason, not
// the SDK's protocol error.
func TestFleetReportMustFollowTheSchema(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/clusters" {
			_, _ = w.Write([]byte(`[{"id":1,"name":"prod"}]`))
			return
		}
		_, _ = w.Write([]byte(`{"schemaVersion":1}`))
	}))
	defer ts.Close()
	f, _ := NewFleet(ts.URL, "")
	cfg := localConfig()
	cfg.Fleet = f
	cs := connect(t, cfg)
	for _, tool := range []string{ToolGetReport, ToolListFindings} {
		res, err := cs.CallTool(context.Background(), &mcpsdk.CallToolParams{Name: tool, Arguments: map[string]any{"cluster": "prod"}})
		if err != nil {
			t.Fatalf("%s: protocol error %v, want a tool error", tool, err)
		}
		if !res.IsError || !strings.Contains(text(res), "does not follow api/report.schema.json") {
			t.Errorf("%s: isError=%v %q", tool, res.IsError, text(res))
		}
	}
}

func TestFleetFollowsNoRedirect(t *testing.T) {
	var elsewhere bool
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		elsewhere = true
		_, _ = w.Write([]byte(`{"targets":[],"clusters":[]}`))
	}))
	defer other.Close()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL+r.URL.Path, http.StatusFound)
	}))
	defer ts.Close()
	f, _ := NewFleet(ts.URL, "tok")
	cfg := localConfig()
	cfg.Fleet = f
	res := call(t, connect(t, cfg), ToolFleetSummary, nil)
	if !res.IsError || !strings.Contains(text(res), "302") || elsewhere {
		t.Errorf("a redirect: isError=%v %q, followed=%v", res.IsError, text(res), elsewhere)
	}
}

func TestCleartextWarning(t *testing.T) {
	for _, tc := range []struct {
		url, token string
		warn       bool
	}{
		{"http://upgradescope.example.com", "tok", true},
		{"HTTP://10.0.0.5:8080", "tok", true},
		{"http://upgradescope.example.com", "", false},
		{"https://upgradescope.example.com", "tok", false},
		{"http://127.0.0.1:8080", "tok", false},
		{"http://localhost:8080", "tok", false},
		{"http://[::1]:8080", "tok", false},
	} {
		if got := CleartextWarning(tc.url, tc.token); (got != "") != tc.warn {
			t.Errorf("CleartextWarning(%q, token %v) = %q, want a warning: %v", tc.url, tc.token != "", got, tc.warn)
		}
	}
}

// TestScanHonoursTheCallsContext: a scan stops when the client cancels the
// call, and a call waiting for another scan to finish gives up when its
// own context ends rather than queueing a cluster read.
func TestScanHonoursTheCallsContext(t *testing.T) {
	started := make(chan struct{}, 2)
	stopped := make(chan error, 2)
	cfg := localConfig()
	cfg.Scan = func(ctx context.Context, _ ScanRequest) ([]json.RawMessage, error) {
		started <- struct{}{}
		<-ctx.Done()
		stopped <- ctx.Err()
		return nil, ctx.Err()
	}
	cs := connect(t, cfg)

	ctx, cancel := context.WithCancel(context.Background())
	first := make(chan error, 1)
	go func() {
		_, err := cs.CallTool(ctx, &mcpsdk.CallToolParams{Name: ToolScan, Arguments: map[string]any{"targets": []any{"1.37"}}})
		first <- err
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("the scan did not start")
	}

	// A second call waits for the slot; its deadline ends the wait.
	short, cancelShort := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancelShort()
	_, err := cs.CallTool(short, &mcpsdk.CallToolParams{Name: ToolScan, Arguments: map[string]any{"targets": []any{"1.38"}}})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("the waiting call: %v, want its deadline", err)
	}
	// The client's cancellation reaches the server as a notification; give
	// it time to end the wait before the slot frees up.
	time.Sleep(500 * time.Millisecond)

	cancel()
	select {
	case err := <-stopped:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("the scan stopped with %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the scan went on after the client cancelled the call")
	}
	<-first
	select {
	case <-started:
		t.Error("the call that gave up waiting ran a scan anyway")
	case <-time.After(300 * time.Millisecond):
	}
}

// TestReportReadsAreBoundedInNumber: each report a tool reads is held
// several times over while it is checked and sent, so a client that issues
// many calls at once (prompt-injected content can ask for that) gets at
// most maxConcurrentReads of them at a time; the rest wait their turn and
// are answered.
func TestReportReadsAreBoundedInNumber(t *testing.T) {
	_, doc := goldenReport(t, "mixed-everything")
	var inFlight, most atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/clusters" {
			_, _ = w.Write([]byte(`[{"id":1,"name":"prod"}]`))
			return
		}
		n := inFlight.Add(1)
		defer inFlight.Add(-1)
		for {
			m := most.Load()
			if n <= m || most.CompareAndSwap(m, n) {
				break
			}
		}
		time.Sleep(50 * time.Millisecond)
		_, _ = w.Write(doc)
	}))
	defer ts.Close()
	f, _ := NewFleet(ts.URL, "")
	cfg := localConfig()
	cfg.Fleet = f
	cs := connect(t, cfg)

	const calls = 4 * maxConcurrentReads
	errs := make(chan error, calls)
	for i := range calls {
		tool := ToolGetReport
		if i%2 == 1 {
			tool = ToolListFindings
		}
		go func() {
			res, err := cs.CallTool(context.Background(), &mcpsdk.CallToolParams{Name: tool, Arguments: map[string]any{"cluster": "prod"}})
			switch {
			case err != nil:
				errs <- err
			case res.IsError:
				errs <- errors.New(text(res))
			default:
				errs <- nil
			}
		}()
	}
	for range calls {
		if err := <-errs; err != nil {
			t.Error(err)
		}
	}
	if got := most.Load(); got > maxConcurrentReads {
		t.Errorf("%d report reads at once, want at most %d", got, maxConcurrentReads)
	}
}

// TestAReportReadWaitingForASlotGivesUp: a call waiting for a read slot
// gives up when its context ends, reads nothing afterwards, and leaves the
// slots for the calls after it.
func TestAReportReadWaitingForASlotGivesUp(t *testing.T) {
	_, doc := goldenReport(t, "mixed-everything")
	var reads atomic.Int32
	started := make(chan struct{}, maxConcurrentReads+1)
	release := make(chan struct{})
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/clusters" {
			_, _ = w.Write([]byte(`[{"id":1,"name":"prod"}]`))
			return
		}
		reads.Add(1)
		started <- struct{}{}
		<-release
		_, _ = w.Write(doc)
	}))
	defer ts.Close()
	f, _ := NewFleet(ts.URL, "")
	cfg := localConfig()
	cfg.Fleet = f
	cs := connect(t, cfg)
	args := map[string]any{"cluster": "prod"}

	held := make(chan *mcpsdk.CallToolResult, maxConcurrentReads)
	for range maxConcurrentReads {
		go func() {
			res, err := cs.CallTool(context.Background(), &mcpsdk.CallToolParams{Name: ToolGetReport, Arguments: args})
			if err != nil {
				t.Error(err)
			}
			held <- res
		}()
		select {
		case <-started:
		case <-time.After(5 * time.Second):
			t.Fatal("a report read did not start")
		}
	}

	short, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if _, err := cs.CallTool(short, &mcpsdk.CallToolParams{Name: ToolGetReport, Arguments: args}); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("the waiting call: %v, want its deadline", err)
	}
	// The cancellation reaches the server as a notification; give it time
	// to end the wait before the slots free up.
	time.Sleep(500 * time.Millisecond)
	close(release)
	for range maxConcurrentReads {
		if res := <-held; res == nil || res.IsError {
			t.Errorf("a call holding a slot: %v", res)
		}
	}
	time.Sleep(300 * time.Millisecond)
	if got := reads.Load(); got != maxConcurrentReads {
		t.Errorf("%d report reads, want %d: the call that gave up read one anyway", got, maxConcurrentReads)
	}
	if res := callWithin(t, cs, 5*time.Second, ToolGetReport, args); res.IsError {
		t.Errorf("a call after the slots freed up: %q", text(res))
	}
}
