package collect

import (
	"context"
	"fmt"
	"reflect"
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
			images:   []nsImage{{"ingress-nginx", "registry.k8s.io/ingress-nginx/controller:v1.9.4"}},
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
			name: "two releases: oldest app version and oldest chart version",
			releases: []inventory.HelmRelease{
				{Name: "a", Namespace: "a", ChartName: "ingress-nginx", ChartVersion: "4.10.0", AppVersion: "1.10.0"},
				{Name: "b", Namespace: "b", ChartName: "ingress-nginx", ChartVersion: "4.9.1", AppVersion: "1.9.6"},
			},
			want: []inventory.AddOnInstance{{ID: "ingress-nginx", Version: "1.9.6", ChartVersion: "4.9.1", Namespaces: []string{"a", "b"}, Source: "chart"}},
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
			name: "namespaces deduped and sorted",
			images: []nsImage{
				{"b-ns", "registry.k8s.io/ingress-nginx/controller:v1.9.4"},
				{"a-ns", "registry.k8s.io/ingress-nginx/controller:v1.9.4"},
				{"a-ns", "registry.k8s.io/ingress-nginx/controller:v1.9.4"},
			},
			want: []inventory.AddOnInstance{{ID: "ingress-nginx", Version: "1.9.4", Namespaces: []string{"a-ns", "b-ns"}, Source: "image"}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, unrec := matchAddOns(tc.images, tc.releases, testRegistry())
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
		{"123456789012.dkr.ecr.us-east-1.amazonaws.com/registry-k8s-io/ingress-nginx/controller:v1.11.2", "ingress-nginx", "1.11.2"},
		{"harbor.corp.example/k8s/ingress-nginx/controller:v1.11.2", "ingress-nginx", "1.11.2"},
		{"harbor.corp.example/registry.k8s.io/ingress-nginx/controller:v1.10.0", "ingress-nginx", "1.10.0"},
		{"registry.k8s.io/ingress-nginx/controller:v1.9.4@sha256:5b161f051d017e55d358435f295f5e9a297e66158f136321d9b04520ec6c48a3", "ingress-nginx", "1.9.4"},
		{"registry.k8s.io/ingress-nginx/controller@sha256:5b161f051d017e55d358435f295f5e9a297e66158f136321d9b04520ec6c48a3", "ingress-nginx", ""},
		{"docker.io/bitnami/nginx-ingress-controller:1.11.3-debian-12-r0", "ingress-nginx", "1.11.3"},
		{"bitnami/nginx-ingress-controller:1.11.3", "ingress-nginx", "1.11.3"},
		{"rancher/nginx-ingress-controller:nginx-1.9.4-hardened1", "rke2-ingress-nginx", "1.9.4"},
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
		{"registry.k8s.io/external-dns/external-dns:v0.14.2", "external-dns", "0.14.2"},
		{"bitnami/external-dns:0.14.2-debian-12-r4", "external-dns", "0.14.2"},
		{"registry.k8s.io/metrics-server/metrics-server:v0.7.2", "metrics-server", "0.7.2"},
		{"quay.io/prometheus-operator/prometheus-operator:v0.75.0", "prometheus-operator", "0.75.0"},
		// Not add-ons the registry tracks.
		{"nginx/nginx-ingress:3.6.0", "", ""}, // F5 NGINX Ingress Controller, a different product
		{"docker.io/library/redis:7", "", ""},
		{"ghcr.io/fluxcd/source-controller:v1.4.1", "", ""}, // controller versions are not Flux versions
	}
	for _, tc := range cases {
		got, unrec := matchAddOns([]nsImage{{"ns", tc.image}}, nil, addons)
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

func TestMatchAddOnsUnrecognizedCap(t *testing.T) {
	var images []nsImage
	for i := 0; i < 250; i++ {
		images = append(images, nsImage{Namespace: "ns", Image: fmt.Sprintf("example.com/app-%03d:1.0", i)})
	}
	_, unrec := matchAddOns(images, nil, nil)
	if len(unrec) != 200 {
		t.Fatalf("len(unrecognized) = %d, want capped at 200", len(unrec))
	}
	if unrec[0] != "example.com/app-000" {
		t.Errorf("unrec[0] = %q, want sorted before capping", unrec[0])
	}
}

func TestCollectAddOnsUsesPodImagesAndHelmReleases(t *testing.T) {
	cs := kubefake.NewClientset(&corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "controller-abc", Namespace: "ingress-nginx"},
		Spec: corev1.PodSpec{
			InitContainers: []corev1.Container{{Name: "init", Image: "docker.io/library/busybox:1.36"}},
			Containers:     []corev1.Container{{Name: "controller", Image: "registry.k8s.io/ingress-nginx/controller:v1.9.4"}},
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
	cs.PrependReactor("list", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
		calls++
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
