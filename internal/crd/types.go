package crd

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
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
