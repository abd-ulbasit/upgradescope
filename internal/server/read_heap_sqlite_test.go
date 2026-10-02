package server

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"runtime/debug"
	"sync"
	"testing"
	"time"

	"github.com/abd-ulbasit/upgradescope/internal/server/store"
)

// newSQLiteTestServer is newTestServer on the SQLite store the server
// ships with: memory figures that matter are the real driver's, which
// copies what it reads, not the fake's, which hands out what it holds.
func newSQLiteTestServer(t *testing.T) *Server {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "upgradescope.db"))
	if err != nil {
		t.Fatalf("open sqlite store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	s, err := New(Config{Store: st, KB: testKB(), IngestToken: "ingest-tok"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	s.now = func() time.Time { return time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC) }
	return s
}

// concurrentGets sends n GETs of path at once and returns how much the heap
// grew, failing the test unless every one answered 200.
func concurrentGets(t *testing.T, s *Server, path string, n int) (grew uint64, size int) {
	t.Helper()
	codes, msgs, sizes := make([]int, n), make([]string, n), make([]int, n)
	grew = heapPeak(func() {
		var wg sync.WaitGroup
		for i := range n {
			wg.Go(func() {
				rec := httptest.NewRecorder()
				s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
				codes[i], sizes[i] = rec.Code, rec.Body.Len()
				msgs[i] = string(rec.Body.Bytes()[:min(rec.Body.Len(), 300)]) // not the whole body, which the heap would count
			})
		}
		wg.Wait()
	})
	for i, code := range codes {
		if code != http.StatusOK {
			t.Fatalf("%s x%d: status = %d (%.300s), want 200", path, n, code, msgs[i])
		}
	}
	return grew, sizes[0]
}

// maxFleetReadHeap is what any number of concurrent fleet-wide reads may
// add to the heap: they take no slot, so each must cost what its response
// costs, never what a stored report costs.
const maxFleetReadHeap = 16 << 20

// The cluster list, the fleet matrix and /metrics take no read slot, and
// each loaded every current evaluation's whole report for its notAssessed:
// after one 17 MB push of PodSecurityPolicy usages (accepted from any
// ingest token; its reports are as large), 30 of any of them at once grew
// the heap by 285-584 MiB on SQLite (10 at once, 175-227 MiB), so ~20
// dashboard polls exceeded the chart's memory limit. They now read the
// summary columns only: 0-2 MiB for 30 at once.
func TestFleetReadsLoadNoReport(t *testing.T) {
	if testing.Short() || raceEnabled {
		t.Skip("stores a snapshot at the node budget; heap figures under the race detector mean nothing")
	}
	defer debug.SetGCPercent(debug.SetGCPercent(10)) // as in TestGateDecodeHeapIsBounded
	for name, shape := range map[string]func(int) string{
		"PSP usages":             pspUsages,
		"PSP usages, long names": longPSPUsages,
	} {
		t.Run(name, func(t *testing.T) {
			s := newSQLiteTestServer(t)
			body := atSnapshotBudget(shape)
			rec := httptest.NewRecorder()
			serveIngest(s, rec, []byte(body), false)
			if rec.Code != http.StatusAccepted {
				t.Fatalf("push: status = %d (%.300s)", rec.Code, rec.Body)
			}
			for _, path := range []string{"/api/v1/clusters", "/api/v1/fleet", "/api/v1/fleet?targets=1.35,1.36,1.40", "/metrics"} {
				const n = 30
				grew, size := concurrentGets(t, s, path, n)
				if grew > maxFleetReadHeap {
					t.Errorf("%s x%d: the heap grew %d MiB, want at most %d MiB", path, n, grew>>20, maxFleetReadHeap>>20)
				}
				t.Logf("%d-byte push, %s x%d: heap grew %d MiB, response %d bytes", len(body), path, n, grew>>20, size)
			}
		})
	}
}
