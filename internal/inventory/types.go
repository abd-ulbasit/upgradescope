package inventory

import "time"

type Capability string

const (
	CapAPIUsage        Capability = "api-usage"
	CapDeprecatedCalls Capability = "deprecated-calls"
	CapHelm            Capability = "helm"
	CapAddOns          Capability = "addons"
	CapVersions        Capability = "versions"
)

type CapabilityStatus struct {
	Available bool   `json:"available"`
	Reason    string `json:"reason,omitempty"` // e.g. `nodes list forbidden`
}

type Inventory struct {
	SchemaVersion      int                             `json:"schemaVersion"` // 1
	ClusterID          string                          `json:"clusterId"`     // kube-system ns UID, or "files"
	CollectedAt        time.Time                       `json:"collectedAt"`
	ServerVersion      string                          `json:"serverVersion,omitempty"` // raw, e.g. "v1.34.2"
	Capabilities       map[Capability]CapabilityStatus `json:"capabilities"`
	APIUsage           []APIUsage                      `json:"apiUsage,omitempty"`
	DeprecatedCalls    []DeprecatedCall                `json:"deprecatedCalls,omitempty"`
	HelmReleases       []HelmRelease                   `json:"helmReleases,omitempty"`
	AddOns             []AddOnInstance                 `json:"addOns,omitempty"`
	Nodes              []NodeInfo                      `json:"nodes,omitempty"`
	ControlPlane       []ComponentVersion              `json:"controlPlane,omitempty"`
	Namespaces         []NamespaceInfo                 `json:"namespaces,omitempty"`
	UnrecognizedImages []string                        `json:"unrecognizedImages,omitempty"` // deduped, sorted, cap 200
}

type APIUsage struct {
	Group      string         `json:"group"` // "" for core
	Version    string         `json:"version"`
	Kind       string         `json:"kind"`
	Count      int            `json:"count"`
	Namespaces map[string]int `json:"namespaces,omitempty"` // ns → count; cluster-scoped key ""
}

type DeprecatedCall struct { // one row of apiserver_requested_deprecated_apis
	Group          string `json:"group"`
	Version        string `json:"version"`
	Resource       string `json:"resource"`
	Subresource    string `json:"subresource,omitempty"`
	RemovedRelease string `json:"removedRelease,omitempty"` // "1.32" (may be empty)
}

type HelmRelease struct {
	Name         string `json:"name"`
	Namespace    string `json:"namespace"`
	ChartName    string `json:"chartName"`
	ChartVersion string `json:"chartVersion"`
	AppVersion   string `json:"appVersion,omitempty"`
	Status       string `json:"status"` // deployed, superseded, failed…
}

type AddOnInstance struct {
	ID         string   `json:"id"`      // registry id, e.g. "ingress-nginx"
	Version    string   `json:"version"` // semver as detected, may be ""
	Namespaces []string `json:"namespaces"`
	Source     string   `json:"source"` // "image" | "chart"
}

// ComponentVersion is one observed control-plane component version,
// detected from kube-system pod image tags (kube-apiserver,
// kube-controller-manager, kube-scheduler, kube-proxy). The list is
// (Component, Version)-deduped and sorted; managed control planes
// (EKS/GKE) expose no such pods, so the slice is empty there.
type ComponentVersion struct {
	Component string `json:"component"` // e.g. "kube-apiserver"
	Version   string `json:"version"`   // raw image tag, e.g. "v1.34.2"; always ParseVersion-able
}

type NodeInfo struct {
	Name           string `json:"name"`
	KubeletVersion string `json:"kubeletVersion"` // raw "v1.33.1"
}

type NamespaceInfo struct {
	Name string `json:"name"`
	Team string `json:"team,omitempty"` // from --team-label (default "team")
}
