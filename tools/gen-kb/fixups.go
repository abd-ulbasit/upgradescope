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
	// Upstream tags apps/v1 DeploymentRollback, but apps/v1 never had that
	// type: the rollback subresource was dropped (use `kubectl rollout
	// undo`), so there is no API to migrate to.
	{Group: "apps", Version: "v1beta1", Kind: "DeploymentRollback"}: nil,
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
