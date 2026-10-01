package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/abd-ulbasit/upgradescope/internal/engine"
)

// bigConfigMap is one YAML document of roughly size bytes made of many
// short keys — the shape that costs the most memory per input byte.
func bigConfigMap(size int) string {
	var b strings.Builder
	b.WriteString("apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: big\ndata:\n")
	for i := 0; b.Len() < size; i++ {
		fmt.Fprintf(&b, "  k%x: %d\n", i, i%10)
	}
	return b.String()
}

// deploymentList is a kubectl-style `kind: List` of n realistic
// Deployments (~2.5 KiB each).
func deploymentList(n int) string {
	var b strings.Builder
	b.WriteString("apiVersion: v1\nkind: List\nitems:\n")
	for i := range n {
		fmt.Fprintf(&b, `- apiVersion: apps/v1
  kind: Deployment
  metadata:
    name: app-%[1]d
    namespace: ns-%[2]d
    labels: {app: app-%[1]d, tier: backend}
    annotations:
      description: %[3]q
  spec:
    replicas: 2
    selector: {matchLabels: {app: app-%[1]d}}
    template:
      metadata: {labels: {app: app-%[1]d}}
      spec:
        containers:
        - name: main
          image: registry.example.com/app-%[1]d:v1.2.3
          env:
`, i, i%40, strings.Repeat("x", 400))
		for k := range 25 {
			fmt.Fprintf(&b, "          - {name: VAR_%d, value: \"value-%d-%s\"}\n", k, k, strings.Repeat("y", 40))
		}
	}
	return b.String()
}

func gateStatus(t *testing.T, s *Server, body string) (int, http.Header, []byte) {
	t.Helper()
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()
	resp, raw := postGate(t, ts, "?target=1.35", "", body, "application/x-yaml")
	return resp.StatusCode, resp.Header, raw
}

// The issue #19 repro: one 19 MiB single-document manifest used to be
// decoded in full (~1 GB RSS). It must now be refused before parsing.
func TestGateRejects19MiBSingleDocument(t *testing.T) {
	s := newTestServer(t, newFakeStore())
	if code, _, raw := gateStatus(t, s, bigConfigMap(19<<20)); code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d (%s), want 413", code, raw)
	}
}

// Under the body cap but over the per-document cap: still refused, and the
// message names the document limit.
func TestGateRejectsOversizedDocument(t *testing.T) {
	s := newTestServer(t, newFakeStore())
	body := pspManifest + "---\n" + bigConfigMap(maxManifestDocBytes+1)
	code, _, raw := gateStatus(t, s, body)
	if code != http.StatusRequestEntityTooLarge || !strings.Contains(string(raw), "document 2") {
		t.Fatalf("status = %d (%s), want 413 naming document 2", code, raw)
	}
}

func TestGateRejectsTooManyDocuments(t *testing.T) {
	s := newTestServer(t, newFakeStore())
	body := strings.Repeat("---\napiVersion: v1\nkind: ConfigMap\n", maxManifestDocs+1)
	code, _, raw := gateStatus(t, s, body)
	if code != http.StatusRequestEntityTooLarge || !strings.Contains(string(raw), "documents") {
		t.Fatalf("status = %d (%s), want 413 about the document count", code, raw)
	}
}

func TestGateBodyCapIsConfigurable(t *testing.T) {
	s := newTestServer(t, newFakeStore(), func(c *Config) { c.MaxGateBytes = 64 })
	code, _, raw := gateStatus(t, s, pspManifest+"# padding past 64 bytes\n")
	if code != http.StatusRequestEntityTooLarge || !strings.Contains(string(raw), "64 bytes") {
		t.Fatalf("status = %d (%s), want 413 naming the 64-byte limit", code, raw)
	}
}

// Billion-laughs: a few hundred bytes that expand to millions of nodes.
func TestGateRejectsAliasBomb(t *testing.T) {
	var b strings.Builder
	b.WriteString("apiVersion: v1\nkind: ConfigMap\nmetadata: {name: bomb}\n")
	b.WriteString(`a: &a ["lol","lol","lol","lol","lol","lol","lol","lol","lol"]` + "\n")
	prev := "a"
	for _, c := range "bcdefghi" {
		fmt.Fprintf(&b, "%c: &%c [%s]\n", c, c, strings.TrimSuffix(strings.Repeat("*"+prev+",", 9), ","))
		prev = string(c)
	}
	s := newTestServer(t, newFakeStore())
	began := time.Now()
	code, _, raw := gateStatus(t, s, b.String())
	if code < 400 || code >= 500 {
		t.Fatalf("status = %d (%s), want 4xx", code, raw)
	}
	if took := time.Since(began); took > 5*time.Second {
		t.Fatalf("alias bomb took %s to reject", took)
	}
}

// A realistic multi-MiB `kind: List` (kubectl get -o yaml) is one legitimate
// document and must still be evaluated.
func TestGateAcceptsLargeList(t *testing.T) {
	body := deploymentList(1300)
	if len(body) < 3<<20 || len(body) > maxManifestDocBytes {
		t.Fatalf("fixture is %d bytes, want 3 MiB..%d", len(body), maxManifestDocBytes)
	}
	s := newTestServer(t, newFakeStore())
	code, _, raw := gateStatus(t, s, body)
	if code != http.StatusOK {
		t.Fatalf("status = %d (%s), want 200", code, raw)
	}
	var rep engine.Report
	if err := json.Unmarshal(raw, &rep); err != nil {
		t.Fatal(err)
	}
	if rep.ClusterID != "manifests" {
		t.Fatalf("report = %+v", rep)
	}
}

// When every evaluation slot stays busy past the queue timeout, the gate
// answers 503 + Retry-After instead of piling another parse on top.
func TestGateConcurrencyLimit(t *testing.T) {
	s := newTestServer(t, newFakeStore())
	s.gateQueueTimeout = 50 * time.Millisecond
	for range cap(s.gateSlots) {
		s.gateSlots <- struct{}{}
	}
	code, hdr, raw := gateStatus(t, s, pspManifest)
	if code != http.StatusServiceUnavailable || hdr.Get("Retry-After") == "" {
		t.Fatalf("status = %d (%s), Retry-After %q; want 503 with Retry-After", code, raw, hdr.Get("Retry-After"))
	}

	// A slot freed while the request waits is taken, not refused.
	s.gateQueueTimeout = 5 * time.Second
	go func() {
		time.Sleep(100 * time.Millisecond)
		<-s.gateSlots
	}()
	if code, _, raw := gateStatus(t, s, pspManifest); code != http.StatusOK {
		t.Fatalf("queued request status = %d (%s), want 200", code, raw)
	}
}

// smallConfigMaps is a stream of ~size bytes of small ConfigMap documents:
// every document and the count stay under the per-document caps.
func smallConfigMaps(size int) string {
	var b strings.Builder
	for i := 0; b.Len() < size; i++ {
		fmt.Fprintf(&b, "---\napiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: cm%d\ndata:\n  k: %s\n", i, strings.Repeat("x", 450))
	}
	return b.String()
}

// serveGate runs one /gate request straight through the handler, so tests
// can run many at once without a listener.
func serveGate(s *Server, w http.ResponseWriter, body string) {
	req := httptest.NewRequest(http.MethodPost, "/api/v1/gate?target=1.35", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/x-yaml")
	s.Handler().ServeHTTP(w, req)
}

// Bodies wait for the evaluation slot in memory, so the bytes buffered
// across every waiting request must be bounded too, not only the
// evaluation. With the slot busy, n concurrent maximum-size streams of
// small documents (each under every per-request cap) may hold at most
// maxBufferedGateBodies bodies; the rest wait unread, and every one is
// evaluated once the slot frees.
func TestGateBufferedBodiesAreBounded(t *testing.T) {
	const limit = 1 << 20
	s := newTestServer(t, newFakeStore(), func(c *Config) { c.MaxGateBytes = limit })
	s.gateQueueTimeout = time.Minute // generous under -race; this test is about memory, not timeouts
	for range cap(s.gateSlots) {
		s.gateSlots <- struct{}{} // hold the evaluation slot
	}
	body := smallConfigMaps(limit - 4<<10)
	var before runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)

	const n = 12
	type result struct {
		code int
		body string
	}
	results := make(chan result, n)
	for range n {
		go func() {
			rec := httptest.NewRecorder()
			serveGate(s, rec, body)
			results <- result{rec.Code, rec.Body.String()}
		}()
	}
	budget := int64(maxBufferedGateBodies * len(body))
	for deadline := time.Now().Add(10 * time.Second); s.gateBuffered.inUse() < budget; time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("buffered %d bytes, want the full %d-byte budget in use while the slot is busy", s.gateBuffered.inUse(), budget)
		}
	}
	time.Sleep(200 * time.Millisecond) // let any unbounded reader show up
	var during runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&during)
	// Unbounded, the 12 bodies alone are 12 MiB (plus read slack); bounded,
	// 3 are 3 MiB. 2x the budget leaves room for the test's own garbage.
	if grew := int64(during.HeapAlloc) - int64(before.HeapAlloc); grew > 2*budget {
		t.Fatalf("heap grew %d MiB with %d requests waiting; budget is %d MiB", grew>>20, n, budget>>20)
	}
	if used := s.gateBuffered.inUse(); used != budget {
		t.Fatalf("%d bytes charged, want exactly the %d-byte budget", used, budget)
	}

	for range cap(s.gateSlots) {
		<-s.gateSlots
	}
	for range n {
		select {
		case r := <-results:
			if r.code != http.StatusOK {
				t.Fatalf("status = %d (%s), want 200 once the slot frees", r.code, r.body)
			}
		case <-time.After(time.Minute):
			t.Fatal("waiting requests did not finish after the slot was freed")
		}
	}
	if used := s.gateBuffered.inUse(); used != 0 {
		t.Fatalf("%d body bytes still charged after every request finished", used)
	}
}

// A request that cannot get room for its body before the queue timeout is
// refused with 503 + Retry-After, without its body being read.
func TestGateBodyBudgetTimeout(t *testing.T) {
	s := newTestServer(t, newFakeStore())
	s.gateQueueTimeout = 50 * time.Millisecond
	s.gateBuffered.take(context.Background(), s.gateBuffered.max, 0)
	rd := &countingReader{r: strings.NewReader(pspManifest)}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/gate?target=1.35", rd)
	req.ContentLength = int64(len(pspManifest))
	req.Header.Set("Content-Type", "application/x-yaml")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable || rec.Header().Get("Retry-After") == "" || rd.n != 0 {
		t.Fatalf("status = %d (%s), Retry-After %q, %d body bytes read; want 503 with Retry-After and an unread body",
			rec.Code, rec.Body, rec.Header().Get("Retry-After"), rd.n)
	}
	s.gateBuffered.give(s.gateBuffered.max)
	if code, _, raw := gateStatus(t, s, pspManifest); code != http.StatusOK {
		t.Fatalf("with room: status = %d (%s), want 200", code, raw)
	}
}

type countingReader struct {
	r io.Reader
	n int
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += n
	return n, err
}

// blockingWriter is a ResponseWriter whose Write blocks until unblock is
// closed: a client that stopped reading the response.
type blockingWriter struct {
	header  http.Header
	entered chan struct{}
	unblock chan struct{}
	once    sync.Once
}

func (w *blockingWriter) Header() http.Header { return w.header }
func (w *blockingWriter) WriteHeader(int)     {}
func (w *blockingWriter) Write(p []byte) (int, error) {
	w.once.Do(func() { close(w.entered) })
	<-w.unblock
	return len(p), nil
}

// The evaluation slot covers decoding and evaluation only. A client that
// stops reading a large response (SARIF, many namespaces) must not pin the
// only slot until WriteTimeout.
func TestGateReleasesSlotBeforeWritingResponse(t *testing.T) {
	s := newTestServer(t, newFakeStore())
	w := &blockingWriter{header: http.Header{}, entered: make(chan struct{}), unblock: make(chan struct{})}
	done := make(chan struct{})
	go func() {
		serveGate(s, w, pspManifest)
		close(done)
	}()
	select {
	case <-w.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("handler never wrote a response")
	}
	held, buffered := len(s.gateSlots), s.gateBuffered.inUse()
	close(w.unblock)
	<-done
	if held != 0 || buffered != 0 {
		t.Fatalf("while writing the response: %d slot(s) held, %d body bytes charged; want 0 and 0", held, buffered)
	}
}

func TestSnapshotBodyCapIsConfigurable(t *testing.T) {
	ts := httptest.NewServer(newTestServer(t, newFakeStore(), func(c *Config) { c.MaxSnapshotBytes = 1 << 10 }).Handler())
	defer ts.Close()
	resp, out := postSnapshot(t, ts, "ingest-tok", bytes.Repeat([]byte("a"), 1<<10+1), false)
	if resp.StatusCode != http.StatusRequestEntityTooLarge || !strings.Contains(fmt.Sprint(out), "1024 bytes") {
		t.Fatalf("status = %d (%v), want 413 naming the 1024-byte limit", resp.StatusCode, out)
	}
	// The default stays 20 MiB.
	s := newTestServer(t, newFakeStore())
	if got := s.maxSnapshotBytes(); got != DefaultMaxSnapshotBytes || DefaultMaxSnapshotBytes != 20<<20 {
		t.Fatalf("default snapshot cap = %d, want 20 MiB", got)
	}
}
