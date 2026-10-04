package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"runtime/debug"
	"strings"
	"testing"
	"time"

	"github.com/abd-ulbasit/upgradescope/internal/inventory"
	"github.com/abd-ulbasit/upgradescope/internal/server/store"
)

// The fleet TestUnreadFleetResponsesAreBounded reads: unreadFleetSize
// clusters with names of unreadFleetNameLen bytes, each evaluated for its
// next minor and two serve --targets minors. Its responses (2.1-9.0 MB) are
// several times what a kernel buffers for a client that does not read (a
// few KiB on Linux with the test's small buffers, ~520 KiB on macOS
// loopback whatever they are set to), so they wait in the server's heap.
const (
	unreadFleetSize    = 2000
	unreadFleetNameLen = 200
	unreadFleetClients = 100
)

// maxFleetSlotHeap is what building the fleet-wide responses in their
// slots (maxConcurrentFleetReads at once) may add to the heap for that
// fleet: the summaries they read, the responses and their encodings (for
// /metrics, the gathered metric families too).
//
// The test's peaks (59-125 MiB on a GitHub-hosted ubuntu-latest runner, #212)
// are the whole heap, held responses included, against the 168 MiB this and
// the 40 MiB held budget allow together; the slot's own share of the
// largest is at most 125 less what was held (32 MiB, so about 93 MiB for
// two builds), not 125 against 128.
const maxFleetSlotHeap = 128 << 20

// pushedFleet is a SQLite server holding one push from each of n
// clusters, named with nameLen bytes, at v1.34 with no other signal, from
// a v0.1.x agent: an unmarked inventory (no collectorSchema) reporting
// every capability. Each of its evaluations lists the required api-usage
// and deprecated-calls gaps a legacy agent is given, the larger summaries
// the server accepts from a push of this size: the fleet's answers are
// several times those of a current collector's, and the held-response
// peak (SE-05c) is measured on them (#212). It evaluates 1.35 and the
// serve --targets 1.36 and 1.37.
func pushedFleet(t *testing.T, n, nameLen int) *Server {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "upgradescope.db"))
	if err != nil {
		t.Fatalf("open sqlite store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	s, err := New(Config{Store: st, KB: testKB(), IngestToken: "ingest-tok", ExtraTargets: []string{"1.36", "1.37"}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	s.now = func() time.Time { return time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC) }
	for i := range n {
		name := fmt.Sprintf("cluster-%05d-", i)
		name += strings.Repeat("x", nameLen-len(name))
		body, err := json.Marshal(map[string]any{
			"schemaVersion": 1, "clusterName": name, "agentVersion": "test", "kbVersion": "agent-kb",
			"inventory": inventory.Inventory{SchemaVersion: 1, ClusterID: fmt.Sprintf("uid-%d", i),
				ServerVersion: "v1.34.2", Capabilities: collectedCaps()},
		})
		if err != nil {
			t.Fatal(err)
		}
		rec := httptest.NewRecorder()
		serveIngest(s, rec, body, false)
		if rec.Code != http.StatusAccepted {
			t.Fatalf("push %d: status = %d (%.300s)", i, rec.Code, rec.Body)
		}
	}
	return s
}

// The reads of the whole fleet (/clusters, /fleet, /metrics) took no slot
// and wrote straight to their clients, so a client that did not read held
// its whole encoded response, for up to the 120s write timeout, with
// nothing capping how many: against this fleet, 100 such clients grew the
// live heap 201 MiB (/clusters) and 251 MiB (/fleet), and 30 grew it
// 433 MiB (/metrics, where promhttp keeps the gathered metric families
// until the write returns). They are now built in fleet slots of their
// own, written to memory and held under the budget the per-cluster reads
// and /gate share, so clients that never read hold at most that budget.
func TestUnreadFleetResponsesAreBounded(t *testing.T) {
	if testing.Short() || raceEnabled || !heapRun {
		t.Skip("seeds a 2000-cluster fleet; make test-heap runs it, without the race detector")
	}
	defer debug.SetGCPercent(debug.SetGCPercent(10)) // as in TestGateDecodeHeapIsBounded
	s := pushedFleet(t, unreadFleetSize, unreadFleetNameLen)
	s.slotWriteTimeout = time.Second
	s.fleetQueueTimeout = 5 * time.Minute // every request is served, none is turned away
	var minors []string                   // the most ?targets= takes: maxFleetTargets columns
	for i := range maxFleetTargets {
		minors = append(minors, fmt.Sprintf("1.%d", 30+i))
	}
	for _, path := range []string{"/api/v1/clusters", "/api/v1/fleet", "/api/v1/fleet?targets=" + strings.Join(minors, ","), "/metrics"} {
		t.Run(path, func(t *testing.T) {
			rec := httptest.NewRecorder()
			s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
			if rec.Code != http.StatusOK {
				t.Fatalf("GET %s = %d, want 200", path, rec.Code)
			}
			t.Logf("GET %s: %d bytes", path, rec.Body.Len())
			rec = nil
			req := "GET " + path + " HTTP/1.1\r\nHost: upgradescope\r\n\r\n"
			checkUnreadBounded(t, s, s.fleetSlots, req, unreadFleetClients, maxFleetSlotHeap)
		})
	}
}
