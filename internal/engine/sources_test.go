package engine

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/abd-ulbasit/upgradescope/internal/inventory"
	"github.com/abd-ulbasit/upgradescope/internal/kb"
)

// TestEveryCategoryHasSources: every Category constant declared in the
// package has an entry in categorySources, so a new category cannot
// leave its findings' capabilities unknown (and the server's notification
// baseline unable to tell a capability gap from a fix).
func TestEveryCategoryHasSources(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	var decls []ast.Decl
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(token.NewFileSet(), name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		decls = append(decls, f.Decls...)
	}
	var cats []Category
	for _, d := range decls {
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
		t.Fatalf("found %d Category constants in package engine, want at least 11: %v", len(cats), cats)
	}
	for _, c := range cats {
		if _, ok := categorySources[c]; !ok {
			t.Errorf("category %q has no entry in categorySources", c)
		}
	}
	for c := range categorySources {
		if !slices.Contains(cats, c) {
			t.Errorf("categorySources lists %q, which package engine does not declare", c)
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

// TestHiddenByGaps: which gaps hide a finding, by gap kind: a capability
// unavailable hides what it produces, a partial one only what it skipped.
func TestHiddenByGaps(t *testing.T) {
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
			// The agent collected with another knowledge base (#268): any
			// API the server flags may have gone unlisted.
			"partial api-usage from another knowledge base", []CapabilityGap{{Capability: inventory.CapAPIUsage, Partial: true, Skipped: []string{inventory.SkippedNewerKB}}},
			[]check{{CatRemovedAPI, psp, true}, {CatRemovedAPI, endpts, true}, {CatDeprecatedAPIInUse, cidrCall, false}},
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
			// #70: a GitOps tool in Skipped (no Helm release, or its
			// resources unread) leaves every chart-derived finding
			// unassessed, as a driver does.
			"partial helm naming a GitOps tool", []CapabilityGap{{Capability: inventory.CapHelm, Partial: true, Skipped: []string{inventory.GitOpsArgoCD}}},
			[]check{{CatEOLAddon, "eol-addon/ingress-nginx", true}, {CatRemovedAPI, web, true}, {CatChartIncompat, "chart-incompat/helm-release/shop/api", true}, {CatRemovedAPI, psp, false}},
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
			if got := HiddenBy(tc.gaps, c.cat, c.key); (len(got) > 0) != c.want {
				t.Errorf("%s: HiddenBy(%s) = %v, want unassessed %v", tc.name, c.key, got, c.want)
			}
		}
	}
}

// TestHiddenBy: the capabilities named are those of the gaps that hide
// the finding, once each, in the gaps' order.
func TestHiddenBy(t *testing.T) {
	gaps := []CapabilityGap{
		{Capability: inventory.CapAddOns, Partial: true, Skipped: []string{"ingressclasses"}},
		{Capability: inventory.CapAPIUsage, Partial: true, Skipped: []string{"v1 Endpoints"}},
		{Capability: inventory.CapHelm},
		{Capability: inventory.CapHelm, Partial: true, Skipped: []string{"shop/web"}}, // defensive: one gap per capability
	}
	for _, tc := range []struct {
		cat  Category
		key  string
		want []inventory.Capability
	}{
		{CatEOLAddon, "eol-addon/argo-cd/2.10", []inventory.Capability{inventory.CapAddOns, inventory.CapHelm}},
		{CatRemovedAPI, "removed-api/helm-release/shop/api", []inventory.Capability{inventory.CapHelm}},
		{CatRemovedAPI, "removed-api/core/v1/Endpoints", []inventory.Capability{inventory.CapAPIUsage}},
		{CatRemovedAPI, "removed-api/policy/v1beta1/PodSecurityPolicy", nil},
		{Category("from-the-future"), "from-the-future/x", []inventory.Capability{inventory.CapAddOns, inventory.CapAPIUsage, inventory.CapHelm}},
	} {
		if got := HiddenBy(gaps, tc.cat, tc.key); !slices.Equal(got, tc.want) {
			t.Errorf("HiddenBy(%s) = %v, want %v", tc.key, got, tc.want)
		}
	}
}

// TestFoldsInto: a caller row whose API has a usage finding folds into it
// (foldDeprecatedCalls), and FoldsInto names that pair from the keys
// alone: the caller's key in a report without the usage finding, the
// usage finding's key in a report with it.
func TestFoldsInto(t *testing.T) {
	k := testKB()
	k.APILifecycle = append(k.APILifecycle, kb.APILifecycleEntry{
		Version: "v1", Kind: "Endpoints", Introduced: inventory.Version{Major: 1}, Deprecated: vp(1, 33),
	})
	target := inventory.Version{Major: 1, Minor: 34}
	for _, tc := range []struct {
		usage inventory.APIUsage
		call  inventory.DeprecatedCall
	}{
		{inventory.APIUsage{Version: "v1", Kind: "ComponentStatus", Count: 1, Namespaces: map[string]int{"": 1}},
			inventory.DeprecatedCall{Version: "v1", Resource: "componentstatuses"}},
		{inventory.APIUsage{Version: "v1", Kind: "Endpoints", Count: 1, Namespaces: map[string]int{"default": 1}},
			inventory.DeprecatedCall{Version: "v1", Resource: "endpoints", Subresource: "status"}},
	} {
		with := Evaluate(inventory.Inventory{APIUsage: []inventory.APIUsage{tc.usage}, DeprecatedCalls: []inventory.DeprecatedCall{tc.call}}, k, target, testNow)
		without := Evaluate(inventory.Inventory{DeprecatedCalls: []inventory.DeprecatedCall{tc.call}}, k, target, testNow)
		if len(with.Findings) != 1 || len(without.Findings) != 1 || without.Findings[0].Category != CatDeprecatedAPIInUse {
			t.Fatalf("%s: findings with usage %+v, without %+v; want one each, the caller's alone", tc.usage.Kind, with.Findings, without.Findings)
		}
		if call, usage := without.Findings[0].Key, with.Findings[0].Key; !FoldsInto(call, usage) {
			t.Errorf("FoldsInto(%s, %s) = false, want true", call, usage)
		}
	}
	const pspCall = "deprecated-api-in-use/policy/v1beta1/podsecuritypolicies"
	for _, tc := range []struct {
		call, usage string
		want        bool
	}{
		{pspCall, "removed-api/policy/v1beta1/PodSecurityPolicy", true},
		{pspCall, "deprecated-api/policy/v1beta1/PodSecurityPolicy", true},
		{pspCall, "unknown-api/policy/v1beta1/PodSecurityPolicy", true},
		{"deprecated-api-in-use/policy/v1/podsecuritypolicies", "removed-api/policy/v1beta1/PodSecurityPolicy", false},
		{"deprecated-api-in-use/extensions/v1beta1/podsecuritypolicies", "removed-api/policy/v1beta1/PodSecurityPolicy", false},
		{"deprecated-api-in-use/policy/v1beta1/poddisruptionbudgets", "removed-api/policy/v1beta1/PodSecurityPolicy", false},
		{"deprecated-api-in-use/helm-release/shop/webs", "removed-api/helm-release/shop/web", false},
		{pspCall, "eol-addon/policy/v1beta1/PodSecurityPolicy", false},
		{"removed-api/policy/v1beta1/podsecuritypolicies", "removed-api/policy/v1beta1/PodSecurityPolicy", false},
		{"deprecated-api-in-use/policy", "removed-api/policy/v1beta1/PodSecurityPolicy", false},
	} {
		if got := FoldsInto(tc.call, tc.usage); got != tc.want {
			t.Errorf("FoldsInto(%s, %s) = %v, want %v", tc.call, tc.usage, got, tc.want)
		}
	}
}
