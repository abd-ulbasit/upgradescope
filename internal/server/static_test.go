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
	"testing/fstest"
)

// testDist is a minimal built-dashboard layout: hashed asset names like
// Vite emits, plus index.html at the root.
func testDist() fstest.MapFS {
	return fstest.MapFS{
		"index.html": {Data: []byte(`<!doctype html><title>upgradescope</title>` +
			`<script type="module" src="./assets/app-abc.js"></script>` +
			`<link rel="stylesheet" href="./assets/app-abc.css">`)},
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

	// One extensionless segment is a client-side route: serve index.html.
	resp, body := getStatic(t, h, "/teams")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /teams: status = %d, want 200 (SPA fallback)", resp.StatusCode)
	}
	if !strings.Contains(body, "upgradescope") {
		t.Fatalf("GET /teams: body %q is not index.html", body)
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

// staticAssetRef finds the script and stylesheet URLs index.html names.
var staticAssetRef = regexp.MustCompile(`(?:src|href)="([^"]+)"`)

// A path the dashboard is served at must be able to load: every asset its
// index.html references, resolved against the request path as a browser
// does, answers 200. A path it cannot load at is a clean JSON 404, never an
// index.html whose assets 404 (a blank page, #243). It must also hold for
// the real embedded bundle (dist), whose assets are the hashed ones.
func TestSPAHandlerNeverServesAPageItsAssetsCannotLoadFrom(t *testing.T) {
	for name, dist := range map[string]fs.FS{"test bundle": testDist(), "embedded bundle": distFS()} {
		if _, err := fs.Stat(dist, "index.html"); err != nil {
			t.Logf("%s: no index.html (run `make web`): skipped", name)
			continue
		}
		ts := httptest.NewServer(spaHandler(dist))
		for _, p := range []string{"/", "/index.html", "/teams", "/cluster/3", "/cluster/3/", "/foo/", "/teams/", "/a/b/c", "/assets", "/assets/"} {
			resp, err := ts.Client().Get(ts.URL + p)
			if err != nil {
				t.Fatalf("%s GET %s: %v", name, p, err)
			}
			raw, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				if resp.StatusCode != http.StatusNotFound || !strings.HasPrefix(resp.Header.Get("Content-Type"), "application/json") {
					t.Errorf("%s GET %s: status %d %s, want 200 or a JSON 404", name, p, resp.StatusCode, resp.Header.Get("Content-Type"))
				}
				if strings.Contains(string(raw), "<!doctype") {
					t.Errorf("%s GET %s: 404 with a page body", name, p)
				}
				continue
			}
			base, _ := url.Parse(ts.URL + p)
			refs := 0
			for _, m := range staticAssetRef.FindAllStringSubmatch(string(raw), -1) {
				if strings.HasPrefix(m[1], "data:") {
					continue
				}
				refs++
				u, err := base.Parse(m[1])
				if err != nil {
					t.Fatalf("resolve %q: %v", m[1], err)
				}
				ar, err := ts.Client().Get(u.String())
				if err != nil {
					t.Fatalf("GET %s: %v", u, err)
				}
				ar.Body.Close()
				if ar.StatusCode != http.StatusOK {
					t.Errorf("%s: GET %s serves a page whose asset %s answers %d", name, p, u.Path, ar.StatusCode)
				}
			}
			if refs == 0 {
				t.Errorf("%s: GET %s served a page that names no assets", name, p)
			}
		}
		ts.Close()
	}
}

// The bundle's file names are not a public index.
func TestSPAHandlerDoesNotListDirectories(t *testing.T) {
	h := spaHandler(testDist())
	for _, p := range []string{"/assets/", "/assets"} {
		resp, body := getStatic(t, h, p)
		if resp.StatusCode != http.StatusNotFound || strings.Contains(body, "app-abc") {
			t.Errorf("GET %s: status %d body %q, want a 404 that lists nothing", p, resp.StatusCode, body)
		}
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
