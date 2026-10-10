package server

import (
	"fmt"
	"net/http"

	"github.com/abd-ulbasit/upgradescope/internal/engine"
	"github.com/abd-ulbasit/upgradescope/internal/suppress"
)

// gateConfigSource is what the gate's ?config= rules are called in
// suppressed entries (SuppressedFinding.Source) and warnings, where scan
// names its config file.
const gateConfigSource = "config"

// maxGateConfigBytes bounds ?config= (its decoded text). The server's
// request-header limit (64 KiB, request line included) bounds it too, but
// the handler does not rely on whatever http.Server serves it: 32 KiB is a
// few hundred reasoned rules. The rules are parsed before the evaluation
// slot is taken (32 KiB of YAML is cheap to parse, and yaml.v3 refuses
// alias bombs) and applied inside it. URL-encoding expands YAML, so a
// config near the bound can exceed the header limit, or a proxy's, first:
// the suppressions guide says so.
const maxGateConfigBytes = 32 << 10

// gateIgnoreRules reads ?config=, a .upgradescope.yaml whose ignore rules
// the gate applies as `scan --config` applies the file (#44), parsed and
// validated by the same code (suppress.ParseConfig): unknown fields are
// errors, every rule needs a reason, expires must be a date. An invalid
// config is refused (422, written here) before the body is read or
// anything is judged, as scan exits 1 before it scans: a typo fails loudly
// instead of suppressing nothing. So is a config over maxGateConfigBytes,
// and config given more than once (which one a proxy or client library
// means is ambiguous).
func gateIgnoreRules(w http.ResponseWriter, r *http.Request) ([]suppress.Rule, bool) {
	values := r.URL.Query()["config"]
	switch {
	case len(values) == 0 || len(values) == 1 && values[0] == "":
		return nil, true
	case len(values) > 1:
		errJSON(w, http.StatusUnprocessableEntity, "invalid config: given more than once (send one .upgradescope.yaml)")
		return nil, false
	case len(values[0]) > maxGateConfigBytes:
		errJSON(w, http.StatusUnprocessableEntity,
			fmt.Sprintf("invalid config: %d bytes, more than the %d KiB limit", len(values[0]), maxGateConfigBytes>>10))
		return nil, false
	}
	cfg, err := suppress.ParseConfig([]byte(values[0]), gateConfigSource)
	if err != nil {
		errJSON(w, http.StatusUnprocessableEntity, "invalid "+err.Error())
		return nil, false
	}
	return cfg.Ignore, true
}

// suppressGate applies the request's ignore rules and the objects'
// upgradescope.basit.engineer/ignore annotations to the proposed state's report, with
// the code scan uses (suppress.Apply), and returns its warnings: expired
// rules, which no longer apply, and annotations without a reason. Rule
// file globs match the objects' file, which is ?path=.
func (s *Server) suppressGate(rep engine.Report, rules []suppress.Rule) (engine.Report, []string) {
	return suppress.Apply(rep, rules, suppress.Options{Now: s.now(), Source: gateConfigSource})
}

// gateSide is what the manifests introduce with ?cluster=, after
// suppression (suppressSide): the keys gateResult blames on them, and
// their suppressed findings, which the CI formats carry.
type gateSide struct {
	keys       map[string]bool
	suppressed []engine.SuppressedFinding
}

// suppressSide applies the request's rules and the objects' annotations to
// the manifests' side report (mergeManifests) before taking what the
// manifests introduce from it (introducedKeys), as suppressGate does to
// the proposed state's. Taken before suppression, the key of a finding
// whose manifest objects are all accepted would stay introduced, and the
// cluster's objects that remain at that key would fail the PR as the
// manifests'; after it, such a key falls back to the baseline's
// attribution. The suppressed findings are the side's own, so they hold
// the manifests' objects only. Its warnings repeat suppressGate's (the
// side's objects are the proposed state's too) and are dropped.
func (s *Server) suppressSide(side engine.Report, rules []suppress.Rule) gateSide {
	produced := introducedKeys(side)
	rep, _ := s.suppressGate(side, rules)
	out := gateSide{keys: introducedKeys(rep)}
	for _, sf := range rep.Suppressed {
		if produced[findingKey(sf.Finding)] {
			out.suppressed = append(out.suppressed, sf)
		}
	}
	return out
}
