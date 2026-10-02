package server

import (
	"net/http"

	"github.com/abd-ulbasit/upgradescope/internal/engine"
	"github.com/abd-ulbasit/upgradescope/internal/suppress"
)

// gateConfigSource is what the gate's ?config= rules are called in
// suppressed entries (SuppressedFinding.Source) and warnings, where scan
// names its config file.
const gateConfigSource = "config"

// gateIgnoreRules reads ?config=, a .upgradescope.yaml whose ignore rules
// the gate applies as `scan --config` applies the file (#44). An invalid
// config is refused (422, written here) before anything is judged, as
// scan exits 1 before it scans: a typo fails loudly instead of suppressing
// nothing. The parameter is bounded by the server's request-header limit
// (64 KiB, request line included).
func gateIgnoreRules(w http.ResponseWriter, r *http.Request) ([]suppress.Rule, bool) {
	raw := r.URL.Query().Get("config")
	if raw == "" {
		return nil, true
	}
	cfg, err := suppress.ParseConfig([]byte(raw), gateConfigSource)
	if err != nil {
		errJSON(w, http.StatusUnprocessableEntity, "invalid "+err.Error())
		return nil, false
	}
	return cfg.Ignore, true
}

// suppressGate applies the request's ignore rules and the objects'
// upgradescope.dev/ignore annotations to the proposed state's report, with
// the code scan uses (suppress.Apply), and returns its warnings: expired
// rules, which no longer apply, and annotations without a reason. Rule
// file globs match the objects' file, which is ?path=.
func (s *Server) suppressGate(rep engine.Report, rules []suppress.Rule) (engine.Report, []string) {
	return suppress.Apply(rep, rules, suppress.Options{Now: s.now(), Source: gateConfigSource})
}
