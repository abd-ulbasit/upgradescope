// registry/validate_test.go
package registry

import (
	"strings"
	"testing"
)

// validAddOn returns an entry that passes every rule; cases mutate one field each.
func validAddOn() AddOn {
	return AddOn{
		SchemaVersion: 2,
		ID:            "ingress-nginx",
		DisplayName:   "Ingress NGINX Controller",
		Matchers: Matchers{
			Images: []string{"ingress-nginx/controller"},
			Charts: []string{"ingress-nginx"},
		},
		Support: Support{
			Status:    "eol",
			EOLDate:   "2026-03-24",
			Citations: []string{"https://kubernetes.io/blog/2025/11/11/ingress-nginx-retirement/"},
		},
		Cycles: []Cycle{{
			Cycle:     "1.11",
			EOL:       &CycleEOL{Date: "2026-03-24"},
			K8sMin:    "1.26",
			K8sMax:    "1.30",
			Citations: []string{"https://github.com/kubernetes/ingress-nginx#supported-versions-table"},
		}},
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
		{"schema_version zero", func(a *AddOn) { a.SchemaVersion = 0 }, "schema_version must be 2"},
		{"schema_version one (pre-cycles schema)", func(a *AddOn) { a.SchemaVersion = 1 }, "schema_version must be 2"},
		{"empty id", func(a *AddOn) { a.ID = "" }, "id must not be empty"},
		{"id with uppercase and underscore", func(a *AddOn) { a.ID = "Ingress_NGINX" }, "kebab-case"},
		{"id trailing dash", func(a *AddOn) { a.ID = "ingress-" }, "kebab-case"},
		{"no matchers at all", func(a *AddOn) { a.Matchers = Matchers{} }, "at least one matcher"},
		{"charts-only matcher is fine", func(a *AddOn) { a.Matchers.Images = nil }, ""},
		{"images-only matcher is fine", func(a *AddOn) { a.Matchers.Charts = nil }, ""},
		{"runtimes-only matcher is fine", func(a *AddOn) { a.Matchers = Matchers{Runtimes: []string{"containerd"}} }, ""},
		{"runtime matcher is not a bare name", func(a *AddOn) { a.Matchers.Runtimes = []string{"containerd://"} }, "matchers.runtimes"},
		{"image matcher with registry host", func(a *AddOn) {
			a.Matchers.Images = []string{"registry.k8s.io/ingress-nginx/controller"}
		}, "without the registry host"},
		{"image matcher with localhost host", func(a *AddOn) { a.Matchers.Images = []string{"localhost/app"} }, "without the registry host"},
		// A provider's own build is named with its host, so only the
		// provider entry claims it (#18).
		{"provider-build image matcher is fine", func(a *AddOn) {
			a.Matchers.Images = []string{"mcr.microsoft.com/oss/kubernetes/ingress/nginx-ingress-controller"}
		}, ""},
		{"provider-build image matcher with a tag", func(a *AddOn) {
			a.Matchers.Images = []string{"gke.gcr.io/calico/node:v3.26.3"}
		}, "tag or digest"},
		{"provider prefix without a repository", func(a *AddOn) { a.Matchers.Images = []string{"gke.gcr.io/"} }, "lowercase repository path"},
		{"non-provider host is still rejected", func(a *AddOn) {
			a.Matchers.Images = []string{"quay.io/calico/node"}
		}, "without the registry host"},
		{"image matcher with tag", func(a *AddOn) { a.Matchers.Images = []string{"ingress-nginx/controller:v1"} }, "tag or digest"},
		{"image matcher with digest", func(a *AddOn) { a.Matchers.Images = []string{"ingress-nginx/controller@sha256:ab"} }, "tag or digest"},
		{"image matcher with empty segment", func(a *AddOn) { a.Matchers.Images = []string{"ingress-nginx//controller"} }, "repository path"},
		{"image matcher with uppercase", func(a *AddOn) { a.Matchers.Images = []string{"Ingress-nginx/controller"} }, "repository path"},
		{"single-segment image matcher is fine", func(a *AddOn) { a.Matchers.Images = []string{"etcd"} }, ""},
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
		// A placeholder host passes the shape rules and cites nothing (#166).
		{"citation on example.com", func(a *AddOn) { a.Support.Citations = []string{"https://example.com/lifecycle"} }, "reserved or local host"},
		{"citation on a subdomain of example.com", func(a *AddOn) { a.Support.Citations = []string{"https://docs.example.com/x"} }, "reserved or local host"},
		{"citation on example.org", func(a *AddOn) { a.Support.Citations = []string{"https://example.org/"} }, "reserved or local host"},
		{"citation on example.net", func(a *AddOn) { a.Support.Citations = []string{"https://EXAMPLE.NET/"} }, "reserved or local host"},
		{"citation on localhost", func(a *AddOn) { a.Support.Citations = []string{"http://localhost:8080/eol"} }, "reserved or local host"},
		{"citation on 127.0.0.1", func(a *AddOn) { a.Support.Citations = []string{"http://127.0.0.1/eol"} }, "reserved or local host"},
		{"citation on an IPv6 loopback", func(a *AddOn) { a.Support.Citations = []string{"http://[::1]/eol"} }, "reserved or local host"},
		{"citation on a private address", func(a *AddOn) { a.Support.Citations = []string{"http://10.0.0.5/eol"} }, "reserved or local host"},
		{"citation on a single-label host", func(a *AddOn) { a.Support.Citations = []string{"http://x"} }, "reserved or local host"},
		{"citation on a .test host", func(a *AddOn) { a.Support.Citations = []string{"https://vendor.test/eol"} }, "reserved or local host"},
		{"citation on a .invalid host", func(a *AddOn) { a.Support.Citations = []string{"https://vendor.invalid/eol"} }, "reserved or local host"},
		{"citation on a lookalike of example.com is fine", func(a *AddOn) { a.Support.Citations = []string{"https://notexample.com/eol"} }, ""},
		{"cycle citation on example.com", func(a *AddOn) { a.Cycles[0].Citations = []string{"https://example.com/"} }, "reserved or local host"},
		{"compat citation on example.com", func(a *AddOn) { a.Compat[0].Citations = []string{"https://example.com/"} }, "reserved or local host"},
		// A date is a claim: without a cited status it would still print as
		// a blocker (#166).
		{"unknown status with an eol_date", func(a *AddOn) {
			a.Support = Support{Status: "unknown", EOLDate: "2020-01-01"}
		}, "eol_date requires support.status supported or eol"},
		{"supported status with an eol_date is fine", func(a *AddOn) { a.Support.Status = "supported" }, ""},
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
		// Upstream matrices often publish one bound only ("≤ 0.9.x works up
		// to 1.21"): either bound may be left out, but not both.
		{"compat with k8s_max only is fine", func(a *AddOn) { a.Compat[0].K8sMin = "" }, ""},
		{"compat with k8s_min only is fine", func(a *AddOn) { a.Compat[0].K8sMax = "" }, ""},
		{"compat without any k8s bound", func(a *AddOn) {
			a.Compat[0].K8sMin = ""
			a.Compat[0].K8sMax = ""
		}, "compat[0]: at least one of k8s_min or k8s_max required"},

		// Release cycles (schema v2).
		{"no cycles is fine", func(a *AddOn) { a.Cycles = nil }, ""},
		{"cycle with eol false is fine", func(a *AddOn) { a.Cycles[0].EOL = &CycleEOL{} }, ""},
		{"cycle with eol true is fine", func(a *AddOn) { a.Cycles[0].EOL = &CycleEOL{Ended: true} }, ""},
		{"cycle without k8s bounds is fine", func(a *AddOn) {
			a.Cycles[0].K8sMin = ""
			a.Cycles[0].K8sMax = ""
		}, ""},
		{"cycle with major-only name is fine", func(a *AddOn) { a.Cycles[0].Cycle = "3" }, ""},
		{"cycle missing eol", func(a *AddOn) { a.Cycles[0].EOL = nil }, "cycles[0] (1.11): eol required"},
		{"cycle eol not a date", func(a *AddOn) { a.Cycles[0].EOL = &CycleEOL{Date: "soon"} }, "YYYY-MM-DD"},
		{"cycle empty name", func(a *AddOn) { a.Cycles[0].Cycle = "" }, "cycle must be a dotted version"},
		{"cycle non-numeric name", func(a *AddOn) { a.Cycles[0].Cycle = "v1.11" }, "cycle must be a dotted version"},
		{"duplicate cycle", func(a *AddOn) { a.Cycles = append(a.Cycles, a.Cycles[0]) }, `duplicate cycle "1.11"`},
		{"cycle without citations", func(a *AddOn) { a.Cycles[0].Citations = nil }, "cycles[0] (1.11): at least one citation"},
		{"cycle non-http citation", func(a *AddOn) { a.Cycles[0].Citations = []string{"ftp://x.example/"} }, "http(s)"},
		{"cycle bad k8s_min", func(a *AddOn) { a.Cycles[0].K8sMin = "1.30.1" }, "k8s_min"},
		{"cycle k8s_min above k8s_max", func(a *AddOn) {
			a.Cycles[0].K8sMin = "1.31"
			a.Cycles[0].K8sMax = "1.30"
		}, `k8s_min "1.31" must not exceed k8s_max "1.30"`},
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
