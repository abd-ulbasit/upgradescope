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

// Source records how an inventory was collected. It decides which
// capabilities the engine requires before it can call a cluster ready.
type Source string

const (
	// SourceCluster: collected from a live cluster (scan, agent). The empty
	// Source means the same — v0.1 agents push inventories without it.
	SourceCluster Source = "cluster"
	// SourceFiles: built from rendered manifests (scan --files, the server's
	// manifest gate). There is no cluster, so no versions to collect.
	SourceFiles Source = "files"
)

type Inventory struct {
	SchemaVersion      int                             `json:"schemaVersion"` // 1
	ClusterID          string                          `json:"clusterId"`     // kube-system ns UID, or "files"
	Source             Source                          `json:"source,omitempty"`
	CollectedAt        time.Time                       `json:"collectedAt"`
	ServerVersion      string                          `json:"serverVersion,omitempty"` // raw GitVersion, e.g. "v1.34.2", "v1.34.2-gke.100"
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
	// Objects identifies the objects behind Count, in collection order,
	// capped at MaxObjectRefs; ObjectsOmitted counts the refs dropped by
	// the cap. Collectors that cannot identify objects leave both empty.
	Objects        []ObjectRef `json:"objects,omitempty"`
	ObjectsOmitted int         `json:"objectsOmitted,omitempty"`
}

// MaxObjectRefs caps APIUsage.Objects so a render with thousands of objects
// of one kind cannot bloat the inventory, the report or SARIF output.
const MaxObjectRefs = 100

// ObjectRef identifies one object using an API. Namespace/Name are the
// object's identity (empty Namespace: metadata.namespace unset, or a
// cluster-scoped object). File and Line are set only for objects read from
// manifest text: File is slash-separated and relative to the scanned root
// (empty for a single posted stream); Line is the 1-based line of the
// object's apiVersion key. RenderedFrom is the Helm template the object was
// rendered from, taken from helm template's "# Source: <chart>/templates/x.yaml"
// comment. Manager is set only for live objects flagged because they were
// written through the deprecated group/version: the metadata.managedFields
// manager that wrote it, or "kubectl last-applied" when only the
// kubectl.kubernetes.io/last-applied-configuration annotation names it.
// Ignore and IgnoreReason are the object's upgradescope.dev/ignore and
// upgradescope.dev/ignore-reason annotation values, verbatim (see
// internal/suppress).
type ObjectRef struct {
	Namespace    string `json:"namespace,omitempty"`
	Name         string `json:"name,omitempty"`
	File         string `json:"file,omitempty"`
	Line         int    `json:"line,omitempty"`
	RenderedFrom string `json:"renderedFrom,omitempty"`
	Manager      string `json:"manager,omitempty"`
	Ignore       string `json:"ignore,omitempty"`
	IgnoreReason string `json:"ignoreReason,omitempty"`
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
	// KubeVersion is the chart's Chart.yaml kubeVersion constraint, verbatim
	// (e.g. ">=1.21.0-0 <1.33.0-0"); "" when the chart declares none.
	KubeVersion string `json:"kubeVersion,omitempty"`
	// Status and Revision identify the revision everything else was read
	// from: the installed one, which is the newest revision unless that one
	// failed (then the newest deployed or superseded revision before it).
	// A release with nothing installed (uninstalled with --keep-history, or
	// only failed revisions) is not listed. Revision is 0 in inventories
	// from agents that predate it, whose Status is the newest revision's.
	Status   string `json:"status"` // deployed, superseded, pending-upgrade…
	Revision int    `json:"revision,omitempty"`
	// ManifestAPIs are the objects in that revision's stored manifest at a
	// group/version/kind the collector's KB flags as deprecated or removed,
	// per GVK as in Inventory.APIUsage. Refs carry the object's line in the
	// manifest (no File) and its template (RenderedFrom); Namespace is
	// empty where the chart leaves metadata.namespace unset.
	ManifestAPIs []APIUsage `json:"manifestApis,omitempty"`
}

type AddOnInstance struct {
	ID string `json:"id"` // registry id, e.g. "ingress-nginx"
	// Version is the app version (a Helm release's appVersion, else the
	// image tag's version), normalised; may be "".
	Version string `json:"version"`
	// ChartVersion is the Helm chart version, kept as evidence when the
	// add-on was found through a release; registry data never uses it.
	ChartVersion string   `json:"chartVersion,omitempty"`
	Namespaces   []string `json:"namespaces"`
	Source       string   `json:"source"` // "image" | "chart"
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
	KubeletVersion string `json:"kubeletVersion"` // raw, e.g. "v1.33.1", "v1.33.1-eks-aeac579"
	// ContainerRuntime is status.nodeInfo.containerRuntimeVersion, raw:
	// "<runtime>://<version>", e.g. "containerd://1.7.27".
	ContainerRuntime string `json:"containerRuntime,omitempty"`
}

type NamespaceInfo struct {
	Name string `json:"name"`
	Team string `json:"team,omitempty"` // from --team-label (default "team")
}
