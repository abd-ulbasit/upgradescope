package server

import (
	"net/http"
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
//   - any other path ("/cluster/3", "/foo/") is a JSON 404. The bundle
//     references its assets relatively (./assets/...) so that it works
//     behind a proxy that serves it under a path prefix and strips it; at
//     a nested path those URLs would resolve to /cluster/assets/... and
//     404, which a browser shows as a blank page (#243). A clean 404 says
//     so instead
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
		// index.html itself, or one extensionless segment that is not a
		// directory request: a client-side route the page can load at.
		if p == "index.html" || (!strings.Contains(p, "/") && !strings.Contains(p, ".") &&
			!strings.HasSuffix(r.URL.Path, "/")) {
			idx, err := fs.ReadFile(dist, "index.html")
			if err != nil {
				errJSON(w, http.StatusNotFound,
					"dashboard not built (run `make web` and rebuild, or use the API under /api/v1)")
				return
			}
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.Header().Set("Cache-Control", "no-cache")
			_, _ = w.Write(idx)
			return
		}
		errJSON(w, http.StatusNotFound, "not found")
	})
}
