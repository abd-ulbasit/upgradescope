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
	fs := evalAPIUsage(inv, testKB(), inventory.Version{Major: 1, Minor: 22})
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
	fs := evalAPIUsage(inv, testKB(), inventory.Version{Major: 1, Minor: 21})
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
		fs := evalAPIUsage(inv, testKB(), target)
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
	fs := evalAPIUsage(inv, testKB(), inventory.Version{Major: 1, Minor: 22})
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
		fs := evalAPIUsage(tc.inv, k, tc.target)
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
	fs := evalAPIUsage(inv, testKB(), inventory.Version{Major: 1, Minor: 34})
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
	fs := evalAPIUsage(inv, testKB(), inventory.Version{Major: 1, Minor: 25})
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
	fs := evalAPIUsage(inv, testKB(), inventory.Version{Major: 1, Minor: 25})
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
	fs := evalAPIUsage(inv, testKB(), inventory.Version{Major: 1, Minor: 25})
	if len(fs) != 1 {
		t.Fatalf("want 1 finding, got %d", len(fs))
	}
	if want := "5 object(s) written through this API version: default (5). Written by (first 2 of 5 objects): helm."; fs[0].Detail != want {
		t.Errorf("detail = %q, want %q", fs[0].Detail, want)
	}
}

func TestEvalAPIUsageUnknownGVKIgnored(t *testing.T) {
	inv := inventory.Inventory{
		APIUsage: []inventory.APIUsage{{Group: "apps", Version: "v1", Kind: "Deployment", Count: 5}},
	}
	if fs := evalAPIUsage(inv, testKB(), inventory.Version{Major: 1, Minor: 34}); len(fs) != 0 {
		t.Fatalf("GVKs absent from the KB must produce no findings, got %+v", fs)
	}
}
