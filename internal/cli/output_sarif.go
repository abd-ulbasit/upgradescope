package cli

import (
	"encoding/json"
	"io"

	"github.com/abd-ulbasit/upgradescope/internal/engine"
)

// Minimal SARIF 2.1.0 model — only the fields we emit.
type sarifLog struct {
	Schema  string     `json:"$schema"`
	Version string     `json:"version"`
	Runs    []sarifRun `json:"runs"`
}

type sarifRun struct {
	Tool    sarifTool     `json:"tool"`
	Results []sarifResult `json:"results"`
}

type sarifTool struct {
	Driver sarifDriver `json:"driver"`
}

type sarifDriver struct {
	Name           string      `json:"name"`
	InformationURI string      `json:"informationUri,omitempty"`
	Version        string      `json:"version,omitempty"`
	Rules          []sarifRule `json:"rules"`
}

type sarifRule struct {
	ID               string    `json:"id"`
	ShortDescription sarifText `json:"shortDescription"`
}

type sarifText struct {
	Text string `json:"text"`
}

type sarifResult struct {
	RuleID    string          `json:"ruleId"`
	Level     string          `json:"level"`
	Message   sarifText       `json:"message"`
	Locations []sarifLocation `json:"locations,omitempty"`
}

type sarifLocation struct {
	LogicalLocations []sarifLogicalLocation `json:"logicalLocations"`
}

type sarifLogicalLocation struct {
	Name string `json:"name"`
	Kind string `json:"kind"`
}

// WriteSARIF renders the report as minimal SARIF 2.1.0 for CI annotation.
// One rule per finding Category (first-seen order — findings are pre-sorted,
// so this is deterministic), one result per finding. Cluster mode has no
// file paths, so results carry logicalLocations (namespaces) instead of
// physicalLocation.
func WriteSARIF(w io.Writer, r engine.Report) error {
	rules := []sarifRule{}
	seen := map[engine.Category]bool{}
	results := []sarifResult{}

	for _, f := range r.Findings {
		if !seen[f.Category] {
			seen[f.Category] = true
			rules = append(rules, sarifRule{
				ID:               string(f.Category),
				ShortDescription: sarifText{Text: string(f.Category)},
			})
		}
		msg := f.Title
		if f.Detail != "" {
			msg = f.Title + " — " + f.Detail
		}
		res := sarifResult{
			RuleID:  string(f.Category),
			Level:   sarifLevel(f.Severity),
			Message: sarifText{Text: msg},
		}
		if len(f.Namespaces) > 0 {
			lls := make([]sarifLogicalLocation, 0, len(f.Namespaces))
			for _, ns := range f.Namespaces {
				lls = append(lls, sarifLogicalLocation{Name: ns, Kind: "namespace"})
			}
			res.Locations = []sarifLocation{{LogicalLocations: lls}}
		}
		results = append(results, res)
	}

	log := sarifLog{
		Schema:  "https://json.schemastore.org/sarif-2.1.0.json",
		Version: "2.1.0",
		Runs: []sarifRun{{
			Tool: sarifTool{Driver: sarifDriver{
				Name:           "upgradescope",
				InformationURI: "https://github.com/abd-ulbasit/upgradescope",
				Version:        version,
				Rules:          rules,
			}},
			Results: results,
		}},
	}

	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(log)
}

func sarifLevel(s engine.Severity) string {
	switch s {
	case engine.SevBlocker:
		return "error"
	case engine.SevWarning:
		return "warning"
	default: // info
		return "note"
	}
}
