package kb

import (
	"cmp"
	"encoding/json"
	"slices"
	"testing"
)

// TestBuiltinGroupsDataset: the dataset records every group k8s.io/api
// registers, not only those with lifecycle entries, so the engine can tell
// an unknown built-in API from a CRD (#172). It asserts invariants, not the
// current list, so a refresh passes unmodified.
func TestBuiltinGroupsDataset(t *testing.T) {
	k, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	versions := map[string][]string{}
	for _, g := range k.BuiltinGroups {
		if _, dup := versions[g.Group]; dup {
			t.Errorf("group %q listed twice", g.Group)
		}
		if len(g.Versions) == 0 || !slices.IsSorted(g.Versions) {
			t.Errorf("group %q versions = %q, want a sorted, non-empty list", g.Group, g.Versions)
		}
		versions[g.Group] = g.Versions
	}
	if !slices.IsSortedFunc(k.BuiltinGroups, func(a, b BuiltinGroup) int { return cmp.Compare(a.Group, b.Group) }) {
		t.Error("builtinGroups is not sorted by group")
	}
	// Groups with no lifecycle entry at all, and the core group.
	for group, version := range map[string]string{
		"internal.apiserver.k8s.io": "v1alpha1", // StorageVersion
		"imagepolicy.k8s.io":        "v1alpha1", // ImageReview
		"":                          "v1",
	} {
		if !slices.Contains(versions[group], version) {
			t.Errorf("builtin group %q versions = %q, want %s", group, versions[group], version)
		}
	}
	// Every entry's group is built-in, tombstones included.
	for _, e := range k.APILifecycle {
		if !slices.Contains(versions[e.Group], e.Version) {
			t.Errorf("entry %s/%s %s: its group version is not in builtinGroups", e.Group, e.Version, e.Kind)
		}
	}
}

// A dataset written before builtinGroups existed still loads; the KB then
// has no groups beyond its entries' and behaves as it did.
func TestLoadWithoutBuiltinGroups(t *testing.T) {
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(apilifecycleJSON, &doc); err != nil {
		t.Fatal(err)
	}
	if _, ok := doc["builtinGroups"]; !ok {
		t.Fatal("embedded dataset has no builtinGroups: run make gen-kb")
	}
	delete(doc, "builtinGroups")
	raw, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	k, err := load(raw)
	if err != nil {
		t.Fatalf("load(without builtinGroups) error = %v", err)
	}
	if len(k.BuiltinGroups) != 0 || len(k.APILifecycle) == 0 {
		t.Errorf("BuiltinGroups = %d, entries = %d; want none and the entries", len(k.BuiltinGroups), len(k.APILifecycle))
	}
}
