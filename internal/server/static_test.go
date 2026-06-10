package server

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
)

// testDist is a minimal built-dashboard layout: hashed asset names like
// Vite emits, plus index.html at the root.
func testDist() fstest.MapFS {
	return fstest.MapFS{
		"index.html":         {Data: []byte("<!doctype html><title>upgradescope</title>")},
		"assets/app-abc.js":  {Data: []byte("console.log('app')")},
		"assets/app-abc.css": {Data: []byte("body{}")},
	}
}

func getStatic(t *testing.T, h http.Handler, path string) (*http.Response, string) {
	t.Helper()
	ts := httptest.NewServer(h)
	defer ts.Close()
	resp, err := ts.Client().Get(ts.URL + path)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return resp, string(body)
}

func TestSPAHandlerServesIndexAndAssets(t *testing.T) {
	h := spaHandler(testDist())

	resp, body := getStatic(t, h, "/")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /: status = %d, want 200", resp.StatusCode)
	}
	if !strings.Contains(body, "upgradescope") {
		t.Fatalf("GET /: body %q does not contain index.html content", body)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Fatalf("GET /: Content-Type = %q, want text/html", ct)
	}

	resp, body = getStatic(t, h, "/assets/app-abc.js")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET asset: status = %d, want 200", resp.StatusCode)
	}
	if !strings.Contains(body, "console.log") {
		t.Fatalf("GET asset: body %q is not the asset content", body)
	}
	if cc := resp.Header.Get("Cache-Control"); !strings.Contains(cc, "immutable") {
		t.Fatalf("GET asset: Cache-Control = %q, want immutable (hashed filenames)", cc)
	}
}

func TestSPAHandlerFallbackForClientRoutes(t *testing.T) {
	h := spaHandler(testDist())

	// Extensionless paths are client-side routes: serve index.html.
	resp, body := getStatic(t, h, "/cluster/3")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /cluster/3: status = %d, want 200 (SPA fallback)", resp.StatusCode)
	}
	if !strings.Contains(body, "upgradescope") {
		t.Fatalf("GET /cluster/3: body %q is not index.html", body)
	}

	// Paths with an extension that don't exist are real 404s, not fallbacks.
	resp, _ = getStatic(t, h, "/missing.png")
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("GET /missing.png: status = %d, want 404", resp.StatusCode)
	}

	// Unknown API paths must stay JSON 404s, never become index.html.
	resp, body = getStatic(t, h, "/api/v1/nope")
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("GET /api/v1/nope: status = %d, want 404", resp.StatusCode)
	}
	if !strings.Contains(resp.Header.Get("Content-Type"), "application/json") {
		t.Fatalf("GET /api/v1/nope: Content-Type = %q, want JSON (body %q)", resp.Header.Get("Content-Type"), body)
	}
}

func TestSPAHandlerWithoutBuiltDashboard(t *testing.T) {
	// No index.html (fresh checkout, or -tags nodashboard): a JSON 404
	// that explains how to get the dashboard.
	h := spaHandler(fstest.MapFS{".gitkeep": {Data: nil}})
	resp, body := getStatic(t, h, "/")
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("GET /: status = %d, want 404", resp.StatusCode)
	}
	if !strings.Contains(body, "make web") {
		t.Fatalf("GET /: body %q should mention `make web`", body)
	}
}

func TestStaticDoesNotShadowAPI(t *testing.T) {
	// The catch-all "GET /" route must not affect /healthz or /api/v1/*.
	s := newTestServer(t, newFakeStore())
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	var health map[string]string
	if resp := getJSON(t, ts, "/healthz", "", &health); resp.StatusCode != http.StatusOK {
		t.Fatalf("/healthz status = %d, want 200", resp.StatusCode)
	}
	var clusters []any
	if resp := getJSON(t, ts, "/api/v1/clusters", "", &clusters); resp.StatusCode != http.StatusOK {
		t.Fatalf("/api/v1/clusters status = %d, want 200", resp.StatusCode)
	}
}
