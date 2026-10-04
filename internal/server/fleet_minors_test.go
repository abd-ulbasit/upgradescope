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
			"inventory": inventory.Inventory{SchemaVersion: 1, CollectorSchema: inventory.CurrentCollectorSchema, ClusterID: fmt.Sprintf("uid-%d", i),
				ServerVersion: fmt.Sprintf("v1.%d.0", i), Capabilities: collectedCaps()},
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

// maxWideGapsFleetHeap is what one fleet-wide read of 500 clusters whose
// evaluations carry the most gaps a push may name may add to the heap:
// docs/operations.md counts two of them in the server's worst case.
const maxWideGapsFleetHeap = 20 << 20 // measured 11.4 MiB for /fleet, at five targets, half the clusters unmarked, on a GitHub-hosted ubuntu-latest runner (CI run 37152753746, #212)

// What an evaluation could not assess is in every fleet-wide read, for
// every cluster and target, and a push within the inventory limits may
// name 32 capabilities, each 16 KiB long, with 64 KiB reasons and long
// skipped lists. 50 such clusters made /fleet grow the heap 516 MiB and
// answer 108 MB; the store's column now keeps each gap cut and the reads
// list at most fleetSummaryBytes of them per evaluation. Each gap here,
// cut, is just under that, so every summary lists one: the dearest
// summaries. Every other cluster pushes as a v0.1.x agent would,
// unmarked: the server adds its required api-usage and deprecated-calls
// gaps to theirs, and its summaries are larger (729 against 588 bytes per
// cluster in /clusters, 2910 against 2205 in /fleet), which the test
// checks, so the fixture keeps the dearest input a push can be while the
// current collector's marked push is read too (#212). 500 such clusters,
// each evaluated at five targets (the default and heapTargets, the most a
// server evaluates), are read within maxWideGapsFleetHeap by each of
// /clusters, /fleet and /metrics; the test logs the figures
// docs/operations.md quotes.
func TestFleetReadsOfTheWidestGapsAreBounded(t *testing.T) {
	if testing.Short() || raceEnabled || !heapRun {
		t.Skip("seeds 500 clusters of 770 KB pushes; make test-heap runs it, without the race detector")
	}
	defer debug.SetGCPercent(debug.SetGCPercent(10)) // as in TestGateDecodeHeapIsBounded
	st, err := store.Open(filepath.Join(t.TempDir(), "upgradescope.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	s, err := New(Config{Store: st, KB: testKB(), IngestToken: "ingest-tok", ExtraTargets: heapTargets})
	if err != nil {
		t.Fatal(err)
	}
	s.now = func() time.Time { return time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC) }
	caps := map[inventory.Capability]inventory.CapabilityStatus{}
	for c := range inventory.MaxCapabilities {
		var skipped []string
		for k := range 10 {
			skipped = append(skipped, fmt.Sprintf("%03d", k)+strings.Repeat("<", 600))
		}
		caps[inventory.Capability(fmt.Sprintf("%02d", c)+strings.Repeat("c", inventory.MaxStringBytes-2))] = inventory.CapabilityStatus{
			Available: true, Partial: true, Reason: strings.Repeat("é", 1000), Skipped: skipped}
	}
	for i := range fleetMinors {
		inv := inventory.Inventory{SchemaVersion: 1, CollectorSchema: inventory.CurrentCollectorSchema, ClusterID: fmt.Sprintf("uid-%d", i),
			ServerVersion: "v1.34.2", Capabilities: caps}
		if i%2 == 1 {
			inv = unmarked(inv) // a v0.1.x agent's push, with no collectorSchema: its summaries are the larger (#212)
		}
		body, err := json.Marshal(map[string]any{
			"schemaVersion": 1, "clusterName": fmt.Sprintf("cluster-%03d", i), "agentVersion": "test", "kbVersion": "agent-kb",
			"inventory": inv,
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
	for _, path := range []string{"/api/v1/clusters", "/api/v1/fleet", "/metrics"} {
		var body []byte
		grew := heapPeak(func() {
			rec := httptest.NewRecorder()
			s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
			if rec.Code != http.StatusOK {
				t.Fatalf("%s: status = %d (%.300s)", path, rec.Code, rec.Body)
			}
			body = rec.Body.Bytes()
		})
		if grew > maxWideGapsFleetHeap {
			t.Errorf("%s grew the heap %d MiB, want at most %d MiB", path, grew>>20, maxWideGapsFleetHeap>>20)
		}
		t.Logf("%d clusters of 31 gaps each: %s grew the heap %.1f MiB, answered %d bytes", fleetMinors, path, float64(grew)/(1<<20), len(body))
		if path == "/metrics" {
			continue // per cluster and target, not per cluster
		}
		marked, legacy := clusterEntryBytes(t, path, body)
		t.Logf("%s: %d bytes per marked cluster, %d per unmarked", path, marked, legacy)
		if legacy <= marked {
			t.Errorf("%s: an unmarked cluster's entry is %d bytes, a marked one's %d: the unmarked push must be the dearer, or the fixture no longer holds the dearest input", path, legacy, marked)
		}
	}
}

// clusterEntryBytes is the mean size of the entries of a /clusters or
// /fleet answer for the clusters TestFleetReadsOfTheWidestGapsAreBounded
// pushes marked (even-numbered) and unmarked (odd-numbered).
func clusterEntryBytes(t *testing.T, path string, body []byte) (marked, unmarked int) {
	t.Helper()
	var entries []json.RawMessage
	if path == "/api/v1/fleet" {
		var fleet struct{ Clusters []json.RawMessage }
		if err := json.Unmarshal(body, &fleet); err != nil {
			t.Fatal(err)
		}
		entries = fleet.Clusters
	} else if err := json.Unmarshal(body, &entries); err != nil {
		t.Fatal(err)
	}
	var sum, n [2]int
	for _, e := range entries {
		var c struct{ Name string }
		if err := json.Unmarshal(e, &c); err != nil {
			t.Fatal(err)
		}
		var i int
		if _, err := fmt.Sscanf(c.Name, "cluster-%d", &i); err != nil {
			t.Fatalf("%s: cluster %q: %v", path, c.Name, err)
		}
		sum[i%2] += len(e)
		n[i%2]++
	}
	if len(entries) != fleetMinors || n[0] != n[1] {
		t.Fatalf("%s: %d clusters, %d marked and %d unmarked; want %d, half of each", path, len(entries), n[0], n[1], fleetMinors)
	}
	return sum[0] / n[0], sum[1] / n[1]
}
