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
//     caching; index.html is always revalidated so deploys take effect)
//   - any extensionless path falls back to index.html — those are
//     client-side routes like /cluster/3
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
		if _, err := fs.Stat(dist, p); err == nil && p != "index.html" {
			if strings.HasPrefix(p, "assets/") {
				// Vite emits content-hashed asset names: safe to cache forever.
				w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
			}
			fileServer.ServeHTTP(w, r)
			return
		}
		// index.html itself, or an extensionless client-side route.
		if p == "index.html" || !strings.Contains(path.Base(p), ".") {
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
