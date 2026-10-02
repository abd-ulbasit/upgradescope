package collect

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"k8s.io/client-go/rest"

	"github.com/abd-ulbasit/upgradescope/internal/inventory"
)

// stallingAPIServer is an API server that accepts every request and never
// answers the ones stall picks, until the client gives up or the test
// ends; the others go to h (404 when nil).
func stallingAPIServer(t *testing.T, stall func(*http.Request) bool, h http.Handler) *httptest.Server {
	t.Helper()
	if h == nil {
		h = http.NotFoundHandler()
	}
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if stall(r) {
			select {
			case <-r.Context().Done():
			case <-release:
			}
			return
		}
		h.ServeHTTP(w, r)
	}))
	t.Cleanup(func() { close(release); srv.Close() })
	return srv
}

func stallPaths(paths ...string) func(*http.Request) bool {
	return func(r *http.Request) bool {
		for _, p := range paths {
			if r.URL.Path == p {
				return true
			}
		}
		return false
	}
}

// within runs f and fails the test if it has not returned after d. A
// regression that ignores the context then fails here instead of hanging
// the test binary.
func within(t *testing.T, d time.Duration, f func()) {
	t.Helper()
	done := make(chan struct{})
	go func() { defer close(done); f() }()
	select {
	case <-done:
	case <-time.After(d):
		t.Fatalf("still running after %v: the call does not honour its context", d)
	}
}

func stallClients(t *testing.T, srv *httptest.Server) Clients {
	t.Helper()
	c, err := NewClients(&rest.Config{Host: srv.URL}) // no per-request timeout
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// #94: a stalled /version held a scan for over seven minutes, because
// ServerVersion ignored the scan's context.
func TestCollectVersionsHonoursContextOnStalledVersion(t *testing.T) {
	c := stallClients(t, stallingAPIServer(t, stallPaths("/version"), nil))
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	var err error
	within(t, 5*time.Second, func() {
		err = collectVersions(ctx, c.Discovery, c.Kube, "team", &inventory.Inventory{})
	})
	if err == nil || !strings.Contains(err.Error(), "server version") {
		t.Errorf("err = %v, want a server version error", err)
	}
}

// Discovery of groups and resources must give up with the context too.
func TestCollectAPIUsageHonoursContextOnStalledDiscovery(t *testing.T) {
	c := stallClients(t, stallingAPIServer(t, stallPaths("/api", "/apis"), nil))
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	var err error
	within(t, 5*time.Second, func() {
		_, err = collectAPIUsage(ctx, c.Discovery, c.Metadata, flaggedLifecycle(), &inventory.Inventory{})
	})
	if err == nil || !strings.Contains(err.Error(), "discovery") {
		t.Errorf("err = %v, want a discovery error", err)
	}
}
