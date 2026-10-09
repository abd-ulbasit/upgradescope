package server

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"runtime"
	"runtime/debug"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/abd-ulbasit/upgradescope/internal/inventory"
	"github.com/abd-ulbasit/upgradescope/internal/kb"
)

// unreadClients sends request (a raw HTTP/1.1 request) on n TCP
// connections to ts, each with a small receive buffer, and never reads a
// response; closeAll closes them.
func unreadClients(t *testing.T, ts *httptest.Server, request string, n int) (closeAll func()) {
	t.Helper()
	conns := make([]net.Conn, 0, n)
	closeAll = func() {
		for _, c := range conns {
			_ = c.Close()
		}
	}
	for range n {
		c, err := net.Dial("tcp", ts.Listener.Addr().String())
		if err != nil {
			closeAll()
			t.Fatalf("dial: %v", err)
		}
		_ = c.(*net.TCPConn).SetReadBuffer(4 << 10)
		conns = append(conns, c)
		if _, err := io.WriteString(c, request); err != nil {
			closeAll()
			t.Fatalf("send: %v", err)
		}
	}
	return closeAll
}

// liveHeap is the heap in use after a full collection (two, so what the
// first one moved to sync.Pool victim caches is freed too).
func liveHeap() uint64 {
	runtime.GC()
	runtime.GC()
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return m.HeapAlloc
}

// A read's response was held in memory until its client read it, for up
// to the 120s write timeout, with nothing capping how many: after one
// 17 MB push of PodSecurityPolicy usages (any ingest token can send it;
// its report is 17.5 MB), 20 clients that asked for the report and never
// read it grew the live heap 366 MiB with the read slot free, and a 120s
// window holds ~100 of them. Responses held past the slot now share a
// budget; one that does not fit is sent in the slot, which a client that
// does not read gives up after a short write deadline. Over real sockets
// whose clients never read the largest report a push stores (~20.9 MB,
// pushedLargestReport), the live heap grows by no more than that budget,
// and it is given back once the clients are gone.
func TestUnreadResponsesAreBounded(t *testing.T) {
	if testing.Short() || raceEnabled || !heapRun {
		t.Skip("stores a snapshot at the node budget; make test-heap runs it, without the race detector")
	}
	defer debug.SetGCPercent(debug.SetGCPercent(10)) // as in TestGateDecodeHeapIsBounded
	s := pushedLargestReport(t)
	s.readQueueTimeout = 5 * time.Minute // every request is served, none is turned away
	for _, n := range []int{8, 20} {
		checkUnreadBounded(t, s, s.readSlots, "GET /api/v1/clusters/1/report HTTP/1.1\r\nHost: localhost\r\n\r\n", n, maxReadHeap)
	}
}

// /gate had the same hole: its response was built and written after the
// evaluation slot was released, so a client that did not read it pinned
// the report, the response and its encoding, ~32 MiB each with ?cluster=
// against a 17 MB push, for up to the 120s write timeout; 10 such clients
// grew the live heap 320 MiB with the slot free. Its responses are now
// encoded in the slot and held under the same budget as the reads'. The
// largest answers within the answer limit (--max-gate-bytes): ?cluster=
// against a cluster with 100 objects of each of the KB's deprecated or
// removed GVKs, with long names.
func TestUnreadGateResponsesAreBounded(t *testing.T) {
	if testing.Short() || raceEnabled || !heapRun {
		t.Skip("stores a snapshot with 13,600 object refs; make test-heap runs it, without the race detector")
	}
	defer debug.SetGCPercent(debug.SetGCPercent(10)) // as in TestGateDecodeHeapIsBounded
	k, err := kb.Load()
	if err != nil {
		t.Fatal(err)
	}
	s := newSQLiteTestServer(t)
	s.cfg.KB = k
	s.slotWriteTimeout = time.Second
	s.gateQueueTimeout = 5 * time.Minute // every request is served, none is turned away
	inv := testInventory()
	for _, e := range k.APILifecycle {
		if e.Deprecated == nil && e.Removed == nil {
			continue
		}
		// The longest names a cluster holds: 63-byte namespaces and
		// 253-byte object names.
		ns := "team-a-" + strings.Repeat("n", 56)
		u := inventory.APIUsage{Group: e.Group, Version: e.Version, Kind: e.Kind, Count: 100, Namespaces: map[string]int{ns: 100}}
		for i := range 100 {
			u.Objects = append(u.Objects, inventory.ObjectRef{Namespace: ns, Name: fmt.Sprintf("o-%03d-%s", i, strings.Repeat("x", 247))})
		}
		inv.APIUsage = append(inv.APIUsage, u)
	}
	rec := httptest.NewRecorder()
	serveIngest(s, rec, pushReqBody(t, inv), false)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("push: status = %d (%.300s)", rec.Code, rec.Body)
	}
	const query = "/api/v1/gate?target=1.35&fail-on=never&cluster=prod-eu-1"
	rec = httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, query, strings.NewReader(deploymentManifest))
	req.Header.Set("Content-Type", "application/x-yaml")
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || rec.Body.Len() < 4<<20 {
		t.Fatalf("one gate: status %d, %d bytes (%.300s); want 200 with an answer of at least 4 MiB", rec.Code, rec.Body.Len(), rec.Body)
	}
	t.Logf("each answer is %d bytes", rec.Body.Len())
	raw := fmt.Sprintf("POST %s HTTP/1.1\r\nHost: localhost\r\nContent-Type: application/x-yaml\r\nContent-Length: %d\r\n\r\n%s",
		query, len(deploymentManifest), deploymentManifest)
	checkUnreadBounded(t, s, s.gateSlots, raw, 10, maxGateDecodeHeap)
}

// pushedLargestReport is a SQLite server that holds one push of
// namespaceUsages(100) as storedBody stores it: its report is about the
// report limit (maxReportBytes), ~20.9 MB.
func pushedLargestReport(t *testing.T) *Server {
	t.Helper()
	s := newSQLiteTestServer(t)
	s.slotWriteTimeout = time.Second
	rec := httptest.NewRecorder()
	serveIngest(s, rec, []byte(storedBody("namespace map, 100 per usage", namespaceUsages(100))), false)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("push: status = %d (%.300s)", rec.Code, rec.Body)
	}
	return s
}

// checkUnreadBounded sends request from n clients that never read their
// responses, over real sockets with small buffers, and checks that once
// every request has run and none holds its slot, the live heap grew by no
// more than the held-response budget, the heap peaked no more than
// inSlot (what one request in its slot may add) above that, and the
// budget is given back once the clients are gone.
func checkUnreadBounded(t *testing.T, s *Server, slots chan struct{}, request string, n int, inSlot int64) {
	t.Helper()
	var started atomic.Int64
	ts := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started.Add(1)
		s.Handler().ServeHTTP(w, r)
	}))
	ts.Config.ConnState = func(c net.Conn, st http.ConnState) {
		if tc, ok := c.(*net.TCPConn); ok && st == http.StateNew {
			_ = tc.SetWriteBuffer(4 << 10)
		}
	}
	ts.Start()
	defer ts.Close()

	base := liveHeap()
	var closeAll func()
	defer func() {
		if closeAll != nil {
			closeAll()
		}
	}()
	quiet := 0
	peak := heapPeak(func() {
		closeAll = unreadClients(t, ts, request, n)
		// Wait until every request has run and none holds or waits for
		// its slot: each response is then held for its client, refused,
		// or its write was given up.
		for deadline := time.Now().Add(3 * time.Minute); quiet < 5 && time.Now().Before(deadline); {
			time.Sleep(100 * time.Millisecond)
			if started.Load() == int64(n) && len(slots) == 0 {
				quiet++
			} else {
				quiet = 0
			}
		}
	})
	if quiet < 5 {
		t.Fatalf("%d of %d requests started, %d slots held after 3m", started.Load(), n, len(slots))
	}
	grew := int64(liveHeap()) - int64(base)
	held := s.heldResponses.inUse()
	closeAll()
	budget := s.heldResponses.max
	if grew > budget+8<<20 {
		t.Errorf("%d clients that do not read: the live heap grew %d MiB, want at most the %d MiB held-response budget (+8)", n, grew>>20, budget>>20)
	}
	if held > budget {
		t.Errorf("held responses charge %d bytes, over their %d-byte budget", held, budget)
	}
	if limit := uint64(inSlot + budget); peak > limit {
		t.Errorf("%d clients that do not read: the heap peaked %d MiB up, want at most %d MiB (one request in its slot and the held-response budget)", n, peak>>20, limit>>20)
	}
	t.Logf("%d unread responses: heap peaked %d MiB up; live heap grew %d MiB, %d MiB held of %d MiB", n, peak>>20, grew>>20, held>>20, budget>>20)

	for deadline := time.Now().Add(10 * time.Second); s.heldResponses.inUse() != 0; time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("held-response budget still at %d bytes after the clients left, want 0", s.heldResponses.inUse())
		}
	}
}
