package engine

import (
	"reflect"
	"testing"

	"github.com/abd-ulbasit/upgradescope/internal/inventory"
	"github.com/abd-ulbasit/upgradescope/internal/kb"
)

func vp(major, minor int) *inventory.Version { return &inventory.Version{Major: major, Minor: minor} }

func testKB() kb.KB {
	return kb.KB{
		Version: "test-kb-1",
		APILifecycle: []kb.APILifecycleEntry{
			{
				Group: "extensions", Version: "v1beta1", Kind: "Ingress",
				Introduced: inventory.Version{Major: 1, Minor: 1},
				Deprecated: vp(1, 14), Removed: vp(1, 22),
				Replacement: &kb.GVK{Group: "networking.k8s.io", Version: "v1", Kind: "Ingress"},
			},
			{
				Group: "networking.k8s.io", Version: "v1beta1", Kind: "Ingress",
				Introduced: inventory.Version{Major: 1, Minor: 14},
				Deprecated: vp(1, 19), Removed: vp(1, 22),
				Replacement: &kb.GVK{Group: "networking.k8s.io", Version: "v1", Kind: "Ingress"},
			},
			{
				Group: "batch", Version: "v1beta1", Kind: "CronJob",
				Introduced: inventory.Version{Major: 1, Minor: 8},
				Deprecated: vp(1, 21), Removed: vp(1, 25),
				Replacement: &kb.GVK{Group: "batch", Version: "v1", Kind: "CronJob"},
			},
			{
				Group: "", Version: "v1", Kind: "ComponentStatus",
				Introduced: inventory.Version{Major: 1, Minor: 0},
				Deprecated: vp(1, 19), // never removed upstream
			},
		},
		Skew:        kb.DefaultSkewPolicy(),
		MaxKnownK8s: inventory.Version{Major: 1, Minor: 36},
	}
}

func testNamespaces() []inventory.NamespaceInfo {
	return []inventory.NamespaceInfo{
		{Name: "default", Team: "core"},
		{Name: "shop", Team: "storefront"},
	}
}

func TestEvalAPIUsageRemovedAtTarget(t *testing.T) {
	inv := inventory.Inventory{
		APIUsage: []inventory.APIUsage{{
			Group: "extensions", Version: "v1beta1", Kind: "Ingress",
			Count: 3, Namespaces: map[string]int{"default": 2, "shop": 1},
		}},
		Namespaces: testNamespaces(),
	}
	fs := evalAPIUsage(inv, testKB(), inventory.Version{Major: 1, Minor: 22}, nil)
	if len(fs) != 1 {
		t.Fatalf("want 1 finding, got %d", len(fs))
	}
	want := Finding{
		Category:    CatRemovedAPI,
		Severity:    SevBlocker,
		Key:         "removed-api/extensions/v1beta1/Ingress",
		Title:       "extensions/v1beta1 Ingress removed in 1.22 (3 objects)",
		Detail:      "3 object(s) still stored/served at this version: default (2), shop (1).",
		Teams:       []string{"core", "storefront"},
		Namespaces:  []string{"default", "shop"},
		Remediation: "migrate to networking.k8s.io/v1 Ingress",
		Citations:   []string{deprecationGuideURL},
	}
	if !reflect.DeepEqual(fs[0], want) {
		t.Fatalf("finding mismatch:\n got %+v\nwant %+v", fs[0], want)
	}
}

func TestEvalAPIUsageRemovedAtTargetPlusOne(t *testing.T) {
	inv := inventory.Inventory{
		APIUsage: []inventory.APIUsage{{
			Group: "extensions", Version: "v1beta1", Kind: "Ingress",
			Count: 1, Namespaces: map[string]int{"default": 1},
		}},
		Namespaces: testNamespaces(),
	}
	// removed in 1.22, target 1.21 → removal lands at target+1 → warning
	fs := evalAPIUsage(inv, testKB(), inventory.Version{Major: 1, Minor: 21}, nil)
	if len(fs) != 1 || fs[0].Severity != SevWarning || fs[0].Category != CatRemovedAPI {
		t.Fatalf("want one removed-api warning, got %+v", fs)
	}
	if fs[0].Title != "extensions/v1beta1 Ingress removed in 1.22 (1 object)" {
		t.Fatalf("title = %q", fs[0].Title)
	}
	if fs[0].Key != "removed-api/extensions/v1beta1/Ingress" {
		t.Fatalf("key = %q", fs[0].Key)
	}
}

// TestEvalAPIUsageKeyIsCountFree pins the notification-identity contract:
// the same GVK with a different object count keeps the SAME key, so count
// fluctuations never re-alert as a "new" blocker downstream.
func TestEvalAPIUsageKeyIsCountFree(t *testing.T) {
	target := inventory.Version{Major: 1, Minor: 22}
	mk := func(count int) Finding {
		inv := inventory.Inventory{
			APIUsage: []inventory.APIUsage{{
				Group: "extensions", Version: "v1beta1", Kind: "Ingress",
				Count: count, Namespaces: map[string]int{"default": count},
			}},
		}
		fs := evalAPIUsage(inv, testKB(), target, nil)
		if len(fs) != 1 {
			t.Fatalf("want 1 finding, got %d", len(fs))
		}
		return fs[0]
	}
	three, two := mk(3), mk(2)
	if three.Title == two.Title {
		t.Fatal("titles should differ (they embed counts) for this test to be meaningful")
	}
	if three.Key == "" || three.Key != two.Key {
		t.Fatalf("keys must be equal and count-free: %q vs %q", three.Key, two.Key)
	}
}

func TestEvalAPIUsageDeprecatedBeyondWindowIsInfo(t *testing.T) {
	inv := inventory.Inventory{
		APIUsage: []inventory.APIUsage{{
			Group: "batch", Version: "v1beta1", Kind: "CronJob",
			Count: 1, Namespaces: map[string]int{"default": 1},
		}},
		Namespaces: testNamespaces(),
	}
	// removed in 1.25, target 1.22 → beyond target+1 → info, deprecated-api
	fs := evalAPIUsage(inv, testKB(), inventory.Version{Major: 1, Minor: 22}, nil)
	if len(fs) != 1 || fs[0].Severity != SevInfo || fs[0].Category != CatDeprecatedAPI {
		t.Fatalf("want one deprecated-api info, got %+v", fs)
	}
	if fs[0].Title != "batch/v1beta1 CronJob deprecated since 1.21 (1 object)" {
		t.Fatalf("title = %q", fs[0].Title)
	}
	if fs[0].Key != "deprecated-api/batch/v1beta1/CronJob" {
		t.Fatalf("key = %q", fs[0].Key)
	}
	if fs[0].Remediation != "migrate to batch/v1 CronJob" {
		t.Fatalf("remediation = %q", fs[0].Remediation)
	}
}

// A deprecation that comes after the target is not "since": the title says
// it is later than the target, and "projected" when the release is beyond
// the KB horizon (k8s.io/api's lifecycle markers project it for betas).
// Real KB: scheduling.k8s.io/v1beta1 Workload deprecated 1.40, horizon
// 1.37; coordination.k8s.io/v1beta1 LeaseCandidate deprecated 1.36.
func TestEvalAPIUsageFutureDeprecationTitle(t *testing.T) {
	k, err := kb.Load()
	if err != nil {
		t.Fatal(err)
	}
	usage := func(group, version, kind string) inventory.Inventory {
		return inventory.Inventory{APIUsage: []inventory.APIUsage{{
			Group: group, Version: version, Kind: kind, Count: 1, Namespaces: map[string]int{"default": 1},
		}}}
	}
	cases := []struct {
		inv    inventory.Inventory
		target inventory.Version
		want   string
	}{
		{usage("scheduling.k8s.io", "v1beta1", "Workload"), inventory.Version{Major: 1, Minor: 37},
			"scheduling.k8s.io/v1beta1 Workload deprecated in 1.40 (projected), after target 1.37 (1 object)"},
		{usage("coordination.k8s.io", "v1beta1", "LeaseCandidate"), inventory.Version{Major: 1, Minor: 34},
			"coordination.k8s.io/v1beta1 LeaseCandidate deprecated in 1.36, after target 1.34 (1 object)"},
		{usage("coordination.k8s.io", "v1beta1", "LeaseCandidate"), inventory.Version{Major: 1, Minor: 36},
			"coordination.k8s.io/v1beta1 LeaseCandidate deprecated since 1.36 (1 object)"},
		// Deprecated at the horizon itself: a known release, not projected.
		{usage("admissionregistration.k8s.io", "v1beta1", "MutatingAdmissionPolicy"), inventory.Version{Major: 1, Minor: 35},
			"admissionregistration.k8s.io/v1beta1 MutatingAdmissionPolicy deprecated in 1.37, after target 1.35 (1 object)"},
	}
	for _, tc := range cases {
		fs := evalAPIUsage(tc.inv, k, tc.target, nil)
		if len(fs) != 1 || fs[0].Severity != SevInfo || fs[0].Category != CatDeprecatedAPI {
			t.Fatalf("want one deprecated-api info, got %+v", fs)
		}
		if fs[0].Title != tc.want {
			t.Errorf("title = %q, want %q", fs[0].Title, tc.want)
		}
	}
}

func TestEvalAPIUsageCoreGroupRendering(t *testing.T) {
	inv := inventory.Inventory{
		APIUsage: []inventory.APIUsage{{
			Group: "", Version: "v1", Kind: "ComponentStatus",
			Count: 1, Namespaces: map[string]int{"": 1}, // cluster-scoped
		}},
	}
	fs := evalAPIUsage(inv, testKB(), inventory.Version{Major: 1, Minor: 34}, nil)
	if len(fs) != 1 {
		t.Fatalf("want 1 finding, got %d", len(fs))
	}
	if fs[0].Title != "v1 ComponentStatus deprecated since 1.19 (1 object)" {
		t.Fatalf("core group must render as bare version; title = %q", fs[0].Title)
	}
	if fs[0].Key != "deprecated-api/core/v1/ComponentStatus" {
		t.Fatalf("core group must render as \"core\" in keys; key = %q", fs[0].Key)
	}
	if fs[0].Detail != "1 object(s) still stored/served at this version: cluster-scoped (1)." {
		t.Fatalf("detail = %q", fs[0].Detail)
	}
	if len(fs[0].Namespaces) != 0 {
		t.Fatalf("cluster-scoped key must not leak into Namespaces: %v", fs[0].Namespaces)
	}
}

// Manifest objects (refs with a line) carry through to the finding in a
// deterministic order, and the detail uses manifest wording: an empty
// namespace in a manifest means metadata.namespace is unset, not that the
// object is cluster-scoped, and nothing is "stored/served" yet.
func TestEvalAPIUsageManifestObjects(t *testing.T) {
	inv := inventory.Inventory{
		Source: inventory.SourceFiles,
		APIUsage: []inventory.APIUsage{{
			Group: "batch", Version: "v1beta1", Kind: "CronJob",
			Count: 4, Namespaces: map[string]int{"": 2, "shop": 1, "jobs": 1},
			Objects: []inventory.ObjectRef{
				{Name: "z", File: "b.yaml", Line: 9},
				{Namespace: "shop", Name: "a", File: "b.yaml", Line: 1},
				{Name: "y", File: "a.yaml", Line: 3, RenderedFrom: "demo/templates/cron.yaml"},
			},
			ObjectsOmitted: 1,
		}},
	}
	fs := evalAPIUsage(inv, testKB(), inventory.Version{Major: 1, Minor: 25}, nil)
	if len(fs) != 1 {
		t.Fatalf("want 1 finding, got %d", len(fs))
	}
	f := fs[0]
	if want := "4 manifest object(s) use this API: namespace unset (2), jobs (1), shop (1)."; f.Detail != want {
		t.Errorf("detail = %q, want %q", f.Detail, want)
	}
	wantObjs := []inventory.ObjectRef{
		{Name: "y", File: "a.yaml", Line: 3, RenderedFrom: "demo/templates/cron.yaml"},
		{Namespace: "shop", Name: "a", File: "b.yaml", Line: 1},
		{Name: "z", File: "b.yaml", Line: 9},
	}
	if !reflect.DeepEqual(f.Objects, wantObjs) || f.ObjectsOmitted != 1 {
		t.Errorf("objects = %+v (omitted %d)\nwant      %+v (omitted 1)", f.Objects, f.ObjectsOmitted, wantObjs)
	}
	// The finding owns its slice: sorting must not reorder the inventory.
	if inv.APIUsage[0].Objects[0].Name != "z" {
		t.Error("evalAPIUsage mutated the inventory's Objects")
	}
}

// A cluster's row the gate upserted manifest objects into can list only
// manifests while it counts cluster objects it does not list (the
// collector's cap, or a team-scoped share whose objects were past it): it
// is not worded as manifests alone, nor as stored objects alone. Every
// counted object listed, or a files-mode row, is manifests' as before.
func TestEvalAPIUsageManifestsInAClusterRow(t *testing.T) {
	row := inventory.APIUsage{
		Group: "extensions", Version: "v1beta1", Kind: "Ingress",
		Count: 3, Namespaces: map[string]int{"pay-prod": 3},
		Objects:        []inventory.ObjectRef{{Namespace: "pay-prod", Name: "pr", File: "", Line: 1}},
		ObjectsOmitted: 2,
	}
	target := inventory.Version{Major: 1, Minor: 35}
	for _, c := range []struct {
		name string
		src  inventory.Source
		mod  func(*inventory.APIUsage)
		want string
	}{
		{"cluster objects unlisted", inventory.SourceCluster, func(*inventory.APIUsage) {}, "3 object(s) use this API: pay-prod (3)."},
		{"v0.1 push, no source", "", func(*inventory.APIUsage) {}, "3 object(s) use this API: pay-prod (3)."},
		{"cluster objects unidentified", inventory.SourceCluster, func(u *inventory.APIUsage) { u.ObjectsOmitted = 0 }, "3 object(s) use this API: pay-prod (3)."},
		{"every object listed", inventory.SourceCluster, func(u *inventory.APIUsage) {
			u.Count, u.Namespaces, u.ObjectsOmitted = 1, map[string]int{"pay-prod": 1}, 0
		}, "1 manifest object(s) use this API: pay-prod (1)."},
		{"files mode over the cap", inventory.SourceFiles, func(*inventory.APIUsage) {}, "3 manifest object(s) use this API: pay-prod (3)."},
	} {
		u := row
		c.mod(&u)
		fs := evalAPIUsage(inventory.Inventory{Source: c.src, APIUsage: []inventory.APIUsage{u}}, testKB(), target, nil)
		if len(fs) != 1 || fs[0].Detail != c.want {
			t.Errorf("%s: findings %+v, want one with detail %q", c.name, fs, c.want)
		}
	}
	// Its unset namespace may be either.
	u := row
	u.Namespaces = map[string]int{"": 3}
	fs := evalAPIUsage(inventory.Inventory{Source: inventory.SourceCluster, APIUsage: []inventory.APIUsage{u}}, testKB(), target, nil)
	if want := "3 object(s) use this API: cluster-scoped or namespace unset (3)."; len(fs) != 1 || fs[0].Detail != want {
		t.Errorf("findings %+v, want one with detail %q", fs, want)
	}
}

// Live objects flagged by authorship name who wrote them; the managers are
// deduped and sorted.
func TestEvalAPIUsageAuthoredObjects(t *testing.T) {
	inv := inventory.Inventory{
		APIUsage: []inventory.APIUsage{{
			Group: "batch", Version: "v1beta1", Kind: "CronJob",
			Count: 3, Namespaces: map[string]int{"default": 3},
			Objects: []inventory.ObjectRef{
				{Namespace: "default", Name: "c", Manager: "kubectl last-applied"},
				{Namespace: "default", Name: "a", Manager: "helm"},
				{Namespace: "default", Name: "b", Manager: "helm"},
			},
		}},
	}
	fs := evalAPIUsage(inv, testKB(), inventory.Version{Major: 1, Minor: 25}, nil)
	if len(fs) != 1 {
		t.Fatalf("want 1 finding, got %d", len(fs))
	}
	if want := "3 object(s) written through this API version: default (3). Written by: helm, kubectl last-applied."; fs[0].Detail != want {
		t.Errorf("detail = %q, want %q", fs[0].Detail, want)
	}
}

// The collector keeps at most inventory.MaxObjectRefs refs, so when some
// were dropped the managers come from a subset and the detail says so:
// another writer may hide among the omitted objects.
func TestEvalAPIUsageAuthoredObjectsOmittedRefs(t *testing.T) {
	inv := inventory.Inventory{
		APIUsage: []inventory.APIUsage{{
			Group: "batch", Version: "v1beta1", Kind: "CronJob",
			Count: 5, Namespaces: map[string]int{"default": 5},
			Objects: []inventory.ObjectRef{
				{Namespace: "default", Name: "a", Manager: "helm"},
				{Namespace: "default", Name: "b", Manager: "helm"},
			},
			ObjectsOmitted: 3,
		}},
	}
	fs := evalAPIUsage(inv, testKB(), inventory.Version{Major: 1, Minor: 25}, nil)
	if len(fs) != 1 {
		t.Fatalf("want 1 finding, got %d", len(fs))
	}
	if want := "5 object(s) written through this API version: default (5). Written by (of 5 objects, not all identified): helm."; fs[0].Detail != want {
		t.Errorf("detail = %q, want %q", fs[0].Detail, want)
	}
}

// A tombstone (a type upstream deleted, RemovedInferred) is judged like
// any removal: blocker from the release that removed it, warning one
// release before (#124: a v1alpha3 DeviceClass at 1.34 read as ready).
func TestEvalAPIUsageTombstoneBoundary(t *testing.T) {
	k := testKB()
	k.APILifecycle = append(k.APILifecycle, kb.APILifecycleEntry{
		Group: "resource.k8s.io", Version: "v1alpha3", Kind: "DeviceClass",
		Introduced: inventory.Version{Major: 1, Minor: 31}, Deprecated: vp(1, 34),
		Removed: vp(1, 34), RemovedInferred: true,
	})
	inv := inventory.Inventory{APIUsage: []inventory.APIUsage{{
		Group: "resource.k8s.io", Version: "v1alpha3", Kind: "DeviceClass", Count: 1,
		Objects: []inventory.ObjectRef{{Name: "gpu", File: "dra.yaml", Line: 1}},
	}}}
	cases := []struct {
		target int
		want   []string
	}{
		{35, []string{"blocker removed-api/resource.k8s.io/v1alpha3/DeviceClass resource.k8s.io/v1alpha3 DeviceClass removed in 1.34 (1 object)"}},
		{34, []string{"blocker removed-api/resource.k8s.io/v1alpha3/DeviceClass resource.k8s.io/v1alpha3 DeviceClass removed in 1.34 (1 object)"}},
		{33, []string{"warning removed-api/resource.k8s.io/v1alpha3/DeviceClass resource.k8s.io/v1alpha3 DeviceClass removed in 1.34 (1 object)"}},
	}
	for _, c := range cases {
		var got []string
		for _, f := range evalAPIUsage(inv, k, inventory.Version{Major: 1, Minor: c.target}, nil) {
			got = append(got, string(f.Severity)+" "+f.Key+" "+f.Title)
		}
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("target 1.%d: findings %q, want %q", c.target, got, c.want)
		}
	}
}

// A GVK the KB does not know is judged by its group. In a group the KB
// has entries for (core here), it is a built-in API the KB has no lifecycle data for:
// one upstream deleted, a typo, or a type never tagged. Dropping it read
// as ready, so it is an unscored info finding (#124 KB-01). A CRD or
// aggregated API group stays silent: the KB never covers those.
func TestEvalAPIUsageUnknownGVK(t *testing.T) {
	inv := inventory.Inventory{
		APIUsage: []inventory.APIUsage{
			// CRD and aggregated-API groups the KB has no entries in.
			{Group: "cert-manager.io", Version: "v1", Kind: "Certificate", Count: 5},
			{Group: "metrics.k8s.io", Version: "v1beta1", Kind: "PodMetrics", Count: 1},
			// Known group, unknown version.
			{Group: "batch", Version: "v2alpha1", Kind: "CronJob", Count: 2,
				Namespaces: map[string]int{"default": 1, "": 1},
				Objects: []inventory.ObjectRef{
					{Namespace: "default", Name: "b", File: "jobs.yaml", Line: 9},
					{Name: "a", File: "jobs.yaml", Line: 1},
				}},
			// Core group, unknown kind.
			{Group: "", Version: "v1", Kind: "PodPreset", Count: 1, Namespaces: map[string]int{"shop": 1}},
		},
		Namespaces: testNamespaces(),
	}
	fs := evalAPIUsage(inv, testKB(), inventory.Version{Major: 1, Minor: 34}, nil)
	want := []Finding{
		{
			Category: CatUnknownAPI, Severity: SevInfo,
			Key:        "unknown-api/batch/v2alpha1/CronJob",
			Title:      "batch/v2alpha1 CronJob is not in the knowledge base (2 objects)",
			Detail:     "2 manifest object(s) use this API: namespace unset (1), default (1). The knowledge base has no lifecycle data for this built-in API: it may have been removed, so check that Kubernetes 1.34 serves it.",
			Teams:      []string{"core"},
			Namespaces: []string{"default"},
			Citations:  []string{deprecationGuideURL},
			Objects: []inventory.ObjectRef{
				{Name: "a", File: "jobs.yaml", Line: 1},
				{Namespace: "default", Name: "b", File: "jobs.yaml", Line: 9},
			},
		},
		{
			Category: CatUnknownAPI, Severity: SevInfo,
			Key:        "unknown-api/core/v1/PodPreset",
			Title:      "v1 PodPreset is not in the knowledge base (1 object)",
			Detail:     "1 object(s) still stored/served at this version: shop (1). The knowledge base has no lifecycle data for this built-in API: it may have been removed, so check that Kubernetes 1.34 serves it.",
			Teams:      []string{"storefront"},
			Namespaces: []string{"shop"},
			Citations:  []string{deprecationGuideURL},
		},
	}
	if !reflect.DeepEqual(fs, want) {
		t.Fatalf("findings =\n%+v\nwant\n%+v", fs, want)
	}
	// Known GVKs that are neither deprecated nor removed stay silent too.
	k := testKB()
	k.APILifecycle = append(k.APILifecycle, kb.APILifecycleEntry{Group: "batch", Version: "v1", Kind: "CronJob",
		Introduced: inventory.Version{Major: 1, Minor: 21}})
	inv.APIUsage = []inventory.APIUsage{{Group: "batch", Version: "v1", Kind: "CronJob", Count: 3}}
	if fs := evalAPIUsage(inv, k, inventory.Version{Major: 1, Minor: 34}, nil); len(fs) != 0 {
		t.Errorf("a known, current API must produce no findings, got %+v", fs)
	}
}

// A built-in group is one the KB lists in BuiltinGroups even when it has no
// lifecycle entry: internal.apiserver.k8s.io StorageVersion and
// imagepolicy.k8s.io ImageReview are registered in k8s.io/api but carry no
// lifecycle markers, so they gave no finding at all, like a CRD (#172). A
// KB without the list (a dataset from before it) behaves as it did.
func TestEvalAPIUsageUnknownGVKInGroupWithoutEntries(t *testing.T) {
	inv := inventory.Inventory{
		APIUsage: []inventory.APIUsage{
			{Group: "internal.apiserver.k8s.io", Version: "v1alpha1", Kind: "StorageVersion", Count: 1,
				Objects: []inventory.ObjectRef{{Name: "sv", File: "sv.yaml", Line: 1}}},
			{Group: "imagepolicy.k8s.io", Version: "v1alpha1", Kind: "ImageReview", Count: 1,
				Objects: []inventory.ObjectRef{{Name: "ir", File: "ir.yaml", Line: 1}}},
			{Group: "cert-manager.io", Version: "v1", Kind: "Certificate", Count: 1},
			{Group: "metrics.k8s.io", Version: "v1beta1", Kind: "PodMetrics", Count: 1},
		},
	}
	target := inventory.Version{Major: 1, Minor: 34}

	if fs := evalAPIUsage(inv, testKB(), target, nil); len(fs) != 0 {
		t.Errorf("a KB without builtinGroups must keep today's behaviour, got %+v", fs)
	}

	k := testKB()
	k.BuiltinGroups = []kb.BuiltinGroup{
		{Group: "imagepolicy.k8s.io", Versions: []string{"v1alpha1"}},
		{Group: "internal.apiserver.k8s.io", Versions: []string{"v1alpha1"}},
	}
	var got []string
	for _, f := range evalAPIUsage(inv, k, target, nil) {
		if f.Category != CatUnknownAPI || f.Severity != SevInfo {
			t.Errorf("%s: %s/%s, want an unknown-api info", f.Key, f.Category, f.Severity)
		}
		got = append(got, f.Key+" | "+f.Title)
	}
	want := []string{
		"unknown-api/internal.apiserver.k8s.io/v1alpha1/StorageVersion | internal.apiserver.k8s.io/v1alpha1 StorageVersion is not in the knowledge base (1 object)",
		"unknown-api/imagepolicy.k8s.io/v1alpha1/ImageReview | imagepolicy.k8s.io/v1alpha1 ImageReview is not in the knowledge base (1 object)",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("findings = %q, want %q (CRD and aggregated groups stay silent)", got, want)
	}
}

// The same against the shipped dataset: manifests of a registered type with
// no lifecycle data are unknown-api infos, a CRD group's are silent.
func TestEvalAPIUsageUnknownGVKEmbeddedKB(t *testing.T) {
	k, err := kb.Load()
	if err != nil {
		t.Fatal(err)
	}
	inv := inventory.Inventory{
		APIUsage: []inventory.APIUsage{
			{Group: "internal.apiserver.k8s.io", Version: "v1alpha1", Kind: "StorageVersion", Count: 1},
			{Group: "imagepolicy.k8s.io", Version: "v1alpha1", Kind: "ImageReview", Count: 1},
			{Group: "cert-manager.io", Version: "v1", Kind: "Certificate", Count: 1},
		},
	}
	var got []string
	for _, f := range evalAPIUsage(inv, k, inventory.Version{Major: 1, Minor: 34}, nil) {
		got = append(got, string(f.Severity)+" "+f.Key)
	}
	want := []string{
		"info unknown-api/internal.apiserver.k8s.io/v1alpha1/StorageVersion",
		"info unknown-api/imagepolicy.k8s.io/v1alpha1/ImageReview",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("findings = %q, want %q", got, want)
	}
}
