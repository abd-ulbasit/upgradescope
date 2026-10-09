package server

import (
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
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

// Every path the dashboard is asked at loads it (#243): a route that is not
// the root is answered with a relative redirect to the root, so the page's
// relative asset and API URLs resolve, with or without a proxy path prefix
// that strips itself. The test follows the redirect as a browser does and
// asks that each asset the final page references answers 200 and that the
// page's relative API base lands on the same prefix. It holds for the real
// embedded bundle (hashed assets) too.
func TestSPAHandlerLoadsTheDashboardAtEveryRoute(t *testing.T) {
	// route -> where it lands (below the prefix) and the hash it carries:
	// the page itself where it can load, otherwise the root.
	type landing struct{ path, fragment string }
	routes := map[string]landing{
		"/": {"/", ""}, "/index.html": {"/index.html", ""}, "/teams": {"/teams", ""}, "/registry": {"/registry", ""},
		"/index.html/": {"/", ""}, "/teams/": {"/", "/teams"}, "/cluster/3": {"/", "/cluster/3"},
		"/cluster/3/": {"/", "/cluster/3"}, "/foo/": {"/", "/foo"}, "/a/b/c": {"/", "/a/b/c"},
	}
	for name, dist := range map[string]fs.FS{"test bundle": testDist(), "embedded bundle": distFS()} {
		if _, err := fs.Stat(dist, "index.html"); err != nil {
			t.Logf("%s: no index.html (run `make web`): skipped", name)
			continue
		}
		for _, prefix := range []string{"", "/upgradescope", "/a/b"} {
			var h http.Handler = spaHandler(dist)
			if prefix != "" {
				h = http.StripPrefix(prefix, h)
			}
			ts := httptest.NewServer(h)
			for route, land := range routes {
				resp, err := ts.Client().Get(ts.URL + prefix + route)
				if err != nil {
					t.Fatalf("%s GET %s%s: %v", name, prefix, route, err)
				}
				raw, _ := io.ReadAll(resp.Body)
				resp.Body.Close()
				if resp.StatusCode != http.StatusOK || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/html") {
					t.Errorf("%s GET %s%s: status %d %s after redirects, want the page", name, prefix, route, resp.StatusCode, resp.Header.Get("Content-Type"))
					continue
				}
				final := resp.Request.URL
				if final.Path != prefix+land.path || final.Fragment != land.fragment {
					t.Errorf("%s GET %s%s: landed on %s#%s, want %s#%s", name, prefix, route, final.Path, final.Fragment, prefix+land.path, land.fragment)
				}
				// The page's relative API base is on the same prefix.
				if api, _ := final.Parse("api/v1/clusters"); api.Path != prefix+"/api/v1/clusters" {
					t.Errorf("%s GET %s%s: relative API base resolves to %s, want %s", name, prefix, route, api.Path, prefix+"/api/v1/clusters")
				}
				refs := 0
				for _, m := range staticAssetRef.FindAllStringSubmatch(string(raw), -1) {
					if strings.HasPrefix(m[1], "data:") {
						continue
					}
					refs++
					u, err := final.Parse(m[1])
					if err != nil {
						t.Fatalf("resolve %q: %v", m[1], err)
					}
					ar, err := ts.Client().Get(u.String())
					if err != nil {
						t.Fatalf("GET %s: %v", u, err)
					}
					ar.Body.Close()
					if ar.StatusCode != http.StatusOK {
						t.Errorf("%s: GET %s%s serves a page whose asset %s answers %d", name, prefix, route, u.Path, ar.StatusCode)
					}
				}
				if refs == 0 {
					t.Errorf("%s: GET %s%s served a page that names no assets", name, prefix, route)
				}
			}
			ts.Close()
		}
	}
}

// The redirect is a hand-written relative reference (http.Redirect would
// make it absolute and drop a proxy's prefix), and what is not a route is a
// JSON 404, never a page or a redirect.
func TestSPAHandlerRedirectsNestedRoutesRelatively(t *testing.T) {
	ts := httptest.NewServer(spaHandler(testDist()))
	defer ts.Close()
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	for route, want := range map[string]string{
		"/cluster/3":     "../#/cluster/3",
		"/cluster/3/":    "../../#/cluster/3",
		"/foo/":          "../#/foo",
		"/teams/":        "../#/teams",
		"/a/b/c":         "../../#/a/b/c",
		"/index.html/":   "../",
		"/cluster/a%20b": "../#/cluster/a%20b",
	} {
		resp, err := client.Get(ts.URL + route)
		if err != nil {
			t.Fatalf("GET %s: %v", route, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusFound || resp.Header.Get("Location") != want {
			t.Errorf("GET %s: %d Location %q, want 302 %q", route, resp.StatusCode, resp.Header.Get("Location"), want)
		}
	}
	for _, route := range []string{"/missing.png", "/cluster/3.png", "/assets", "/assets/", "/assets/missing", "/assets/x/y", "/api/v1/nope"} {
		resp, err := client.Get(ts.URL + route)
		if err != nil {
			t.Fatalf("GET %s: %v", route, err)
		}
		raw, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound || !strings.HasPrefix(resp.Header.Get("Content-Type"), "application/json") ||
			resp.Header.Get("Location") != "" || strings.Contains(string(raw), "<!doctype") {
			t.Errorf("GET %s: %d %s Location %q, want a JSON 404", route, resp.StatusCode, resp.Header.Get("Content-Type"), resp.Header.Get("Location"))
		}
	}
	// Without a built dashboard a nested route is the explaining 404, not
	// a redirect to a root that is one too.
	empty := httptest.NewServer(spaHandler(fstest.MapFS{".gitkeep": {}}))
	defer empty.Close()
	resp, err := client.Get(empty.URL + "/cluster/3")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("no bundle, GET /cluster/3: %d, want 404", resp.StatusCode)
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
