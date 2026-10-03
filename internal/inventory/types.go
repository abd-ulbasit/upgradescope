package inventory

import (
	"time"

	"k8s.io/apimachinery/pkg/version"
)

type Capability string

const (
	CapAPIUsage        Capability = "api-usage"
	CapDeprecatedCalls Capability = "deprecated-calls"
	CapHelm            Capability = "helm"
	CapAddOns          Capability = "addons"
	CapVersions        Capability = "versions"
	// CapCRDs: CustomResourceDefinitions' versions, and the custom
	// resources that use one the CRD deprecates or does not serve.
	// Inventories from collectors that predate it do not report it.
	CapCRDs Capability = "crds"
)

type CapabilityStatus struct {
	Available bool   `json:"available"`
	Reason    string `json:"reason,omitempty"` // e.g. `nodes list forbidden`
	// Partial marks an available capability that could not read all it
	// covers: one forbidden resource among many, an API group whose
	// discovery failed, a Helm release that could not be read. Its data is
	// incomplete; Reason says what failed. An available capability with a
	// Reason but not Partial (helm's per-driver release counts) is complete.
	Partial bool `json:"partial,omitempty"`
	// Skipped names what a Partial capability did not read, sorted, in the
	// capability's own terms:
	//   - api-usage: the flagged APIs whose objects went unchecked,
	//     "group/version Kind" ("policy/v1beta1 PodSecurityPolicy"; core
	//     renders as "v1 Endpoints");
	//   - deprecated-calls: the resources the scanner lists itself at a
	//     deprecated version, "group/version resource", whose metric rows
	//     cannot be told apart from its own requests;
	//   - helm: storage drivers not read ("configmaps") and releases not
	//     read, not decodable or whose manifest was not fully parsed
	//     ("namespace/name");
	//   - versions: the control-plane components with a kube-system pod
	//     whose version could not be read where upstream would have told
	//     it ("kube-proxy", "kube-scheduler"): an upstream-named image
	//     with no version tag, or any unread kube-apiserver,
	//     kube-controller-manager or kube-scheduler pod. A kube-proxy pod
	//     on a vendor image of another name (OKE's oke-public-kube-proxy)
	//     is named in Reason only, so Skipped may be empty;
	//   - addons: resources not read for add-on evidence,
	//     "group/version resource" ("networking.k8s.io/v1 ingressclasses");
	//   - crds: the custom resources not checked for use of a deprecated
	//     or unserved CRD version, "group/version Kind"
	//     ("cert-manager.io/v1alpha2 Certificate").
	// May be empty when nothing nameable was skipped (a discovery failure
	// in a group without flagged APIs).
	Skipped []string `json:"skipped,omitempty"`
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
	UnrecognizedImages []string                        `json:"unrecognizedImages,omitempty"` // repos ("host/path") no image matcher claims; deduped, sorted, cap MaxUnrecognizedImages

	// UnrecognizedImagesOmitted counts the UnrecognizedImages the cap
	// dropped. Both are add-on detection gaps, never findings.
	UnrecognizedImagesOmitted int `json:"unrecognizedImagesOmitted,omitempty"`

	CRDs []CRD `json:"crds,omitempty"` // sorted by Group, then Kind
}

// CRD is one CustomResourceDefinition (apiextensions.k8s.io/v1): the
// versions it serves and stores, and the custom resources that use a
// version it deprecates, does not serve, or no longer lists.
type CRD struct {
	Group    string       `json:"group"`
	Kind     string       `json:"kind"`
	Plural   string       `json:"plural"`
	Versions []CRDVersion `json:"versions"` // spec.versions, in spec order
	// StoredVersions is status.storedVersions: every version objects may
	// have been persisted at since the CRD was created. Empty in files
	// mode, where a manifest's status says nothing about a cluster.
	StoredVersions []string `json:"storedVersions,omitempty"`
	// Usage counts, per version and as Inventory.APIUsage does per GVK
	// (with this CRD's Group and Kind), the custom resources that use a
	// version that is deprecated, not served, or missing from Versions:
	// live objects some manager still writes through it (ObjectRef.Manager
	// names it), manifest objects at it. Sorted by Version.
	Usage []APIUsage `json:"usage,omitempty"`
}

// PreferredVersion is the version to read and write a CRD's custom
// resources at: the storage version when it is served and not deprecated,
// else the highest-priority such version; "" when there is none. The
// collector lists custom resources at it, and the engine names it as the
// version to move them to.
func (c CRD) PreferredVersion() string {
	best := ""
	for _, v := range c.Versions {
		if !v.Served || v.Deprecated {
			continue
		}
		if v.Storage {
			return v.Name
		}
		if best == "" || version.CompareKubeAwareVersionStrings(v.Name, best) > 0 {
			best = v.Name
		}
	}
	return best
}

// CRDVersion is one entry of a CRD's spec.versions.
type CRDVersion struct {
	Name               string `json:"name"`
	Served             bool   `json:"served"`
	Storage            bool   `json:"storage"`
	Deprecated         bool   `json:"deprecated,omitempty"`
	DeprecationWarning string `json:"deprecationWarning,omitempty"`
}

// MaxUnrecognizedImages caps Inventory.UnrecognizedImages, so a cluster
// running thousands of distinct images cannot bloat the inventory or the
// report.
const MaxUnrecognizedImages = 200

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

// AddOnInstance is one install of a registry add-on. Collectors emit one
// per add-on and namespace (two where a Helm release's pods run images on
// another release line than its appVersion), so an ID can appear several
// times, each with its own version; inventories from agents that predate that carry one
// merged entry per ID (the oldest version, every namespace).
type AddOnInstance struct {
	ID string `json:"id"` // registry id, e.g. "ingress-nginx"
	// Version is the app version (a Helm release's appVersion, else the
	// image tag's or app.kubernetes.io/version label's), normalised; may be "".
	Version string `json:"version"`
	// ChartVersion is the Helm chart version, kept as evidence when the
	// add-on was found through a release; registry data never uses it.
	ChartVersion string `json:"chartVersion,omitempty"`
	// Namespaces is empty for an install known only from a cluster-scoped
	// IngressClass.
	Namespaces []string `json:"namespaces"`
	// Source is the strongest evidence found: "chart" (a Helm release),
	// "image" (an image matcher), "labels" (pod labels naming the add-on)
	// or "ingressclass" (an IngressClass controller; no version).
	Source string `json:"source"`
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
