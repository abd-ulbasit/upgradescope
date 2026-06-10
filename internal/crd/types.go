package crd

import (
	"fmt"
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
	Ready       bool           `json:"ready"`
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
}

// maxTopFindings bounds CRD status size; the full list lives in server/CLI.
const maxTopFindings = 20

// TargetStatusFromReport summarizes one engine.Report for CRD status:
// severity/category counts over all findings, plus the first maxTopFindings
// findings (Report.Findings is already severity-sorted per engine contract).
func TargetStatusFromReport(r engine.Report) TargetStatus {
	ts := TargetStatus{Target: r.Target.String(), Score: r.Score, Ready: r.Ready}
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
// All reports come from the same inventory, so KBVersion and NotAssessed are
// taken from the first report. NotAssessed gaps render as "capability: reason".
func StatusFromReports(reports []engine.Report, observedServerVersion, agentVersion string, now time.Time) Status {
	st := Status{
		ObservedServerVersion: observedServerVersion,
		LastEvaluated:         metav1.NewTime(now.UTC()),
		AgentVersion:          agentVersion,
	}
	for _, r := range reports {
		st.Targets = append(st.Targets, TargetStatusFromReport(r))
	}
	if len(reports) > 0 {
		st.KBVersion = reports[0].KBVersion
		for _, g := range reports[0].NotAssessed {
			st.NotAssessed = append(st.NotAssessed, fmt.Sprintf("%s: %s", g.Capability, g.Reason))
		}
	}
	return st
}
