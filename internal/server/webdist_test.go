//go:build !nodashboard

package server

import (
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
)

var (
	scriptTag = regexp.MustCompile(`(?s)<script\b([^>]*)>(.*?)</script>`)
	assetRef  = regexp.MustCompile(`<(?:script|link)\b[^>]*\b(?:src|href)="([^"]+)"`)
	// A root-absolute API path in the bundle ("/api/v1/…" in quotes or a
	// template literal) would escape a path prefix.
	absoluteAPI = regexp.MustCompile("[\"'`]/api/")
)

// The committed bundle (what `go install` users get) must work behind a
// reverse proxy that serves it under a path prefix and strips the prefix:
// index.html references its assets relatively, the SPA calls the API with
// relative URLs, and no inline script needs a CSP exception.
func TestEmbeddedDashboardWorksUnderPathPrefix(t *testing.T) {
	dist := distFS()
	idx, err := fs.ReadFile(dist, "index.html")
	if err != nil {
		t.Fatalf("embedded index.html: %v (run `make web` and commit internal/server/webdist)", err)
	}
	html := string(idx)

	for _, m := range scriptTag.FindAllStringSubmatch(html, -1) {
		if !strings.Contains(m[1], "src=") || strings.TrimSpace(m[2]) != "" {
			t.Errorf("index.html has an inline script %q: the CSP (script-src 'self') blocks it", m[0])
		}
	}

	var refs []string
	for _, m := range assetRef.FindAllStringSubmatch(html, -1) {
		if strings.HasPrefix(m[1], "data:") {
			continue // the favicon
		}
		refs = append(refs, m[1])
		if strings.HasPrefix(m[1], "/") || strings.Contains(m[1], "://") {
			t.Errorf("index.html references %q: assets must be relative to work under a path prefix", m[1])
		}
	}
	if len(refs) < 2 {
		t.Fatalf("index.html references %v: want the module script and the stylesheet", refs)
	}

	s := newTestServer(t, newFakeStore())
	ts := httptest.NewServer(http.StripPrefix("/upgradescope", s.Handler()))
	defer ts.Close()
	base, _ := url.Parse(ts.URL + "/upgradescope/")
	get := func(ref string) (*http.Response, string) {
		t.Helper()
		u, err := base.Parse(ref)
		if err != nil {
			t.Fatalf("resolve %q: %v", ref, err)
		}
		resp, err := ts.Client().Get(u.String())
		if err != nil {
			t.Fatalf("GET %s: %v", u, err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET %s (%q under the prefix): status %d", u, ref, resp.StatusCode)
		}
		return resp, string(body)
	}

	if _, body := get(""); body != html {
		t.Errorf("GET /upgradescope/ did not serve the embedded index.html")
	}
	for _, ref := range refs {
		_, body := get(ref)
		if strings.HasSuffix(ref, ".js") {
			if absoluteAPI.MatchString(body) {
				t.Errorf("%s calls the API by a root-absolute path: it must be relative (api/v1/…)", ref)
			}
			if !strings.Contains(body, "api/v1/") {
				t.Errorf("%s has no api/v1/ call: is this the dashboard bundle?", ref)
			}
		}
	}
	if resp, _ := get("api/v1/clusters"); !strings.HasPrefix(resp.Header.Get("Content-Type"), "application/json") {
		t.Errorf("api/v1/clusters under the prefix: Content-Type %q, want JSON", resp.Header.Get("Content-Type"))
	}
}
