package crd

import (
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/abd-ulbasit/upgradescope/internal/crd/apigroup"
	"github.com/abd-ulbasit/upgradescope/internal/engine"
	"github.com/abd-ulbasit/upgradescope/internal/suppress"
)

const (
	// Group is the API group, spelled once in internal/crd/apigroup.
	Group    = apigroup.Group
	Version  = "v1alpha1"
	Kind     = "ClusterReadiness"
	Plural   = "clusterreadinesses"
	Singular = "clusterreadiness"
	// DefaultName is the conventional singleton object name.
	DefaultName = "cluster"
	// StatusErrorAnnotation marks a ClusterReadiness whose status the agent
	// failed to write, so the verdict it still shows is not read as
	// current: its value is the time of the failure (RFC 3339, UTC) and a
	// short reason. The next successful status write removes it.
	StatusErrorAnnotation = apigroup.StatusErrorAnnotation
)

// GVR is the dynamic-client resource identifier for ClusterReadiness.
func GVR() schema.GroupVersionResource {
	return schema.GroupVersionResource{Group: Group, Version: Version, Resource: Plural}
}

type Spec struct {
	// Targets are Kubernetes minor versions to evaluate against, e.g. ["1.36","1.37"].
	// Empty → agent defaults to next minor above the observed server version.
	Targets []string `json:"targets,omitempty"`
	// Ignore accepts findings, as .upgradescope.yaml's ignore list does;
	// the agent applies it every tick. File globs never match live objects.
	Ignore []suppress.Rule `json:"ignore,omitempty"`
}

type TargetStatus struct {
	Target      string         `json:"target"` // "1.36"
	Score       int            `json:"score"`
	Ready       bool           `json:"ready"`             // == (Verdict == "ready"); kept for v0.1 readers
	Verdict     string         `json:"verdict,omitempty"` // "ready" | "blocked" | "unknown" (engine.Verdict)
	Blockers    int            `json:"blockers"`
	Warnings    int            `json:"warnings"`
	Infos       int            `json:"infos"`
	Suppressed  int            `json:"suppressed,omitempty"`  // accepted by spec.ignore or annotations; not in the counts above
	ByCategory  map[string]int `json:"byCategory,omitempty"`  // category → count
	TopFindings []TopFinding   `json:"topFindings,omitempty"` // ≤20, severity-sorted
}

type TopFinding struct {
	Category    string `json:"category"`
	Severity    string `json:"severity"`
	Title       string `json:"title"`
	Remediation string `json:"remediation,omitempty"`
}

type Status struct {
	ObservedServerVersion string         `json:"observedServerVersion,omitempty"`
	KBVersion             string         `json:"kbVersion,omitempty"`
	LastEvaluated         metav1.Time    `json:"lastEvaluated,omitempty"`
	Targets               []TargetStatus `json:"targets,omitempty"`
	NotAssessed           []string       `json:"notAssessed,omitempty"` // "helm: secrets list forbidden"
	AgentVersion          string         `json:"agentVersion,omitempty"`
	// AddOnEvidenceAgeSeconds is engine.Report.AddOnEvidenceAgeSeconds: how
	// old the pod evidence the add-ons were detected from was when the agent
	// reused its last full pod pass (#228); absent when every pod was read.
	AddOnEvidenceAgeSeconds int64 `json:"addOnEvidenceAgeSeconds,omitempty"`
	// SupportPhase, ExtendedSupportFrom, ExtendedSupportEnds,
	// AnnualCostDelta, Currency, PriceAsOf, AnnualCostNote and
	// ExtendedSupportCondition are the engine's Report.Support for a cluster on EKS, GKE
	// or AKS whose Kubernetes minor the provider dataset dates: where
	// the minor stands in the provider's support calendar (standard,
	// ending, extended, ended), the day extended support begins
	// (YYYY-MM-DD) and, where the provider offers any, the day it ends, and, only where the provider's price is cited, what
	// extended support adds per cluster per year at list price and the day
	// that price was read. Phases ending and extended are the provider's
	// calendar: where extended support is opt-in (GKE, AKS) it applies to
	// this cluster only if ExtendedSupportCondition holds, and
	// AnnualCostNote says whom the price is charged to. They are the
	// cluster's, the same for every target, and absent otherwise.
	SupportPhase        string `json:"supportPhase,omitempty"`
	ExtendedSupportFrom string `json:"extendedSupportFrom,omitempty"`
	ExtendedSupportEnds string `json:"extendedSupportEnds,omitempty"`
	AnnualCostDelta     string `json:"annualCostDelta,omitempty"`
	Currency            string `json:"currency,omitempty"`
	PriceAsOf           string `json:"priceAsOf,omitempty"`
	// AnnualCostNote and ExtendedSupportCondition are the provider's
	// caveats, carried so a status reader does not take the figure or the
	// phase for a fact about this cluster's configuration.
	AnnualCostNote           string `json:"annualCostNote,omitempty"`
	ExtendedSupportCondition string `json:"extendedSupportCondition,omitempty"`
	// ObservedGeneration is the metadata.generation whose spec was
	// evaluated (WriteStatus stamps the current one when it is zero), and
	// Conditions carry the Ready condition (ReadyCondition): the standard
	// shape Argo CD, kstatus and `kubectl wait` read.
	ObservedGeneration int64              `json:"observedGeneration,omitempty"`
	Conditions         []metav1.Condition `json:"conditions,omitempty"`

	// ReadyGaps labels the first target's gaps (engine.CapabilityGap.Label,
	// required ones marked) for the Ready condition message. Not stored:
	// whether a gap is required depends on the target, and NotAssessed is
	// the union over all targets.
	ReadyGaps []string `json:"-"`
}

// ConditionReady is the condition type summarizing the first target's
// verdict; its reasons map the three verdicts.
const (
	ConditionReady    = "Ready"
	ReasonReady       = "Ready"       // True: verdict ready
	ReasonBlocked     = "Blocked"     // False: at least one blocker
	ReasonNotAssessed = "NotAssessed" // Unknown: verdict unknown, or no target evaluated
)

// ReadyCondition derives the Ready condition from the first target's
// verdict (the one the Ready printer column shows). LastTransitionTime is
// the evaluation time; WriteStatus keeps the stored one while the
// condition's status does not change.
func ReadyCondition(st Status) metav1.Condition {
	c := metav1.Condition{Type: ConditionReady, LastTransitionTime: st.LastEvaluated}
	if len(st.Targets) == 0 {
		c.Status, c.Reason = metav1.ConditionUnknown, ReasonNotAssessed
		c.Message = "no target evaluated"
		if len(st.NotAssessed) > 0 {
			c.Message += ": " + strings.Join(st.NotAssessed, "; ")
		}
		return c
	}
	t := st.Targets[0]
	// A ready or blocked verdict still names what it did not cover.
	notCovered := ""
	if len(st.ReadyGaps) > 0 {
		notCovered = "; not fully assessed: " + strings.Join(st.ReadyGaps, ", ") + "; see status.notAssessed"
	}
	switch {
	case t.Verdict == string(engine.VerdictReady) || (t.Verdict == "" && t.Ready):
		c.Status, c.Reason = metav1.ConditionTrue, ReasonReady
		c.Message = fmt.Sprintf("%s: ready (score %d)%s", t.Target, t.Score, notCovered)
	case t.Verdict == string(engine.VerdictBlocked) || t.Blockers > 0:
		c.Status, c.Reason = metav1.ConditionFalse, ReasonBlocked
		c.Message = fmt.Sprintf("%s: %d blocker(s) (score %d)%s", t.Target, t.Blockers, t.Score, notCovered)
	default:
		c.Status, c.Reason = metav1.ConditionUnknown, ReasonNotAssessed
		which := ""
		if len(st.ReadyGaps) > 0 {
			which = ": " + strings.Join(st.ReadyGaps, ", ")
		}
		c.Message = fmt.Sprintf("%s: no blockers found, but a required check was not assessed%s; see status.notAssessed (score %d)",
			t.Target, which, t.Score)
	}
	return c
}

// The status is bounded by construction, so a status write can never grow
// past the apiserver's request limit (3 MiB) and wedge (issue #191). Four
// numbers hold it: MaxTargets rows, maxTopFindings findings per row, each
// title and remediation clipped to a budget of encoded bytes (see clip), and
// a capped notAssessed. At the limits that is 8 × 20 × (512 + 1024) bytes of
// finding text plus 32 × 512 of notes: ~280 KiB whatever the characters are,
// because the budgets count what the text costs in the stored JSON, not
// characters (counted in characters, a title of '<' costs six bytes each and
// the worst case is 1.5 MiB). The full finding list lives in the server and
// CLI.
const (
	// MaxTargets is the most targets a ClusterReadiness evaluates, and the
	// maxItems of spec.targets in manifest.yaml (a test holds the two
	// together). Each target costs a status row, and 8 is twice the
	// server's --targets cap of 4: room for the two or three minors an
	// upgrade plan spans, and for a CI matrix, without letting a CR grow
	// the status without limit: the red-team measured 1.88 MB at 500
	// targets and a rejected write at 1000.
	MaxTargets = 8

	maxTopFindings    = 20
	maxTitleLen       = 512  // encoded bytes; a title that names objects can pass it
	maxRemediationLen = 1024 // encoded bytes
	maxNoteLen        = 512  // encoded bytes, per notAssessed entry
	maxNotAssessed    = 32   // entries; one summary line follows if more
)

// jsonCost is what r takes up in a JSON string as the apiserver stores it,
// never less: Go's encoder writes < > & and U+2028/9 as \uXXXX, control
// characters as at most six bytes, and bytes that are not UTF-8 as �.
func jsonCost(r rune) int {
	switch {
	case r == '"' || r == '\\':
		return 2
	case r < 0x20, r == '<', r == '>', r == '&', r == ' ', r == ' ', r == utf8.RuneError:
		return 6
	}
	return utf8.RuneLen(r)
}

// clip cuts s to at most n bytes of JSON encoding (see jsonCost), on a rune
// boundary, marking the cut with an ellipsis (3 bytes more).
func clip(s string, n int) string {
	if len(s)*6 <= n { // even all six-byte characters fit
		return s
	}
	cost := 0
	for pos, r := range s {
		cost += jsonCost(r)
		if cost > n {
			return s[:pos] + "…"
		}
	}
	return s
}

// boundNotAssessed returns notes with each entry clipped and at most
// maxNotAssessed of them; a summary line counts the ones left out.
func boundNotAssessed(notes []string) []string {
	if len(notes) == 0 {
		return nil
	}
	out := make([]string, 0, min(len(notes), maxNotAssessed+1))
	for i, n := range notes {
		if i == maxNotAssessed {
			out = append(out, fmt.Sprintf("… and %d more not listed", len(notes)-maxNotAssessed))
			break
		}
		out = append(out, clip(n, maxNoteLen))
	}
	return out
}

// TargetStatusFromReport summarizes one engine.Report for CRD status:
// severity/category counts over all findings, plus the first maxTopFindings
// findings (Report.Findings is already severity-sorted per engine contract).
func TargetStatusFromReport(r engine.Report) TargetStatus {
	ts := TargetStatus{Target: r.Target.String(), Score: r.Score, Ready: r.Ready, Verdict: string(r.Verdict), Suppressed: len(r.Suppressed)}
	for _, f := range r.Findings {
		switch f.Severity {
		case engine.SevBlocker:
			ts.Blockers++
		case engine.SevWarning:
			ts.Warnings++
		case engine.SevInfo:
			ts.Infos++
		}
		if ts.ByCategory == nil {
			ts.ByCategory = make(map[string]int)
		}
		ts.ByCategory[string(f.Category)]++
		if len(ts.TopFindings) < maxTopFindings {
			ts.TopFindings = append(ts.TopFindings, TopFinding{
				Category:    string(f.Category),
				Severity:    string(f.Severity),
				Title:       clip(f.Title, maxTitleLen),
				Remediation: clip(f.Remediation, maxRemediationLen),
			})
		}
	}
	return ts
}

// StatusFromReports builds the full CRD status from per-target reports.
// All reports come from the same inventory, so KBVersion is taken from the
// first report. NotAssessed is the deduped union over all reports, in first-
// seen order — most gaps are per-inventory, but kb-coverage is per-target.
// Gaps render as "capability: reason", a partial one as "capability
// (partial): reason; skipped: a, b". Whether a gap is required depends on
// the target, so only the Ready condition, which reads the first target,
// says that.
func StatusFromReports(reports []engine.Report, observedServerVersion, agentVersion string, now time.Time) Status {
	st := Status{
		ObservedServerVersion: observedServerVersion,
		LastEvaluated:         metav1.NewTime(now.UTC()),
		AgentVersion:          agentVersion,
	}
	seen := map[string]bool{}
	for i, r := range reports {
		st.Targets = append(st.Targets, TargetStatusFromReport(r))
		for _, g := range r.NotAssessed {
			if i == 0 {
				st.ReadyGaps = append(st.ReadyGaps, g.Label())
			}
			s := fmt.Sprintf("%s: %s", g.Capability, g.Reason)
			if g.Partial {
				s = fmt.Sprintf("%s (partial): %s", g.Capability, g.Reason)
			}
			if len(g.Skipped) > 0 {
				s += "; skipped: " + strings.Join(g.Skipped, ", ")
			}
			if !seen[s] {
				seen[s] = true
				st.NotAssessed = append(st.NotAssessed, s)
			}
		}
	}
	if len(reports) > 0 {
		st.KBVersion = reports[0].KBVersion
		st.AddOnEvidenceAgeSeconds = reports[0].AddOnEvidenceAgeSeconds
		if s := reports[0].Support; s != nil {
			st.SupportPhase, st.ExtendedSupportFrom, st.ExtendedSupportEnds = string(s.Phase), s.ExtendedSupportFrom, s.ExtendedSupportEnds
			st.AnnualCostDelta, st.Currency, st.PriceAsOf = s.AnnualCostDelta, s.Currency, s.PriceAsOf
			st.AnnualCostNote, st.ExtendedSupportCondition = s.AnnualCostNote, s.ExtendedSupportCondition
		}
	}
	return st
}
