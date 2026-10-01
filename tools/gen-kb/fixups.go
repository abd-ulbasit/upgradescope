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
