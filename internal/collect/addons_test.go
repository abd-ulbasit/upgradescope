package collect

import (
	"context"
	"fmt"
	"reflect"
	"slices"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	kubefake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/abd-ulbasit/upgradescope/internal/inventory"
	"github.com/abd-ulbasit/upgradescope/registry"
)

func testRegistry() []registry.AddOn {
	return []registry.AddOn{
		{ID: "ingress-nginx", Matchers: registry.Matchers{
			Images: []string{"ingress-nginx/controller"},
			Charts: []string{"ingress-nginx"},
		}},
		{ID: "cilium", Matchers: registry.Matchers{
			Images: []string{"cilium/cilium"},
		}},
		{ID: "istio", Matchers: registry.Matchers{
			Images: []string{"istio/proxyv2", "istio/pilot"},
			Charts: []string{"istiod"},
		}},
	}
}

func TestMatchAddOns(t *testing.T) {
	cases := []struct {
		name      string
		images    []nsImage
		releases  []inventory.HelmRelease
		want      []inventory.AddOnInstance
		wantUnrec []string
	}{
		{
			name:   "image with tag",
			images: []nsImage{{"ingress-nginx", "registry.k8s.io/ingress-nginx/controller:v1.9.4"}},
			want:   []inventory.AddOnInstance{{ID: "ingress-nginx", Version: "1.9.4", Namespaces: []string{"ingress-nginx"}, Source: "image"}},
		},
		{
			name:   "image with tag and digest",
			images: []nsImage{{"ingress-nginx", "registry.k8s.io/ingress-nginx/controller:v1.9.4@sha256:0123abcd"}},
			want:   []inventory.AddOnInstance{{ID: "ingress-nginx", Version: "1.9.4", Namespaces: []string{"ingress-nginx"}, Source: "image"}},
		},
		{
			name:   "digest only — version unknown",
			images: []nsImage{{"kube-system", "quay.io/cilium/cilium@sha256:0123abcd"}},
			want:   []inventory.AddOnInstance{{ID: "cilium", Version: "", Namespaces: []string{"kube-system"}, Source: "image"}},
		},
		{
			name:   "no tag at all",
			images: []nsImage{{"kube-system", "quay.io/cilium/cilium"}},
			want:   []inventory.AddOnInstance{{ID: "cilium", Version: "", Namespaces: []string{"kube-system"}, Source: "image"}},
		},
		{
			// The registry speaks app versions; the chart version is evidence only.
			name:     "chart evidence: version is the release's appVersion, chart version kept, v-prefixes stripped",
			images:   []nsImage{{"ingress-nginx", "registry.k8s.io/ingress-nginx/controller:v1.8.0"}},
			releases: []inventory.HelmRelease{{Name: "ingress-nginx", Namespace: "ingress-nginx", ChartName: "ingress-nginx", ChartVersion: "v4.7.1", AppVersion: "v1.8.1", Status: "deployed"}},
			want:     []inventory.AddOnInstance{{ID: "ingress-nginx", Version: "1.8.1", ChartVersion: "4.7.1", Namespaces: []string{"ingress-nginx"}, Source: "chart"}},
		},
		{
			name:     "chart without appVersion falls back to the image version, never the chart version",
			images:   []nsImage{{"ingress-nginx", "registry.k8s.io/ingress-nginx/controller:v1.9.4"}},
			releases: []inventory.HelmRelease{{Name: "ingress-nginx", Namespace: "ingress-nginx", ChartName: "ingress-nginx", ChartVersion: "4.8.3", Status: "deployed"}},
			want:     []inventory.AddOnInstance{{ID: "ingress-nginx", Version: "1.9.4", ChartVersion: "4.8.3", Namespaces: []string{"ingress-nginx"}, Source: "chart"}},
		},
		{
			name:     "chart without appVersion and no image: version unknown",
			releases: []inventory.HelmRelease{{Name: "ingress-nginx", Namespace: "ingress-nginx", ChartName: "ingress-nginx", ChartVersion: "4.8.3", Status: "deployed"}},
			want:     []inventory.AddOnInstance{{ID: "ingress-nginx", ChartVersion: "4.8.3", Namespaces: []string{"ingress-nginx"}, Source: "chart"}},
		},
		{
			name: "two releases in two namespaces: one instance each",
			releases: []inventory.HelmRelease{
				{Name: "a", Namespace: "a", ChartName: "ingress-nginx", ChartVersion: "4.10.0", AppVersion: "1.10.0"},
				{Name: "b", Namespace: "b", ChartName: "ingress-nginx", ChartVersion: "4.9.1", AppVersion: "1.9.6"},
			},
			want: []inventory.AddOnInstance{
				{ID: "ingress-nginx", Version: "1.10.0", ChartVersion: "4.10.0", Namespaces: []string{"a"}, Source: "chart"},
				{ID: "ingress-nginx", Version: "1.9.6", ChartVersion: "4.9.1", Namespaces: []string{"b"}, Source: "chart"},
			},
		},
		{
			name: "two releases in one namespace: oldest app version and oldest chart version",
			releases: []inventory.HelmRelease{
				{Name: "a", Namespace: "ingress", ChartName: "ingress-nginx", ChartVersion: "4.10.0", AppVersion: "1.10.0"},
				{Name: "b", Namespace: "ingress", ChartName: "ingress-nginx", ChartVersion: "4.9.1", AppVersion: "1.9.6"},
			},
			want: []inventory.AddOnInstance{{ID: "ingress-nginx", Version: "1.9.6", ChartVersion: "4.9.1", Namespaces: []string{"ingress"}, Source: "chart"}},
		},
		{
			// Each namespace is its own install, judged at its own version:
			// merging them put the oldest line's EOL on every namespace.
			name: "mixed versions across namespaces are not merged",
			images: []nsImage{
				{"mesh-new", "istio/proxyv2:1.31.1"},
				{"mesh-mid", "docker.io/istio/proxyv2:1.30.5"},
				{"mesh-old", "istio/proxyv2:1.28.10"},
			},
			want: []inventory.AddOnInstance{
				{ID: "istio", Version: "1.30.5", Namespaces: []string{"mesh-mid"}, Source: "image"},
				{ID: "istio", Version: "1.31.1", Namespaces: []string{"mesh-new"}, Source: "image"},
				{ID: "istio", Version: "1.28.10", Namespaces: []string{"mesh-old"}, Source: "image"},
			},
		},
		{
			// The release's appVersion beats image tags in its own namespace
			// only: an older image-only install elsewhere keeps its version.
			name: "Helm appVersion applies only in the release's namespace",
			images: []nsImage{
				{"istio-system", "istio/pilot:1.31.1"},
				{"istio-legacy", "istio/pilot:1.28.10"},
			},
			releases: []inventory.HelmRelease{{Name: "istiod", Namespace: "istio-system", ChartName: "istiod", ChartVersion: "1.31.1", AppVersion: "1.31.1", Status: "deployed"}},
			want: []inventory.AddOnInstance{
				{ID: "istio", Version: "1.28.10", Namespaces: []string{"istio-legacy"}, Source: "image"},
				{ID: "istio", Version: "1.31.1", ChartVersion: "1.31.1", Namespaces: []string{"istio-system"}, Source: "chart"},
			},
		},
		{
			// Images on the release's own line agree with it: the
			// appVersion stands for them (a patch-lagging pod is not
			// another install).
			name: "Helm appVersion wins over image tags on its release line",
			images: []nsImage{
				{"istio-system", "istio/pilot:1.31.1"},
				{"istio-system", "istio/proxyv2:1.31.0"},
				{"istio-system", "istio/proxyv2@sha256:0123abcd"},
			},
			releases: []inventory.HelmRelease{{Name: "istiod", Namespace: "istio-system", ChartName: "istiod", ChartVersion: "1.31.1", AppVersion: "1.31.1", Status: "deployed"}},
			want:     []inventory.AddOnInstance{{ID: "istio", Version: "1.31.1", ChartVersion: "1.31.1", Namespaces: []string{"istio-system"}, Source: "chart"}},
		},
		{
			// #165: an istioctl canary revision on an older line beside a
			// newer istiod release was hidden by the appVersion. An image
			// on another release line is its own install in the namespace.
			name: "an image on another release line than the Helm release is its own install",
			images: []nsImage{
				{"istio-system", "istio/pilot:1.31.1"},
				{"istio-system", "docker.io/istio/pilot:1.28.10"},
			},
			releases: []inventory.HelmRelease{{Name: "istiod", Namespace: "istio-system", ChartName: "istiod", ChartVersion: "1.31.1", AppVersion: "1.31.1", Status: "deployed"}},
			want: []inventory.AddOnInstance{
				{ID: "istio", Version: "1.28.10", Namespaces: []string{"istio-system"}, Source: "image"},
				{ID: "istio", Version: "1.31.1", ChartVersion: "1.31.1", Namespaces: []string{"istio-system"}, Source: "chart"},
			},
		},
		{
			// Both ways: an image tag overridden in the release's values
			// onto a newer line than the chart's appVersion is what runs,
			// so it is judged too; the release keeps its appVersion.
			name: "an image on a newer release line than the Helm release is its own install too",
			images: []nsImage{
				{"ingress-nginx", "registry.k8s.io/ingress-nginx/controller:v1.9.4"},
			},
			releases: []inventory.HelmRelease{{Name: "ingress-nginx", Namespace: "ingress-nginx", ChartName: "ingress-nginx", ChartVersion: "4.7.1", AppVersion: "1.8.1", Status: "deployed"}},
			want: []inventory.AddOnInstance{
				{ID: "ingress-nginx", Version: "1.8.1", ChartVersion: "4.7.1", Namespaces: []string{"ingress-nginx"}, Source: "chart"},
				{ID: "ingress-nginx", Version: "1.9.4", Namespaces: []string{"ingress-nginx"}, Source: "image"},
			},
		},
		{
			// The images off the release's lines are judged like a
			// namespace without a release: at their oldest version.
			name: "images off the release's lines form one install at their oldest version",
			images: []nsImage{
				{"istio-system", "istio/pilot:1.29.3"},
				{"istio-system", "istio/proxyv2:1.28.10"},
				{"istio-system", "istio/proxyv2:1.31.1"},
			},
			releases: []inventory.HelmRelease{{Name: "istiod", Namespace: "istio-system", ChartName: "istiod", ChartVersion: "1.31.1", AppVersion: "1.31.1", Status: "deployed"}},
			want: []inventory.AddOnInstance{
				{ID: "istio", Version: "1.28.10", Namespaces: []string{"istio-system"}, Source: "image"},
				{ID: "istio", Version: "1.31.1", ChartVersion: "1.31.1", Namespaces: []string{"istio-system"}, Source: "chart"},
			},
		},
		{
			name: "oldest version wins semver-aware, not lexicographically",
			images: []nsImage{
				{"kube-system", "quay.io/cilium/cilium:v1.10.0"},
				{"kube-system", "quay.io/cilium/cilium:v1.9.4"},
			},
			want: []inventory.AddOnInstance{{ID: "cilium", Version: "1.9.4", Namespaces: []string{"kube-system"}, Source: "image"}},
		},
		{
			name:      "unrecognized images deduped by repo, tag-stripped",
			images:    []nsImage{{"shop", "docker.io/library/redis:7"}, {"crm", "docker.io/library/redis:7.2"}},
			wantUnrec: []string{"docker.io/library/redis"},
		},
		{
			name: "one instance per namespace, sorted by add-on then namespace",
			images: []nsImage{
				{"b-ns", "registry.k8s.io/ingress-nginx/controller:v1.9.4"},
				{"kube-system", "quay.io/cilium/cilium:v1.16.1"},
				{"a-ns", "registry.k8s.io/ingress-nginx/controller:v1.9.4"},
				{"a-ns", "registry.k8s.io/ingress-nginx/controller:v1.9.4"},
			},
			want: []inventory.AddOnInstance{
				{ID: "cilium", Version: "1.16.1", Namespaces: []string{"kube-system"}, Source: "image"},
				{ID: "ingress-nginx", Version: "1.9.4", Namespaces: []string{"a-ns"}, Source: "image"},
				{ID: "ingress-nginx", Version: "1.9.4", Namespaces: []string{"b-ns"}, Source: "image"},
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, unrec := matchAddOns(addOnEvidence{images: tc.images, releases: tc.releases}, testRegistry())
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("addons = %#v\nwant   %#v", got, tc.want)
			}
			if !reflect.DeepEqual(unrec, tc.wantUnrec) {
				t.Errorf("unrecognized = %#v, want %#v", unrec, tc.wantUnrec)
			}
		})
	}
}

func TestParseImage(t *testing.T) {
	cases := []struct {
		image string
		want  imageRef
	}{
		{"registry.k8s.io/ingress-nginx/controller:v1.9.4", imageRef{"registry.k8s.io", "ingress-nginx/controller", "v1.9.4"}},
		{"redis", imageRef{"docker.io", "library/redis", ""}},
		{"redis:7.2", imageRef{"docker.io", "library/redis", "7.2"}},
		{"velero/velero:v1.14.0", imageRef{"docker.io", "velero/velero", "v1.14.0"}},
		{"index.docker.io/library/traefik:v3.1", imageRef{"docker.io", "library/traefik", "v3.1"}},
		{"registry-1.docker.io/bitnami/etcd:3.5.15", imageRef{"docker.io", "bitnami/etcd", "3.5.15"}},
		{"localhost/app:1", imageRef{"localhost", "app", "1"}},
		{"reg.example:5000/team/app:1.2@sha256:0123", imageRef{"reg.example:5000", "team/app", "1.2"}},
		{"quay.io/cilium/cilium@sha256:0123", imageRef{"quay.io", "cilium/cilium", ""}},
		{"harbor.corp.example/registry.k8s.io/ingress-nginx/controller:v1.10.0",
			imageRef{"harbor.corp.example", "registry.k8s.io/ingress-nginx/controller", "v1.10.0"}},
	}
	for _, tc := range cases {
		if got := parseImage(tc.image); got != tc.want {
			t.Errorf("parseImage(%q) = %+v, want %+v", tc.image, got, tc.want)
		}
	}
}

func TestVersionFromTag(t *testing.T) {
	cases := map[string]string{
		"v1.9.4":                 "1.9.4",
		"1.11.3-debian-12-r0":    "1.11.3",
		"nginx-1.9.4-hardened1":  "1.9.4",
		"v1.11.1-eksbuild.4":     "1.11.1",
		"3.5.15-0":               "3.5.15",
		"1.31.1-distroless":      "1.31.1",
		"v1.20.0-rc.1":           "1.20.0-rc.1",
		"1.31.0-beta.2":          "1.31.0-beta.2",
		"v3.1":                   "3.1",
		"latest":                 "",
		"":                       "",
		"sha-1a2b3c":             "",
		"v2.8.3+fips":            "2.8.3",
		"release-v1.7.27-ubuntu": "1.7.27",
	}
	for tag, want := range cases {
		if got := versionFromTag(tag); got != want {
			t.Errorf("versionFromTag(%q) = %q, want %q", tag, got, want)
		}
	}
}

// Real-world image references against the embedded registry: each must map
// to the expected add-on whatever registry, mirror, pull-through cache,
// digest or tag suffix it carries. Vendor forks map to their own entries,
// never to upstream ingress-nginx (whose EOL verdict would be false there).
func TestMatchAddOnsRealWorldImages(t *testing.T) {
	addons, err := registry.Load()
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		image, wantID, wantVersion string
	}{
		{"registry.k8s.io/ingress-nginx/controller:v1.11.3", "ingress-nginx", "1.11.3"},
		{"k8s.gcr.io/ingress-nginx/controller:v1.1.1", "ingress-nginx", "1.1.1"},
		{"registry.k8s.io/ingress-nginx/controller-chroot:v1.11.3", "ingress-nginx", "1.11.3"},
		{"quay.io/kubernetes-ingress-controller/nginx-ingress-controller:0.34.1", "ingress-nginx", "0.34.1"}, // pre-1.0 image
		{"123456789012.dkr.ecr.us-east-1.amazonaws.com/registry-k8s-io/ingress-nginx/controller:v1.11.2", "ingress-nginx", "1.11.2"},
		{"harbor.corp.example/k8s/ingress-nginx/controller:v1.11.2", "ingress-nginx", "1.11.2"},
		{"harbor.corp.example/registry.k8s.io/ingress-nginx/controller:v1.10.0", "ingress-nginx", "1.10.0"},
		{"registry.k8s.io/ingress-nginx/controller:v1.9.4@sha256:5b161f051d017e55d358435f295f5e9a297e66158f136321d9b04520ec6c48a3", "ingress-nginx", "1.9.4"},
		{"registry.k8s.io/ingress-nginx/controller@sha256:5b161f051d017e55d358435f295f5e9a297e66158f136321d9b04520ec6c48a3", "ingress-nginx", ""},
		{"docker.io/bitnami/nginx-ingress-controller:1.11.3-debian-12-r0", "ingress-nginx", "1.11.3"},
		{"bitnami/nginx-ingress-controller:1.11.3", "ingress-nginx", "1.11.3"},
		{"docker.io/bitnamilegacy/nginx-ingress-controller:1.11.3-debian-12-r0", "ingress-nginx", "1.11.3"},
		{"rancher/nginx-ingress-controller:nginx-1.9.4-hardened1", "rke2-ingress-nginx", "1.9.4"},
		// RKE2 and RKE1 publish their builds of ingress-nginx on one
		// repository, told apart by the tag only (#265): RKE2's are
		// "-hardenedN" (rke2-images-all lists of v1.28.15, v1.33.5 and
		// v1.35.1), RKE1's "-rancherN" (kontainer-driver-metadata). RKE1
		// itself is end of life, so its build is upstream ingress-nginx.
		{"docker.io/rancher/nginx-ingress-controller:v1.12.6-hardened1", "rke2-ingress-nginx", "1.12.6"},
		{"rancher/nginx-ingress-controller:v1.14.3-hardened2", "rke2-ingress-nginx", "1.14.3"},
		{"registry.rancher.com/rancher/nginx-ingress-controller:v1.12.6-hardened1", "rke2-ingress-nginx", "1.12.6"},
		{"rancher/nginx-ingress-controller:nginx-1.12.1-rancher4", "ingress-nginx", "1.12.1"},
		{"rancher/nginx-ingress-controller:nginx-1.9.4-rancher1", "ingress-nginx", "1.9.4"},
		{"rancher/nginx-ingress-controller:0.21.0-rancher1", "ingress-nginx", "0.21.0"},
		{"rancher/nginx-ingress-controller:0.16.2-rancher1", "ingress-nginx", "0.16.2"},
		// RKE2's first releases shipped "-rancherN" builds too (the
		// rke2-images.linux-amd64.txt of v1.20.11+rke2r1 and v1.21.2+rke2r1),
		// long out of support: Ingress NGINX alike.
		{"docker.io/rancher/nginx-ingress-controller:nginx-0.30.0-rancher1", "ingress-nginx", "0.30.0"},
		{"docker.io/rancher/nginx-ingress-controller:nginx-0.46.0-rancher1", "ingress-nginx", "0.46.0"},
		// A digest-only reference has no tag: on its own, the path-only
		// entry (labels and releases: TestMatchAddOnsUntaggedImageNamedByLabelsOrRelease).
		{"rancher/nginx-ingress-controller@sha256:5b161f051d017e55d358435f295f5e9a297e66158f136321d9b04520ec6c48a3", "ingress-nginx", ""},
		{"myregistry.example.com:5000/mirror/rancher/nginx-ingress-controller:nginx-1.9.4-rancher1", "ingress-nginx", "1.9.4"},
		{"rancher/nginx-ingress-controller", "ingress-nginx", ""},
		{"mcr.microsoft.com/oss/kubernetes/ingress/nginx-ingress-controller:v1.11.5", "aks-app-routing-nginx", "1.11.5"},
		{"coredns/coredns:1.11.1", "coredns", "1.11.1"},
		{"registry.k8s.io/coredns/coredns:v1.11.3", "coredns", "1.11.3"},
		{"602401143452.dkr.ecr.us-west-2.amazonaws.com/eks/coredns:v1.11.1-eksbuild.4", "coredns", "1.11.1"},
		{"quay.io/coreos/kube-state-metrics:v1.9.8", "kube-state-metrics", "1.9.8"},
		{"registry.k8s.io/kube-state-metrics/kube-state-metrics:v2.13.0", "kube-state-metrics", "2.13.0"},
		{"docker.io/istio/proxyv2:1.31.1", "istio", "1.31.1"},
		{"istio/pilot:1.31.1-distroless", "istio", "1.31.1"},
		{"gcr.io/istio-release/proxyv2:1.30.2", "istio", "1.30.2"},
		{"traefik:v3.1.2", "traefik", "3.1.2"},
		{"velero/velero:v1.14.0", "velero", "1.14.0"},
		{"quay.io/jetstack/cert-manager-controller:v1.15.3", "cert-manager", "1.15.3"},
		{"docker.io/calico/node:v3.26.1", "calico", "3.26.1"},
		{"quay.io/calico/cni:v3.28.0", "calico", "3.28.0"},
		{"quay.io/cilium/cilium:v1.16.1", "cilium", "1.16.1"},
		{"ghcr.io/kedacore/keda:2.15.1", "keda", "2.15.1"},
		{"ghcr.io/kyverno/kyverno:v1.12.5", "kyverno", "1.12.5"},
		{"reg.kyverno.io/kyverno/kyverno:v1.14.1", "kyverno", "1.14.1"},
		{"quay.io/argoproj/argocd:v2.12.3", "argo-cd", "2.12.3"},
		{"registry.k8s.io/etcd:3.5.15-0", "etcd", "3.5.15"},
		{"bitnami/etcd:3.5.15-debian-12-r3", "etcd", "3.5.15"},
		{"quay.io/coreos/etcd:v3.5.15", "etcd", "3.5.15"},
		{"123456789012.dkr.ecr.eu-west-1.amazonaws.com/registry-k8s-io/etcd:3.5.15-0", "etcd", "3.5.15"},
		{"gcr.io/etcd-development/etcd:v3.5.15", "etcd", "3.5.15"},
		// etcd opts into "*/etcd", so it is found behind a kubeadm
		// imageRepository, a Harbor proxy cache or any other mirror, and
		// under Bitnami's legacy namespace.
		{"harbor.corp/k8s/etcd:3.5.15-0", "etcd", "3.5.15"},
		{"myregistry.example.com/etcd:3.5.15-0", "etcd", "3.5.15"},
		{"myregistry.example.com:5000/mirror/registry.k8s.io/etcd:3.5.15-0", "etcd", "3.5.15"},
		{"docker.io/bitnami/etcd:3.5.15-debian-12-r3", "etcd", "3.5.15"},
		{"docker.io/bitnamilegacy/etcd:3.5.15-debian-12-r3", "etcd", "3.5.15"},
		{"etcd:3.5.15", "etcd", "3.5.15"},
		// Only the repository etcd: neighbours that merely contain the word
		// are other products.
		{"quay.io/coreos/etcd-operator:v0.9.4", "", ""},
		{"harbor.corp/k8s/etcd-backup:1.0.0", "", ""},
		{"harbor.corp/k8s/etcd/backup:1.0.0", "", ""},
		{"registry.k8s.io/external-dns/external-dns:v0.14.2", "external-dns", "0.14.2"},
		{"bitnami/external-dns:0.14.2-debian-12-r4", "external-dns", "0.14.2"},
		{"registry.k8s.io/metrics-server/metrics-server:v0.7.2", "metrics-server", "0.7.2"},
		{"quay.io/prometheus-operator/prometheus-operator:v0.75.0", "prometheus-operator", "0.75.0"},
		// Mirrors keep a vendor or upstream path as a suffix, whatever the
		// registry host and prefix (#49); a bare "controller" is no path.
		{"myregistry.example.com/ingress-nginx/controller:v1.11.3", "ingress-nginx", "1.11.3"},
		{"myregistry/mirror/ingress-nginx/controller:v1.11.3", "ingress-nginx", "1.11.3"},
		{"myregistry.example.com:5000/mirror/rancher/nginx-ingress-controller:nginx-1.9.4-hardened1", "rke2-ingress-nginx", "1.9.4"},
		{"myregistry.example.com/controller:v1.11.3", "", ""},
		{"myregistry/mirror/controller:v1.11.3", "", ""},
		// Retired as a whole (#49): each is end of life whatever its version.
		{"docker.io/kubernetesui/dashboard:v2.7.0", "kubernetes-dashboard", "2.7.0"},
		{"kubernetesui/dashboard-api:1.10.1", "kubernetes-dashboard", "1.10.1"},
		{"kubernetesui/dashboard-auth:1.2.2", "kubernetes-dashboard", "1.2.2"},
		{"kubernetesui/dashboard-web:1.6.0", "kubernetes-dashboard", "1.6.0"},
		// The v1.x line, one repository per architecture (#265).
		{"k8s.gcr.io/kubernetes-dashboard-amd64:v1.10.1", "kubernetes-dashboard", "1.10.1"},
		{"registry.k8s.io/kubernetes-dashboard-amd64:v1.10.1", "kubernetes-dashboard", "1.10.1"},
		{"gcr.io/google_containers/kubernetes-dashboard-amd64:v1.8.3", "kubernetes-dashboard", "1.8.3"},
		{"k8s.gcr.io/kubernetes-dashboard-arm:v1.10.1", "kubernetes-dashboard", "1.10.1"},
		{"k8s.gcr.io/kubernetes-dashboard-arm64:v1.8.3", "kubernetes-dashboard", "1.8.3"},
		{"k8s.gcr.io/kubernetes-dashboard-ppc64le:v1.8.1", "kubernetes-dashboard", "1.8.1"},
		{"k8s.gcr.io/kubernetes-dashboard-s390x:v1.6.2", "kubernetes-dashboard", "1.6.2"},
		{"gcr.io/google_containers/kubernetes-dashboard-arm:v1.6.3", "kubernetes-dashboard", "1.6.3"},
		// The sidecars version separately: matched, they would report their
		// version as the Dashboard's.
		{"kubernetesui/dashboard-metrics-scraper:1.2.1", "", ""},
		{"kubernetesui/metrics-scraper:v1.0.8", "", ""},
		{"grafana/promtail:3.0.0", "promtail", "3.0.0"},
		{"docker.io/grafana/promtail:2.9.4", "promtail", "2.9.4"},
		{"grafana/agent:v0.44.2", "grafana-agent", "0.44.2"},
		{"harbor.corp.example/dockerhub/grafana/agent-operator:v0.44.2", "grafana-agent", "0.44.2"},
		{"weaveworks/weave-kube:2.8.1", "weave-net", "2.8.1"},
		{"docker.io/weaveworks/weave-npc:2.8.1", "weave-net", "2.8.1"},
		// Synced from endoflife.date (#49).
		{"public.ecr.aws/karpenter/controller:1.0.8@sha256:5b161f051d017e55d358435f295f5e9a297e66158f136321d9b04520ec6c48a3", "karpenter", "1.0.8"},
		{"openpolicyagent/gatekeeper:v3.20.1", "gatekeeper", "3.20.1"},
		{"openpolicyagent/gatekeeper-crds:v3.20.1", "gatekeeper", "3.20.1"},
		{"cr.fluentbit.io/fluent/fluent-bit:4.0.3", "fluent-bit", "4.0.3"},
		{"fluent/fluent-bit:3.2.10", "fluent-bit", "3.2.10"},
		// Provider-managed builds follow the provider's support policy, not
		// upstream's (#18): GKE network policy / Dataplane V2, AKS Calico,
		// Azure CNI powered by Cilium, the AKS Istio and KEDA add-ons. No
		// upstream entry claims them, also through a mirror.
		{"gke.gcr.io/calico/node:v3.26.3-gke.13", "", ""},
		{"mcr.microsoft.com/oss/calico/node:v3.28.2", "", ""},
		{"mcr.microsoft.com/oss/cilium/cilium:1.16.6", "", ""},
		{"gke.gcr.io/cilium/cilium:v1.15.10-gke.9", "", ""},
		{"gcr.io/gke-release/calico/node:v3.26.3-gke.13", "", ""},
		{"mcr.microsoft.com/oss/istio/pilot:1.24.3-distroless", "", ""},
		{"mcr.microsoft.com/oss/kedacore/keda:2.14.1", "", ""},
		{"harbor.corp.example/mcr.microsoft.com/oss/calico/node:v3.28.2", "", ""},
		{"harbor.corp.example/mcr.microsoft.com/oss/kubernetes/ingress/nginx-ingress-controller:v1.11.5", "aks-app-routing-nginx", "1.11.5"},
		// Not add-ons the registry tracks.
		{"nginx/nginx-ingress:3.6.0", "", ""}, // F5 NGINX Ingress Controller, a different product
		{"docker.io/library/redis:7", "", ""},
		{"fluxcd/helm-operator:1.4.4", "", ""}, // Flux v1's Helm Operator, versioned on its own
		// Flux v2's controllers carry their own versions: each line maps
		// to the Flux line whose release ships it (#265, the flux2
		// releases' install.yaml and Components changelog), never read as
		// a Flux version.
		{"ghcr.io/fluxcd/source-controller:v1.5.0", "flux", "2.5"},
		{"ghcr.io/fluxcd/kustomize-controller:v1.5.1", "flux", "2.5"},
		{"ghcr.io/fluxcd/helm-controller:v1.2.0", "flux", "2.5"},
		{"ghcr.io/fluxcd/notification-controller:v1.6.0", "flux", "2.6"},
		{"ghcr.io/fluxcd/image-reflector-controller:v0.35.2", "flux", "2.6"},
		{"ghcr.io/fluxcd/image-automation-controller:v1.0.4", "flux", "2.7"},
		{"ghcr.io/fluxcd/source-watcher:v2.2.4", "flux", "2.9"},
		{"ghcr.io/fluxcd/source-watcher:v2.0.1", "flux", "2.7"},  // an extra component in Flux 2.7 (flux2 v2.7.0 notes)
		{"ghcr.io/fluxcd/source-controller:v0.36.1", "flux", ""}, // Flux v2 pre-GA (0.x): deliberately unmapped
		{"docker.io/fluxcd/helm-controller:v0.37.4", "flux", "2.2"},
		{"harbor.corp.example/ghcr/fluxcd/source-controller:v1.4.1@sha256:5b161f051d017e55d358435f295f5e9a297e66158f136321d9b04520ec6c48a3", "flux", "2.4"},
		{"ghcr.io/fluxcd/source-controller:v1.99.0", "flux", ""}, // a line not mapped yet: Flux, version unknown
		// Flux v1: the tag is the Flux release.
		{"docker.io/fluxcd/flux:1.25.4", "flux", "1.25.4"},
		{"quay.io/weaveworks/flux:1.12.0", "flux", "1.12.0"},
		{"docker.io/weaveworks/flux:1.13.0", "flux", "1.13.0"},
	}
	for _, tc := range cases {
		// One entry claims an image: a second would judge it twice.
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

// A tag-qualified matcher takes the tags it names ahead of another entry's
// path-only matcher of the same repository (#265); other tags, and an
// image without a tag, stay with the path-only one.
func TestImageTagPatternTakesPrecedence(t *testing.T) {
	addons := []registry.AddOn{
		{ID: "plain", Matchers: registry.Matchers{Images: []string{"acme/ctl"}}},
		{ID: "vendor", Matchers: registry.Matchers{Images: []string{"acme/ctl:*-vendor*"}}},
	}
	for image, want := range map[string]string{
		"acme/ctl:1.2.0-vendor1":               "vendor",
		"mirror.corp/x/acme/ctl:1.2.0-vendor1": "vendor",
		"acme/ctl:1.2.0-other1":                "plain",
		"acme/ctl:1.2.0":                       "plain",
		"acme/ctl":                             "plain",
	} {
		if ids := imageAddOns(parseImage(image), addons); !slices.Equal(ids, []string{want}) {
			t.Errorf("%s: claimed by %v, want [%s]", image, ids, want)
		}
	}
}

// An image pinned by digest alone has no tag for a tag pattern to match, so
// on its own a digest-only rancher/nginx-ingress-controller is RKE1's build
// as much as RKE2's. The pod's own labels, or a Helm release in its
// namespace, naming the entry whose tag-qualified matcher covers that
// repository settle it (#265): an RKE2 ingress pod pinned by digest is
// rke2-ingress-nginx, not a false end-of-life blocker. A tag always
// decides over labels, and without such evidence the path-only entry keeps
// the image.
func TestMatchAddOnsUntaggedImageNamedByLabelsOrRelease(t *testing.T) {
	addons, err := registry.Load()
	if err != nil {
		t.Fatal(err)
	}
	const digest = "rancher/nginx-ingress-controller@sha256:5b161f051d017e55d358435f295f5e9a297e66158f136321d9b04520ec6c48a3"
	rke2Labels := map[string]string{
		"helm.sh/chart":             "rke2-ingress-nginx-4.12.401",
		"app.kubernetes.io/name":    "rke2-ingress-nginx",
		"app.kubernetes.io/version": "1.12.4",
	}
	rke2Release := []inventory.HelmRelease{{Name: "rke2-ingress-nginx", Namespace: "kube-system", ChartName: "rke2-ingress-nginx", ChartVersion: "4.12.401", AppVersion: "1.12.4", Status: "deployed"}}
	pod := func(labels map[string]string, image string) addOnEvidence {
		var ev addOnEvidence
		ev.addPod("kube-system", labels, []string{image})
		return ev
	}
	withReleases := func(ev addOnEvidence, rels []inventory.HelmRelease) addOnEvidence {
		ev.releases = rels
		return ev
	}
	for _, tc := range []struct {
		name string
		ev   addOnEvidence
		want []inventory.AddOnInstance
	}{
		{"digest-only, RKE2 chart labels", pod(rke2Labels, digest),
			[]inventory.AddOnInstance{{ID: "rke2-ingress-nginx", Version: "1.12.4", Namespaces: []string{"kube-system"}, Source: "image"}}},
		{"digest-only, an rke2-ingress-nginx release in the namespace", withReleases(pod(nil, digest), rke2Release),
			[]inventory.AddOnInstance{{ID: "rke2-ingress-nginx", Version: "1.12.4", ChartVersion: "4.12.401", Namespaces: []string{"kube-system"}, Source: "chart"}}},
		{"digest-only, no labels or release: the path-only entry", pod(nil, digest),
			[]inventory.AddOnInstance{{ID: "ingress-nginx", Namespaces: []string{"kube-system"}, Source: "image"}}},
		{"digest-only, upstream's labels: the path-only entry", pod(nginxLabels, digest),
			[]inventory.AddOnInstance{{ID: "ingress-nginx", Namespaces: []string{"kube-system"}, Source: "image"}}},
		{"the release is in another namespace", withReleases(pod(nil, digest), []inventory.HelmRelease{{Name: "x", Namespace: "edge", ChartName: "rke2-ingress-nginx", AppVersion: "1.12.4", Status: "deployed"}}),
			[]inventory.AddOnInstance{
				{ID: "ingress-nginx", Namespaces: []string{"kube-system"}, Source: "image"},
				{ID: "rke2-ingress-nginx", Version: "1.12.4", Namespaces: []string{"edge"}, Source: "chart"},
			}},
		// A tag decides: RKE1's (or early RKE2's) "-rancherN" build is
		// ingress-nginx whatever the labels say.
		{"a -rancherN tag with RKE2 labels", pod(rke2Labels, "rancher/nginx-ingress-controller:nginx-0.30.0-rancher1"),
			[]inventory.AddOnInstance{{ID: "ingress-nginx", Version: "0.30.0", Namespaces: []string{"kube-system"}, Source: "image"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, _ := matchAddOns(tc.ev, addons)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("got %+v, want %+v", got, tc.want)
			}
		})
	}
}

// A Flux install from its own manifests (flux install, flux bootstrap or a
// release's install.yaml) runs the controllers only, labelled part-of flux
// with, on bootstrap layouts, the Flux version: it is Flux at the line its
// controllers ship in (#265), whatever the labels say.
func TestMatchAddOnsFluxFromControllers(t *testing.T) {
	addons, err := registry.Load()
	if err != nil {
		t.Fatal(err)
	}
	labels := map[string]string{"app.kubernetes.io/part-of": "flux", "app.kubernetes.io/version": "v2.7.0"}
	var ev addOnEvidence
	for _, img := range []string{"ghcr.io/fluxcd/source-controller:v1.7.0", "ghcr.io/fluxcd/kustomize-controller:v1.7.0", "ghcr.io/fluxcd/helm-controller:v1.4.0", "ghcr.io/fluxcd/notification-controller:v1.7.1"} {
		ev.addPod("flux-system", labels, []string{img})
	}
	got, unrec := matchAddOns(ev, addons)
	want := []inventory.AddOnInstance{{ID: "flux", Version: "2.7", Namespaces: []string{"flux-system"}, Source: "image"}}
	if !reflect.DeepEqual(got, want) || len(unrec) != 0 {
		t.Errorf("got %+v unrecognized %v, want %+v and none", got, unrec, want)
	}
	// A flux2 chart release in the namespace agrees with the controllers'
	// line, so it is one install at the release's appVersion.
	ev.releases = []inventory.HelmRelease{{Name: "flux", Namespace: "flux-system", ChartName: "flux2", ChartVersion: "2.16.0", AppVersion: "2.7.0", Status: "deployed"}}
	got, _ = matchAddOns(ev, addons)
	want = []inventory.AddOnInstance{{ID: "flux", Version: "2.7.0", ChartVersion: "2.16.0", Namespaces: []string{"flux-system"}, Source: "chart"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("with the chart release: got %+v, want %+v", got, want)
	}
}

// Helm charts against the embedded registry (#49): a vendor build's chart is
// its own entry, never upstream ingress-nginx's.
func TestMatchAddOnsRealWorldCharts(t *testing.T) {
	addons, err := registry.Load()
	if err != nil {
		t.Fatal(err)
	}
	for chart, wantID := range map[string]string{
		"ingress-nginx":        "ingress-nginx",
		"rke2-ingress-nginx":   "rke2-ingress-nginx",
		"kubernetes-dashboard": "kubernetes-dashboard",
		"promtail":             "promtail",
		"grafana-agent":        "grafana-agent",
		"karpenter":            "karpenter",
		"gatekeeper":           "gatekeeper",
		"fluent-bit":           "fluent-bit",
	} {
		rel := inventory.HelmRelease{Name: chart, Namespace: "ns", ChartName: chart, ChartVersion: "1.0.0", AppVersion: "1.2.3", Status: "deployed"}
		got, _ := matchAddOns(addOnEvidence{releases: []inventory.HelmRelease{rel}}, addons)
		if len(got) != 1 || got[0].ID != wantID || got[0].Source != "chart" || got[0].Version != "1.2.3" {
			t.Errorf("chart %s: got %+v, want %s from the chart at 1.2.3", chart, got, wantID)
		}
	}
}

// Mirror matching is by a path suffix of at least two segments (#49): a
// one-segment matcher is an exact repository, so an operator entry (or the
// embedded etcd) cannot claim the same-named repository of another product.
func TestImageMatchersNeedTwoSegmentsToSuffixMatch(t *testing.T) {
	for _, tc := range []struct {
		image, matcher string
		want           bool
	}{
		{"quay.io/cilium/operator:v1.16.1", "operator", false},
		{"registry.k8s.io/ingress-nginx/controller:v1.11.3", "controller", false},
		{"myregistry/mirror/ingress-nginx/controller:v1.11.3", "controller", false},
		{"myregistry.example.com/operator:v1", "operator", true},
		{"quay.io/cilium/operator:v1.16.1", "cilium/operator", true},
		{"myregistry/mirror/cilium/operator:v1.16.1", "cilium/operator", true},
		{"quay.io/xcilium/operator:v1.16.1", "cilium/operator", false},
		// "*/name" is the explicit opt-in to a suffix match on one segment.
		{"harbor.corp/k8s/etcd:3.5.15-0", "*/etcd", true},
		{"myregistry.example.com/etcd:3.5.15-0", "*/etcd", true},
		{"registry.k8s.io/etcd:3.5.15-0", "*/etcd", true},
		{"harbor.corp/k8s/my-etcd:3.5.15-0", "*/etcd", false},
		// A provider build is claimed by a matcher naming the provider only.
		{"mcr.microsoft.com/oss/etcd:3.5.15", "*/etcd", false},
		{"harbor.corp/mcr.microsoft.com/oss/etcd:3.5.15", "*/etcd", false},
	} {
		if got := imageMatches(parseImage(tc.image), tc.matcher); got != tc.want {
			t.Errorf("imageMatches(%s, %q) = %v, want %v", tc.image, tc.matcher, got, tc.want)
		}
	}
}

// The reported failure: an operator's entry with the bare matcher "operator"
// made quay.io/cilium/operator Cilium and the operator's product at once.
func TestExtraSingleSegmentMatcherLeavesEmbeddedImagesAlone(t *testing.T) {
	base, err := registry.Load()
	if err != nil {
		t.Fatal(err)
	}
	mine := registry.AddOn{ID: "my-operator", Matchers: registry.Matchers{Images: []string{"operator", "controller"}}}
	addons := registry.Merge(base, []registry.AddOn{mine})
	if errs := registry.ClaimConflicts(addons); len(errs) != 0 {
		t.Fatal(errs)
	}
	for _, image := range []string{"quay.io/cilium/operator:v1.16.1", "registry.k8s.io/ingress-nginx/controller:v1.11.3", "myregistry/mirror/ingress-nginx/controller:v1.11.3"} {
		if ids := imageAddOns(parseImage(image), addons); slices.Contains(ids, "my-operator") {
			t.Errorf("%s claimed by %v: the one-segment matcher reached another product", image, ids)
		}
	}
	if ids := imageAddOns(parseImage("myregistry.example.com/operator:v1"), addons); !slices.Equal(ids, []string{"my-operator"}) {
		t.Errorf("the exact repository operator: claimed by %v, want my-operator", ids)
	}
}

func TestMatchAddOnsUnrecognizedCap(t *testing.T) {
	var images []nsImage
	for i := 0; i < 250; i++ {
		images = append(images, nsImage{Namespace: "ns", Image: fmt.Sprintf("example.com/app-%03d:1.0", i)})
	}
	_, unrec := matchAddOns(addOnEvidence{images: images}, nil)
	var inv inventory.Inventory
	setUnrecognized(&inv, unrec)
	if len(inv.UnrecognizedImages) != 200 || inv.UnrecognizedImagesOmitted != 50 {
		t.Fatalf("unrecognized = %d, omitted %d; want capped at 200, 50 omitted", len(inv.UnrecognizedImages), inv.UnrecognizedImagesOmitted)
	}
	if inv.UnrecognizedImages[0] != "example.com/app-000" {
		t.Errorf("unrec[0] = %q, want sorted before capping", inv.UnrecognizedImages[0])
	}
}

func TestCollectAddOnsUsesPodImagesAndHelmReleases(t *testing.T) {
	cs := kubefake.NewClientset(&corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "controller-abc", Namespace: "ingress-nginx"},
		Spec: corev1.PodSpec{
			InitContainers: []corev1.Container{{Name: "init", Image: "docker.io/library/busybox:1.36"}},
			Containers:     []corev1.Container{{Name: "controller", Image: "registry.k8s.io/ingress-nginx/controller:v1.8.0"}},
		},
	})
	inv := inventory.Inventory{HelmReleases: []inventory.HelmRelease{
		{Name: "ingress-nginx", Namespace: "ingress-nginx", ChartName: "ingress-nginx", ChartVersion: "4.7.1", AppVersion: "1.8.1", Status: "deployed"},
	}}
	if err := collectAddOns(context.Background(), cs, testRegistry(), &inv); err != nil {
		t.Fatal(err)
	}
	want := []inventory.AddOnInstance{{ID: "ingress-nginx", Version: "1.8.1", ChartVersion: "4.7.1", Namespaces: []string{"ingress-nginx"}, Source: "chart"}}
	if !reflect.DeepEqual(inv.AddOns, want) {
		t.Errorf("addons = %#v\nwant   %#v", inv.AddOns, want)
	}
	if !reflect.DeepEqual(inv.UnrecognizedImages, []string{"docker.io/library/busybox"}) {
		t.Errorf("unrecognized = %#v, want busybox repo (init container counted)", inv.UnrecognizedImages)
	}
}

func TestCollectAddOnsFollowsListPagination(t *testing.T) {
	pod := func(name, image string) corev1.Pod {
		return corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "kube-system"},
			Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "c", Image: image}}},
		}
	}
	cs := kubefake.NewClientset()
	calls := 0
	cs.PrependReactor("list", "pods", func(a k8stesting.Action) (bool, runtime.Object, error) {
		calls++
		want := int64(listPageSize) // then sized by the first page's pods: small ones, so the most a page may hold
		if calls > 1 {
			want = podPageSize
		}
		if l := a.(interface{ GetListOptions() metav1.ListOptions }).GetListOptions().Limit; l != want {
			t.Errorf("pod list %d asked for pages of %d, want %d", calls, l, want)
		}
		switch calls {
		case 1:
			return true, &corev1.PodList{
				ListMeta: metav1.ListMeta{Continue: "page-2"},
				Items:    []corev1.Pod{pod("cilium-1", "quay.io/cilium/cilium:v1.14.0")},
			}, nil
		default:
			return true, &corev1.PodList{
				Items: []corev1.Pod{pod("nginx-1", "registry.k8s.io/ingress-nginx/controller:v1.9.4")},
			}, nil
		}
	})

	var inv inventory.Inventory
	if err := collectAddOns(context.Background(), cs, testRegistry(), &inv); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Errorf("list calls = %d, want 2 (Continue token must be followed)", calls)
	}
	want := []inventory.AddOnInstance{
		{ID: "cilium", Version: "1.14.0", Namespaces: []string{"kube-system"}, Source: "image"},
		{ID: "ingress-nginx", Version: "1.9.4", Namespaces: []string{"kube-system"}, Source: "image"},
	}
	if !reflect.DeepEqual(inv.AddOns, want) {
		t.Errorf("addons = %#v\nwant   %#v (images from every page must count)", inv.AddOns, want)
	}
}
