package server

import (
	"context"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/abd-ulbasit/upgradescope/internal/inventory"
	"github.com/abd-ulbasit/upgradescope/internal/kb"
)

// testKB is a tiny deterministic KB: one lifecycle entry (PodSecurityPolicy,
// deprecated 1.30, removed 1.35) and a MaxKnownK8s high enough that kb-stale
// never fires in tests.
func testKB() kb.KB {
	deprecated := inventory.Version{Major: 1, Minor: 30}
	removed := inventory.Version{Major: 1, Minor: 35}
	return kb.KB{
		Version: "test-kb",
		APILifecycle: []kb.APILifecycleEntry{{
			Group:      "policy",
			Version:    "v1beta1",
			Kind:       "PodSecurityPolicy",
			Introduced: inventory.Version{Major: 1, Minor: 10},
			Deprecated: &deprecated,
			Removed:    &removed,
		}},
		Skew:        kb.DefaultSkewPolicy(),
		MaxKnownK8s: inventory.Version{Major: 1, Minor: 99},
	}
}

func TestNewValidation(t *testing.T) {
	st := newFakeStore()
	if _, err := New(Config{Store: nil, KB: testKB(), IngestToken: "tok"}); err == nil {
		t.Fatal("New with nil Store: want error, got nil")
	}
	if _, err := New(Config{Store: st, KB: testKB(), IngestToken: ""}); err == nil {
		t.Fatal("New with empty IngestToken: want error, got nil")
	}
	if _, err := New(Config{Store: st, KB: testKB(), IngestToken: "tok", ExtraTargets: []string{"bogus"}}); err == nil {
		t.Fatal("New with unparseable extra target: want error, got nil")
	}
	s, err := New(Config{Store: st, KB: testKB(), IngestToken: "tok", ExtraTargets: []string{"1.37", "v1.38"}})
	if err != nil {
		t.Fatalf("New with valid config: %v", err)
	}
	if s.Handler() == nil {
		t.Fatal("Handler() returned nil")
	}
}

func TestStartShutdown(t *testing.T) {
	s, err := New(Config{
		Listen:      "127.0.0.1:0",
		Store:       newFakeStore(),
		KB:          testKB(),
		IngestToken: "tok",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	errc := make(chan error, 1)
	go func() { errc <- s.Start() }()
	select {
	case <-s.Ready():
	case err := <-errc:
		t.Fatalf("server exited before becoming ready: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("server never became ready")
	}
	resp, err := http.Get(fmt.Sprintf("http://%s/nope", s.Addr()))
	if err != nil {
		t.Fatalf("GET while serving: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("GET /nope status = %d, want 404 (mux serving, route unregistered)", resp.StatusCode)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := s.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if err := <-errc; err != nil {
		t.Fatalf("Start returned %v after graceful Shutdown, want nil", err)
	}
}
