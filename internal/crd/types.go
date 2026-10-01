package crd

import (
	"fmt"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/abd-ulbasit/upgradescope/internal/engine"
)

const (
	Group    = "upgradescope.dev"
	Version  = "v1alpha1"
	Kind     = "ClusterReadiness"
	Plural   = "clusterreadinesses"
	Singular = "clusterreadiness"
	// DefaultName is the conventional singleton object name.
	DefaultName = "cluster"
)

// GVR is the dynamic-client resource identifier for ClusterReadiness.
func GVR() schema.GroupVersionResource {
	return schema.GroupVersionResource{Group: Group, Version: Version, Resource: Plural}
}

type Spec struct {
	// Targets are Kubernetes minor versions to evaluate against, e.g. ["1.36","1.37"].
	// Empty → agent defaults to next minor above the observed server version.
	Targets []string `json:"targets,omitempty"`
}

type TargetStatus struct {
	Target      string         `json:"target"` // "1.36"
	Score       int            `json:"score"`
	Ready       bool           `json:"ready"`             // == (Verdict == "ready"); kept for v0.1 readers
	Verdict     string         `json:"verdict,omitempty"` // "ready" | "blocked" | "unknown" (engine.Verdict)
	Blockers    int            `json:"blockers"`
	Warnings    int            `json:"warnings"`
	Infos       int            `json:"infos"`
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
	// ObservedGeneration and Conditions are stamped by WriteStatus: the
	// object's metadata.generation and the Ready condition (ReadyCondition),
	// the standard shape Argo CD, kstatus and `kubectl wait` read.
	ObservedGeneration int64              `json:"observedGeneration,omitempty"`
	Conditions         []metav1.Condition `json:"conditions,omitempty"`
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
	switch {
	case t.Verdict == string(engine.VerdictReady) || (t.Verdict == "" && t.Ready):
		c.Status, c.Reason = metav1.ConditionTrue, ReasonReady
		c.Message = fmt.Sprintf("%s: ready (score %d)", t.Target, t.Score)
	case t.Verdict == string(engine.VerdictBlocked) || t.Blockers > 0:
		c.Status, c.Reason = metav1.ConditionFalse, ReasonBlocked
		c.Message = fmt.Sprintf("%s: %d blocker(s) (score %d)", t.Target, t.Blockers, t.Score)
	default:
		c.Status, c.Reason = metav1.ConditionUnknown, ReasonNotAssessed
		c.Message = fmt.Sprintf("%s: no blockers found, but a required check was not assessed; see status.notAssessed (score %d)",
			t.Target, t.Score)
	}
	return c
}

// maxTopFindings bounds CRD status size; the full list lives in server/CLI.
const maxTopFindings = 20

// TargetStatusFromReport summarizes one engine.Report for CRD status:
// severity/category counts over all findings, plus the first maxTopFindings
// findings (Report.Findings is already severity-sorted per engine contract).
func TargetStatusFromReport(r engine.Report) TargetStatus {
	ts := TargetStatus{Target: r.Target.String(), Score: r.Score, Ready: r.Ready, Verdict: string(r.Verdict)}
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
				Title:       f.Title,
				Remediation: f.Remediation,
			})
		}
	}
	return ts
}

// StatusFromReports builds the full CRD status from per-target reports.
// All reports come from the same inventory, so KBVersion is taken from the
// first report. NotAssessed is the deduped union over all reports, in first-
// seen order — most gaps are per-inventory, but kb-coverage is per-target.
// Gaps render as "capability: reason".
func StatusFromReports(reports []engine.Report, observedServerVersion, agentVersion string, now time.Time) Status {
	st := Status{
		ObservedServerVersion: observedServerVersion,
		LastEvaluated:         metav1.NewTime(now.UTC()),
		AgentVersion:          agentVersion,
	}
	seen := map[string]bool{}
	for _, r := range reports {
		st.Targets = append(st.Targets, TargetStatusFromReport(r))
		for _, g := range r.NotAssessed {
			if s := fmt.Sprintf("%s: %s", g.Capability, g.Reason); !seen[s] {
				seen[s] = true
				st.NotAssessed = append(st.NotAssessed, s)
			}
		}
	}
	if len(reports) > 0 {
		st.KBVersion = reports[0].KBVersion
	}
	return st
}
