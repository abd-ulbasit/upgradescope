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
	// CatAddOnNoData (info): a detected add-on whose version has no
	// lifecycle data, so its EOL and compatibility were not assessed.
	CatAddOnNoData Category = "addon-no-data"
)

type Severity string

const (
	SevBlocker Severity = "blocker"
	SevWarning Severity = "warning"
	SevInfo    Severity = "info"
)

type Finding struct {
	Category Category `json:"category"`
	Severity Severity `json:"severity"`
	// Key is the finding's stable, count-free identity: it never embeds
	// object counts, node counts, or other volatile numbers, so the same
	// underlying problem keeps the same Key across snapshots even as Title
	// fluctuates ("3 objects" → "2 objects"). Notification delta diffing
	// keys on it. Convention: category + "/" + stable discriminator
	// (e.g. "removed-api/extensions/v1beta1/Ingress", "eol-addon/ingress-nginx").
	Key         string   `json:"key,omitempty"`
	Title       string   `json:"title"`           // one line, deterministic
	Detail      string   `json:"detail"`          // evidence sentence(s), deterministic
	Teams       []string `json:"teams,omitempty"` // sorted, deduped
	Namespaces  []string `json:"namespaces,omitempty"`
	Remediation string   `json:"remediation,omitempty"`
	Citations   []string `json:"citations,omitempty"`
	// Objects identifies the affected objects for API-usage findings
	// (copied from inventory.APIUsage, so at most inventory.MaxObjectRefs),
	// sorted by file, line, namespace, name; ObjectsOmitted counts the
	// affected objects not listed. Empty when the collector could not
	// identify objects.
	Objects        []inventory.ObjectRef `json:"objects,omitempty"`
	ObjectsOmitted int                   `json:"objectsOmitted,omitempty"`
	// BaselineState is set only when the report was compared with a
	// baseline (scan --baseline): BaselineUnchanged when the baseline
	// already had this Key and every object listed here, else BaselineNew.
	// The CLI gate fails only on new findings; score and verdict count both.
	BaselineState BaselineState `json:"baselineState,omitempty"`
}

// BaselineState mirrors SARIF's result.baselineState values.
type BaselineState string

const (
	BaselineNew       BaselineState = "new"
	BaselineUnchanged BaselineState = "unchanged"
)

// SuppressedFinding is a finding, or some of its objects (Objects then
// lists only those), that an ignore rule or object annotation accepted.
// It is excluded from score and verdict but still reported, with why.
type SuppressedFinding struct {
	Finding
	Reason string `json:"reason"`
	// Source names what suppressed it: the config file, "annotation", or
	// "ClusterReadiness spec.ignore".
	Source  string `json:"source"`
	Expires string `json:"expires,omitempty"` // YYYY-MM-DD, as the rule gave it
}

// CapabilityGap is one thing the evaluation could not assess. Capability is
// usually a collector capability whose sub-collector failed (Reason is its
// error). The engine also adds gaps of its own:
//
//   - capability "versions", when the inventory's versions capability is
//     available but the server version is missing or unparseable — the
//     kubelet and control-plane skew rules then had no reference version;
//   - capability "kb-coverage" (GapKBCoverage), when the target is newer
//     than the knowledge base's MaxKnownK8s — removals in releases the KB
//     does not know about cannot be found.
//
// Required marks gaps that make the verdict unknown: api-usage and
// kb-coverage always, versions for cluster inventories. Other gaps only
// narrow what the report covers.
type CapabilityGap struct {
	Capability inventory.Capability `json:"capability"`
	Reason     string               `json:"reason"`
	Required   bool                 `json:"required,omitempty"`
}

// GapKBCoverage is the CapabilityGap capability recorded when the target is
// beyond the knowledge base horizon. It is not a collector capability.
const GapKBCoverage inventory.Capability = "kb-coverage"

// Verdict is the report's readiness answer.
type Verdict string

const (
	// VerdictReady: no blockers, and everything required was assessed.
	VerdictReady Verdict = "ready"
	// VerdictBlocked: at least one blocker finding.
	VerdictBlocked Verdict = "blocked"
	// VerdictUnknown: no blockers found, but a required gap (see
	// CapabilityGap.Required) means blockers may have been missed.
	VerdictUnknown Verdict = "unknown"
)

type Report struct {
	ClusterID string            `json:"clusterId"`
	Target    inventory.Version `json:"target"`
	KBVersion string            `json:"kbVersion"`
	Score     int               `json:"score"` // from findings only; see Score
	// Ready is Verdict == VerdictReady, kept for v0.1 consumers.
	Ready       bool            `json:"ready"`
	Verdict     Verdict         `json:"verdict"`
	Findings    []Finding       `json:"findings"`              // sorted: severity desc, category, title
	NotAssessed []CapabilityGap `json:"notAssessed,omitempty"` // sorted by capability
	// Suppressed lists what ignore rules took out of Findings (see
	// internal/suppress); Evaluate never sets it.
	Suppressed []SuppressedFinding `json:"suppressed,omitempty"`
}

// Rescore recomputes Score, Verdict and Ready from Findings and
// NotAssessed, for callers that remove findings after Evaluate.
func (r *Report) Rescore() {
	r.Score, _ = Score(r.Findings)
	r.Verdict = verdictFor(r.Findings, r.NotAssessed)
	r.Ready = r.Verdict == VerdictReady
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
