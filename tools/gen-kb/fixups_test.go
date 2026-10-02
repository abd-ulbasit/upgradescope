package main

import (
	"reflect"
	"testing"

	"k8s.io/apimachinery/pkg/runtime"
)

func TestFixReplacement(t *testing.T) {
	g := func(group, ver, kind string) *gvkOut { return &gvkOut{Group: group, Version: ver, Kind: kind} }
	cases := []struct {
		name string
		in   entry
		want *gvkOut
	}{
		{"upstream tag typo: List kind for a non-List type",
			entry{Group: "networking.k8s.io", Version: "v1beta1", Kind: "IngressClass", Replacement: g("networking.k8s.io", "v1", "IngressClassList")},
			g("networking.k8s.io", "v1", "IngressClass")},
		{"missing upstream tag: events",
			entry{Group: "events.k8s.io", Version: "v1beta1", Kind: "Event"},
			g("events.k8s.io", "v1", "Event")},
		{"missing upstream tag: RuntimeClass",
			entry{Group: "node.k8s.io", Version: "v1beta1", Kind: "RuntimeClass"},
			g("node.k8s.io", "v1", "RuntimeClass")},
		{"tag names a type that never existed",
			entry{Group: "apps", Version: "v1beta1", Kind: "DeploymentRollback", Replacement: g("apps", "v1", "DeploymentRollback")},
			nil},
		{"correct tag untouched",
			entry{Group: "batch", Version: "v1beta1", Kind: "CronJob", Replacement: g("batch", "v1", "CronJob")},
			g("batch", "v1", "CronJob")},
		{"no tag untouched",
			entry{Group: "", Version: "v1", Kind: "Pod"},
			nil},
	}
	for _, c := range cases {
		e := c.in
		fixReplacement(&e)
		if !reflect.DeepEqual(e.Replacement, c.want) {
			t.Errorf("%s: Replacement = %+v, want %+v", c.name, e.Replacement, c.want)
		}
	}

	// Fixed entries must not share the override table's pointers.
	a := entry{Group: "events.k8s.io", Version: "v1beta1", Kind: "Event"}
	fixReplacement(&a)
	a.Replacement.Kind = "Mutated"
	b := entry{Group: "events.k8s.io", Version: "v1beta1", Kind: "Event"}
	fixReplacement(&b)
	if b.Replacement.Kind != "Event" {
		t.Errorf("fixReplacement aliases its override table: got %+v", b.Replacement)
	}
}

func TestFixRemoval(t *testing.T) {
	cases := []struct {
		name         string
		in           entry
		want         *version
		wantInferred bool
	}{
		{"kube-apiserver stopped serving it before k8s.io/api deleted it",
			entry{Group: "networking.k8s.io", Version: "v1alpha1", Kind: "IPAddress", Removed: v(1, 33)},
			v(1, 31), true},
		{"no removal recorded",
			entry{Group: "scheduling.k8s.io", Version: "v1alpha1", Kind: "PriorityClass"},
			v(1, 23), true},
		{"an earlier removal stands",
			entry{Group: "scheduling.k8s.io", Version: "v1alpha1", Kind: "PriorityClass", Removed: v(1, 20)},
			v(1, 20), false},
		{"no override untouched",
			entry{Group: "batch", Version: "v1beta1", Kind: "CronJob", Removed: v(1, 25)},
			v(1, 25), false},
	}
	for _, c := range cases {
		e := c.in
		fixRemoval(&e)
		if !reflect.DeepEqual(e.Removed, c.want) || e.RemovedInferred != c.wantInferred {
			t.Errorf("%s: Removed = %v (inferred %v), want %v (inferred %v)", c.name, e.Removed, e.RemovedInferred, c.want, c.wantInferred)
		}
	}
}

// removalFixes correct tombstones only: a type the pinned k8s.io/api
// still registers keeps its upstream tags.
func TestRemovalFixesAreDeletedTypes(t *testing.T) {
	scheme := runtime.NewScheme()
	for _, add := range addToSchemes {
		if err := add(scheme); err != nil {
			t.Fatal(err)
		}
	}
	_, upstream, _ := extract(scheme)
	for k := range removalFixes {
		if upstream[k] {
			t.Errorf("removalFixes has %s/%s %s, which k8s.io/api still registers", k.Group, k.Version, k.Kind)
		}
	}
}
