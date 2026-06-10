package server

import (
	"fmt"
	"os"
	"path"

	"sigs.k8s.io/yaml"

	"github.com/abd-ulbasit/upgradescope/internal/inventory"
)

// TeamMapRule maps namespaces matching a glob Pattern (path.Match grammar)
// to Team. Rules are ordered; the first matching pattern wins.
type TeamMapRule struct {
	Pattern string `json:"pattern"`
	Team    string `json:"team"`
}

// TeamMap is the server-side namespace→team override (spec: labels + server
// override). It is applied over the namespace team labels carried in the
// inventory at evaluation time — a matching rule replaces the label; a
// namespace matching no rule keeps its label. A nil/empty TeamMap is a no-op.
type TeamMap []TeamMapRule

// LoadTeamMap reads and parses a --team-map YAML file.
func LoadTeamMap(p string) (TeamMap, error) {
	data, err := os.ReadFile(p)
	if err != nil {
		return nil, fmt.Errorf("read team map: %w", err)
	}
	tm, err := ParseTeamMap(data)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", p, err)
	}
	return tm, nil
}

// ParseTeamMap parses YAML of the form:
//
//	- pattern: "payments-*"
//	  team: payments
//
// Every rule must have a non-empty pattern (valid path.Match glob) and team.
func ParseTeamMap(data []byte) (TeamMap, error) {
	var tm TeamMap
	if err := yaml.UnmarshalStrict(data, &tm); err != nil {
		return nil, fmt.Errorf("parse team map: %w", err)
	}
	for i, r := range tm {
		if r.Pattern == "" {
			return nil, fmt.Errorf("team map rule %d: pattern is required", i+1)
		}
		if r.Team == "" {
			return nil, fmt.Errorf("team map rule %d (%q): team is required", i+1, r.Pattern)
		}
		// path.Match validates the pattern syntax regardless of the name
		// matched against; bad globs must fail at load, not silently never
		// match at evaluation time.
		if _, err := path.Match(r.Pattern, ""); err != nil {
			return nil, fmt.Errorf("team map rule %d: invalid glob %q: %w", i+1, r.Pattern, err)
		}
	}
	return tm, nil
}

// Apply returns a rewritten copy of ns: for each namespace, the first rule
// whose glob matches sets the team; no match keeps the inventory label.
// The input slice is never mutated (it may alias a caller's inventory).
func (m TeamMap) Apply(ns []inventory.NamespaceInfo) []inventory.NamespaceInfo {
	if len(m) == 0 || len(ns) == 0 {
		return ns
	}
	out := make([]inventory.NamespaceInfo, len(ns))
	copy(out, ns)
	for i := range out {
		for _, r := range m {
			// Pattern validity is checked at load; Match cannot error here.
			if ok, _ := path.Match(r.Pattern, out[i].Name); ok {
				out[i].Team = r.Team
				break
			}
		}
	}
	return out
}
