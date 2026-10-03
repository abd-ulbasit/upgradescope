package server

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
)

// The gate's response is encoded in the evaluation slot and sent after it
// is released, like a read's: a client that is slow to read holds its
// bytes under the held-response budget, never the slot. With that budget
// busy, the evaluation is answered 503 + Retry-After without its verdict
// header, and the slot is free for the next request.
func TestGateSlotIsNotHeldWhileSending(t *testing.T) {
	s := newTestServer(t, newFakeStore())
	w := &unreadResponse{httptest.NewRecorder(), make(chan struct{}), make(chan struct{})}
	done := make(chan struct{})
	go func() {
		defer close(done)
		serveGate(s, w, deploymentManifest)
	}()
	<-w.writing
	if n, used := len(s.gateSlots), s.heldResponses.inUse(); n != 0 || used == 0 {
		t.Fatalf("while the response is sent: %d gate slots held, %d bytes held; want 0 and the response charged", n, used)
	}
	close(w.release)
	<-done
	if w.Code != http.StatusOK || w.Header().Get("X-Upgradescope-Verdict") == "" ||
		w.Header().Get("Content-Length") != strconv.Itoa(w.Body.Len()) || s.heldResponses.inUse() != 0 {
		t.Fatalf("response %d, verdict %q, Content-Length %q for %d bytes, %d bytes still held; want 200 with a verdict and its length, none held",
			w.Code, w.Header().Get("X-Upgradescope-Verdict"), w.Header().Get("Content-Length"), w.Body.Len(), s.heldResponses.inUse())
	}

	full := s.heldResponses.max - 2
	if !s.heldResponses.charge(0, full) {
		t.Fatal("could not fill the held-response budget")
	}
	rec := httptest.NewRecorder()
	serveGate(s, rec, deploymentManifest)
	if rec.Code != http.StatusServiceUnavailable || rec.Header().Get("Retry-After") == "" || rec.Header().Get("X-Upgradescope-Verdict") != "" {
		t.Fatalf("with the budget busy: %d, Retry-After %q, verdict %q (%s); want 503 with Retry-After and no verdict",
			rec.Code, rec.Header().Get("Retry-After"), rec.Header().Get("X-Upgradescope-Verdict"), rec.Body)
	}
	if n, used := len(s.gateSlots), s.heldResponses.inUse(); n != 0 || used != full {
		t.Fatalf("after the 503: %d gate slots held, budget at %d; want 0, %d", n, used, full)
	}
}
