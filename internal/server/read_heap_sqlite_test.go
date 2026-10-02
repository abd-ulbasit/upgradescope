package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"runtime/debug"
	"strings"
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

// maxReevaluationHeap is what the background re-evaluation pass may add to
// the heap: it takes clusters one at a time, and for each target loads the
// current evaluation's report, evaluates, and holds the new report until
// the cluster's commit.
const maxReevaluationHeap = 100 << 20

// storedHeapShapes are the snapshots that cost the most once stored: the
// dearest to decode at the node budget, and two whose every API-usage
// entry is a finding, so each stored report is as large as the snapshot
// (~4 MB, and 17 MB with long object names).
func storedHeapShapes() map[string]func(int) string {
	return map[string]func(int) string{
		"ObjectRefs {}":          ingestHeapShapes()["ObjectRefs {}"],
		"PSP usages":             pspUsages,
		"PSP usages, long names": longPSPUsages,
	}
}

// The steps that load what was stored, measured on the SQLite store, which
// copies every snapshot and report it reads: one push and its evaluation
// within maxIngestDecodeHeap, the re-evaluation pass with every
// evaluation outdated within maxReevaluationHeap, and the dearest /gate
// stream with ?cluster= against the snapshot within maxGateDecodeHeap.
// docs/operations.md adds these up for the chart's memory limit.
func TestStoredSnapshotHeapIsBounded(t *testing.T) {
	if testing.Short() || raceEnabled {
		t.Skip("stores snapshots at the node budget; heap figures under the race detector mean nothing")
	}
	defer debug.SetGCPercent(debug.SetGCPercent(10)) // as in TestGateDecodeHeapIsBounded
	gate := atNodeBudget(gateHeapShapes()["many keys and an alias"])
	for name, shape := range storedHeapShapes() {
		t.Run(name, func(t *testing.T) {
			s := newSQLiteTestServer(t)
			body := []byte(atSnapshotBudget(shape))
			rec := httptest.NewRecorder()
			grew := heapPeak(func() { serveIngest(s, rec, body, false) })
			if rec.Code != http.StatusAccepted || grew > maxIngestDecodeHeap {
				t.Errorf("push: status %d, the heap grew %d MiB; want 202 within %d MiB", rec.Code, grew>>20, maxIngestDecodeHeap>>20)
			}
			t.Logf("%d bytes: push grew the heap %d MiB", len(body), grew>>20)

			next := s.now().Add(24 * time.Hour) // every evaluation is outdated
			s.now = func() time.Time { return next }
			grew = heapPeak(func() { s.reevaluateAll(context.Background()) })
			if grew > maxReevaluationHeap {
				t.Errorf("re-evaluation pass: the heap grew %d MiB, want at most %d MiB", grew>>20, maxReevaluationHeap>>20)
			}
			e, err := s.cfg.Store.CurrentEvaluationSummary(context.Background(), 1, "1.35")
			if err != nil || !e.EvaluatedAt.Equal(next) {
				t.Fatalf("after the pass, 1.35 = (evaluated %v, %v), want re-evaluated at %v", e.EvaluatedAt, err, next)
			}
			t.Logf("re-evaluation pass grew the heap %d MiB", grew>>20)

			rec = httptest.NewRecorder()
			grew = heapPeak(func() {
				req := httptest.NewRequest(http.MethodPost, "/api/v1/gate?target=1.35&fail-on=never&cluster=prod-eu-1", strings.NewReader(gate))
				req.Header.Set("Content-Type", "application/x-yaml")
				s.Handler().ServeHTTP(rec, req)
			})
			if rec.Code != http.StatusOK || grew > maxGateDecodeHeap {
				t.Errorf("gate ?cluster=: status %d (%.300s), the heap grew %d MiB; want 200 within %d MiB", rec.Code, rec.Body, grew>>20, maxGateDecodeHeap>>20)
			}
			t.Logf("gate ?cluster= grew the heap %d MiB", grew>>20)
		})
	}
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
	for name, shape := range storedHeapShapes() {
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
