package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/runtime"
)

func TestDeletedTypes(t *testing.T) {
	snap := func(minor int, types ...entry) snapshot {
		s := snapshot{k8s: version{Major: 1, Minor: minor}, types: map[gvkOut]entry{}}
		for _, e := range types {
			s.types[e.gvk()] = e
		}
		return s
	}
	// Tagged like resource.k8s.io/v1alpha3 DeviceClass in k8s.io/api
	// v0.33: removal scheduled for 1.37, yet v0.34 deleted the type.
	early := entry{Group: "resource.k8s.io", Version: "v1alpha3", Kind: "DeviceClass",
		Introduced: *v(1, 31), Deprecated: v(1, 34), Removed: v(1, 37),
		Replacement: &gvkOut{Group: "resource.k8s.io", Version: "v1beta1", Kind: "DeviceClassList"}}
	// Untagged, like resource.k8s.io/v1alpha2.
	untagged := entry{Group: "resource.k8s.io", Version: "v1alpha2", Kind: "ResourceClass"}
	// Tagged removal before the package went away: the tag stands.
	lingered := entry{Group: "flowcontrol.apiserver.k8s.io", Version: "v1beta1", Kind: "FlowSchema",
		Introduced: *v(1, 20), Deprecated: v(1, 23), Removed: v(1, 26)}
	// Tagged removal in exactly the release that deleted it.
	onTime := entry{Group: "example.k8s.io", Version: "v1alpha1", Kind: "OnTime",
		Introduced: *v(1, 30), Removed: v(1, 33)}
	// Gone in 1.31, back in 1.32, gone again in 1.33.
	flicker := entry{Group: "example.k8s.io", Version: "v1alpha1", Kind: "Flicker"}
	kept := entry{Group: "apps", Version: "v1", Kind: "Deployment", Introduced: *v(1, 9)}
	// Tagged and deleted in 1.33, but kube-apiserver 1.31 no longer
	// serves it (removalFixes).
	unserved := entry{Group: "networking.k8s.io", Version: "v1alpha1", Kind: "IPAddress",
		Introduced: *v(1, 27), Deprecated: v(1, 30), Removed: v(1, 33)}
	hist := []snapshot{
		snap(30, untagged, lingered, onTime, flicker, kept, unserved, entry{Group: early.Group, Version: early.Version, Kind: early.Kind}),
		snap(31, lingered, onTime, kept, unserved, early),
		snap(32, lingered, onTime, flicker, kept, unserved, early),
	}
	upstream := map[gvkOut]bool{kept.gvk(): true}

	got := deletedTypes(hist, upstream)

	want := []entry{
		{Group: "example.k8s.io", Version: "v1alpha1", Kind: "Flicker", Introduced: *v(1, 30),
			Removed: v(1, 33), RemovedInferred: true},
		{Group: "example.k8s.io", Version: "v1alpha1", Kind: "OnTime", Introduced: *v(1, 30), Removed: v(1, 33)},
		lingered,
		{Group: "networking.k8s.io", Version: "v1alpha1", Kind: "IPAddress",
			Introduced: *v(1, 27), Deprecated: v(1, 30), Removed: v(1, 31), RemovedInferred: true},
		// No tags: introduced is the first release history read.
		{Group: "resource.k8s.io", Version: "v1alpha2", Kind: "ResourceClass", Introduced: *v(1, 30),
			Removed: v(1, 31), RemovedInferred: true},
		// The newest tags win (the 1.30 snapshot had none); the deletion
		// overrides the later scheduled removal; the List replacement
		// is normalized like a generated entry's.
		{Group: "resource.k8s.io", Version: "v1alpha3", Kind: "DeviceClass",
			Introduced: *v(1, 31), Deprecated: v(1, 34), Removed: v(1, 33), RemovedInferred: true,
			Replacement: &gvkOut{Group: "resource.k8s.io", Version: "v1beta1", Kind: "DeviceClass"}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("deletedTypes() =\n%+v\nwant\n%+v", got, want)
	}
	// deletedTypes must not alias the snapshots' pointers.
	if early.Removed.Minor != 37 || hist[2].types[early.gvk()].Removed.Minor != 37 {
		t.Error("deletedTypes mutated a snapshot entry")
	}
}

// readSnapshot parses k8s.io/api source instead of compiling it. Read
// against the pinned module, it must find exactly the types and lifecycle
// tags the runtime.Scheme extraction finds, so the parser cannot drift from
// what upstream registers.
func TestReadSnapshotMatchesScheme(t *testing.T) {
	out, err := exec.Command("go", "list", "-m", "-f", "{{.Dir}}", "k8s.io/api").Output()
	if err != nil {
		t.Fatalf("go list k8s.io/api: %v", err)
	}
	dir := strings.TrimSpace(string(out))
	if dir == "" {
		t.Skip("k8s.io/api is not in the module cache (run go mod download)")
	}
	got, err := readSnapshot(dir)
	if err != nil {
		t.Fatalf("readSnapshot(%s): %v", dir, err)
	}

	scheme := runtime.NewScheme()
	for _, add := range addToSchemes {
		if err := add(scheme); err != nil {
			t.Fatal(err)
		}
	}
	entries, upstream, _ := extract(scheme)
	tags := map[gvkOut]entry{}
	for _, e := range entries {
		tags[e.gvk()] = e
	}
	for k := range upstream {
		// The other modules gen-kb extracts from are not k8s.io/api.
		if k.Group == "apiextensions.k8s.io" || k.Group == "apiregistration.k8s.io" {
			continue
		}
		e, ok := got[k]
		if !ok {
			t.Errorf("readSnapshot missing %s/%s %s", k.Group, k.Version, k.Kind)
			continue
		}
		fixReplacement(&e)
		want, tagged := tags[k]
		if !tagged {
			want = entry{Group: k.Group, Version: k.Version, Kind: k.Kind}
			fixReplacement(&want)
		}
		if !reflect.DeepEqual(e, want) {
			t.Errorf("readSnapshot %s/%s %s = %+v, want %+v", k.Group, k.Version, k.Kind, e, want)
		}
	}
	for k := range got {
		if !upstream[k] {
			t.Errorf("readSnapshot found %s/%s %s, which the scheme does not register", k.Group, k.Version, k.Kind)
		}
	}
}

// Older releases are read from source too: an untagged package (no
// zz_generated.prerelease-lifecycle.go), several AddKnownTypes calls,
// types from other packages (metav1.Status) and List kinds.
func TestReadSnapshotUntaggedPackage(t *testing.T) {
	dir := t.TempDir()
	pkg := filepath.Join(dir, "resource", "v1alpha2")
	if err := os.MkdirAll(pkg, 0o755); err != nil {
		t.Fatal(err)
	}
	// Directories without a register.go (testdata) are not packages.
	if err := os.MkdirAll(filepath.Join(dir, "testdata", "HEAD"), 0o755); err != nil {
		t.Fatal(err)
	}
	src := `package v1alpha2

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

const GroupName = "resource.k8s.io"

var SchemeGroupVersion = schema.GroupVersion{Group: GroupName, Version: "v1alpha2"}

func addKnownTypes(scheme *runtime.Scheme) error {
	scheme.AddKnownTypes(SchemeGroupVersion,
		&ResourceClass{},
		&ResourceClassList{},
		&PodSchedulingContext{},
	)
	scheme.AddKnownTypes(SchemeGroupVersion, &metav1.Status{})
	metav1.AddToGroupVersion(scheme, SchemeGroupVersion)
	return nil
}
`
	if err := os.WriteFile(filepath.Join(pkg, "register.go"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := readSnapshot(dir)
	if err != nil {
		t.Fatal(err)
	}
	want := map[gvkOut]entry{
		{Group: "resource.k8s.io", Version: "v1alpha2", Kind: "ResourceClass"}:        {Group: "resource.k8s.io", Version: "v1alpha2", Kind: "ResourceClass"},
		{Group: "resource.k8s.io", Version: "v1alpha2", Kind: "PodSchedulingContext"}: {Group: "resource.k8s.io", Version: "v1alpha2", Kind: "PodSchedulingContext"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("readSnapshot() = %+v\nwant %+v", got, want)
	}

	// A register.go this parser does not understand fails loudly rather
	// than reading as a package without types.
	if err := os.WriteFile(filepath.Join(pkg, "register.go"), []byte("package v1alpha2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := readSnapshot(dir); err == nil {
		t.Error("readSnapshot(register.go without GroupName) = nil error, want error")
	}
}
