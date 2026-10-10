package main

import (
	"os"
	"reflect"
	"slices"
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

// taggedRemovalFixes correct the removal tag of a type k8s.io/api still
// registers, and only while the tag is later than the release that stopped
// serving it: once upstream corrects the tag the override must go, or the
// dataset would claim an inferred removal that is the tag.
func TestTaggedRemovalFixesAreEarlierThanTheTag(t *testing.T) {
	scheme := runtime.NewScheme()
	for _, add := range addToSchemes {
		if err := add(scheme); err != nil {
			t.Fatal(err)
		}
	}
	_, upstream, _ := extract(scheme)
	if len(taggedRemovalFixes) == 0 {
		t.Fatal("taggedRemovalFixes is empty")
	}
	for k, r := range taggedRemovalFixes {
		name := k.Group + "/" + k.Version + " " + k.Kind
		if !upstream[k] {
			t.Errorf("%s: k8s.io/api no longer registers it; move the override to removalFixes", name)
			continue
		}
		if _, deleted := removalFixes[k]; deleted {
			t.Errorf("%s is in both removalFixes and taggedRemovalFixes", name)
		}
		typ, err := scheme.New(schema.GroupVersionKind{Group: k.Group, Version: k.Version, Kind: k.Kind})
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		rm, ok := typ.(removedIface)
		if !ok {
			t.Errorf("%s: upstream tags no removal, so the override is not a correction; add it to untaggedLifecycles or delete it", name)
			continue
		}
		maj, min := rm.APILifecycleRemoved()
		if tag := (version{Major: maj, Minor: min}); !r.before(tag) {
			t.Errorf("%s: upstream tags removal %s, not later than the override %s; delete the override", name, tag, r)
		}
	}
}

// #266: storage.k8s.io/v1alpha1 VolumeAttachment is dated by when
// kube-apiserver stopped serving it (1.23), though k8s.io/api tags 1.24,
// and the removal is marked inferred.
func TestExtractDatesVolumeAttachmentByTheServedRelease(t *testing.T) {
	scheme := runtime.NewScheme()
	for _, add := range addToSchemes {
		if err := add(scheme); err != nil {
			t.Fatal(err)
		}
	}
	entries, _, _ := extract(scheme)
	for _, e := range entries {
		if e.gvk() == (gvkOut{Group: "storage.k8s.io", Version: "v1alpha1", Kind: "VolumeAttachment"}) {
			if e.Removed == nil || *e.Removed != *v(1, 23) || !e.RemovedInferred {
				t.Fatalf("VolumeAttachment v1alpha1 = removed %v (inferred %v), want 1.23 inferred", e.Removed, e.RemovedInferred)
			}
			return
		}
	}
	t.Fatal("extract returned no storage.k8s.io/v1alpha1 VolumeAttachment")
}

func TestFixRemovalOfARegisteredTaggedType(t *testing.T) {
	va := gvkOut{Group: "storage.k8s.io", Version: "v1alpha1", Kind: "VolumeAttachment"}
	e := entry{Group: va.Group, Version: va.Version, Kind: va.Kind, Removed: v(1, 24)}
	fixRemoval(&e)
	if e.Removed == nil || *e.Removed != *v(1, 23) || !e.RemovedInferred {
		t.Errorf("tag 1.24: removed %v inferred %v, want 1.23 inferred", e.Removed, e.RemovedInferred)
	}
	// An upstream tag already at or before the override stands.
	e = entry{Group: va.Group, Version: va.Version, Kind: va.Kind, Removed: v(1, 23)}
	fixRemoval(&e)
	if e.RemovedInferred {
		t.Error("tag 1.23: marked inferred, want upstream's tag kept")
	}
}

func TestDefaultGAReplacements(t *testing.T) {
	g := func(version, kind string) *gvkOut { return &gvkOut{Group: "g.k8s.io", Version: version, Kind: kind} }
	entries := []entry{
		// Removed beta, GA successor: defaulted.
		{Group: "g.k8s.io", Version: "v1beta1", Kind: "Thing", Introduced: *v(1, 28), Deprecated: v(1, 31), Removed: v(1, 34)},
		{Group: "g.k8s.io", Version: "v1", Kind: "Thing", Introduced: *v(1, 30)},
		// The newest GA wins, not the first.
		{Group: "g.k8s.io", Version: "v1alpha1", Kind: "Multi", Introduced: *v(1, 20), Removed: v(1, 22)},
		{Group: "g.k8s.io", Version: "v1", Kind: "Multi", Introduced: *v(1, 22)},
		{Group: "g.k8s.io", Version: "v2", Kind: "Multi", Introduced: *v(1, 30)},
		// A deprecated or removed sibling is no successor.
		{Group: "g.k8s.io", Version: "v1beta1", Kind: "NoGA", Introduced: *v(1, 20), Removed: v(1, 25)},
		{Group: "g.k8s.io", Version: "v1beta2", Kind: "NoGA", Introduced: *v(1, 22), Deprecated: v(1, 26)},
		// An upstream or fixed replacement stands.
		{Group: "g.k8s.io", Version: "v1beta1", Kind: "Tagged", Introduced: *v(1, 20), Removed: v(1, 25), Replacement: g("v1beta2", "Tagged")},
		{Group: "g.k8s.io", Version: "v1beta2", Kind: "Tagged", Introduced: *v(1, 22), Deprecated: v(1, 27), Removed: v(1, 30)},
		{Group: "g.k8s.io", Version: "v1", Kind: "Tagged", Introduced: *v(1, 26)},
		// Neither deprecated nor removed: nothing to migrate from.
		{Group: "g.k8s.io", Version: "v1", Kind: "Alone", Introduced: *v(1, 1)},
		// The group is part of the kind's identity.
		{Group: "h.k8s.io", Version: "v1beta1", Kind: "Thing", Introduced: *v(1, 20), Removed: v(1, 25)},
	}
	changed := defaultGAReplacements(entries)
	var got []string
	for _, c := range changed {
		got = append(got, c.Group+"/"+c.Version+" "+c.Kind+" -> "+c.Replacement.Version)
	}
	want := []string{"g.k8s.io/v1beta1 Thing -> v1", "g.k8s.io/v1alpha1 Multi -> v2", "g.k8s.io/v1beta2 Tagged -> v1"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("defaulted %q, want %q", got, want)
	}
	for i, e := range entries {
		wantFlag := slices.ContainsFunc(changed, func(c entry) bool { return c.gvk() == e.gvk() })
		if e.ReplacementDefaulted != wantFlag {
			t.Errorf("entries[%d] %s: ReplacementDefaulted = %v, want %v (only a defaulted replacement is flagged)", i, e.gvk(), e.ReplacementDefaulted, wantFlag)
		}
	}
	if r := entries[7].Replacement; r == nil || r.Version != "v1beta2" {
		t.Errorf("a tagged replacement was overwritten: %+v", r)
	}
	if r := entries[11].Replacement; r != nil {
		t.Errorf("h.k8s.io Thing got %+v from another group", r)
	}
	// The citations are the migration guide and the successor's release notes.
	cites := replacementCitations(entries[1])
	if len(cites) != 2 || cites[0] != deprecationGuideURL || cites[1] != "https://github.com/kubernetes/kubernetes/blob/master/CHANGELOG/CHANGELOG-1.30.md" {
		t.Errorf("citations = %v", cites)
	}
}

// Every entry whose replacement gen-kb defaults (#266) from the dataset as
// committed: the 33 removed and deprecated alpha and beta types upstream
// tags no replacement on, though their kind went GA. A new one is a refresh
// that found another untagged type; it fails here until it is read, since
// a wrong default is advice to migrate to an API that is not the successor.
func TestDefaultedReplacementsAreListed(t *testing.T) {
	committed, err := readDataset("../../internal/kb/data/apilifecycle.json")
	if err != nil || len(committed) == 0 {
		t.Fatalf("reading the committed dataset: %v (%d entries)", err, len(committed))
	}
	wantList := strings.Fields(`
admissionregistration.k8s.io/v1alpha1/MutatingAdmissionPolicy admissionregistration.k8s.io/v1alpha1/MutatingAdmissionPolicyBinding
admissionregistration.k8s.io/v1alpha1/ValidatingAdmissionPolicy admissionregistration.k8s.io/v1alpha1/ValidatingAdmissionPolicyBinding
admissionregistration.k8s.io/v1beta1/ValidatingAdmissionPolicy admissionregistration.k8s.io/v1beta1/ValidatingAdmissionPolicyBinding
authentication.k8s.io/v1alpha1/SelfSubjectReview authentication.k8s.io/v1beta1/SelfSubjectReview
batch/v2alpha1/CronJob
certificates.k8s.io/v1alpha1/ClusterTrustBundle certificates.k8s.io/v1alpha1/PodCertificateRequest
discovery.k8s.io/v1alpha1/EndpointSlice
networking.k8s.io/v1alpha1/IPAddress networking.k8s.io/v1alpha1/ServiceCIDR networking.k8s.io/v1beta1/IPAddress networking.k8s.io/v1beta1/ServiceCIDR
resource.k8s.io/v1alpha1/ResourceClaim resource.k8s.io/v1alpha1/ResourceClaimTemplate
resource.k8s.io/v1alpha2/ResourceClaim resource.k8s.io/v1alpha2/ResourceClaimTemplate resource.k8s.io/v1alpha2/ResourceSlice
resource.k8s.io/v1alpha3/DeviceTaintRule
resource.k8s.io/v1beta1/DeviceClass resource.k8s.io/v1beta1/ResourceClaim resource.k8s.io/v1beta1/ResourceClaimTemplate resource.k8s.io/v1beta1/ResourceSlice
resource.k8s.io/v1beta2/DeviceClass resource.k8s.io/v1beta2/DeviceTaintRule resource.k8s.io/v1beta2/ResourceClaim resource.k8s.io/v1beta2/ResourceClaimTemplate resource.k8s.io/v1beta2/ResourceSlice
scheduling.k8s.io/v1alpha1/PriorityClass
storagemigration.k8s.io/v1alpha1/StorageVersionMigration`)
	want := map[string]bool{}
	for _, s := range wantList {
		want[s] = true
	}
	// Strip the listed replacements; defaulting must restore exactly them.
	stripped := make([]entry, len(committed))
	for i, e := range committed {
		stripped[i] = e
		if want[e.Group+"/"+e.Version+"/"+e.Kind] {
			stripped[i].Replacement = nil
		}
	}
	var got []string
	for _, c := range defaultGAReplacements(stripped) {
		got = append(got, c.Group+"/"+c.Version+"/"+c.Kind)
		orig := entryOf(committed, c.gvk())
		if !reflect.DeepEqual(orig.Replacement, c.Replacement) {
			t.Errorf("%s: committed replacement %+v, defaulting gives %+v", c.gvk(), orig.Replacement, c.Replacement)
		}
	}
	sort.Strings(got)
	sort.Strings(wantList)
	if !reflect.DeepEqual(got, wantList) {
		t.Errorf("defaulted entries =\n%q\nwant\n%q", got, wantList)
	}
	if len(wantList) != 33 {
		t.Errorf("the list has %d entries, want 33", len(wantList))
	}
}

// #332: the introduced minor of a tombstone older than the history gen-kb
// reads is only a bound (clamped to 1.17), and k8s.io/api tags
// discovery.k8s.io/v1beta1 EndpointSlice with the release of v1alpha1.
func TestFixIntroduced(t *testing.T) {
	cases := []struct {
		name string
		in   entry
		want version
	}{
		{"clamped alpha", entry{Group: "batch", Version: "v2alpha1", Kind: "CronJob", Introduced: *v(1, 17)}, *v(1, 5)},
		{"clamped alpha PriorityClass", entry{Group: "scheduling.k8s.io", Version: "v1alpha1", Kind: "PriorityClass", Introduced: *v(1, 17)}, *v(1, 8)},
		{"clamped alpha PodPreset", entry{Group: "settings.k8s.io", Version: "v1alpha1", Kind: "PodPreset", Introduced: *v(1, 17)}, *v(1, 6)},
		{"EndpointSlice v1alpha1 is 1.16", entry{Group: "discovery.k8s.io", Version: "v1alpha1", Kind: "EndpointSlice", Introduced: *v(1, 17)}, *v(1, 16)},
		{"EndpointSlice v1beta1 is 1.17", entry{Group: "discovery.k8s.io", Version: "v1beta1", Kind: "EndpointSlice", Introduced: *v(1, 16)}, *v(1, 17)},
		{"no override untouched", entry{Group: "batch", Version: "v1beta1", Kind: "CronJob", Introduced: *v(1, 8)}, *v(1, 8)},
	}
	for _, c := range cases {
		e := c.in
		fixIntroduced(&e)
		if e.Introduced != c.want {
			t.Errorf("%s: Introduced = %s, want %s", c.name, e.Introduced, c.want)
		}
	}
}

// Each introducedFixes entry names the changelog of the release it states,
// and the committed dataset holds that value, so regenerating keeps it.
func TestIntroducedFixesAreCitedAndInTheDataset(t *testing.T) {
	committed, err := readDataset("../../internal/kb/data/apilifecycle.json")
	if err != nil {
		t.Fatal(err)
	}
	if len(introducedFixes) == 0 {
		t.Fatal("introducedFixes is empty")
	}
	for k, f := range introducedFixes {
		name := k.Group + "/" + k.Version + " " + k.Kind
		if want := changelog(f.introduced.Minor); f.citation != want || f.introduced.Major != 1 {
			t.Errorf("%s: citation %q for introduced %s, want %q", name, f.citation, f.introduced, want)
		}
		if got := entryOf(committed, k); got.Kind == "" || got.Introduced != f.introduced {
			t.Errorf("%s: committed dataset has introduced %s (present: %v), want %s; run make gen-kb", name, got.Introduced, got.Kind != "", f.introduced)
		}
	}
}
