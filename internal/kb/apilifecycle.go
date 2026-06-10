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
