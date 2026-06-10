package server

import (
	"net/http"
	"sync"

	"github.com/abd-ulbasit/upgradescope/registry"
)

// loadRegistry parses the embedded add-on registry once per process — the
// dataset is compiled in, so the result can never change at runtime.
var loadRegistry = sync.OnceValues(registry.Load)

// handleRegistry: GET /api/v1/registry — the embedded add-on EOL/compat
// registry, sorted by id. Lets the dashboard (and curl) browse exactly the
// dataset this binary evaluates against.
func (s *Server) handleRegistry(w http.ResponseWriter, _ *http.Request) {
	addons, err := loadRegistry()
	if err != nil {
		// Only reachable with a corrupt embedded dataset (build artifact bug);
		// registry tests catch this long before a release.
		internalErr(w, "loading embedded registry", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"addons": addons})
}
