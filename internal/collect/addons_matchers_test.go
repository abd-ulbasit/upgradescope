package collect

import (
	"testing"

	"github.com/abd-ulbasit/upgradescope/registry"
)

// Image paths that managed distributions and mirrors publish the add-ons
// under (#342). Every path below was read from the registry that serves it
// on 2026-10-10 (MCR's tag list API, Docker Hub's tags API, gcr.io's tag
// list, public ECR's registry API): the repository exists and its tags have
// the shape of the examples. The tags are real ones or of a real shape.
//
// An image is only worth matching when its tag reads back as the upstream
// version: the engine judges the upstream release line of whatever
// version it reads, so a tag that parsed to another version would raise a
// false end-of-life blocker. The vendor build suffixes ("-16", "-hotfix.N",
// "-gke.N", "-eks-1-27-N", "-debian-12-rN") must therefore not reach the
// version, which versionRe guarantees by reading the first dotted number.
func TestMatchAddOnsManagedDistroImagePaths(t *testing.T) {
	addons, err := registry.Load()
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		image, wantID, wantVersion string
	}{
		// AKS: mcr.microsoft.com/oss/kubernetes/* and the v2 tree AKS moved
		// to; Microsoft appends a build number or "-hotfix.DATE" to the
		// upstream tag.
		{"mcr.microsoft.com/oss/kubernetes/coredns:v1.9.4", "coredns", "1.9.4"},
		{"mcr.microsoft.com/oss/kubernetes/coredns:v1.9.4-hotfix.20240704", "coredns", "1.9.4"},
		{"mcr.microsoft.com/oss/v2/kubernetes/coredns:v1.9.4-7", "coredns", "1.9.4"},
		{"mcr.microsoft.com/oss/kubernetes/metrics-server:v0.6.3", "metrics-server", "0.6.3"},
		{"mcr.microsoft.com/oss/v2/kubernetes/metrics-server:v0.9.0-7", "metrics-server", "0.9.0"},
		{"mcr.microsoft.com/oss/kubernetes/kube-state-metrics:v2.9.2", "kube-state-metrics", "2.9.2"},
		{"mcr.microsoft.com/oss/v2/kubernetes/kube-state-metrics:v2.20.0-6", "kube-state-metrics", "2.20.0"},
		// Azure CNI powered by Cilium and AKS's Calico are Microsoft's
		// builds under AKS's support policy. Their tags do map to the
		// upstream version (v1.14.0-16, release-v3.26), but an upstream
		// release line's end of life is not AKS's, so matching them would
		// raise a false end-of-life blocker (#18,
		// TestProviderBuildsGetNoUpstreamEOL): they stay unrecognized, an
		// informational gap, until AKS has entries of its own.
		{"mcr.microsoft.com/oss/cilium/cilium:v1.14.0", "", ""},
		{"mcr.microsoft.com/oss/cilium/cilium:1.14.0-16", "", ""},
		{"mcr.microsoft.com/oss/cilium/operator-generic:1.14.0", "", ""},
		{"mcr.microsoft.com/oss/v2/cilium/cilium:v1.20.0-7", "", ""},
		{"mcr.microsoft.com/oss/calico/node:release-v3.26", "", ""},
		{"mcr.microsoft.com/oss/v2/calico/node:v3.32.1-6", "", ""},
		// Through a pull-through cache that keeps the MCR location.
		{"harbor.corp.example/mcr.microsoft.com/oss/kubernetes/coredns:v1.9.4", "coredns", "1.9.4"},
		// The AKS application routing build keeps its own entry.
		{"mcr.microsoft.com/oss/kubernetes/ingress/nginx-ingress-controller:v1.11.5", "aks-app-routing-nginx", "1.11.5"},

		// EKS: the public EKS Distro build (tag "<upstream>-eks-<k8s>-<n>")
		// and the EKS add-on's own ECR repository.
		{"public.ecr.aws/eks-distro/kubernetes-sigs/metrics-server:v0.6.4-eks-1-27-20", "metrics-server", "0.6.4"},
		{"public.ecr.aws/eks-distro/coredns/coredns:v1.13.2-eks-1-36-1", "coredns", "1.13.2"},
		{"602401143452.dkr.ecr.us-west-2.amazonaws.com/eks/coredns:v1.11.1-eksbuild.4", "coredns", "1.11.1"},

		// GKE: gcr.io/gke-release/* and its gke.gcr.io alias, tag
		// "<upstream>-gke.<n>". Only add-ons without release-line end dates.
		{"gcr.io/gke-release/metrics-server:v0.8.0-gke.13", "metrics-server", "0.8.0"},
		{"gke.gcr.io/metrics-server:v0.8.0-gke.13", "metrics-server", "0.8.0"},
		{"gcr.io/gke-release/kube-state-metrics:v2.14.0-gke.68", "kube-state-metrics", "2.14.0"},
		{"gke.gcr.io/kube-state-metrics:v2.14.0-gke.68", "kube-state-metrics", "2.14.0"},
		// GKE's Calico and Dataplane V2 Cilium are Google's builds under
		// GKE's support policy: not matched, so no upstream end of life is
		// raised for them.
		{"gcr.io/gke-release/calico/node:v3.30.4-gke.0", "", ""},
		{"gcr.io/gke-release/cilium/cilium:v1.9.5-gke.42", "", ""},
		{"gke.gcr.io/cilium/cilium:v1.9.5-gke.42", "", ""},

		// RKE2 and k3s: Rancher's verbatim mirrors, docker.io/rancher/
		// mirrored-<org>-<name> with the upstream tag.
		{"docker.io/rancher/mirrored-coredns-coredns:1.9.4", "coredns", "1.9.4"},
		{"rancher/mirrored-coredns-coredns:1.14.7", "coredns", "1.14.7"},
		{"rancher/mirrored-metrics-server:v0.8.0", "metrics-server", "0.8.0"},
		{"rancher/mirrored-kube-state-metrics-kube-state-metrics:v2.15.0", "kube-state-metrics", "2.15.0"},
		{"rancher/mirrored-prometheus-operator-prometheus-operator:v0.89.0", "prometheus-operator", "0.89.0"},
		{"rancher/mirrored-cilium-cilium:v1.20.2", "cilium", "1.20.2"},
		{"rancher/mirrored-cilium-operator-generic:v1.20.2", "cilium", "1.20.2"},
		{"rancher/mirrored-cilium-operator-aws:v1.20.2", "cilium", "1.20.2"},
		{"rancher/mirrored-cilium-operator-azure:v1.20.2", "cilium", "1.20.2"},
		{"rancher/mirrored-cilium-hubble-relay:v1.20.2", "cilium", "1.20.2"},
		{"rancher/mirrored-calico-node:v3.33.0", "calico", "3.33.0"},
		{"rancher/mirrored-calico-cni:v3.32.2", "calico", "3.32.2"},
		{"rancher/mirrored-calico-typha:v3.32.2", "calico", "3.32.2"},
		{"rancher/mirrored-calico-kube-controllers:v3.32.2", "calico", "3.32.2"},
		{"registry.rancher.com/rancher/mirrored-coredns-coredns:1.9.4", "coredns", "1.9.4"},
		// The hardened-* images are Rancher's own rebuilds with their own
		// support: not matched.
		{"rancher/hardened-coredns:v1.12.0-build20250403", "", ""},
		{"rancher/hardened-k8s-metrics-server:v0.9.0-build20260928", "", ""},

		// Bitnami's rebuilds keep the upstream version at the front of the
		// tag ("2.9.0-debian-11-r5"). public.ecr.aws/bitnami and
		// docker.io/bitnami no longer hold these repositories (checked
		// 2026-10-10); the legacy namespace does.
		{"public.ecr.aws/bitnami/kube-state-metrics:2.9.0", "kube-state-metrics", "2.9.0"},
		{"docker.io/bitnami/kube-state-metrics:2.9.0-debian-11-r5", "kube-state-metrics", "2.9.0"},
		{"docker.io/bitnamilegacy/kube-state-metrics:2.16.0-debian-12-r5", "kube-state-metrics", "2.16.0"},
		{"docker.io/bitnamilegacy/metrics-server:0.8.0-debian-12-r4", "metrics-server", "0.8.0"},
		{"docker.io/bitnamilegacy/prometheus-operator:0.85.0-debian-12-r0", "prometheus-operator", "0.85.0"},
		{"docker.io/bitnamilegacy/external-dns:0.18.0-debian-12-r4", "external-dns", "0.18.0"},
	}
	for _, tc := range cases {
		if ids := imageAddOns(parseImage(tc.image), addons); len(ids) > 1 {
			t.Errorf("%s: claimed by %v, want at most one entry", tc.image, ids)
		}
		got, unrec := matchAddOns(addOnEvidence{images: []nsImage{{"ns", tc.image}}}, addons)
		if tc.wantID == "" {
			if len(got) != 0 || len(unrec) != 1 {
				t.Errorf("%s: want unrecognized, got addons=%+v unrecognized=%v", tc.image, got, unrec)
			}
			continue
		}
		if len(got) != 1 || got[0].ID != tc.wantID || got[0].Version != tc.wantVersion {
			t.Errorf("%s: got %+v, want %s %q", tc.image, got, tc.wantID, tc.wantVersion)
		}
	}
}

// A matched managed-distro image whose tag names no version (latest, a
// digest) is an install with no version: the engine reports it as an info
// "no lifecycle data" finding and never a blocker, so a tag that cannot be
// mapped to an upstream release is not judged by one.
func TestMatchAddOnsManagedDistroImageWithoutVersionIsUnjudged(t *testing.T) {
	addons, err := registry.Load()
	if err != nil {
		t.Fatal(err)
	}
	for _, image := range []string{
		"mcr.microsoft.com/oss/kubernetes/coredns:latest",
		"mcr.microsoft.com/oss/kubernetes/metrics-server",
		"rancher/mirrored-calico-node:stable",
	} {
		got, _ := matchAddOns(addOnEvidence{images: []nsImage{{"ns", image}}}, addons)
		if len(got) != 1 || got[0].Version != "" {
			t.Errorf("%s: got %+v, want one install with no version", image, got)
		}
	}
}
