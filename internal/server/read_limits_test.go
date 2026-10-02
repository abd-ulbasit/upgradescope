package server

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"runtime/debug"
	"strings"
	"sync"
	"testing"
	"time"
)

// pspUsages is a push of ~size bytes whose every API-usage entry is a
// PodSecurityPolicy (removed in testKB's 1.35) in its own namespace, with
// the most objects a usage lists: one finding per entry, so the stored
// report is about as large as the inventory.
func pspUsages(size int) string {
	var b strings.Builder
	b.WriteString(pushHead + `"apiUsage":[`)
	for i := 0; b.Len() < size-12000; i++ {
		if i > 0 {
			b.WriteString(",")
		}
		fmt.Fprintf(&b, `{"group":"policy","version":"v1beta1","kind":"PodSecurityPolicy","count":100,"namespaces":{"team-%d":100},"objects":[`, i)
		for j := range 100 {
			if j > 0 {
				b.WriteString(",")
			}
			fmt.Fprintf(&b, `{"namespace":"team-%d","name":"o-%d-%d","manager":"m"}`, i, i, j)
		}
		b.WriteString("]}")
	}
	b.WriteString("]}}")
	return b.String()
}

// maxReadHeap is what any number of concurrent reads of one cluster may
// add to the heap: one read in the slot (~45 MB at the snapshot node
// budget) and the garbage the one before it left.
const maxReadHeap = 100 << 20

// The read API decoded a cluster's stored inventory on every request, with
// no limit on how many at once: after one push of 370 KB of `{}` object
// refs (accepted from any ingest token), 10 concurrent GETs of the
// cluster grew the heap ~400 MiB, so ~13 exceeded the chart's 512Mi.
// Every read that loads a snapshot now runs in the read slot, and only a
// what-if decodes the whole inventory: 10 at once of each, against the
// dearest snapshot to decode and against one whose stored report is as
// large as its inventory, stay within maxReadHeap and all succeed.
func TestReadHeapIsBounded(t *testing.T) {
	if testing.Short() || raceEnabled {
		t.Skip("decodes snapshots at the node budget; heap figures under the race detector mean nothing")
	}
	defer debug.SetGCPercent(debug.SetGCPercent(10)) // as in TestGateDecodeHeapIsBounded
	paths := []string{
		"/api/v1/clusters/1",
		"/api/v1/clusters/1/report",
		"/api/v1/clusters/1/report?target=1.40", // no stored evaluation: a what-if
		"/api/v1/clusters/1/findings",
		"/api/v1/clusters/1/teams?target=1.40",
		"/api/v1/clusters/1/history",
		"/api/v1/clusters/1/export?format=html",
		"/api/v1/fleet/teams?target=1.40",
	}
	for name, shape := range map[string]func(int) string{
		"ObjectRefs {}": ingestHeapShapes()["ObjectRefs {}"],
		"PSP usages":    pspUsages,
	} {
		t.Run(name, func(t *testing.T) {
			s := newTestServer(t, newFakeStore())
			s.readQueueTimeout = 5 * time.Minute // the reads run one at a time
			body := atSnapshotBudget(shape)
			rec := httptest.NewRecorder()
			serveIngest(s, rec, []byte(body), false)
			if rec.Code != http.StatusAccepted {
				t.Fatalf("push: status = %d (%.300s)", rec.Code, rec.Body)
			}
			for _, path := range paths {
				const n = 10
				codes := make([]int, n)
				msgs, sizes := make([]string, n), make([]int, n)
				grew := heapPeak(func() {
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
				if grew > maxReadHeap {
					t.Fatalf("%s x%d: the heap grew %d MiB, want at most %d MiB", path, n, grew>>20, maxReadHeap>>20)
				}
				t.Logf("%d bytes (%d units), %s x%d: heap grew %d MiB, response %d bytes",
					len(body), snapshotUnits(body), path, n, grew>>20, sizes[0])
			}
		})
	}
}

// When the read slot stays busy past the queue timeout, a read answers
// 503 + Retry-After; a slot freed while it waits is taken.
func TestReadConcurrencyLimit(t *testing.T) {
	s := newTestServer(t, newFakeStore())
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()
	seedViaPush(t, ts)
	s.readQueueTimeout = 50 * time.Millisecond
	for range cap(s.readSlots) {
		s.readSlots <- struct{}{}
	}
	for _, path := range []string{"/api/v1/clusters/1", "/api/v1/clusters/1/report", "/api/v1/fleet/teams?target=1.35"} {
		resp, body := getRaw(t, ts, path, "")
		if resp.StatusCode != http.StatusServiceUnavailable || resp.Header.Get("Retry-After") == "" {
			t.Fatalf("%s: status = %d (%s), Retry-After %q; want 503 with Retry-After", path, resp.StatusCode, body, resp.Header.Get("Retry-After"))
		}
	}
	// Reads of the whole fleet load no inventory and take no slot.
	for _, path := range []string{"/api/v1/clusters", "/api/v1/fleet", "/metrics"} {
		if resp, body := getRaw(t, ts, path, ""); resp.StatusCode != http.StatusOK {
			t.Fatalf("%s with the read slot busy: status = %d (%s), want 200", path, resp.StatusCode, body)
		}
	}

	s.readQueueTimeout = 5 * time.Second
	go func() {
		time.Sleep(100 * time.Millisecond)
		for range cap(s.readSlots) {
			<-s.readSlots
		}
	}()
	if resp, body := getRaw(t, ts, "/api/v1/clusters/1/report", ""); resp.StatusCode != http.StatusOK {
		t.Fatalf("queued read: status = %d (%s), want 200", resp.StatusCode, body)
	}
}

// unreadResponse is a client that has not read its response yet: Write
// blocks until release is closed.
type unreadResponse struct {
	*httptest.ResponseRecorder
	writing chan struct{}
	release chan struct{}
}

func (b *unreadResponse) Write(p []byte) (int, error) {
	close(b.writing)
	<-b.release
	return b.ResponseRecorder.Write(p)
}

// A read's response is sent after the slot is released, so a client that
// is slow to read it holds only its bytes, never the slot; the status and
// headers the handler set reach the client unchanged.
func TestReadSlotIsNotHeldWhileSending(t *testing.T) {
	s := newTestServer(t, newFakeStore())
	h := s.inReadSlot(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/csv")
		w.WriteHeader(http.StatusTeapot)
		_, _ = w.Write([]byte("a,"))
		_, _ = w.Write([]byte("b\n"))
	})
	w := &unreadResponse{httptest.NewRecorder(), make(chan struct{}), make(chan struct{})}
	done := make(chan struct{})
	go func() {
		defer close(done)
		h(w, httptest.NewRequest(http.MethodGet, "/", nil))
	}()
	<-w.writing
	if n := len(s.readSlots); n != 0 {
		t.Fatalf("%d read slots held while the response is being sent, want 0", n)
	}
	close(w.release)
	<-done
	if w.Code != http.StatusTeapot || w.Header().Get("Content-Type") != "text/csv" || w.Body.String() != "a,b\n" {
		t.Fatalf("response = %d %q %q, want 418 text/csv \"a,b\\n\"", w.Code, w.Header().Get("Content-Type"), w.Body)
	}
}
