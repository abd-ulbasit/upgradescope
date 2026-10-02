package main

import (
	"reflect"
	"testing"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
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

// nonPersisted kinds are wrappers and subresource bodies no manifest or
// stored object can be: the KB must not record them, however the history
// reads (#166).
func TestNonPersistedKindsAreNotRecorded(t *testing.T) {
	gvk := func(group, ver, kind string) schema.GroupVersionKind {
		return schema.GroupVersionKind{Group: group, Version: ver, Kind: kind}
	}
	for _, k := range []schema.GroupVersionKind{
		gvk("", "v1", "PodStatusResult"),
		gvk("", "v1", "EphemeralContainers"),
		gvk("extensions", "v1beta1", "ReplicationControllerDummy"),
	} {
		if !skipKind(k) {
			t.Errorf("skipKind(%s) = false, want true: not a persisted resource", k)
		}
	}
	// The exclusion is per GVK: a same-named kind elsewhere is a real type.
	if skipKind(gvk("example.k8s.io", "v1", "PodStatusResult")) {
		t.Error("skipKind(example.k8s.io/v1 PodStatusResult) = true, want false")
	}
	if len(nonPersisted) != 3 {
		t.Errorf("nonPersisted has %d entries; extend this test with the new kind", len(nonPersisted))
	}
	for k, why := range nonPersisted {
		if why == "" {
			t.Errorf("nonPersisted[%s/%s %s] has no reason", k.Group, k.Version, k.Kind)
		}
	}

	// A dataset written before the exclusion existed loses the entries on
	// the next run instead of carrying them forward as tombstones.
	prev := []entry{
		{Group: "", Version: "v1", Kind: "PodStatusResult", Introduced: *v(1, 0), Removed: v(1, 37), RemovedInferred: true},
		{Group: "", Version: "v1", Kind: "Pod", Introduced: *v(1, 0)},
	}
	gen := []entry{{Group: "", Version: "v1", Kind: "Pod", Introduced: *v(1, 0)}}
	got, tombstoned := carryForward(prev, gen, map[gvkOut]bool{{Version: "v1", Kind: "Pod"}: true}, version{Major: 1, Minor: 37})
	if !reflect.DeepEqual(got, gen) || len(tombstoned) != 0 {
		t.Errorf("carryForward kept a non-persisted kind: got %+v, tombstoned %+v", got, tombstoned)
	}
}
