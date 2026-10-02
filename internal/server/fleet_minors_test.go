package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"runtime/debug"
	"testing"
	"time"

	"github.com/abd-ulbasit/upgradescope/internal/inventory"
	"github.com/abd-ulbasit/upgradescope/internal/server/store"
)

// fleetMinors is how many clusters TestFleetDefaultColumnsAreMeasured
// pushes, each at a minor of its own.
const fleetMinors = 500

// maxFleetDefaultHeap is what that read may add to the heap: the ~5 MiB
// docs/operations.md gives a 500-cluster fleet read.
const maxFleetDefaultHeap = 5 << 20

// Without ?targets=, /fleet has a column for the next minor of every
// minor some cluster runs, up to maxFleetTargets of them: each column is
// a store query per cluster. 500 clusters at 500 minors, the widest a
// 500-cluster fleet can be, get maxFleetTargets columns (uncapped, 500
// grew the heap 36 MiB and answered 4.4 MB) and cost no more than a
// 16-target ?targets= read, within maxFleetDefaultHeap; the test logs the
// time, heap and response docs/operations.md quotes.
func TestFleetDefaultColumnsAreMeasured(t *testing.T) {
	if testing.Short() || raceEnabled || !heapRun {
		t.Skip("seeds 500 clusters; make test-heap runs it, without the race detector")
	}
	defer debug.SetGCPercent(debug.SetGCPercent(10)) // as in TestGateDecodeHeapIsBounded
	st, err := store.Open(filepath.Join(t.TempDir(), "upgradescope.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	s, err := New(Config{Store: st, KB: testKB(), IngestToken: "ingest-tok"})
	if err != nil {
		t.Fatal(err)
	}
	s.now = func() time.Time { return time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC) }
	for i := range fleetMinors {
		body, err := json.Marshal(map[string]any{
			"schemaVersion": 1, "clusterName": fmt.Sprintf("cluster-%03d", i), "agentVersion": "test", "kbVersion": "agent-kb",
			"inventory": inventory.Inventory{SchemaVersion: 1, ClusterID: fmt.Sprintf("uid-%d", i), ServerVersion: fmt.Sprintf("v1.%d.0", i)},
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
	var columns, omitted, size int
	start := time.Now()
	grew := heapPeak(func() {
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/fleet", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d (%.300s)", rec.Code, rec.Body)
		}
		var resp struct {
			Targets        []string `json:"targets"`
			TargetsOmitted int      `json:"targetsOmitted"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatal(err)
		}
		columns, omitted, size = len(resp.Targets), resp.TargetsOmitted, rec.Body.Len()
	})
	took := time.Since(start)
	if columns != maxFleetTargets || omitted != fleetMinors-maxFleetTargets {
		t.Fatalf("%d columns, %d omitted; want %d and %d", columns, omitted, maxFleetTargets, fleetMinors-maxFleetTargets)
	}
	if grew > maxFleetDefaultHeap {
		t.Fatalf("the heap grew %d MiB, want at most %d MiB", grew>>20, maxFleetDefaultHeap>>20)
	}
	t.Logf("%d clusters at %d minors: /fleet took %v, grew the heap %.1f MiB, answered %d bytes with %d columns", fleetMinors, fleetMinors, took.Round(time.Millisecond), float64(grew)/(1<<20), size, columns)
}
