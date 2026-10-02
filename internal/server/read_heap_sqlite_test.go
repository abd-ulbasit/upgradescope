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

// heapTargets are MaxExtraTargets extra targets above the 1.34 the heap
// shapes run, so that, with the default 1.35, every push and every
// re-evaluation of one builds and stores the most reports a server does:
// each target adds one of up to --max-snapshot-bytes. The heap tests that
// store snapshots run at them (atTargetCap), to guard the worst case.
var heapTargets = []string{"1.36", "1.37", "1.38", "1.39"}

// atTargetCap configures a test server with heapTargets.
func atTargetCap(c *Config) { c.ExtraTargets = heapTargets }

// newSQLiteTestServer is newTestServer on the SQLite store the server
// ships with: memory figures that matter are the real driver's, which
// copies what it reads, not the fake's, which hands out what it holds.
func newSQLiteTestServer(t *testing.T, opts ...func(*Config)) *Server {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "upgradescope.db"))
	if err != nil {
		t.Fatalf("open sqlite store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	cfg := Config{Store: st, KB: testKB(), IngestToken: "ingest-tok"}
	for _, o := range opts {
		o(&cfg)
	}
	s, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	s.now = func() time.Time { return time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC) }
	return s
}

// concurrentGets sends n GETs of path at once and returns how much the heap
// grew, failing the test unless every one answered 200, or, for an
// export, 413 for one over the report limit (maxReportBytes).
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
		overLimit := code == http.StatusRequestEntityTooLarge && strings.Contains(path, "/export") && strings.Contains(msgs[i], "the export would be over")
		if code != http.StatusOK && !overLimit {
			t.Fatalf("%s x%d: status = %d (%.300s), want 200", path, n, code, msgs[i])
		}
	}
	return grew, sizes[0]
}

// maxReevaluationHeap is what the background re-evaluation pass may add to
// the heap: it takes clusters one at a time, and for each target loads the
// current evaluation's report, evaluates, and holds the new report until
// the cluster's commit. Measured up to ~189 MiB at the most targets a
// server takes (atTargetCap), for a snapshot whose five reports are each
// about the report limit: each extra target adds one, ~19-24 MiB, to the
// ~81-117 MiB of a pass with one.
const maxReevaluationHeap = 208 << 20

// storedHeapShapes are the snapshots that cost the most once stored, each
// pushed as storedBody stores it: the dearest to decode at the node
// budget; those whose every API-usage entry is a finding, so each stored
// report is as large as the snapshot (~4 MB, and 17 MB with long object
// names), the long names also of characters encoding/json escapes as six
// bytes (<, and U+2028, of three); and those whose reports the engine
// builds larger than their push, which reach the report limit
// (maxReportBytes) first: namespaces listed twice in each finding, teams
// repeated in each, and Helm releases, field managers, deprecated-API
// callers and capability gaps of characters the exports lengthen
// (escapes), in every string an export renders that a push can carry.
// Namespaces and team labels cannot carry them (ingest refuses those).
func storedHeapShapes() map[string]func(int) string {
	return map[string]func(int) string{
		"ObjectRefs {}, 100 per usage": objectRefUsages,
		"PSP usages":                   pspUsages,
		"PSP usages, long names":       longPSPUsages,
		"PSP usages, long names of <": func(size int) string {
			return pspUsagesNamed(size, strings.Repeat("<", 190))
		},
		"PSP usages, long names of U+2028": func(size int) string {
			return pspUsagesNamed(size, strings.Repeat("\u2028", 63))
		},
		"namespace map, 100 per usage": namespaceUsages(100),
		"teams":                        teamUsages,
		"Helm releases of escapes":     helmReleases,
		"managers of escapes":          managerUsages,
		"deprecated calls of escapes":  deprecatedCalls,
		"capability gaps of escapes":   capabilityGaps,
	}
}

// The steps that load what was stored, measured on the SQLite store, which
// copies every snapshot and report it reads, on a server at the most
// targets it takes (atTargetCap): one push and its five evaluations
// within maxIngestDecodeHeap, the re-evaluation pass with every
// evaluation outdated within maxReevaluationHeap, and the dearest /gate
// stream with ?cluster= against the snapshot within maxGateDecodeHeap
// (answered, or 413 when the answer would be over the answer limit).
// docs/operations.md adds these up for the chart's memory limit.
func TestStoredSnapshotHeapIsBounded(t *testing.T) {
	if testing.Short() || raceEnabled || !heapRun {
		t.Skip("stores snapshots at the node budget; make test-heap runs it, without the race detector")
	}
	defer debug.SetGCPercent(debug.SetGCPercent(10)) // as in TestGateDecodeHeapIsBounded
	gate := atNodeBudget(gateHeapShapes()["many keys and an alias"])
	for name, shape := range storedHeapShapes() {
		t.Run(name, func(t *testing.T) {
			s := newSQLiteTestServer(t, atTargetCap)
			body := []byte(storedBody(name, shape))
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
			for _, target := range append([]string{"1.35"}, heapTargets...) {
				e, err := s.cfg.Store.CurrentEvaluationSummary(context.Background(), 1, target)
				if err != nil || !e.EvaluatedAt.Equal(next) {
					t.Fatalf("after the pass, %s = (evaluated %v, %v), want re-evaluated at %v", target, e.EvaluatedAt, err, next)
				}
			}
			t.Logf("re-evaluation pass grew the heap %d MiB", grew>>20)

			rec = httptest.NewRecorder()
			grew = heapPeak(func() {
				req := httptest.NewRequest(http.MethodPost, "/api/v1/gate?target=1.35&fail-on=never&cluster=prod-eu-1", strings.NewReader(gate))
				req.Header.Set("Content-Type", "application/x-yaml")
				s.Handler().ServeHTTP(rec, req)
			})
			// Against a cluster whose report is this large, the answer
			// would be over the answer limit: 413, after the evaluation
			// that costs the heap measured here.
			if answered := rec.Code == http.StatusOK || rec.Code == http.StatusRequestEntityTooLarge; !answered || grew > maxGateDecodeHeap {
				t.Errorf("gate ?cluster=: status %d (%.300s), the heap grew %d MiB; want 200 or 413 within %d MiB", rec.Code, rec.Body, grew>>20, maxGateDecodeHeap>>20)
			}
			t.Logf("gate ?cluster=: status %d, grew the heap %d MiB", rec.Code, grew>>20)
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
	if testing.Short() || raceEnabled || !heapRun {
		t.Skip("stores a snapshot at the node budget; make test-heap runs it, without the race detector")
	}
	defer debug.SetGCPercent(debug.SetGCPercent(10)) // as in TestGateDecodeHeapIsBounded
	for name, shape := range storedHeapShapes() {
		t.Run(name, func(t *testing.T) {
			s := newSQLiteTestServer(t)
			body := storedBody(name, shape)
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
