package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"testing"

	"k8s.io/apimachinery/pkg/runtime"
)

func v(major, minor int) *version { return &version{Major: major, Minor: minor} }

func TestCarryForward(t *testing.T) {
	gen := []entry{
		{Group: "scheduling.k8s.io", Version: "v1", Kind: "PriorityClass", Introduced: *v(1, 14)},
		// Upstream now marks this removed; the fresh data must win over prev.
		{Group: "storagemigration.k8s.io", Version: "v1beta1", Kind: "StorageVersionMigration",
			Introduced: *v(1, 35), Deprecated: v(1, 37), Removed: v(1, 40)},
	}
	prev := []entry{
		{Group: "scheduling.k8s.io", Version: "v1", Kind: "PriorityClass", Introduced: *v(1, 14)},
		{Group: "storagemigration.k8s.io", Version: "v1beta1", Kind: "StorageVersionMigration",
			Introduced: *v(1, 35)},
		// Package deleted upstream, never marked removed: tombstone at 1.37.
		{Group: "scheduling.k8s.io", Version: "v1alpha2", Kind: "Workload", Introduced: *v(1, 35)},
		// Package deleted upstream after a documented removal: keep as-is.
		{Group: "flowcontrol.apiserver.k8s.io", Version: "v1beta2", Kind: "FlowSchema",
			Introduced: *v(1, 23), Deprecated: v(1, 26), Removed: v(1, 29),
			Replacement: &gvkOut{Group: "flowcontrol.apiserver.k8s.io", Version: "v1", Kind: "FlowSchema"}},
		// Tombstoned by an earlier run: the inferred removal must not move.
		{Group: "example.k8s.io", Version: "v1alpha1", Kind: "Gone", Introduced: *v(1, 30),
			Removed: v(1, 36), RemovedInferred: true},
		// Still registered upstream but lost its lifecycle methods: keep the
		// last known data rather than inventing a removal.
		{Group: "node.k8s.io", Version: "v1alpha1", Kind: "RuntimeClass", Introduced: *v(1, 12)},
		// Package deleted upstream before its scheduled removal (alpha DRA
		// types were tagged for removal releases after they were deleted):
		// the deletion is the removal.
		{Group: "example.k8s.io", Version: "v1alpha1", Kind: "Early", Introduced: *v(1, 33),
			Deprecated: v(1, 36), Removed: v(1, 39)},
	}
	upstream := map[gvkOut]bool{
		{Group: "scheduling.k8s.io", Version: "v1", Kind: "PriorityClass"}:                      true,
		{Group: "storagemigration.k8s.io", Version: "v1beta1", Kind: "StorageVersionMigration"}: true,
		{Group: "node.k8s.io", Version: "v1alpha1", Kind: "RuntimeClass"}:                       true,
	}

	got, tombstoned := carryForward(prev, gen, upstream, version{Major: 1, Minor: 37})

	want := []entry{
		gen[0],
		gen[1],
		{Group: "scheduling.k8s.io", Version: "v1alpha2", Kind: "Workload", Introduced: *v(1, 35),
			Removed: v(1, 37), RemovedInferred: true},
		prev[3],
		prev[4],
		prev[5],
		{Group: "example.k8s.io", Version: "v1alpha1", Kind: "Early", Introduced: *v(1, 33),
			Deprecated: v(1, 36), Removed: v(1, 37), RemovedInferred: true},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("carryForward() =\n%+v\nwant\n%+v", got, want)
	}
	if len(tombstoned) != 4 {
		t.Errorf("tombstoned = %+v, want the 4 entries missing upstream", tombstoned)
	}
	if prev[6].Removed.Minor != 39 {
		t.Errorf("carryForward mutated prev: Early.Removed = %v", prev[6].Removed)
	}
	// carryForward must not alias prev's pointers into the output.
	if prev[2].Removed != nil {
		t.Errorf("carryForward mutated prev: Workload.Removed = %v", prev[2].Removed)
	}
}

func TestCarryForwardNoPrev(t *testing.T) {
	gen := []entry{{Group: "", Version: "v1", Kind: "Pod", Introduced: *v(1, 0)}}
	got, tombstoned := carryForward(nil, gen, nil, version{Major: 1, Minor: 37})
	if !reflect.DeepEqual(got, gen) || len(tombstoned) != 0 {
		t.Errorf("carryForward(nil prev) = %+v, %+v; want gen unchanged and no tombstones", got, tombstoned)
	}
}

func TestReadDataset(t *testing.T) {
	dir := t.TempDir()

	// A missing file is a first run, not an error.
	entries, err := readDataset(filepath.Join(dir, "missing.json"))
	if err != nil || entries != nil {
		t.Fatalf("readDataset(missing) = %v, %v; want nil, nil", entries, err)
	}

	p := filepath.Join(dir, "apilifecycle.json")
	raw := `{"generatedFrom":"k8s.io/api v0.36.1","maxKnownK8s":"1.36","entries":[
		{"group":"batch","version":"v1beta1","kind":"CronJob","introduced":"1.8","deprecated":"1.21","removed":"1.25",
		 "replacement":{"group":"batch","version":"v1","kind":"CronJob"}},
		{"group":"x.k8s.io","version":"v1alpha1","kind":"Gone","introduced":"1.30","removed":"1.36","removedInferred":true}]}`
	if err := os.WriteFile(p, []byte(raw), 0o644); err != nil {
		t.Fatal(err)
	}
	entries, err = readDataset(p)
	if err != nil {
		t.Fatalf("readDataset() error = %v", err)
	}
	want := []entry{
		{Group: "batch", Version: "v1beta1", Kind: "CronJob", Introduced: *v(1, 8), Deprecated: v(1, 21),
			Removed: v(1, 25), Replacement: &gvkOut{Group: "batch", Version: "v1", Kind: "CronJob"}},
		{Group: "x.k8s.io", Version: "v1alpha1", Kind: "Gone", Introduced: *v(1, 30), Removed: v(1, 36),
			RemovedInferred: true},
	}
	if !reflect.DeepEqual(entries, want) {
		t.Errorf("readDataset() =\n%+v\nwant\n%+v", entries, want)
	}

	if err := os.WriteFile(p, []byte(`{"entries":[{"introduced":"one.two"}]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := readDataset(p); err == nil {
		t.Error("readDataset(bad version) = nil error, want error")
	}
}

// The dataset records every group k8s.io/api registers, with or without
// lifecycle entries (internal.apiserver.k8s.io and imagepolicy.k8s.io have
// none), plus the groups of carried-forward entries (#172).
func TestBuiltinGroups(t *testing.T) {
	scheme := runtime.NewScheme()
	for _, add := range addToSchemes {
		if err := add(scheme); err != nil {
			t.Fatal(err)
		}
	}
	extra := []entry{
		{Group: "gone.k8s.io", Version: "v1alpha1", Kind: "Gone", Introduced: *v(1, 30)},
		{Group: "batch", Version: "v1beta9", Kind: "Gone", Introduced: *v(1, 30)},
	}
	got := map[string][]string{}
	var order []string
	for _, g := range builtinGroups(scheme, extra) {
		got[g.Group] = g.Versions
		order = append(order, g.Group)
	}
	if !sort.StringsAreSorted(order) {
		t.Errorf("groups not sorted: %q", order)
	}
	for group, want := range map[string][]string{
		"internal.apiserver.k8s.io": {"v1alpha1"},
		"imagepolicy.k8s.io":        {"v1alpha1"},
		"gone.k8s.io":               {"v1alpha1"},
	} {
		if !reflect.DeepEqual(got[group], want) {
			t.Errorf("versions of %q = %q, want %q", group, got[group], want)
		}
	}
	if vs, ok := got[""]; !ok || !slices.Contains(vs, "v1") {
		t.Errorf("core group = %q, %v; want it registered with v1", vs, ok)
	}
	if vs := got["batch"]; !slices.Contains(vs, "v1") || !slices.Contains(vs, "v1beta9") || !sort.StringsAreSorted(vs) {
		t.Errorf("batch versions = %q, want sorted, with v1 (registered) and v1beta9 (from an entry)", vs)
	}
	for group, vs := range got {
		if slices.Contains(vs, runtime.APIVersionInternal) {
			t.Errorf("group %q records the internal version", group)
		}
	}
}

func TestVersionJSONRoundTrip(t *testing.T) {
	buf, err := json.Marshal(version{Major: 1, Minor: 37})
	if err != nil || string(buf) != `"1.37"` {
		t.Fatalf("Marshal = %s, %v; want \"1.37\"", buf, err)
	}
	var got version
	if err := json.Unmarshal(buf, &got); err != nil || got != (version{Major: 1, Minor: 37}) {
		t.Errorf("Unmarshal(%s) = %+v, %v; want 1.37", buf, got, err)
	}
}
