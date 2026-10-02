package server

import (
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"runtime"
	"runtime/debug"
	"sync/atomic"
	"testing"
	"time"
)

// unreadClients sends GET path on n TCP connections to ts, each with a
// small receive buffer, and never reads a response; closeAll closes them.
func unreadClients(t *testing.T, ts *httptest.Server, path string, n int) (closeAll func()) {
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
		if _, err := fmt.Fprintf(c, "GET %s HTTP/1.1\r\nHost: upgradescope\r\n\r\n", path); err != nil {
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
// whose clients never read, the live heap grows by no more than that
// budget, and it is given back once the clients are gone.
func TestUnreadResponsesAreBounded(t *testing.T) {
	if testing.Short() || raceEnabled {
		t.Skip("stores a snapshot at the node budget; heap figures under the race detector mean nothing")
	}
	defer debug.SetGCPercent(debug.SetGCPercent(10)) // as in TestGateDecodeHeapIsBounded
	s := newSQLiteTestServer(t)
	s.readQueueTimeout = 5 * time.Minute // every request is served, none is turned away
	s.slotWriteTimeout = time.Second
	rec := httptest.NewRecorder()
	serveIngest(s, rec, []byte(atSnapshotBudget(longPSPUsages)), false)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("push: status = %d (%.300s)", rec.Code, rec.Body)
	}

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
	const n = 8
	var closeAll func()
	defer func() {
		if closeAll != nil {
			closeAll()
		}
	}()
	quiet := 0
	peak := heapPeak(func() {
		closeAll = unreadClients(t, ts, "/api/v1/clusters/1/report", n)
		// Wait until every request has run and none waits for the slot:
		// each response is then held for its client, or its write was
		// given up.
		for deadline := time.Now().Add(3 * time.Minute); quiet < 5 && time.Now().Before(deadline); {
			time.Sleep(100 * time.Millisecond)
			if started.Load() == n && len(s.readSlots) == 0 {
				quiet++
			} else {
				quiet = 0
			}
		}
	})
	if quiet < 5 {
		t.Fatalf("%d of %d requests started, %d read slots held after 3m", started.Load(), n, len(s.readSlots))
	}
	grew := int64(liveHeap()) - int64(base)
	held := s.readHeld.inUse()
	closeAll()
	budget := s.readHeld.max
	if grew > budget+8<<20 {
		t.Errorf("%d clients that do not read their report: the live heap grew %d MiB, want at most the %d MiB held-response budget (+8)", n, grew>>20, budget>>20)
	}
	if held > budget {
		t.Errorf("held responses charge %d bytes, over their %d-byte budget", held, budget)
	}
	if limit := uint64(maxReadHeap + budget); peak > limit {
		t.Errorf("%d clients that do not read their report: the heap peaked %d MiB up, want at most %d MiB (maxReadHeap and the held-response budget)", n, peak>>20, limit>>20)
	}
	t.Logf("%d unread reports: heap peaked %d MiB up; live heap grew %d MiB, %d MiB held of %d MiB", n, peak>>20, grew>>20, held>>20, budget>>20)

	for deadline := time.Now().Add(10 * time.Second); s.readHeld.inUse() != 0; time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("held-response budget still at %d bytes after the clients left, want 0", s.readHeld.inUse())
		}
	}
}
