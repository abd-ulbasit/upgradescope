package engine

import (
	"slices"
	"strings"
	"testing"

	"github.com/abd-ulbasit/upgradescope/internal/inventory"
	"github.com/abd-ulbasit/upgradescope/internal/kb"
	"github.com/abd-ulbasit/upgradescope/registry"
)

// The Kubernetes compatibility rows of the embedded registry (#341, #343),
// judged through evalAddOns with the real registry, a fixed clock and, unless
// a case says otherwise, the target the sweep used, Kubernetes 1.37. Each
// range is covered by one affected and one clean evaluation. The titles are
// what the CLI prints, so a change to a bound or a source shows up here.
//
// Where upstream publishes only a minimum, an affected case needs a target
// below it: a forward-upgrade scan cannot trip a minimum.
func TestEmbeddedCompatRanges(t *testing.T) {
	addons, err := registry.Load()
	if err != nil {
		t.Fatal(err)
	}
	k := kb.KB{AddOns: addons}
	now := day("2026-10-10")
	v37 := inventory.Version{Major: 1, Minor: 37}
	at := func(minor int) inventory.Version { return inventory.Version{Major: 1, Minor: minor} }

	cases := []struct {
		id, version string
		target      inventory.Version
		want        string // chart-incompat title, "" for none
		cite        string // a citation the finding must carry
	}{
		// cert-manager: both ends, from the releases page.
		{"cert-manager", "1.21.0", v37, "cert-manager 1.21.0 supports Kubernetes up to 1.36 (target 1.37)", "https://cert-manager.io/docs/releases/"},
		{"cert-manager", "1.21.0", at(36), "", ""},
		{"cert-manager", "1.20.2", v37, "cert-manager 1.20.2 supports Kubernetes up to 1.35 (target 1.37)", "https://cert-manager.io/docs/releases/"},
		{"cert-manager", "1.19.0", v37, "cert-manager 1.19.0 supports Kubernetes up to 1.35 (target 1.37)", "https://cert-manager.io/docs/releases/"},
		{"cert-manager", "1.19.0", at(35), "", ""},
		{"cert-manager", "1.19.0", at(30), "cert-manager 1.19.0 requires Kubernetes 1.31 or newer (target 1.30)", "https://cert-manager.io/docs/releases/"},
		{"cert-manager", "1.12.3", v37, "cert-manager 1.12.3 supports Kubernetes up to 1.32 (target 1.37)", "https://cert-manager.io/docs/releases/"},
		{"cert-manager", "1.12.3", at(32), "", ""},
		{"cert-manager", "1.10.0", at(27), "cert-manager 1.10.0 supports Kubernetes up to 1.26 (target 1.27)", "https://cert-manager.io/docs/releases/"},
		{"cert-manager", "1.10.0", at(26), "", ""},

		// Karpenter: derived maxima, 1.15 (the first for 1.37) has none.
		{"karpenter", "1.6.0", v37, "Karpenter 1.6.0 supports Kubernetes up to 1.34 (target 1.37)", "https://karpenter.sh/v1.14/upgrading/compatibility/"},
		{"karpenter", "1.6.0", at(34), "", ""},
		{"karpenter", "1.8.9", at(35), "Karpenter 1.8.9 supports Kubernetes up to 1.34 (target 1.35)", "https://karpenter.sh/v1.14/upgrading/compatibility/"},
		{"karpenter", "1.9.0", at(35), "", ""},
		{"karpenter", "1.14.0", v37, "Karpenter 1.14.0 supports Kubernetes up to 1.36 (target 1.37)", "https://karpenter.sh/v1.14/upgrading/compatibility/"},
		{"karpenter", "1.14.0", at(36), "", ""},
		{"karpenter", "1.2.0", at(33), "Karpenter 1.2.0 supports Kubernetes up to 1.32 (target 1.33)", "https://karpenter.sh/v1.14/upgrading/compatibility/"},
		{"karpenter", "1.5.1", at(33), "", ""},
		{"karpenter", "1.15.0", v37, "", ""},

		// Cilium: minimums of 1.13 and 1.14 only; no maximum is published.
		{"cilium", "1.13.4", at(15), "Cilium 1.13.4 requires Kubernetes 1.16 or newer (target 1.15)", "https://docs.cilium.io/en/v1.13/network/kubernetes/compatibility/"},
		{"cilium", "1.13.4", at(16), "", ""},
		{"cilium", "1.14.0", at(18), "Cilium 1.14.0 requires Kubernetes 1.19 or newer (target 1.18)", "https://docs.cilium.io/en/v1.14/network/kubernetes/compatibility/"},
		{"cilium", "1.14.0", at(19), "", ""},
		{"cilium", "1.18.0", v37, "", ""}, // "newer versions depend on Kubernetes' compatibility": not a bound
		{"cilium", "1.18.0", at(20), "", ""},

		// Calico: the minimum each release's page states.
		{"calico", "3.25.1", at(15), "Calico 3.25.1 requires Kubernetes 1.16 or newer (target 1.15)", "https://docs.tigera.io/calico/3.25/getting-started/kubernetes/requirements"},
		{"calico", "3.25.1", at(16), "", ""},
		{"calico", "3.26.0", at(20), "Calico 3.26.0 requires Kubernetes 1.21 or newer (target 1.20)", "https://docs.tigera.io/calico/3.26/getting-started/kubernetes/requirements"},
		{"calico", "3.31.2", at(20), "Calico 3.31.2 requires Kubernetes 1.21 or newer (target 1.20)", "https://docs.tigera.io/calico/3.31/getting-started/kubernetes/requirements"},
		{"calico", "3.31.2", v37, "", ""},
		{"calico", "3.33.0", at(20), "", ""}, // 3.33's page states no minimum

		// metrics-server (#343): 0.3.x stops at 1.21, the rest are floors.
		{"metrics-server", "0.3.6", v37, "Kubernetes Metrics Server 0.3.6 supports Kubernetes up to 1.21 (target 1.37)", "https://github.com/kubernetes-sigs/metrics-server/blob/e32f7808d40e6f743c4111c9a746a88a99878765/README.md#compatibility-matrix"},
		{"metrics-server", "0.3.6", at(21), "", ""},
		{"metrics-server", "0.8.1", v37, "", ""},
		{"metrics-server", "0.8.1", at(30), "Kubernetes Metrics Server 0.8.1 requires Kubernetes 1.31 or newer (target 1.30)", "https://github.com/kubernetes-sigs/metrics-server/blob/e32f7808d40e6f743c4111c9a746a88a99878765/README.md#compatibility-matrix"},
		{"metrics-server", "0.6.3", at(24), "Kubernetes Metrics Server 0.6.3 requires Kubernetes 1.25 or newer (target 1.24)", "https://github.com/kubernetes-sigs/metrics-server/blob/e32f7808d40e6f743c4111c9a746a88a99878765/README.md#compatibility-matrix"},
		{"metrics-server", "0.6.3", at(25), "", ""},
		{"metrics-server", "0.7.2", at(26), "Kubernetes Metrics Server 0.7.2 requires Kubernetes 1.27 or newer (target 1.26)", "https://github.com/kubernetes-sigs/metrics-server/blob/e32f7808d40e6f743c4111c9a746a88a99878765/README.md#compatibility-matrix"},
		{"metrics-server", "0.9.0", at(33), "Kubernetes Metrics Server 0.9.0 requires Kubernetes 1.34 or newer (target 1.33)", "https://github.com/kubernetes-sigs/metrics-server/blob/e32f7808d40e6f743c4111c9a746a88a99878765/README.md#compatibility-matrix"},
		{"metrics-server", "0.5.2", at(7), "", ""}, // 0.4 and 0.5 are not encoded

		// Velero: "1.18-latest" expected compatibility is a floor.
		{"velero", "1.14.0", at(17), "Velero 1.14.0 requires Kubernetes 1.18 or newer (target 1.17)", "https://github.com/velero-io/velero/blob/f8c23ef4e089f34e8904e6908ca5e8f2d39e94e0/README.md#velero-compatibility-matrix"},
		{"velero", "1.14.0", v37, "", ""},
		{"velero", "1.5.0", v37, "", ""}, // not in the table: not judged

		// prometheus-operator: minimums by the 0.84.0 split.
		{"prometheus-operator", "0.84.0", at(24), "Prometheus Operator 0.84.0 requires Kubernetes 1.25 or newer (target 1.24)", "https://github.com/prometheus-operator/prometheus-operator/blob/7e6ede52280c009e23e99aebac66d4cac82664d4/Documentation/getting-started/compatibility.md#kubernetes"},
		{"prometheus-operator", "0.89.0", v37, "", ""},
		{"prometheus-operator", "0.30.0", at(15), "Prometheus Operator 0.30.0 requires Kubernetes 1.16 or newer (target 1.15)", "https://github.com/prometheus-operator/prometheus-operator/blob/7e6ede52280c009e23e99aebac66d4cac82664d4/Documentation/getting-started/compatibility.md#kubernetes"},
		{"prometheus-operator", "0.30.0", v37, "", ""},
		{"prometheus-operator", "0.83.0", at(16), "", ""},

		// Products whose upstream publishes no range, or only a tested-with
		// list, stay unjudged for compatibility on purpose (see the comments
		// of their registry files).
		{"argo-cd", "3.4.0", at(20), "", ""},
		{"flux", "2.7.0", at(20), "", ""},
		{"traefik", "3.6.0", at(20), "", ""},
		{"gatekeeper", "3.21.0", at(20), "", ""},
		{"kube-state-metrics", "1.9.7", v37, "", ""},
		{"coredns", "1.6.2", v37, "", ""},
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
		if !strings.Contains(f.Detail, "compatibility range") {
			t.Errorf("%s %s at %s: detail %q does not name the compatibility range", tc.id, tc.version, tc.target, f.Detail)
		}
	}
}

// The three golden pins of the sweep, at Kubernetes 1.37 on 2026-10-10: the
// whole finding, so that the wording, the range quoted and the sources move
// only on purpose.
func TestEmbeddedCompatGoldenAt137(t *testing.T) {
	addons, err := registry.Load()
	if err != nil {
		t.Fatal(err)
	}
	k := kb.KB{AddOns: addons}
	target := inventory.Version{Major: 1, Minor: 37}
	cases := []struct {
		id, version, title, detail string
		citations                  []string
	}{
		{"karpenter", "1.6.0", "Karpenter 1.6.0 supports Kubernetes up to 1.34 (target 1.37)",
			`Installed version 1.6.0 matches compatibility range ">=1.6.0 <1.9.0", which supports Kubernetes up to 1.34.`,
			[]string{"https://karpenter.sh/v1.14/upgrading/compatibility/", "https://github.com/aws/karpenter-provider-aws/blob/6d1426c4c5155fbcba2a36175f26557e6ffc9363/website/content/en/docs/upgrading/compatibility.md"}},
		{"cert-manager", "1.19.0", "cert-manager 1.19.0 supports Kubernetes up to 1.35 (target 1.37)",
			`Installed version 1.19.0 matches compatibility range ">=1.19.0 <1.20.0", which supports Kubernetes 1.31 through 1.35.`,
			[]string{"https://cert-manager.io/docs/releases/"}},
		{"metrics-server", "0.3.6", "Kubernetes Metrics Server 0.3.6 supports Kubernetes up to 1.21 (target 1.37)",
			`Installed version 0.3.6 matches compatibility range ">=0.3.0 <0.4.0", which supports Kubernetes 1.8 through 1.21.`,
			[]string{"https://github.com/kubernetes-sigs/metrics-server/blob/e32f7808d40e6f743c4111c9a746a88a99878765/README.md#compatibility-matrix"}},
	}
	for _, tc := range cases {
		var got []Finding
		for _, f := range evalAddOns(addOnAt(tc.id, tc.version), k, target, day("2026-10-10")) {
			if f.Category == CatChartIncompat {
				got = append(got, f)
			}
		}
		if len(got) != 1 {
			t.Fatalf("%s %s: %d compatibility findings, want 1", tc.id, tc.version, len(got))
		}
		f := got[0]
		if f.Severity != SevBlocker || f.Title != tc.title || f.Detail != tc.detail || !slices.Equal(f.Citations, tc.citations) {
			t.Errorf("%s %s:\n got  %s %q\n      %q\n      %q\n want %q\n      %q\n      %q", tc.id, tc.version,
				f.Severity, f.Title, f.Detail, f.Citations, tc.title, tc.detail, tc.citations)
		}
	}
}
