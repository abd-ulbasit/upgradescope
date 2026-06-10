package engine

import (
	"reflect"
	"testing"
	"time"

	"github.com/abd-ulbasit/upgradescope/internal/inventory"
	"github.com/abd-ulbasit/upgradescope/internal/kb"
	"github.com/abd-ulbasit/upgradescope/registry"
)

var testNow = time.Date(2026, 6, 10, 0, 0, 0, 0, time.UTC)

func testRegistryKB() kb.KB {
	return kb.KB{
		Version: "test-kb-1",
		AddOns: []registry.AddOn{
			{
				SchemaVersion: 1,
				ID:            "ingress-nginx",
				DisplayName:   "ingress-nginx",
				Matchers:      registry.Matchers{Charts: []string{"ingress-nginx"}},
				Support: registry.Support{
					Status:    "eol",
					EOLDate:   "2026-03-26",
					Citations: []string{"https://kubernetes.io/blog/2025/11/11/ingress-nginx-retirement/"},
				},
				Compat: []registry.Compat{{
					Range:     ">=4.0.0",
					K8sMin:    "1.21",
					K8sMax:    "1.33",
					Citations: []string{"https://github.com/kubernetes/ingress-nginx#supported-versions-table"},
				}},
				Recommendation: "migrate to Gateway API (ingress2gateway) or another maintained ingress controller",
			},
			{
				SchemaVersion: 1,
				ID:            "legacy-mesh",
				DisplayName:   "Legacy Mesh",
				Matchers:      registry.Matchers{Images: []string{"example.com/legacy-mesh"}},
				Support: registry.Support{
					Status:    "supported",
					EOLDate:   "2026-08-15", // 66 days after testNow → inside 90-day window
					Citations: []string{"https://endoflife.date/legacy-mesh"},
				},
			},
		},
		Skew:        kb.DefaultSkewPolicy(),
		MaxKnownK8s: inventory.Version{Major: 1, Minor: 36},
	}
}

func TestEvalAddOnsEOLBlocker(t *testing.T) {
	inv := inventory.Inventory{
		AddOns:     []inventory.AddOnInstance{{ID: "ingress-nginx", Version: "4.7.1", Namespaces: []string{"ingress-nginx"}, Source: "chart"}},
		Namespaces: []inventory.NamespaceInfo{{Name: "ingress-nginx", Team: "platform"}},
	}
	// target 1.30: K8sMax 1.33 ≥ target → no compat finding; EOL blocker only.
	fs := evalAddOns(inv, testRegistryKB(), inventory.Version{Major: 1, Minor: 30}, testNow)
	want := []Finding{{
		Category:    CatEOLAddon,
		Severity:    SevBlocker,
		Title:       "ingress-nginx is end-of-life since 2026-03-26",
		Detail:      "Detected ingress-nginx version 4.7.1 via chart in namespace(s): ingress-nginx. Upstream support has ended.",
		Teams:       []string{"platform"},
		Namespaces:  []string{"ingress-nginx"},
		Remediation: "migrate to Gateway API (ingress2gateway) or another maintained ingress controller",
		Citations:   []string{"https://kubernetes.io/blog/2025/11/11/ingress-nginx-retirement/"},
	}}
	if !reflect.DeepEqual(fs, want) {
		t.Fatalf("findings mismatch:\n got %+v\nwant %+v", fs, want)
	}
}

func TestEvalAddOnsApproachingEOLWarning(t *testing.T) {
	inv := inventory.Inventory{
		AddOns: []inventory.AddOnInstance{{ID: "legacy-mesh", Version: "1.2.0", Namespaces: []string{"mesh"}, Source: "image"}},
	}
	fs := evalAddOns(inv, testRegistryKB(), inventory.Version{Major: 1, Minor: 34}, testNow)
	if len(fs) != 1 || fs[0].Category != CatEOLApproaching || fs[0].Severity != SevWarning {
		t.Fatalf("want one eol-approaching warning, got %+v", fs)
	}
	if fs[0].Title != "Legacy Mesh reaches end-of-life on 2026-08-15" {
		t.Fatalf("title = %q", fs[0].Title)
	}
	// Same fixture, now pushed past the window → no finding at all.
	later := testNow.AddDate(0, 0, -100) // 90-day window not yet open
	if fs := evalAddOns(inv, testRegistryKB(), inventory.Version{Major: 1, Minor: 34}, later); len(fs) != 0 {
		t.Fatalf("outside window must yield nothing, got %+v", fs)
	}
}

func TestEvalAddOnsChartIncompatBlocker(t *testing.T) {
	inv := inventory.Inventory{
		AddOns:     []inventory.AddOnInstance{{ID: "ingress-nginx", Version: "4.7.1", Namespaces: []string{"ingress-nginx"}, Source: "chart"}},
		Namespaces: []inventory.NamespaceInfo{{Name: "ingress-nginx", Team: "platform"}},
	}
	// target 1.38 > K8sMax 1.33 → compat blocker in addition to the EOL blocker.
	fs := evalAddOns(inv, testRegistryKB(), inventory.Version{Major: 1, Minor: 38}, testNow)
	if len(fs) != 2 {
		t.Fatalf("want eol + compat findings, got %d: %+v", len(fs), fs)
	}
	compat := fs[1]
	if compat.Category != CatChartIncompat || compat.Severity != SevBlocker {
		t.Fatalf("got %+v", compat)
	}
	if compat.Title != "ingress-nginx 4.7.1 supports Kubernetes up to 1.33 (target 1.38)" {
		t.Fatalf("title = %q", compat.Title)
	}
	if compat.Detail != `Installed version 4.7.1 matches compatibility range ">=4.0.0", which supports Kubernetes 1.21 through 1.33.` {
		t.Fatalf("detail = %q", compat.Detail)
	}
	if !reflect.DeepEqual(compat.Citations, []string{"https://github.com/kubernetes/ingress-nginx#supported-versions-table"}) {
		t.Fatalf("citations = %v", compat.Citations)
	}
}

func TestEvalAddOnsNoVersionSkipsCompat(t *testing.T) {
	inv := inventory.Inventory{
		AddOns: []inventory.AddOnInstance{{ID: "ingress-nginx", Version: "", Namespaces: []string{"ingress-nginx"}, Source: "image"}},
	}
	fs := evalAddOns(inv, testRegistryKB(), inventory.Version{Major: 1, Minor: 38}, testNow)
	if len(fs) != 1 || fs[0].Category != CatEOLAddon {
		t.Fatalf("empty version must skip compat matching, got %+v", fs)
	}
}

func TestMatchesRange(t *testing.T) {
	cases := []struct {
		version, constraint string
		want                bool
	}{
		{"4.7.1", ">=4.0.0", true},
		{"3.9.0", ">=4.0.0", false},
		{"4.7.1", ">=4.0.0 <5.0.0", true},
		{"5.0.0", ">=4.0.0 <5.0.0", false},
		{"4.7.1", ">=4.0.0,<5.0.0", true},
		{"1.2.0", "=1.2.0", true},
		{"v4.7.1", ">=4.0.0", true},
		{"4.7.1-beta.1", ">=4.7.0", true}, // pre-release suffix trimmed
		{"not-a-version", ">=1.0.0", false},
		{"4.7.1", "", false}, // empty constraint never matches
	}
	for _, tc := range cases {
		if got := matchesRange(tc.version, tc.constraint); got != tc.want {
			t.Errorf("matchesRange(%q, %q) = %v, want %v", tc.version, tc.constraint, got, tc.want)
		}
	}
}
