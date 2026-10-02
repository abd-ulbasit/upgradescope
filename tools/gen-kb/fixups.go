package main

import "strings"

// replacementFixes overrides upstream APILifecycleReplacement tags that are
// missing or name a type that does not exist. A nil value clears the tag.
// Every override is checked by internal/kb's dataset tests, which require
// each replacement to name a GVK in the KB.
var replacementFixes = map[gvkOut]*gvkOut{
	// Deprecation guide, v1.25 "Event": migrate to events.k8s.io/v1.
	// Upstream tags no replacement.
	{Group: "events.k8s.io", Version: "v1beta1", Kind: "Event"}: {Group: "events.k8s.io", Version: "v1", Kind: "Event"},
	// Deprecation guide, v1.25 "RuntimeClass": migrate to node.k8s.io/v1.
	// Upstream tags no replacement.
	{Group: "node.k8s.io", Version: "v1beta1", Kind: "RuntimeClass"}: {Group: "node.k8s.io", Version: "v1", Kind: "RuntimeClass"},
}

// fixReplacement corrects e's replacement tag in place: explicit overrides
// first, then a List-kind replacement for a non-List type is normalized to
// the item kind (networking.k8s.io/v1beta1 IngressClass is tagged
// "networking.k8s.io/v1 IngressClassList" upstream).
func fixReplacement(e *entry) {
	if r, ok := replacementFixes[e.gvk()]; ok {
		e.Replacement = nil
		if r != nil {
			c := *r
			e.Replacement = &c
		}
		return
	}
	if r := e.Replacement; r != nil && strings.HasSuffix(r.Kind, "List") && !strings.HasSuffix(e.Kind, "List") {
		e.Replacement = &gvkOut{Group: r.Group, Version: r.Version, Kind: strings.TrimSuffix(r.Kind, "List")}
	}
}

// removalFixes overrides the removal of types k8s.io/api deleted only
// releases after kube-apiserver stopped serving them, so neither the
// deletion nor the upstream tag is the release that removed them. Each
// value is the first release whose kube-apiserver registers no storage for
// the type at that version (pkg/registry/<group>/rest in
// kubernetes/kubernetes, cited per entry). Only a removal earlier than the
// recorded one is applied (fixRemoval), and only to deleted types
// (deletedTypes; TestRemovalFixesAreDeletedTypes).
var removalFixes = map[gvkOut]version{
	// pkg/registry/networking/rest/storage_settings.go: v1.30.0 maps
	// ipaddresses and servicecidrs under v1alpha1, v1.31.0 only under
	// v1beta1. k8s.io/api tagged a 1.33 removal and deleted them in v0.34.
	{Group: "networking.k8s.io", Version: "v1alpha1", Kind: "IPAddress"}:   {Major: 1, Minor: 31},
	{Group: "networking.k8s.io", Version: "v1alpha1", Kind: "ServiceCIDR"}: {Major: 1, Minor: 31},
	// pkg/registry/scheduling/rest/storage_scheduling.go: v1.22.0 serves
	// scheduling.k8s.io/v1alpha1 priorityclasses, v1.23.0 has no v1alpha1
	// storage. k8s.io/api never tagged it and deleted it in v0.36.
	{Group: "scheduling.k8s.io", Version: "v1alpha1", Kind: "PriorityClass"}: {Major: 1, Minor: 23},
}

// untaggedLifecycle is the lifecycle of a type k8s.io/api registers without
// APILifecycle* markers, from the Kubernetes release notes.
type untaggedLifecycle struct {
	introduced  version
	removed     *version // first release whose kube-apiserver does not serve it
	replacement *gvkOut
	citations   []string // release notes or source that state introduced and removed
}

// entry returns u as a dataset entry for k. Removed is no upstream tag, so
// it is marked inferred, as removalFixes' are.
func (u untaggedLifecycle) entry(k gvkOut) entry {
	e := entry{Group: k.Group, Version: k.Version, Kind: k.Kind, Introduced: u.introduced}
	if u.removed != nil {
		r := *u.removed
		e.Removed, e.RemovedInferred = &r, true
	}
	if u.replacement != nil {
		r := *u.replacement
		e.Replacement = &r
	}
	return e
}

var (
	rbacV1alpha1Cites = []string{
		"https://github.com/kubernetes/kubernetes/blob/master/CHANGELOG/CHANGELOG-1.3.md",  // "Alpha RBAC authorization API group"
		"https://github.com/kubernetes/kubernetes/blob/master/CHANGELOG/CHANGELOG-1.23.md", // "The rbac.authorization.k8s.io/v1alpha1 API version is removed" (#104248)
	}
	rbacV1alpha1Removed = version{Major: 1, Minor: 23}
)

// untaggedLifecycles gives a lifecycle to types k8s.io/api registers but
// never tagged with APILifecycle* markers, which extract would otherwise
// skip: a manifest using one that kube-apiserver no longer serves scored as
// an unknown-api info, not a blocker (#166). Only add a type whose removal
// is stated in the Kubernetes changelog, and cite it; the test fails once
// upstream tags the type, when the entry must go. Registered, untagged
// types with no entry here (scheduling.k8s.io/v1alpha3 Workload, PodGroup
// and CompositePodGroup, still served; imagepolicy.k8s.io/v1alpha1
// ImageReview, a webhook payload; internal.apiserver.k8s.io/v1alpha1
// StorageVersion) stay unknown-api infos: nothing says when they leave.
var untaggedLifecycles = map[gvkOut]untaggedLifecycle{
	{Group: "rbac.authorization.k8s.io", Version: "v1alpha1", Kind: "ClusterRole"}: {
		introduced: version{Major: 1, Minor: 3}, removed: &rbacV1alpha1Removed, citations: rbacV1alpha1Cites,
		replacement: &gvkOut{Group: "rbac.authorization.k8s.io", Version: "v1", Kind: "ClusterRole"},
	},
	{Group: "rbac.authorization.k8s.io", Version: "v1alpha1", Kind: "ClusterRoleBinding"}: {
		introduced: version{Major: 1, Minor: 3}, removed: &rbacV1alpha1Removed, citations: rbacV1alpha1Cites,
		replacement: &gvkOut{Group: "rbac.authorization.k8s.io", Version: "v1", Kind: "ClusterRoleBinding"},
	},
	{Group: "rbac.authorization.k8s.io", Version: "v1alpha1", Kind: "Role"}: {
		introduced: version{Major: 1, Minor: 3}, removed: &rbacV1alpha1Removed, citations: rbacV1alpha1Cites,
		replacement: &gvkOut{Group: "rbac.authorization.k8s.io", Version: "v1", Kind: "Role"},
	},
	{Group: "rbac.authorization.k8s.io", Version: "v1alpha1", Kind: "RoleBinding"}: {
		introduced: version{Major: 1, Minor: 3}, removed: &rbacV1alpha1Removed, citations: rbacV1alpha1Cites,
		replacement: &gvkOut{Group: "rbac.authorization.k8s.io", Version: "v1", Kind: "RoleBinding"},
	},
	{Group: "node.k8s.io", Version: "v1alpha1", Kind: "RuntimeClass"}: {
		introduced:  version{Major: 1, Minor: 12},
		removed:     &version{Major: 1, Minor: 24},
		replacement: &gvkOut{Group: "node.k8s.io", Version: "v1", Kind: "RuntimeClass"},
		citations: []string{
			"https://github.com/kubernetes/kubernetes/blob/master/CHANGELOG/CHANGELOG-1.12.md", // "The RuntimeClass API has been added. This feature is in alpha" (a CRD until it became built-in in 1.14)
			"https://github.com/kubernetes/kubernetes/blob/master/CHANGELOG/CHANGELOG-1.24.md", // "The node.k8s.io/v1alpha1 RuntimeClass API is no longer served" (#103061)
		},
	},
}

// nonPersisted are kinds k8s.io/api registers (or registered) that are
// wrappers or subresource bodies, not resources: kube-apiserver never
// stored or served them, so no manifest or live object can be one and a
// removal for them is meaningless. Without this, a deleted one became a
// removed-api blocker through deletedTypes's inferred removal (#166). Each
// value is the evidence, from the k8s.io/api source that registers the type.
var nonPersisted = map[gvkOut]string{
	// core/v1 types.go: "PodStatusResult is a wrapper for PodStatus returned
	// by kubelet that can be encode/decoded". It has no storage under
	// pkg/registry/core, and v0.37 stopped registering it.
	{Group: "", Version: "v1", Kind: "PodStatusResult"}: "kubelet wrapper for PodStatus",
	// core/v1 types.go (v0.21): "A list of ephemeral containers used with
	// the Pod ephemeralcontainers subresource": the body of a subresource.
	{Group: "", Version: "v1", Kind: "EphemeralContainers"}: "body of the pods/ephemeralcontainers subresource",
	// extensions/v1beta1 types.go (v0.17): "Dummy definition", kept for
	// the API documentation only.
	{Group: "extensions", Version: "v1beta1", Kind: "ReplicationControllerDummy"}: "documentation placeholder",
	// Upstream tags a removal on the types below, but they are the same
	// kind of thing: none has storage in kube-apiserver's pkg/registry, so a
	// manifest of one cannot exist and "removed" is a false blocker. Their
	// current-version siblings (autoscaling/v1 Scale, admission.k8s.io/v1
	// AdmissionReview, apiextensions.k8s.io/v1 ConversionReview) are not
	// removed and stay.
	//
	// batch JobTemplate: the template embedded in a CronJob.
	// pkg/registry/batch/rest/storage_batch.go (release-1.20) maps only
	// jobs and cronjobs; v2alpha1 maps cronjobs only.
	{Group: "batch", Version: "v1beta1", Kind: "JobTemplate"}:  "template embedded in a CronJob",
	{Group: "batch", Version: "v2alpha1", Kind: "JobTemplate"}: "template embedded in a CronJob",
	// Scale: the body of the /scale subresource of deployments, replica
	// sets, stateful sets and replication controllers.
	{Group: "apps", Version: "v1beta1", Kind: "Scale"}:       "body of a /scale subresource",
	{Group: "apps", Version: "v1beta2", Kind: "Scale"}:       "body of a /scale subresource",
	{Group: "extensions", Version: "v1beta1", Kind: "Scale"}: "body of a /scale subresource",
	// DeploymentRollback: the body of the deployments/rollback subresource,
	// dropped in apps/v1 (use `kubectl rollout undo`).
	{Group: "apps", Version: "v1beta1", Kind: "DeploymentRollback"}:       "body of the deployments/rollback subresource",
	{Group: "extensions", Version: "v1beta1", Kind: "DeploymentRollback"}: "body of the deployments/rollback subresource",
	// Review payloads: what kube-apiserver sends to and reads from a
	// webhook, never an object a manifest or a cluster holds.
	{Group: "admission.k8s.io", Version: "v1beta1", Kind: "AdmissionReview"}:      "admission webhook payload",
	{Group: "apiextensions.k8s.io", Version: "v1beta1", Kind: "ConversionReview"}: "conversion webhook payload",
}

func isNonPersisted(g gvkOut) bool {
	_, ok := nonPersisted[g]
	return ok
}

// fixRemoval applies removalFixes to e: an override earlier than e's
// removal, or e without one, sets the removal, marked inferred since it is
// no upstream tag.
func fixRemoval(e *entry) {
	r, ok := removalFixes[e.gvk()]
	if !ok || e.Removed != nil && !r.before(*e.Removed) {
		return
	}
	e.Removed, e.RemovedInferred = &r, true
}
