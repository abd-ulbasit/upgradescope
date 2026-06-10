package engine

import (
	"sort"

	"github.com/abd-ulbasit/upgradescope/internal/inventory"
)

type Category string

const (
	CatRemovedAPI         Category = "removed-api"
	CatDeprecatedAPI      Category = "deprecated-api"
	CatDeprecatedAPIInUse Category = "deprecated-api-in-use"
	CatEOLAddon           Category = "eol-addon"
	CatEOLApproaching     Category = "eol-approaching"
	CatVersionSkew        Category = "version-skew"
	CatChartIncompat      Category = "chart-incompat"
	CatKBStale            Category = "kb-stale"
)

type Severity string

const (
	SevBlocker Severity = "blocker"
	SevWarning Severity = "warning"
	SevInfo    Severity = "info"
)

type Finding struct {
	Category    Category `json:"category"`
	Severity    Severity `json:"severity"`
	Title       string   `json:"title"`  // one line, deterministic
	Detail      string   `json:"detail"` // evidence sentence(s), deterministic
	Teams       []string `json:"teams,omitempty"` // sorted, deduped
	Namespaces  []string `json:"namespaces,omitempty"`
	Remediation string   `json:"remediation,omitempty"`
	Citations   []string `json:"citations,omitempty"`
}

type CapabilityGap struct {
	Capability inventory.Capability `json:"capability"`
	Reason     string               `json:"reason"`
}

type Report struct {
	ClusterID   string            `json:"clusterId"`
	Target      inventory.Version `json:"target"`
	KBVersion   string            `json:"kbVersion"`
	Score       int               `json:"score"`
	Ready       bool              `json:"ready"`
	Findings    []Finding         `json:"findings"` // sorted: severity desc, category, title
	NotAssessed []CapabilityGap   `json:"notAssessed,omitempty"`
}

var severityRank = map[Severity]int{SevBlocker: 0, SevWarning: 1, SevInfo: 2}

// sortFindings orders findings deterministically: severity (blocker > warning
// > info), then category (lexical), then title (lexical).
func sortFindings(fs []Finding) {
	sort.SliceStable(fs, func(i, j int) bool {
		if severityRank[fs[i].Severity] != severityRank[fs[j].Severity] {
			return severityRank[fs[i].Severity] < severityRank[fs[j].Severity]
		}
		if fs[i].Category != fs[j].Category {
			return fs[i].Category < fs[j].Category
		}
		return fs[i].Title < fs[j].Title
	})
}
