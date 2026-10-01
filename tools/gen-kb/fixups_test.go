package main

import (
	"reflect"
	"testing"
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
