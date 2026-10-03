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

// SkippedNewerKB is the Skipped entry of api-usage, helm and addons that
// the server (never a collector) adds when a snapshot was collected with a
// knowledge base other than its own: the agent listed API usage and matched
// add-ons by its own data, so evidence the server's data would have
// collected is missing. The engine reads it as a required gap for api-usage
// and addons, so the verdict is unknown.
const SkippedNewerKB = "knowledge base differs from the agent's"

// SkippedPods is the addons capability's Skipped entry for a cluster-wide
// pod list that failed: pod images and labels find an add-on whatever
// installed it, so the engine keeps that gap required.
const SkippedPods = "v1 pods"

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
	//     read, not decodable, left out for a name or chart metadata the
	//     server would refuse, or whose manifest was not fully parsed
	//     ("namespace/name", an invalid name quoted and cut: see
	//     Inventory.Conform); GitOps tools that deploy charts without
	//     leaving a Helm release the scan can read, or whose custom
	//     resources it could not read (GitOpsArgoCD, GitOpsFlux);
	//   - versions: the control-plane components with a kube-system pod
	//     whose version could not be read where upstream would have told
	//     it ("kube-proxy", "kube-scheduler"): an upstream-named image
	//     with no version tag, or any unread kube-apiserver,
	//     kube-controller-manager or kube-scheduler pod. A kube-proxy pod
	//     on a vendor image of another name (OKE's oke-public-kube-proxy)
	//     is named in Reason only, so Skipped may be empty. "nodes"
	//     means the Node list was empty, so kubelet skew and node
	//     runtimes were not assessed (#174);
	//   - addons: resources not read for add-on evidence,
	//     "group/version resource" ("networking.k8s.io/v1 ingressclasses",
	//     "v1 pods", SkippedPods);
	//   - api-usage, helm and addons, as the server adds them to a snapshot
	//     collected with another knowledge base than its own:
	//     SkippedNewerKB;
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

// CurrentCollectorSchema is the generation of field meanings this
// package's collectors fill an Inventory with, stamped as
// Inventory.CollectorSchema. 1 is v0.2.0's: api-usage counts the objects
// written through a deprecated version and names them in Objects, and a
// chart-found add-on's Version is its app version. Bump it when a field's
// meaning changes without a schemaVersion bump.
const CurrentCollectorSchema = 1

// Provider is the managed Kubernetes service a cluster's control plane is
// bought from, inferred by the collector from signals only that service
// produces (see collect.providerEvidence). It is never guessed: a cluster
// that does not show one of the three is ProviderOther, and one whose
// evidence could not be read is left empty (undetermined), not ProviderOther.
type Provider string

const (
	ProviderEKS   Provider = "eks"
	ProviderGKE   Provider = "gke"
	ProviderAKS   Provider = "aks"
	ProviderOther Provider = "other"
)

type Inventory struct {
	SchemaVersion int    `json:"schemaVersion"` // 1
	ClusterID     string `json:"clusterId"`     // kube-system ns UID, or "files"
	// CollectorSchema is CurrentCollectorSchema in every inventory this
	// package collects. It is absent (0) from those of collectors that
	// predate it — v0.1.x and v0.2.0's release candidates — which the
	// server tells apart by the pushing agent's version.
	CollectorSchema int       `json:"collectorSchema,omitempty"`
	Source          Source    `json:"source,omitempty"`
	CollectedAt     time.Time `json:"collectedAt"`
	ServerVersion   string    `json:"serverVersion,omitempty"` // raw GitVersion, e.g. "v1.34.2", "v1.34.2-gke.100"
	// Provider is eks, gke, aks, or other when the cluster shows none of
	// them; empty when it was not determined: a files inventory, one from
	// a collector that predates the field, or a live cluster whose nodes
	// could not be listed and whose server version names no provider.
	Provider           Provider                        `json:"provider,omitempty"`
	Capabilities       map[Capability]CapabilityStatus `json:"capabilities"`
	APIUsage           []APIUsage                      `json:"apiUsage,omitempty"`
	DeprecatedCalls    []DeprecatedCall                `json:"deprecatedCalls,omitempty"`
	HelmReleases       []HelmRelease                   `json:"helmReleases,omitempty"`
	GitOpsCharts       []GitOpsChart                   `json:"gitopsCharts,omitempty"`
	AddOns             []AddOnInstance                 `json:"addOns,omitempty"`
	Nodes              []NodeInfo                      `json:"nodes,omitempty"`
	ControlPlane       []ComponentVersion              `json:"controlPlane,omitempty"`
	Namespaces         []NamespaceInfo                 `json:"namespaces,omitempty"`
	UnrecognizedImages []string                        `json:"unrecognizedImages,omitempty"` // repos ("host/path") no image matcher claims; deduped, sorted, cap MaxUnrecognizedImages

	// UnrecognizedImagesOmitted counts the UnrecognizedImages the cap
	// dropped. Both are add-on detection gaps, never findings.
	UnrecognizedImagesOmitted int `json:"unrecognizedImagesOmitted,omitempty"`

	CRDs []CRD `json:"crds,omitempty"` // sorted by Group, then Kind

	// APIAuthorshipUnknown holds the objects of a flagged kind that nothing
	// can be attributed to (live clusters only): no managedFields entry
	// outside the status subresource and the control plane's managers, and
	// no usable last-applied annotation, as an object created through a
	// deprecated version with an empty spec has. Shaped like APIUsage, once
	// per kind (under one of its flagged versions). The object may as well
	// have been created through the replacement, so the engine reports it as
	// info and never as use of the deprecated API.
	APIAuthorshipUnknown []APIUsage `json:"apiAuthorshipUnknown,omitempty"`

	// APIServerStartTime is the process_start_time_seconds of the
	// kube-apiserver whose /metrics DeprecatedCalls were read from, in
	// whole seconds: apiserver_requested_deprecated_apis counts requests
	// since then. Zero when the scrape did not report it, and in
	// inventories from collectors that predate it. It describes which
	// apiserver answered, not the cluster, so it is not part of a
	// snapshot's identity: the agent's and the server's canonical hashes
	// leave it out, as they do CollectedAt, and an agent that moves between
	// HA apiservers sends the same snapshot.
	APIServerStartTime time.Time `json:"apiServerStartTime,omitzero"`

	// AddOnEvidenceAgeSeconds is how old the pod evidence the add-ons were
	// detected from was, in whole seconds (at least 1), when a collection
	// reused it: the agent lists the pods outside kube-system only every
	// --pod-pass-every ticks (or --pod-pass-max-age), and in between detects
	// add-ons from the images and labels the last full pass read (#228). An
	// add-on installed or upgraded since is then reported as it was at that
	// pass, for at most this long: a pass of --pod-pass-max-age or more is
	// not reused, so this is below it when the collection began, and the
	// report stays up until the next one. A change in the Helm releases or
	// GitOps charts that name an add-on forces a full pass instead.
	// Absent (0) when every pod was read in this collection, as a scan's
	// always are, and in inventories from collectors that predate the field.
	// The kube-system pods, Helm releases, GitOps resources and IngressClasses
	// are read in every collection whatever this says. It describes how the
	// snapshot was collected, not the cluster, so it is not part of a
	// snapshot's identity: Canonical leaves it out, as it does the times.
	AddOnEvidenceAgeSeconds int64 `json:"addOnEvidenceAgeSeconds,omitempty"`
}

// Canonical returns inv without what describes the collection and not the
// cluster: CollectedAt (it changes every tick), APIServerStartTime (it
// says which apiserver answered the /metrics scrape, and moves with HA
// apiservers) and AddOnEvidenceAgeSeconds (it grows with each tick that
// reuses a pod pass). The agent's and the server's snapshot hashes are
// over this form, so a push that differs only in those is a duplicate.
func (inv Inventory) Canonical() Inventory {
	inv.CollectedAt, inv.APIServerStartTime, inv.AddOnEvidenceAgeSeconds = time.Time{}, time.Time{}, 0
	return inv
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
// Ignore and IgnoreReason are the object's ignore and ignore-reason
// annotation values (apigroup.IgnoreAnnotation, IgnoreReasonAnnotation),
// verbatim (see internal/suppress). IgnoreLegacyKey marks values read
// from the pre-v0.2.0 keys (apigroup.ReadIgnore), for suppress's
// deprecation warning; it never leaves the process (json "-"), so the
// wire format and the report keep no field that v0.3.0 drops.
type ObjectRef struct {
	Namespace    string `json:"namespace,omitempty"`
	Name         string `json:"name,omitempty"`
	File         string `json:"file,omitempty"`
	Line         int    `json:"line,omitempty"`
	RenderedFrom string `json:"renderedFrom,omitempty"`
	Manager      string `json:"manager,omitempty"`
	Ignore       string `json:"ignore,omitempty"`
	IgnoreReason string `json:"ignoreReason,omitempty"`

	IgnoreLegacyKey bool `json:"-"`
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

// The GitOps tools whose chart sources the helm capability reads, as
// GitOpsChart.Tool and a helm capability's Skipped name them.
const (
	GitOpsArgoCD = "argocd"
	GitOpsFlux   = "flux"
)

// GitOpsChart is a Helm chart a GitOps tool deploys into the cluster, read
// from the tool's own custom resource: an Argo CD Application source with
// chart set (or a native OCI source: repoURL oci://..., no chart field), or a Flux HelmRelease. Argo CD renders with helm template and
// leaves no release the Helm collector can read, so this is all that is
// known of its chart: no appVersion, no stored manifest. (Flux's
// helm-controller does leave a release Secret, which the Helm collector
// reads on its own.) Only resources that deploy into the scanned cluster
// are listed.
type GitOpsChart struct {
	Tool string `json:"tool"` // GitOpsArgoCD or GitOpsFlux
	// Name and Namespace are the Application's or HelmRelease's own.
	Name      string `json:"name"`
	Namespace string `json:"namespace,omitempty"`
	// Target is the namespace the chart deploys into: an Application's
	// destination.namespace, a HelmRelease's targetNamespace or else its
	// own namespace; "" when the resource leaves it to the manifests.
	Target string `json:"target,omitempty"`
	Chart  string `json:"chart"`
	// Version is the chart version as the resource spells it, which may be
	// a constraint ("4.*", ">=4.0.0"), a tag, or the digest a Flux
	// OCIRepository pins ("sha256:..."; Flux applies a digest over a semver
	// range over a tag, and so is it recorded); "" when it names none. An
	// Argo CD source's is its targetRevision.
	Version string `json:"version,omitempty"`
	// Repo is an Application's repoURL, a HelmRelease's chart source
	// ("HelmRepository/flux-system/ingress-nginx") or the URL of the
	// OCIRepository it references. A URL is recorded without userinfo,
	// query string or fragment. A token embedded in the path (Cloudsmith's
	// dl.cloudsmith.io/<token>/...) cannot be told from a path and is kept.
	Repo string `json:"repo,omitempty"`
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
	// "image" (an image matcher), "gitops" (a GitOps tool's chart source,
	// which gives no app version), "labels" (pod labels naming the add-on)
	// or "ingressclass" (an IngressClass controller; no version).
	Source string `json:"source"`
}

// ComponentVersion is one observed control-plane component version,
// detected from kube-system pod image tags (kube-apiserver,
// kube-controller-manager, kube-scheduler, kube-proxy). The list is
// (Component, Version)-deduped and sorted, except kube-proxy, which is
// listed once per node it runs on (Node); managed control planes
// (EKS/GKE) expose no such pods, so the slice is empty there.
type ComponentVersion struct {
	Component string `json:"component"` // e.g. "kube-apiserver"
	Version   string `json:"version"`   // raw image tag, e.g. "v1.34.2"; always ParseVersion-able
	// Node is the node a kube-proxy pod runs on (pod.Spec.NodeName), so the
	// engine can pair it with that node's kubelet. Empty for other
	// components, for a pod not yet scheduled, and in inventories from
	// collectors before the field.
	Node string `json:"node,omitempty"`
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
