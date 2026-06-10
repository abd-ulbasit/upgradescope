// registry/validate_test.go
package registry

import (
	"strings"
	"testing"
)

// validAddOn returns an entry that passes every rule; cases mutate one field each.
func validAddOn() AddOn {
	return AddOn{
		SchemaVersion: 1,
		ID:            "ingress-nginx",
		DisplayName:   "Ingress NGINX Controller",
		Matchers: Matchers{
			Images: []string{"registry.k8s.io/ingress-nginx/controller"},
			Charts: []string{"ingress-nginx"},
		},
		Support: Support{
			Status:    "eol",
			EOLDate:   "2026-03-24",
			Citations: []string{"https://kubernetes.io/blog/2025/11/11/ingress-nginx-retirement/"},
		},
		Compat: []Compat{{
			Range:     ">=4.0.0",
			K8sMin:    "1.21",
			K8sMax:    "1.34",
			Citations: []string{"https://github.com/kubernetes/ingress-nginx#supported-versions-table"},
		}},
	}
}

func TestValidate(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*AddOn)
		wantErr string // substring of one returned error; "" means no errors at all
	}{
		{"valid entry", func(a *AddOn) {}, ""},
		{"schema_version zero", func(a *AddOn) { a.SchemaVersion = 0 }, "schema_version must be 1"},
		{"schema_version two", func(a *AddOn) { a.SchemaVersion = 2 }, "schema_version must be 1"},
		{"empty id", func(a *AddOn) { a.ID = "" }, "id must not be empty"},
		{"id with uppercase and underscore", func(a *AddOn) { a.ID = "Ingress_NGINX" }, "kebab-case"},
		{"id trailing dash", func(a *AddOn) { a.ID = "ingress-" }, "kebab-case"},
		{"no matchers at all", func(a *AddOn) { a.Matchers = Matchers{} }, "at least one matcher"},
		{"charts-only matcher is fine", func(a *AddOn) { a.Matchers.Images = nil }, ""},
		{"images-only matcher is fine", func(a *AddOn) { a.Matchers.Charts = nil }, ""},
		{"invalid support status", func(a *AddOn) { a.Support.Status = "deprecated" }, "support.status must be one of"},
		{"unknown status needs no citations", func(a *AddOn) {
			a.Support = Support{Status: "unknown"}
		}, ""},
		{"supported status without citations", func(a *AddOn) {
			a.Support = Support{Status: "supported"}
		}, "at least one citation required"},
		{"eol status without citations", func(a *AddOn) {
			a.Support = Support{Status: "eol", EOLDate: "2026-03-24"}
		}, "at least one citation required"},
		{"non-http citation scheme", func(a *AddOn) {
			a.Support.Citations = []string{"ftp://example.com/notes"}
		}, "http(s)"},
		{"citation without host", func(a *AddOn) {
			a.Support.Citations = []string{"https://"}
		}, "host"},
		{"eol_date wrong format", func(a *AddOn) { a.Support.EOLDate = "24-03-2026" }, "YYYY-MM-DD"},
		{"eol_date not a date", func(a *AddOn) { a.Support.EOLDate = "2026-13-99" }, "YYYY-MM-DD"},
		{"empty eol_date is fine", func(a *AddOn) { a.Support.EOLDate = "" }, ""},
		{"compat invalid semver constraint", func(a *AddOn) { a.Compat[0].Range = "not-a-constraint" }, "invalid semver constraint"},
		{"compat bad k8s_min", func(a *AddOn) { a.Compat[0].K8sMin = "v1.21" }, "k8s_min"},
		{"compat bad k8s_max", func(a *AddOn) { a.Compat[0].K8sMax = "1" }, "k8s_max"},
		{"compat without citations", func(a *AddOn) { a.Compat[0].Citations = nil }, "compat[0]: at least one citation"},
		{"compat non-http citation", func(a *AddOn) {
			a.Compat[0].Citations = []string{"file:///etc/passwd"}
		}, "http(s)"},
		{"no compat rows is fine", func(a *AddOn) { a.Compat = nil }, ""},
		{"no endoflife_product is fine", func(a *AddOn) { a.EndoflifeProduct = "" }, ""},
		{"valid endoflife_product slug", func(a *AddOn) { a.EndoflifeProduct = "argo-cd" }, ""},
		{"endoflife_product with uppercase", func(a *AddOn) { a.EndoflifeProduct = "Istio" }, "endoflife_product"},
		{"endoflife_product with space", func(a *AddOn) { a.EndoflifeProduct = "argo cd" }, "endoflife_product"},
		{"endoflife_product trailing dash", func(a *AddOn) { a.EndoflifeProduct = "istio-" }, "endoflife_product"},
		{"endoflife_product with dot is fine", func(a *AddOn) { a.EndoflifeProduct = "graalvm-ce.17" }, ""},
		{"compat k8s_min above k8s_max (transposed)", func(a *AddOn) {
			a.Compat[0].K8sMin = "1.30"
			a.Compat[0].K8sMax = "1.21"
		}, `k8s_min "1.30" must not exceed k8s_max "1.21"`},
		{"compat k8s_min equals k8s_max is fine", func(a *AddOn) {
			a.Compat[0].K8sMin = "1.28"
			a.Compat[0].K8sMax = "1.28"
		}, ""},
		{"compat minors compared numerically not lexicographically", func(a *AddOn) {
			a.Compat[0].K8sMin = "1.9" // "1.9" > "1.21" as strings, but 9 < 21
			a.Compat[0].K8sMax = "1.21"
		}, ""},
		{"compat major beats minor in comparison", func(a *AddOn) {
			a.Compat[0].K8sMin = "2.0"
			a.Compat[0].K8sMax = "1.34"
		}, `k8s_min "2.0" must not exceed k8s_max "1.34"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := validAddOn()
			tt.mutate(&a)
			errs := Validate(a)
			if tt.wantErr == "" {
				if len(errs) != 0 {
					t.Fatalf("want no errors, got %v", errs)
				}
				return
			}
			if len(errs) == 0 {
				t.Fatalf("want error containing %q, got none", tt.wantErr)
			}
			for _, err := range errs {
				if strings.Contains(err.Error(), tt.wantErr) {
					return
				}
			}
			t.Fatalf("want error containing %q, got %v", tt.wantErr, errs)
		})
	}
}
