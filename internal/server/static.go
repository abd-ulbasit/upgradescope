package server

import (
	"net/http"
	"net/url"
	"path"
	"strings"

	"io/fs"
)

// spaHandler serves the built dashboard from dist (the contents of web/dist,
// embedded by `make web` + go:embed). Routing rules:
//
//   - an existing file is served as-is (hashed /assets/* get immutable
//     caching; index.html is always revalidated so deploys take effect);
//     a directory (/assets/) is a JSON 404, never a listing of file names
//   - "/" and "/index.html" are the dashboard. The dashboard routes by URL
//     hash (#/cluster/3), so it has no other path of its own
//   - a single extensionless segment without a trailing slash ("/teams")
//     falls back to index.html too, as it always has
//   - any other extensionless path ("/cluster/3", "/foo/", "/index.html/")
//     is answered with a 302 to the dashboard's root, written as a
//     relative Location ("../" once per directory level, then the route as
//     a hash: "../#/cluster/3"). The bundle references its assets and the
//     API relatively (./assets/..., api/v1/...) so that it works behind a
//     proxy that serves it under a path prefix and strips it; served at a
//     nested path those URLs would resolve to /cluster/assets/... and 404,
//     a blank page (#243). The redirect lands on the root, where they
//     resolve, and a relative Location keeps the proxy's prefix, which an
//     absolute one (http.Redirect makes one) would drop
//   - any other path (one with a file extension that is not a file, or
//     under /assets/) is a JSON 404
//   - when index.html is absent (fresh checkout, or -tags nodashboard),
//     everything is a JSON 404 pointing at `make web`
func spaHandler(dist fs.FS) http.Handler {
	fileServer := http.FileServer(http.FS(dist))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := strings.TrimPrefix(path.Clean(r.URL.Path), "/")
		if p == "" || p == "." {
			p = "index.html"
		}
		// Server.handler routes /api/* to the mux before this handler runs;
		// the guard keeps spaHandler safe standalone — an API path must
		// never be answered with index.html (curl, agents).
		if strings.HasPrefix(p, "api/") {
			errJSON(w, http.StatusNotFound, "unknown API path")
			return
		}
		trailing := strings.HasSuffix(r.URL.Path, "/") && r.URL.Path != "/"
		if fi, err := fs.Stat(dist, p); err == nil && p != "index.html" {
			if fi.IsDir() {
				errJSON(w, http.StatusNotFound, "not found")
				return
			}
			if strings.HasPrefix(p, "assets/") {
				// Vite emits content-hashed asset names: safe to cache forever.
				w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
			}
			fileServer.ServeHTTP(w, r)
			return
		}
		if p == "assets" || strings.HasPrefix(p, "assets/") || (path.Ext(p) != "" && p != "index.html") {
			errJSON(w, http.StatusNotFound, "not found")
			return
		}
		idx, err := fs.ReadFile(dist, "index.html")
		if err != nil {
			errJSON(w, http.StatusNotFound,
				"dashboard not built (run `make web` and rebuild, or use the API under /api/v1)")
			return
		}
		// The dashboard itself: "/", "/index.html", or one extensionless
		// segment that is not a directory request (a client-side route).
		if !trailing && (p == "index.html" || !strings.Contains(p, "/")) {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.Header().Set("Cache-Control", "no-cache")
			_, _ = w.Write(idx)
			return
		}
		// Anywhere else the page's relative URLs would resolve wrongly:
		// send the browser to the root, carrying the route in the hash.
		w.Header().Set("Location", rootRelativeTo(p, trailing))
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusFound)
	})
}

// rootRelativeTo is the relative reference from the URL path p (cleaned, no
// leading slash; trailing says the request ended in "/") to the dashboard's
// root: "../" once per directory the URL's base has, followed by the route
// as a hash when p is a plain route ("../#/cluster/3"). It is relative on
// purpose: behind a proxy that strips a path prefix, the browser resolves it
// against the prefixed URL and stays under the prefix.
func rootRelativeTo(p string, trailing bool) string {
	depth := strings.Count(p, "/")
	if trailing {
		depth++
	}
	u := url.URL{Path: strings.Repeat("../", depth)}
	if p != "index.html" {
		u.Fragment = "/" + p
	}
	return u.String()
}
