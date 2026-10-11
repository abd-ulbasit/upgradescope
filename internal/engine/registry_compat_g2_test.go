package engine

import (
	"slices"
	"strings"
	"testing"

	"github.com/abd-ulbasit/upgradescope/internal/inventory"
	"github.com/abd-ulbasit/upgradescope/internal/kb"
	"github.com/abd-ulbasit/upgradescope/registry"
)

const (
	istioDoc   = "https://istio.io/latest/docs/releases/supported-releases/"
	linkerdDoc = "https://linkerd.io/docs/reference/k8s-versions/"
	albcDoc    = "https://github.com/kubernetes-sigs/aws-load-balancer-controller/blob/0e7eefa9b20ba84ec0bd198fc42b41b8ee3a139f/docs/deploy/installation.md#supported-kubernetes-versions"
)

// The Kubernetes compatibility ranges of the networking, mesh and ingress
// entries (Istio, Linkerd, AWS Load Balancer Controller), judged through
// evalAddOns with the real registry. Each range has an affected and a clean
// evaluation, and the titles are what the CLI prints. Where upstream
// publishes a minimum only, an affected case needs a target below it.
func TestEmbeddedCompatRangesNetworking(t *testing.T) {
	addons, err := registry.Load()
	if err != nil {
		t.Fatal(err)
	}
	k := kb.KB{AddOns: addons}
	now := day("2026-10-12")
	at := func(minor int) inventory.Version { return inventory.Version{Major: 1, Minor: minor} }
	v37 := at(37)

	cases := []struct {
		id, version string
		target      inventory.Version
		want        string // chart-incompat title, "" for none
		cite        string // a citation the finding must carry
	}{
		// Istio: the "Supported Kubernetes Versions" column. The cycle
		// carries the range from endoflife.date and is judged first, the
		// compat rows read from istio.io are the vendor's own record.
		{"istio", "1.31.0", v37, "", ""},
		{"istio", "1.31.0", at(38), "Istio 1.31.0 supports Kubernetes up to 1.37 (target 1.38)", "https://endoflife.date/istio"},
		{"istio", "1.30.2", v37, "Istio 1.30.2 supports Kubernetes up to 1.36 (target 1.37)", "https://endoflife.date/istio"},
		{"istio", "1.30.2", at(36), "", ""},
		{"istio", "1.29.0", at(30), "Istio 1.29.0 requires Kubernetes 1.31 or newer (target 1.30)", "https://endoflife.date/istio"},
		{"istio", "1.28.4", v37, "Istio 1.28.4 supports Kubernetes up to 1.34 (target 1.37)", "https://endoflife.date/istio"},
		{"istio", "1.28.4", at(34), "", ""},
		{"istio", "1.20.0", at(30), "Istio 1.20.0 supports Kubernetes up to 1.29 (target 1.30)", "https://endoflife.date/istio"},
		{"istio", "1.13.2", at(24), "Istio 1.13.2 supports Kubernetes up to 1.23 (target 1.24)", "https://endoflife.date/istio"},

		// Linkerd: the Minimum and Maximum Kubernetes Version columns.
		{"linkerd", "2.14.10", v37, "Linkerd 2.14.10 supports Kubernetes up to 1.28 (target 1.37)", linkerdDoc},
		{"linkerd", "2.14.10", at(28), "", ""},
		{"linkerd", "2.14.10", at(20), "Linkerd 2.14.10 requires Kubernetes 1.21 or newer (target 1.20)", linkerdDoc},
		{"linkerd", "2.20.0", v37, "Linkerd 2.20.0 supports Kubernetes up to 1.35 (target 1.37)", linkerdDoc},
		{"linkerd", "2.20.0", at(35), "", ""},
		{"linkerd", "2.20.0", at(30), "Linkerd 2.20.0 requires Kubernetes 1.31 or newer (target 1.30)", linkerdDoc},
		{"linkerd", "2.19.1", at(36), "Linkerd 2.19.1 supports Kubernetes up to 1.35 (target 1.36)", linkerdDoc},
		{"linkerd", "2.18.0", at(33), "Linkerd 2.18.0 supports Kubernetes up to 1.32 (target 1.33)", linkerdDoc},
		{"linkerd", "2.17.1", at(32), "Linkerd 2.17.1 supports Kubernetes up to 1.31 (target 1.32)", linkerdDoc},
		{"linkerd", "2.16.0", at(30), "Linkerd 2.16.0 supports Kubernetes up to 1.29 (target 1.30)", linkerdDoc},
		{"linkerd", "2.15.2", at(30), "Linkerd 2.15.2 supports Kubernetes up to 1.29 (target 1.30)", linkerdDoc},
		{"linkerd", "2.13.5", at(29), "Linkerd 2.13.5 supports Kubernetes up to 1.28 (target 1.29)", linkerdDoc},
		{"linkerd", "2.12.5", at(25), "Linkerd 2.12.5 supports Kubernetes up to 1.24 (target 1.25)", linkerdDoc},
		{"linkerd", "2.11.0", at(15), "Linkerd 2.11.0 requires Kubernetes 1.16 or newer (target 1.15)", linkerdDoc}, // 2.11.0 alone supported 1.16
		{"linkerd", "2.11.0", at(16), "", ""},
		{"linkerd", "2.11.2", at(16), "Linkerd 2.11.2 requires Kubernetes 1.17 or newer (target 1.16)", linkerdDoc},
		{"linkerd", "2.11.2", at(23), "", ""},
		{"linkerd", "2.10.2", at(24), "Linkerd 2.10.2 supports Kubernetes up to 1.23 (target 1.24)", linkerdDoc},
		// Edge releases: "all Kubernetes versions readily available" at the
		// time, no table, so no bound.
		{"linkerd", "26.10.1", v37, "", ""},
		{"linkerd", "26.10.1", at(20), "", ""},

		// AWS Load Balancer Controller: the "Supported Kubernetes versions" list.
		{"aws-load-balancer-controller", "2.1.3", at(22), "AWS Load Balancer Controller 2.1.3 supports Kubernetes up to 1.21 (target 1.22)", albcDoc},
		{"aws-load-balancer-controller", "2.1.3", at(21), "", ""},
		{"aws-load-balancer-controller", "2.0.1", at(14), "AWS Load Balancer Controller 2.0.1 requires Kubernetes 1.15 or newer (target 1.14)", albcDoc},
		{"aws-load-balancer-controller", "2.3.1", at(22), "AWS Load Balancer Controller 2.3.1 supports Kubernetes up to 1.21 (target 1.22)", albcDoc},
		{"aws-load-balancer-controller", "2.3.1", at(15), "AWS Load Balancer Controller 2.3.1 requires Kubernetes 1.16 or newer (target 1.15)", albcDoc},
		{"aws-load-balancer-controller", "2.4.7", at(18), "AWS Load Balancer Controller 2.4.7 requires Kubernetes 1.19 or newer (target 1.18)", albcDoc},
		{"aws-load-balancer-controller", "2.4.7", v37, "", ""},
		{"aws-load-balancer-controller", "2.5.0", at(21), "AWS Load Balancer Controller 2.5.0 requires Kubernetes 1.22 or newer (target 1.21)", albcDoc},
		{"aws-load-balancer-controller", "3.6.0", at(21), "AWS Load Balancer Controller 3.6.0 requires Kubernetes 1.22 or newer (target 1.21)", albcDoc},
		{"aws-load-balancer-controller", "3.6.0", v37, "", ""}, // "v2.5.0+": a floor, no maximum

		// Products whose upstream publishes no range, or only a tested-with
		// table, stay unjudged for compatibility on purpose (see the comments
		// of their registry files).
		{"traefik", "3.6.0", at(20), "", ""},
		{"coredns", "1.6.2", v37, "", ""},
		{"ingress-nginx", "1.9.4", v37, "", ""}, // README table: E2E-tested versions
		{"ingress-nginx", "1.15.1", at(20), "", ""},
		{"aks-app-routing-nginx", "1.11.5", v37, "", ""},
	}
	for _, tc := range cases {
		var got []Finding
		for _, f := range evalAddOns(addOnAt(tc.id, tc.version), k, tc.target, now) {
			if f.Category == CatChartIncompat {
				got = append(got, f)
			}
		}
		if tc.want == "" {
			if len(got) != 0 {
				t.Errorf("%s %s at %s: unexpected compatibility finding %q", tc.id, tc.version, tc.target, got[0].Title)
			}
			continue
		}
		if len(got) != 1 {
			t.Errorf("%s %s at %s: %d compatibility findings, want 1", tc.id, tc.version, tc.target, len(got))
			continue
		}
		f := got[0]
		if f.Severity != SevBlocker || f.Title != tc.want {
			t.Errorf("%s %s at %s: %s %q, want blocker %q", tc.id, tc.version, tc.target, f.Severity, f.Title, tc.want)
		}
		if !slices.Contains(f.Citations, tc.cite) {
			t.Errorf("%s %s at %s: citations %q lack %s", tc.id, tc.version, tc.target, f.Citations, tc.cite)
		}
		if !strings.Contains(f.Detail, "compatibility range") && !strings.Contains(f.Detail, "release line") {
			t.Errorf("%s %s at %s: detail %q does not name the range", tc.id, tc.version, tc.target, f.Detail)
		}
	}
}

// Istio's compat rows are read from istio.io's own table, while the cycles'
// ranges come from endoflife.date through eol-sync and are judged first.
// A release line both sources cover must agree, so a weekly refresh that
// moves endoflife.date away from istio.io fails here instead of silently
// overriding the vendor's page.
func TestIstioCompatRowsAgreeWithCycleRanges(t *testing.T) {
	addons, err := registry.Load()
	if err != nil {
		t.Fatal(err)
	}
	i := slices.IndexFunc(addons, func(a registry.AddOn) bool { return a.ID == "istio" })
	if i < 0 {
		t.Fatal("istio not in the embedded registry")
	}
	a := addons[i]
	if len(a.Compat) == 0 {
		t.Fatal("istio has no compat rows")
	}
	for _, row := range a.Compat {
		if !slices.Contains(row.Citations, istioDoc) {
			t.Errorf("row %s does not cite %s", row.Range, istioDoc)
		}
		// ">=1.28.0 <1.29.0" is the 1.28 line.
		line := strings.TrimSuffix(strings.TrimPrefix(strings.Fields(row.Range)[0], ">="), ".0")
		c, ok := cycleFor(line+".0", a.Cycles)
		if !ok {
			t.Errorf("row %s: no cycle %s", row.Range, line)
			continue
		}
		if c.K8sMin != row.K8sMin || c.K8sMax != row.K8sMax {
			t.Errorf("istio %s: cycle (endoflife.date) says Kubernetes %s-%s, istio.io row says %s-%s",
				line, c.K8sMin, c.K8sMax, row.K8sMin, row.K8sMax)
		}
	}
}
