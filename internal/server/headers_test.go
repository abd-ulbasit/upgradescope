package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
)

// assertSecurityHeaders checks the defense-in-depth headers every response
// must carry (the SPA keeps the read token in localStorage, so an XSS or a
// framing page must find as little room as possible).
func assertSecurityHeaders(t *testing.T, what string, h http.Header) {
	t.Helper()
	if got := h.Get("X-Content-Type-Options"); got != "nosniff" {
		t.Errorf("%s: X-Content-Type-Options = %q, want nosniff", what, got)
	}
	if got := h.Get("Referrer-Policy"); got != "no-referrer" {
		t.Errorf("%s: Referrer-Policy = %q, want no-referrer", what, got)
	}
	if got := h.Get("X-Frame-Options"); got != "DENY" {
		t.Errorf("%s: X-Frame-Options = %q, want DENY", what, got)
	}
	csp := h.Get("Content-Security-Policy")
	for _, directive := range []string{"default-src 'self'", "script-src 'self'", "frame-ancestors 'none'", "object-src 'none'", "base-uri 'none'"} {
		if !strings.Contains(csp, directive) {
			t.Errorf("%s: CSP %q lacks %q", what, csp, directive)
		}
	}
	for _, directive := range strings.Split(csp, ";") {
		directive = strings.TrimSpace(directive)
		if strings.HasPrefix(directive, "script-src") || strings.HasPrefix(directive, "default-src") {
			if strings.Contains(directive, "unsafe-") {
				t.Errorf("%s: %q must not allow inline or eval'd scripts", what, directive)
			}
		}
	}
}

func TestSecurityHeadersOnAPIAndExport(t *testing.T) {
	ts, done := exportFixture(t)
	defer done()

	for _, path := range []string{"/healthz", "/api/v1/clusters", "/api/v1/nope", "/", "/cluster/1"} {
		resp, err := ts.Client().Get(ts.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		assertSecurityHeaders(t, path, resp.Header)
	}

	// The HTML export carries inline <style>: the policy must let it render.
	resp, _ := getExport(t, ts, "?format=html")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("export status = %d", resp.StatusCode)
	}
	assertSecurityHeaders(t, "export", resp.Header)
	if csp := resp.Header.Get("Content-Security-Policy"); !strings.Contains(csp, "style-src 'self' 'unsafe-inline'") {
		t.Errorf("export CSP %q must allow its inline styles", csp)
	}
}

// The dashboard and its hashed assets get the same headers. distFS() is
// empty in a fresh checkout, so this drives a fake bundle through the
// same wrapper.
func TestSecurityHeadersOnDashboard(t *testing.T) {
	dist := fstest.MapFS{
		"index.html":           {Data: []byte("<!doctype html><title>x</title>")},
		"assets/index-abc.js":  {Data: []byte("console.log(1)")},
		"assets/index-abc.css": {Data: []byte("body{}")},
	}
	h := securityHeaders(spaHandler(dist))
	for _, path := range []string{"/", "/cluster/3", "/assets/index-abc.js", "/assets/index-abc.css"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: status = %d", path, rec.Code)
		}
		assertSecurityHeaders(t, path, rec.Header())
	}
}
