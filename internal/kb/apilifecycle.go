// Package kb loads and indexes the three upgrade-readiness datasets:
// generated API lifecycle data, the add-on EOL registry, and the
// hardcoded Kubernetes version-skew policy.
package kb

import (
	"encoding/json"
	"fmt"

	"github.com/abd-ulbasit/upgradescope/internal/inventory"
)

// APILifecycleEntry describes one versioned API kind's lifecycle, as
// extracted from k8s.io/api generated APILifecycle* methods by tools/gen-kb.
type APILifecycleEntry struct {
	Group       string             `json:"group"` // "" for core
	Version     string             `json:"version"`
	Kind        string             `json:"kind"`
	Introduced  inventory.Version  `json:"introduced"`
	Deprecated  *inventory.Version `json:"deprecated,omitempty"`
	Removed     *inventory.Version `json:"removed,omitempty"`
	Replacement *GVK               `json:"replacement,omitempty"`
	// RemovedInferred marks an entry whose Removed is no upstream lifecycle
	// tag: tools/gen-kb set it to the k8s.io/api minor a deleted type
	// disappeared in (when untagged, or tagged for a later removal), to the
	// earlier release kube-apiserver stopped serving it in (its
	// removalFixes), or to the release the Kubernetes changelog states for a
	// type upstream still registers but never tagged (its untaggedLifecycles).
	RemovedInferred bool `json:"removedInferred,omitempty"`
}

// BuiltinGroup is a built-in API group (one gen-kb's scheme registers),
// with its versions. The engine matches on the group alone; Versions is
// informational.
type BuiltinGroup struct {
	Group    string   `json:"group"` // "" for core
	Versions []string `json:"versions"`
}

type GVK struct {
	Group   string `json:"group"`
	Version string `json:"version"`
	Kind    string `json:"kind"`
}

// lifecycleFile is the on-disk format of data/apilifecycle.json,
// written by tools/gen-kb.
type lifecycleFile struct {
	GeneratedFrom string              `json:"generatedFrom"` // e.g. "k8s.io/api v0.36.1"
	MaxKnownK8s   string              `json:"maxKnownK8s"`   // e.g. "1.36"
	Entries       []APILifecycleEntry `json:"entries"`
	// BuiltinGroups is every group gen-kb's scheme registers (k8s.io/api
	// plus the apiextensions and apiregistration schemes), including those
	// with no entry (internal.apiserver.k8s.io, imagepolicy.k8s.io). Absent
	// in a dataset written before #172: the built-in groups are then only
	// those of Entries.
	BuiltinGroups []BuiltinGroup `json:"builtinGroups,omitempty"`
}

func parseLifecycle(data []byte) (lifecycleFile, error) {
	var f lifecycleFile
	if err := json.Unmarshal(data, &f); err != nil {
		return lifecycleFile{}, fmt.Errorf("kb: corrupt apilifecycle.json: %w", err)
	}
	if f.MaxKnownK8s == "" {
		return lifecycleFile{}, fmt.Errorf("kb: apilifecycle.json missing maxKnownK8s")
	}
	if len(f.Entries) == 0 {
		return lifecycleFile{}, fmt.Errorf("kb: apilifecycle.json has no entries")
	}
	return f, nil
}

// The smallest dataset Load accepts. The shipped one has about 200
// entries, over 120 of them with a removal; a refresh only adds (gen-kb
// carries deleted types forward as tombstones) except for the explicit
// nonPersisted list, so a file under these
// floors is not an old dataset but a damaged or gutted one: valid JSON that
// would let every removed API scan as ready. (TestDatasetSanity checks the
// content; this check runs in every binary.)
const (
	minLifecycleEntries  = 150
	minLifecycleRemovals = 100
)

// checkLifecycleFloors reports a lifecycle dataset too small, or with too
// few removals, to be the one gen-kb wrote.
func checkLifecycleFloors(f lifecycleFile) error {
	removals := 0
	for _, e := range f.Entries {
		if e.Removed != nil {
			removals++
		}
	}
	if len(f.Entries) < minLifecycleEntries || removals < minLifecycleRemovals {
		return fmt.Errorf("kb: embedded apilifecycle.json is corrupt: %d entries (want >= %d), %d with a removal (want >= %d); "+
			"it would judge removed APIs as served, so rebuild from a clean source tree (make gen-kb)",
			len(f.Entries), minLifecycleEntries, removals, minLifecycleRemovals)
	}
	return nil
}

// Index is an O(1) lookup over lifecycle entries by group/version/kind.
type Index struct {
	byGVK map[GVK]APILifecycleEntry
}

func NewIndex(entries []APILifecycleEntry) Index {
	m := make(map[GVK]APILifecycleEntry, len(entries))
	for _, e := range entries {
		m[GVK{Group: e.Group, Version: e.Version, Kind: e.Kind}] = e
	}
	return Index{byGVK: m}
}

// Lookup returns the entry for group/version/kind. Group is "" for core.
func (i Index) Lookup(group, version, kind string) (APILifecycleEntry, bool) {
	e, ok := i.byGVK[GVK{Group: group, Version: version, Kind: kind}]
	return e, ok
}

// ResolveReplacement returns the API to migrate e to for an upgrade to
// target: it follows the replacement chain (flowcontrol v1beta1 → v1beta3 →
// v1) past every hop that is itself removed at or before target, so advice
// never points at an API the KB knows the target no longer serves. A hop
// the KB has no entry for carries no removal evidence and is returned as
// is (the dataset tests require every shipped replacement to be a known
// GVK). It reports false when e has no replacement, or the chain dead-ends
// at a removed API with no further replacement, or loops.
func (i Index) ResolveReplacement(e APILifecycleEntry, target inventory.Version) (GVK, bool) {
	seen := map[GVK]bool{{Group: e.Group, Version: e.Version, Kind: e.Kind}: true}
	for next := e.Replacement; next != nil; {
		g := *next
		if seen[g] {
			return GVK{}, false
		}
		seen[g] = true
		r, ok := i.byGVK[g]
		if !ok || r.Removed == nil || r.Removed.Compare(target) > 0 {
			return g, true
		}
		next = r.Replacement
	}
	return GVK{}, false
}
