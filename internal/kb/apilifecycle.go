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
	// RemovedInferred marks a tombstone tools/gen-kb carried forward after
	// upstream deleted the type's package without ever tagging a removal:
	// Removed is then the k8s.io/api minor the package disappeared in.
	RemovedInferred bool `json:"removedInferred,omitempty"`
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
