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
	// byKind lists the entries of each group and kind, in entry order.
	byKind map[GVK][]APILifecycleEntry // Version unset
}

func NewIndex(entries []APILifecycleEntry) Index {
	m := make(map[GVK]APILifecycleEntry, len(entries))
	byKind := make(map[GVK][]APILifecycleEntry)
	for _, e := range entries {
		m[GVK{Group: e.Group, Version: e.Version, Kind: e.Kind}] = e
		gk := GVK{Group: e.Group, Kind: e.Kind}
		byKind[gk] = append(byKind[gk], e)
	}
	return Index{byGVK: m, byKind: byKind}
}

// Lookup returns the entry for group/version/kind. Group is "" for core.
func (i Index) Lookup(group, version, kind string) (APILifecycleEntry, bool) {
	e, ok := i.byGVK[GVK{Group: group, Version: version, Kind: kind}]
	return e, ok
}

// servedAt reports whether target serves e: introduced at or before it,
// and not removed by it.
func servedAt(e APILifecycleEntry, target inventory.Version) bool {
	return e.Introduced.Compare(target) <= 0 && (e.Removed == nil || e.Removed.Compare(target) > 0)
}

// ResolveReplacement returns the API to migrate e to for an upgrade to
// target, only ever one the KB knows target serves: introduced at or
// before it, and not removed by it. It follows the replacement chain
// (flowcontrol v1beta1 → v1beta3 → v1) past every hop removed at or
// before target. A hop introduced after target is not served yet: the
// newest version of the hop's group and kind that target serves and that
// was introduced after e is recommended instead (storage.k8s.io/v1alpha1
// VolumeAttributesClass names v1, served from 1.34; at 1.33 that is
// v1beta1), and none when there is no such version. A hop the KB has no
// entry for carries no lifecycle evidence and is returned as is (the
// dataset tests require every shipped replacement to be a known GVK). It
// reports false when no served replacement is known: e has no
// replacement, the chain dead-ends at a removed API, loops, or reaches a
// hop not served yet with no served alternative (LaterReplacement then
// names it).
func (i Index) ResolveReplacement(e APILifecycleEntry, target inventory.Version) (GVK, bool) {
	g, r, known, ok := i.chain(e, target)
	switch {
	case !ok:
		return GVK{}, false
	case !known || servedAt(r, target):
		return g, true
	}
	// r is not introduced yet at target.
	var best *APILifecycleEntry
	for _, c := range i.byKind[GVK{Group: g.Group, Kind: g.Kind}] {
		if c.Version == e.Version && c.Group == e.Group || !servedAt(c, target) || c.Introduced.Compare(e.Introduced) <= 0 {
			continue
		}
		if best == nil || c.Introduced.Compare(best.Introduced) > 0 {
			best = &c
		}
	}
	if best == nil {
		return GVK{}, false
	}
	return GVK{Group: best.Group, Version: best.Version, Kind: best.Kind}, true
}

// ServedAlternative returns the newest version of e's group and kind that
// target serves (introduced at or before it, not removed by it), other
// than e's own: the API to write a manifest in when e itself is not served
// yet (resource.k8s.io/v1 DeviceClass at 1.33 is v1beta2, v1 is served from
// 1.34). Newest is the latest Introduced; the same release is broken by
// the version name, so the answer does not depend on entry order. It
// reports false when the KB knows no such version.
func (i Index) ServedAlternative(e APILifecycleEntry, target inventory.Version) (GVK, bool) {
	var best *APILifecycleEntry
	for _, c := range i.byKind[GVK{Group: e.Group, Kind: e.Kind}] {
		if c.Version == e.Version || !servedAt(c, target) {
			continue
		}
		if best == nil {
			best = &c
			continue
		}
		if cmpIntro := c.Introduced.Compare(best.Introduced); cmpIntro > 0 || cmpIntro == 0 && c.Version > best.Version {
			best = &c
		}
	}
	if best == nil {
		return GVK{}, false
	}
	return GVK{Group: best.Group, Version: best.Version, Kind: best.Kind}, true
}

// LaterReplacement returns, when ResolveReplacement knows no replacement
// target serves, the replacement e's chain reaches that a later release
// serves, and the release that introduces it: certificates.k8s.io/v1
// ClusterTrustBundle from 1.37 for v1beta1 at 1.36. It reports false
// otherwise.
func (i Index) LaterReplacement(e APILifecycleEntry, target inventory.Version) (GVK, inventory.Version, bool) {
	if _, ok := i.ResolveReplacement(e, target); ok {
		return GVK{}, inventory.Version{}, false
	}
	g, r, known, ok := i.chain(e, target)
	if !ok || !known || r.Introduced.Compare(target) <= 0 {
		return GVK{}, inventory.Version{}, false
	}
	return g, r.Introduced, true
}

// chain follows e's replacements past every hop removed at or before
// target and returns the first that is not, with its entry when the KB
// has one (known); ok is false when the chain ends or loops first.
func (i Index) chain(e APILifecycleEntry, target inventory.Version) (g GVK, r APILifecycleEntry, known, ok bool) {
	seen := map[GVK]bool{{Group: e.Group, Version: e.Version, Kind: e.Kind}: true}
	for next := e.Replacement; next != nil; {
		g = *next
		if seen[g] {
			return GVK{}, APILifecycleEntry{}, false, false
		}
		seen[g] = true
		r, known = i.byGVK[g]
		if !known || r.Removed == nil || r.Removed.Compare(target) > 0 {
			return g, r, known, true
		}
		next = r.Replacement
	}
	return GVK{}, APILifecycleEntry{}, false, false
}
