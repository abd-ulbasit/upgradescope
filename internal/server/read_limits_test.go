package server

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"runtime/debug"
	"strings"
	"testing"
	"time"
)

// pspUsages is a push of ~size bytes whose every API-usage entry is a
// PodSecurityPolicy (removed in testKB's 1.35) in its own namespace, with
// the most objects a usage lists: one finding per entry, so the stored
// report is about as large as the inventory.
func pspUsages(size int) string { return pspUsagesNamed(size, "") }

// longPSPUsages is pspUsages with 200-character object names: the same
// findings in about four times the bytes, so the push at the node budget
// is ~18 MB rather than ~4 MB, and so are its stored reports.
func longPSPUsages(size int) string { return pspUsagesNamed(size, strings.Repeat("x", 190)) }

func pspUsagesNamed(size int, suffix string) string {
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
			fmt.Fprintf(&b, `{"namespace":"team-%d","name":"o-%d-%d%s","manager":"m"}`, i, i, j, suffix)
		}
		b.WriteString("]}")
	}
	b.WriteString("]}}")
	return b.String()
}

// maxReadHeap is what concurrent reads of one cluster may add to the
// heap while their clients take what they are sent, as the recorders here
// do at once: one read in the slot (up to ~90 MiB on SQLite at the
// snapshot node budget: a stored report as large as a 17 MB snapshot,
// read through a driver that copies both) and what the one before it
// left (measured up to 90 MiB in all, 10 at once). Clients that do not
// read add at most the held-response budget, over real sockets
// (TestUnreadResponsesAreBounded).
const maxReadHeap = 128 << 20

// The read API decoded a cluster's stored inventory on every request, with
// no limit on how many at once: after one push of 370 KB of `{}` object
// refs (accepted from any ingest token), 10 concurrent GETs of the
// cluster grew the heap ~400 MiB, so ~13 exceeded the chart's 512Mi.
// Every read that loads a snapshot now runs in the read slot, and only a
// what-if decodes the whole inventory: 10 at once of each, on the SQLite
// store, against the dearest snapshot to decode and against two whose
// stored reports are as large as their inventories, stay within
// maxReadHeap and all succeed.
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
	for name, shape := range storedHeapShapes() {
		t.Run(name, func(t *testing.T) {
			s := newSQLiteTestServer(t)
			s.readQueueTimeout = 5 * time.Minute // the reads run one at a time
			body := atSnapshotBudget(shape)
			rec := httptest.NewRecorder()
			serveIngest(s, rec, []byte(body), false)
			if rec.Code != http.StatusAccepted {
				t.Fatalf("push: status = %d (%.300s)", rec.Code, rec.Body)
			}
			for _, path := range paths {
				one, size := concurrentGets(t, s, path, 1)
				const n = 10
				grew, _ := concurrentGets(t, s, path, n)
				if grew > maxReadHeap {
					t.Errorf("%s x%d: the heap grew %d MiB, want at most %d MiB", path, n, grew>>20, maxReadHeap>>20)
				}
				t.Logf("%d bytes (%d units), %s: heap grew %d MiB alone, %d MiB x%d; response %d bytes",
					len(body), snapshotUnits(body), path, one>>20, grew>>20, n, size)
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

// A read's response that fits the held-response budget is sent after the
// slot is released, so a client that is slow to read it holds only its
// bytes, never the slot; the status and headers the handler set reach
// the client unchanged.
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

// A response that does not fit what is left of the held-response budget,
// but would fit it whole, is answered 503 + Retry-After, without the
// handler's headers, and the slot is released: a client that does not
// read can make a large read wait for memory, but it cannot hold the
// slot. The budget is untouched, and a response held after the slot gives
// its bytes back once it is sent, with its Content-Length.
func TestHeldBudgetBusyAnswers503(t *testing.T) {
	s := newTestServer(t, newFakeStore())
	h := s.inReadSlot(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/csv")
		_, _ = w.Write([]byte("report"))
	})
	full := s.heldResponses.max - 2
	if !s.heldResponses.charge(0, full) {
		t.Fatal("could not fill the held-response budget")
	}
	rec := httptest.NewRecorder()
	h(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusServiceUnavailable || rec.Header().Get("Retry-After") == "" ||
		rec.Header().Get("Content-Type") != "application/json" || !strings.Contains(rec.Body.String(), "retry") {
		t.Fatalf("with the budget busy: %d %q, Retry-After %q, body %s; want a JSON 503 with Retry-After",
			rec.Code, rec.Header().Get("Content-Type"), rec.Header().Get("Retry-After"), rec.Body)
	}
	if n, used := len(s.readSlots), s.heldResponses.inUse(); n != 0 || used != full {
		t.Fatalf("after the 503: %d read slots held, budget at %d; want 0, %d", n, used, full)
	}

	s.heldResponses.give(full)
	w := &unreadResponse{httptest.NewRecorder(), make(chan struct{}), make(chan struct{})}
	done := make(chan struct{})
	go func() {
		defer close(done)
		h(w, httptest.NewRequest(http.MethodGet, "/", nil))
	}()
	<-w.writing
	if used := s.heldResponses.inUse(); used == 0 || len(s.readSlots) != 0 {
		t.Fatalf("held after the slot: budget at %d, %d read slots; want the response charged and no slot", used, len(s.readSlots))
	}
	close(w.release)
	<-done
	if used := s.heldResponses.inUse(); used != 0 {
		t.Fatalf("held-response budget at %d after the response was sent, want 0", used)
	}
	if w.Body.String() != "report" || w.Header().Get("Content-Length") != "6" {
		t.Fatalf("response %q, Content-Length %q; want \"report\", 6", w.Body, w.Header().Get("Content-Length"))
	}
}

// A response larger than the whole held-response budget could never be
// held, so it is sent in the read slot, under the slot write deadline:
// the next read waits for its client rather than the server keeping one
// more copy. The budget is untouched.
func TestReadSlotSendsWhatTheHeldBudgetCannotTake(t *testing.T) {
	s := newTestServer(t, newFakeStore())
	s.heldResponses = newByteBudget(8)
	h := s.inReadSlot(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("report"))
	})
	w := &unreadResponse{httptest.NewRecorder(), make(chan struct{}), make(chan struct{})}
	done := make(chan struct{})
	go func() {
		defer close(done)
		h(w, httptest.NewRequest(http.MethodGet, "/", nil))
	}()
	<-w.writing
	if n := len(s.readSlots); n != 1 {
		t.Fatalf("%d read slots held while a response the budget cannot take is sent, want 1", n)
	}
	if used := s.heldResponses.inUse(); used != 0 {
		t.Fatalf("held-response budget at %d while sending in the slot, want 0", used)
	}
	close(w.release)
	<-done
	if n := len(s.readSlots); n != 0 || w.Body.String() != "report" {
		t.Fatalf("after sending: %d read slots held, body %q; want 0, \"report\"", n, w.Body)
	}
}
