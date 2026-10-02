package collect

import (
	"slices"
	"testing"
	"time"

	"github.com/abd-ulbasit/upgradescope/internal/engine"
	"github.com/abd-ulbasit/upgradescope/internal/inventory"
	"github.com/abd-ulbasit/upgradescope/internal/kb"
	"github.com/abd-ulbasit/upgradescope/registry"
)

// Helm installs are judged by the release's appVersion, end to end against
// the embedded registry. The chart/app pairs are real entries of the
// upstream chart indexes: kubernetes-sigs.github.io/external-dns (1.14.5 →
// 0.14.2, 1.15.0 → 0.15.0) and argoproj.github.io/argo-helm (5.46.0 →
// v2.8.3). Before, the chart version (1.x) was compared against compat
// ranges written in app versions (0.10–0.17) and never matched.
func TestHelmInstallsJudgedByAppVersion(t *testing.T) {
	addons, err := registry.Load()
	if err != nil {
		t.Fatal(err)
	}
	k := kb.KB{AddOns: addons, Skew: kb.DefaultSkewPolicy(), MaxKnownK8s: inventory.Version{Major: 1, Minor: 99}}
	now := time.Date(2026, 10, 2, 0, 0, 0, 0, time.UTC)
	cases := []struct {
		name       string
		images     []nsImage
		release    inventory.HelmRelease
		target     inventory.Version
		wantID     string
		wantVer    string
		wantFinder string // a finding key the report must contain
	}{
		{
			name:       "external-dns chart 1.14.5 / app 0.14.2 matches the 0.10–0.17 compat row",
			release:    inventory.HelmRelease{Name: "external-dns", Namespace: "dns", ChartName: "external-dns", ChartVersion: "1.14.5", AppVersion: "0.14.2"},
			target:     inventory.Version{Major: 1, Minor: 34},
			wantID:     "external-dns",
			wantVer:    "0.14.2",
			wantFinder: "chart-incompat/external-dns",
		},
		{
			name:       "external-dns chart 1.15.0 / app 0.15.0 plus image v0.15.0",
			images:     []nsImage{{"dns", "registry.k8s.io/external-dns/external-dns:v0.15.0"}},
			release:    inventory.HelmRelease{Name: "external-dns", Namespace: "dns", ChartName: "external-dns", ChartVersion: "1.15.0", AppVersion: "0.15.0"},
			target:     inventory.Version{Major: 1, Minor: 34},
			wantID:     "external-dns",
			wantVer:    "0.15.0",
			wantFinder: "chart-incompat/external-dns",
		},
		{
			name:       "argo-cd chart 5.46.0 / app v2.8.3 maps to the 2.8 cycle, EOL since 2024-05-07",
			release:    inventory.HelmRelease{Name: "argocd", Namespace: "argocd", ChartName: "argo-cd", ChartVersion: "5.46.0", AppVersion: "v2.8.3"},
			target:     inventory.Version{Major: 1, Minor: 34},
			wantID:     "argo-cd",
			wantVer:    "2.8.3",
			wantFinder: "eol-addon/argo-cd/2.8",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			detected, _ := matchAddOns(addOnEvidence{images: tc.images, releases: []inventory.HelmRelease{tc.release}}, addons)
			if len(detected) != 1 || detected[0].ID != tc.wantID || detected[0].Version != tc.wantVer ||
				detected[0].ChartVersion != tc.release.ChartVersion || detected[0].Source != "chart" {
				t.Fatalf("detected %+v, want %s app %s chart %s", detected, tc.wantID, tc.wantVer, tc.release.ChartVersion)
			}
			rep := engine.Evaluate(inventory.Inventory{AddOns: detected}, k, tc.target, now)
			var keys []string
			for _, f := range rep.Findings {
				keys = append(keys, f.Key)
			}
			if !slices.Contains(keys, tc.wantFinder) {
				t.Fatalf("findings %v missing %s", keys, tc.wantFinder)
			}
		})
	}
}

// Image-only installs (kubectl apply, kustomize, Argo CD's helm template:
// no release secret) end to end: a mirrored upstream ingress-nginx gets the
// EOL blocker, while the vendor-supported AKS and RKE2 builds do not.
func TestImageOnlyIngressNginxVerdicts(t *testing.T) {
	addons, err := registry.Load()
	if err != nil {
		t.Fatal(err)
	}
	k := kb.KB{AddOns: addons, Skew: kb.DefaultSkewPolicy(), MaxKnownK8s: inventory.Version{Major: 1, Minor: 99}}
	now := time.Date(2026, 10, 2, 0, 0, 0, 0, time.UTC)
	cases := []struct {
		image       string
		wantBlocker bool
	}{
		{"harbor.corp.example/k8s/ingress-nginx/controller:v1.11.2", true},
		{"quay.io/kubernetes-ingress-controller/nginx-ingress-controller:0.26.1", true},
		{"123456789012.dkr.ecr.us-east-1.amazonaws.com/registry-k8s-io/ingress-nginx/controller-chroot:v1.11.3", true},
		{"mcr.microsoft.com/oss/kubernetes/ingress/nginx-ingress-controller:v1.11.5", false},
		{"rancher/nginx-ingress-controller:nginx-1.9.4-hardened1", false},
	}
	for _, tc := range cases {
		detected, _ := matchAddOns(addOnEvidence{images: []nsImage{{"ingress", tc.image}}}, addons)
		rep := engine.Evaluate(inventory.Inventory{AddOns: detected}, k, inventory.Version{Major: 1, Minor: 35}, now)
		got := slices.ContainsFunc(rep.Findings, func(f engine.Finding) bool { return f.Key == "eol-addon/ingress-nginx" })
		if got != tc.wantBlocker {
			t.Errorf("%s: ingress-nginx EOL blocker = %v, want %v (findings %+v)", tc.image, got, tc.wantBlocker, rep.Findings)
		}
	}
}

// Managed CNI and add-on builds carry the provider's support, so the
// upstream release line's end of life must not block them (#18). Each of
// these old lines has ended upstream; the upstream builds next to them
// prove the lines still block when the build is upstream's.
func TestProviderBuildsGetNoUpstreamEOL(t *testing.T) {
	addons, err := registry.Load()
	if err != nil {
		t.Fatal(err)
	}
	k := kb.KB{AddOns: addons, Skew: kb.DefaultSkewPolicy(), MaxKnownK8s: inventory.Version{Major: 1, Minor: 99}}
	now := time.Date(2026, 10, 2, 0, 0, 0, 0, time.UTC)
	cases := []struct {
		image       string
		wantBlocker bool
	}{
		{"gke.gcr.io/calico/node:v3.26.3-gke.13", false},
		{"mcr.microsoft.com/oss/calico/node:v3.28.2", false},
		{"mcr.microsoft.com/oss/cilium/cilium:1.16.6", false},
		{"gke.gcr.io/cilium/cilium:v1.15.10-gke.9", false},
		{"mcr.microsoft.com/oss/istio/pilot:1.24.3-distroless", false},
		{"quay.io/calico/node:v3.26.3", true},
		{"quay.io/cilium/cilium:v1.15.10", true},
	}
	for _, tc := range cases {
		detected, _ := matchAddOns(addOnEvidence{images: []nsImage{{"kube-system", tc.image}}}, addons)
		rep := engine.Evaluate(inventory.Inventory{AddOns: detected}, k, inventory.Version{Major: 1, Minor: 36}, now)
		got := slices.ContainsFunc(rep.Findings, func(f engine.Finding) bool { return f.Severity == engine.SevBlocker })
		if got != tc.wantBlocker {
			t.Errorf("%s: blocker = %v, want %v (findings %+v)", tc.image, got, tc.wantBlocker, rep.Findings)
		}
	}
}
