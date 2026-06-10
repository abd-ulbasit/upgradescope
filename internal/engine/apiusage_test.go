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
	if fs[0].Title != "extensions/v1beta1 Ingress removed in 1.22 (1 objects)" {
		t.Fatalf("title = %q", fs[0].Title)
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
	if fs[0].Title != "batch/v1beta1 CronJob deprecated since 1.21 (1 objects)" {
		t.Fatalf("title = %q", fs[0].Title)
	}
	if fs[0].Remediation != "migrate to batch/v1 CronJob" {
		t.Fatalf("remediation = %q", fs[0].Remediation)
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
	if fs[0].Title != "v1 ComponentStatus deprecated since 1.19 (1 objects)" {
		t.Fatalf("core group must render as bare version; title = %q", fs[0].Title)
	}
	if fs[0].Detail != "1 object(s) still stored/served at this version: cluster-scoped (1)." {
		t.Fatalf("detail = %q", fs[0].Detail)
	}
	if len(fs[0].Namespaces) != 0 {
		t.Fatalf("cluster-scoped key must not leak into Namespaces: %v", fs[0].Namespaces)
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
