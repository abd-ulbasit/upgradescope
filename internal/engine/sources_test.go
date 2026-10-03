package engine

import (
	"go/ast"
	"go/parser"
	"go/token"
	"reflect"
	"slices"
	"strconv"
	"testing"

	"github.com/abd-ulbasit/upgradescope/internal/inventory"
)

// TestEveryCategoryHasSources: every Category constant declared in
// findings.go has an entry in categorySources, so a new category cannot
// leave its findings' capabilities unknown (and the server's notification
// baseline unable to tell a capability gap from a fix).
func TestEveryCategoryHasSources(t *testing.T) {
	f, err := parser.ParseFile(token.NewFileSet(), "findings.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var cats []Category
	for _, d := range f.Decls {
		gd, ok := d.(*ast.GenDecl)
		if !ok || gd.Tok != token.CONST {
			continue
		}
		for _, s := range gd.Specs {
			vs := s.(*ast.ValueSpec)
			if id, ok := vs.Type.(*ast.Ident); !ok || id.Name != "Category" {
				continue
			}
			for _, v := range vs.Values {
				lit, ok := v.(*ast.BasicLit)
				if !ok {
					t.Fatalf("Category constant %v is not a string literal", vs.Names)
				}
				s, err := strconv.Unquote(lit.Value)
				if err != nil {
					t.Fatal(err)
				}
				cats = append(cats, Category(s))
			}
		}
	}
	if len(cats) < 11 {
		t.Fatalf("found %d Category constants in findings.go, want at least 11: %v", len(cats), cats)
	}
	for _, c := range cats {
		if _, ok := categorySources[c]; !ok {
			t.Errorf("category %q has no entry in categorySources", c)
		}
	}
	for c := range categorySources {
		if !slices.Contains(cats, c) {
			t.Errorf("categorySources lists %q, which findings.go does not declare", c)
		}
	}
}

func TestSources(t *testing.T) {
	addOnData := []inventory.Capability{inventory.CapAddOns, inventory.CapVersions, inventory.CapHelm}
	for _, tc := range []struct {
		cat  Category
		key  string
		want []inventory.Capability
	}{
		{CatRemovedAPI, "removed-api/policy/v1beta1/PodSecurityPolicy", []inventory.Capability{inventory.CapAPIUsage}},
		{CatRemovedAPI, "removed-api/helm-release/shop/web", []inventory.Capability{inventory.CapHelm}},
		{CatDeprecatedAPI, "deprecated-api/helm-release/shop/web", []inventory.Capability{inventory.CapHelm}},
		{CatChartIncompat, "chart-incompat/helm-release/shop/web", []inventory.Capability{inventory.CapHelm}},
		{CatChartIncompat, "chart-incompat/ingress-nginx/1.9", addOnData},
		{CatEOLAddon, "eol-addon/ingress-nginx", addOnData},
		{CatEOLApproaching, "eol-approaching/cert-manager", addOnData},
		{CatAddOnNoData, "addon-no-data/argo-cd", addOnData},
		{CatDeprecatedAPIInUse, "deprecated-api-in-use/networking.k8s.io/v1beta1/servicecidrs", []inventory.Capability{inventory.CapDeprecatedCalls}},
		{CatVersionSkew, "version-skew/kubelet-post-upgrade", []inventory.Capability{inventory.CapVersions}},
		{CatCRDVersion, "crd-version/unserved/cert-manager.io/v1alpha2/Certificate", []inventory.Capability{inventory.CapCRDs}},
		{CatKBStale, "kb-stale", []inventory.Capability{}},
	} {
		if got := Sources(tc.cat, tc.key); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("Sources(%s, %s) = %v, want %v", tc.cat, tc.key, got, tc.want)
		}
	}
}

func TestUnassessed(t *testing.T) {
	const (
		psp      = "removed-api/policy/v1beta1/PodSecurityPolicy"
		endpts   = "removed-api/core/v1/Endpoints"
		pspCall  = "deprecated-api-in-use/policy/v1beta1/podsecuritypolicies"
		cidrCall = "deprecated-api-in-use/networking.k8s.io/v1beta1/servicecidrs"
		web      = "removed-api/helm-release/shop/web"
		api      = "removed-api/helm-release/shop/api"
		skew     = "version-skew/kube-proxy-post-upgrade"
	)
	type check struct {
		cat  Category
		key  string
		want bool
	}
	for _, tc := range []struct {
		name   string
		gaps   []CapabilityGap
		checks []check
	}{
		{"no gaps", nil, []check{{CatRemovedAPI, psp, false}, {CatDeprecatedAPIInUse, cidrCall, false}}},
		{
			"unavailable optional capability", []CapabilityGap{{Capability: inventory.CapDeprecatedCalls, Reason: "forbidden"}},
			[]check{{CatDeprecatedAPIInUse, cidrCall, true}, {CatRemovedAPI, psp, false}},
		},
		{
			"unavailable required capability", []CapabilityGap{{Capability: inventory.CapAPIUsage, Required: true}},
			[]check{{CatRemovedAPI, psp, true}, {CatRemovedAPI, web, false}, {CatDeprecatedAPIInUse, cidrCall, false}},
		},
		{
			// The scanner listed PSP itself: its metric rows are not
			// evidence, the other resources' rows are.
			"partial deprecated-calls", []CapabilityGap{{Capability: inventory.CapDeprecatedCalls, Partial: true, Skipped: []string{"policy/v1beta1 podsecuritypolicies"}}},
			[]check{{CatDeprecatedAPIInUse, pspCall, true}, {CatDeprecatedAPIInUse, cidrCall, false}},
		},
		{
			"partial api-usage", []CapabilityGap{{Capability: inventory.CapAPIUsage, Partial: true, Skipped: []string{"v1 Endpoints"}}},
			[]check{{CatRemovedAPI, endpts, true}, {CatRemovedAPI, psp, false}},
		},
		{
			// A discovery failure in a group without flagged APIs:
			// every flagged API was read.
			"partial api-usage naming nothing", []CapabilityGap{{Capability: inventory.CapAPIUsage, Partial: true}},
			[]check{{CatRemovedAPI, psp, false}},
		},
		{
			"partial helm release", []CapabilityGap{{Capability: inventory.CapHelm, Partial: true, Skipped: []string{"shop/web"}}},
			[]check{{CatRemovedAPI, web, true}, {CatRemovedAPI, api, false}, {CatRemovedAPI, psp, false}},
		},
		{
			"partial helm driver", []CapabilityGap{{Capability: inventory.CapHelm, Partial: true, Skipped: []string{"configmaps"}}},
			[]check{{CatRemovedAPI, web, true}, {CatRemovedAPI, api, true}, {CatChartIncompat, "chart-incompat/helm-release/shop/api", true}},
		},
		{
			// An add-on found through its chart alone is gone from the
			// inventory with helm (#189).
			"unavailable helm", []CapabilityGap{{Capability: inventory.CapHelm, Reason: "list secrets: forbidden"}},
			[]check{{CatEOLAddon, "eol-addon/ingress-nginx", true}, {CatChartIncompat, "chart-incompat/ingress-nginx/1.9", true},
				{CatRemovedAPI, web, true}, {CatRemovedAPI, psp, false}, {CatVersionSkew, skew, false}},
		},
		{
			// An add-on's key does not say which release it came from.
			"partial helm release, add-on keys", []CapabilityGap{{Capability: inventory.CapHelm, Partial: true, Skipped: []string{"shop/web"}}},
			[]check{{CatEOLAddon, "eol-addon/ingress-nginx", true}, {CatEOLApproaching, "eol-approaching/cert-manager", true}},
		},
		{
			"partial helm naming nothing", []CapabilityGap{{Capability: inventory.CapHelm, Partial: true}},
			[]check{{CatEOLAddon, "eol-addon/ingress-nginx", false}, {CatRemovedAPI, web, false}},
		},
		{
			"partial versions", []CapabilityGap{{Capability: inventory.CapVersions, Partial: true, Skipped: []string{"kube-proxy"}}},
			[]check{{CatVersionSkew, skew, true}, {CatEOLAddon, "eol-addon/containerd/1.6", true}},
		},
		{
			// A vendor kube-proxy image: no version could be read, so no
			// finding could have come from it either.
			"partial versions naming nothing", []CapabilityGap{{Capability: inventory.CapVersions, Partial: true}},
			[]check{{CatVersionSkew, skew, false}},
		},
		{
			"engine gap", []CapabilityGap{{Capability: GapKBCoverage, Required: true}},
			[]check{{CatRemovedAPI, psp, false}},
		},
		{
			"category this binary does not know", []CapabilityGap{{Capability: inventory.CapCRDs}},
			[]check{{Category("from-the-future"), "from-the-future/x", true}},
		},
	} {
		for _, c := range tc.checks {
			if got := Unassessed(tc.gaps, c.cat, c.key); got != c.want {
				t.Errorf("%s: Unassessed(%s) = %v, want %v", tc.name, c.key, got, c.want)
			}
		}
	}
}
