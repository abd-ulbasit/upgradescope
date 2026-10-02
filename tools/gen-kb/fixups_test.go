package main

import (
	"os"
	"reflect"
	"sort"
	"strings"
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

// A type k8s.io/api registers without lifecycle markers gets its lifecycle
// from untaggedLifecycles, so an API kube-apiserver stopped serving is not
// just an unknown-api info (rbac.authorization.k8s.io/v1alpha1, #166).
func TestExtractUntaggedLifecycles(t *testing.T) {
	scheme := runtime.NewScheme()
	for _, add := range addToSchemes {
		if err := add(scheme); err != nil {
			t.Fatal(err)
		}
	}
	entries, _, noLifecycle := extract(scheme)
	got := map[gvkOut]entry{}
	for _, e := range entries {
		got[e.gvk()] = e
	}
	rbac := func(kind string) gvkOut {
		return gvkOut{Group: "rbac.authorization.k8s.io", Version: "v1alpha1", Kind: kind}
	}
	for _, kind := range []string{"ClusterRole", "ClusterRoleBinding", "Role", "RoleBinding"} {
		want := entry{Group: "rbac.authorization.k8s.io", Version: "v1alpha1", Kind: kind,
			Introduced: *v(1, 3), Removed: v(1, 23), RemovedInferred: true,
			Replacement: &gvkOut{Group: "rbac.authorization.k8s.io", Version: "v1", Kind: kind}}
		if !reflect.DeepEqual(got[rbac(kind)], want) {
			t.Errorf("rbac v1alpha1 %s = %+v, want %+v", kind, got[rbac(kind)], want)
		}
	}
	// Types without a table entry stay out of the dataset and are reported.
	if e, ok := got[gvkOut{Group: "scheduling.k8s.io", Version: "v1alpha3", Kind: "Workload"}]; ok {
		t.Errorf("scheduling v1alpha3 Workload has no lifecycle source but is in the dataset: %+v", e)
	}
	for _, s := range noLifecycle {
		if strings.HasPrefix(s, "rbac.authorization.k8s.io/v1alpha1 ") {
			t.Errorf("noLifecycle still lists %q", s)
		}
	}
}

// Every untaggedLifecycles entry is cited and sane, and it is a stop-gap
// for types upstream does not tag: once k8s.io/api tags one, the table must
// go (the tag would win, and the table would claim a source it no longer is).
func TestUntaggedLifecyclesAreCitedAndUntagged(t *testing.T) {
	scheme := runtime.NewScheme()
	for _, add := range addToSchemes {
		if err := add(scheme); err != nil {
			t.Fatal(err)
		}
	}
	_, upstream, _ := extract(scheme)
	if len(untaggedLifecycles) == 0 {
		t.Fatal("untaggedLifecycles is empty")
	}
	for k, u := range untaggedLifecycles {
		name := k.Group + "/" + k.Version + " " + k.Kind
		if !upstream[k] {
			t.Errorf("%s: k8s.io/api no longer registers it; move the removal into removalFixes (deletedTypes would infer it from the deletion release, not the one that stopped serving it) and delete the entry", name)
		}
		typ, err := scheme.New(schema.GroupVersionKind{Group: k.Group, Version: k.Version, Kind: k.Kind})
		if err != nil {
			t.Errorf("%s: %v", name, err)
		} else if _, tagged := typ.(introducedIface); tagged {
			t.Errorf("%s: k8s.io/api tags it now; delete the entry", name)
		}
		if len(u.citations) == 0 {
			t.Errorf("%s: no citation", name)
		}
		for _, c := range u.citations {
			if !strings.HasPrefix(c, "https://") {
				t.Errorf("%s: citation %q is not an https URL", name, c)
			}
		}
		if u.removed != nil && !u.introduced.before(*u.removed) {
			t.Errorf("%s: removed %s is not after introduced %s", name, u.removed, u.introduced)
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
		// Removed in the upstream tags, but wrappers all the same: a removed-api
		// blocker for these is as false as for PodStatusResult.
		gvk("batch", "v1beta1", "JobTemplate"),
		gvk("batch", "v2alpha1", "JobTemplate"),
		gvk("apps", "v1beta1", "Scale"),
		gvk("apps", "v1beta2", "Scale"),
		gvk("extensions", "v1beta1", "Scale"),
		gvk("apps", "v1beta1", "DeploymentRollback"),
		gvk("extensions", "v1beta1", "DeploymentRollback"),
		gvk("admission.k8s.io", "v1beta1", "AdmissionReview"),
		gvk("apiextensions.k8s.io", "v1beta1", "ConversionReview"),
		gvk("policy", "v1beta1", "Eviction"),
		gvk("apidiscovery.k8s.io", "v2beta1", "APIGroupDiscovery"),
	} {
		if !skipKind(k) {
			t.Errorf("skipKind(%s) = false, want true: not a persisted resource", k)
		}
	}
	// The exclusion is per GVK: a same-named kind elsewhere is a real type.
	if skipKind(gvk("example.k8s.io", "v1", "PodStatusResult")) {
		t.Error("skipKind(example.k8s.io/v1 PodStatusResult) = true, want false")
	}
	if len(nonPersisted) != 14 {
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

// A registered type with no lifecycle source is reported by extract and
// left out of the dataset, so a manifest of one is at most an unknown-api
// info (no finding when the KB has no other entry in its group). The
// set must match what docs/concepts/knowledge-base.md ("What it does not
// cover") tells users, or a new untagged type is only a log line and the
// docs drift.
func TestNoLifecycleSetMatchesDocs(t *testing.T) {
	scheme := runtime.NewScheme()
	for _, add := range addToSchemes {
		if err := add(scheme); err != nil {
			t.Fatal(err)
		}
	}
	_, _, noLifecycle := extract(scheme)
	sort.Strings(noLifecycle)
	want := []string{
		"imagepolicy.k8s.io/v1alpha1 ImageReview",
		"internal.apiserver.k8s.io/v1alpha1 StorageVersion",
		"scheduling.k8s.io/v1alpha3 CompositePodGroup",
		"scheduling.k8s.io/v1alpha3 PodGroup",
		"scheduling.k8s.io/v1alpha3 Workload",
	}
	if !reflect.DeepEqual(noLifecycle, want) {
		t.Errorf("registered types with no lifecycle = %q, want %q: add a cited untaggedLifecycles entry, or list the type under \"What it does not cover\" in docs/concepts/knowledge-base.md and here", noLifecycle, want)
	}

	doc, err := os.ReadFile("../../docs/concepts/knowledge-base.md")
	if err != nil {
		t.Fatal(err)
	}
	section := string(doc)
	i := strings.Index(section, "## What it does not cover")
	if i < 0 {
		t.Fatal("docs/concepts/knowledge-base.md has no \"What it does not cover\" section")
	}
	section = section[i+1:]
	if j := strings.Index(section, "\n## "); j >= 0 {
		section = section[:j]
	}
	section = strings.Join(strings.Fields(section), " ") // undo the line wrapping
	for _, s := range noLifecycle {
		gv, kind, _ := strings.Cut(s, " ")
		if !strings.Contains(section, "`"+gv+"`") || !strings.Contains(section, kind) {
			t.Errorf("%s is registered without lifecycle markers but \"What it does not cover\" does not name %s and %s", s, gv, kind)
		}
	}
}
