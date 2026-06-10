package collect

import (
	"context"
	"fmt"
	"reflect"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kubefake "k8s.io/client-go/kubernetes/fake"

	"github.com/abd-ulbasit/upgradescope/internal/inventory"
	"github.com/abd-ulbasit/upgradescope/registry"
)

func testRegistry() []registry.AddOn {
	return []registry.AddOn{
		{ID: "ingress-nginx", Matchers: registry.Matchers{
			Images: []string{"registry.k8s.io/ingress-nginx"},
			Charts: []string{"ingress-nginx"},
		}},
		{ID: "cilium", Matchers: registry.Matchers{
			Images: []string{"quay.io/cilium/cilium"},
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
			name:     "chart evidence wins over image for source and version",
			images:   []nsImage{{"ingress-nginx", "registry.k8s.io/ingress-nginx/controller:v1.9.4"}},
			releases: []inventory.HelmRelease{{Name: "ingress-nginx", Namespace: "ingress-nginx", ChartName: "ingress-nginx", ChartVersion: "4.7.1", Status: "deployed"}},
			want:     []inventory.AddOnInstance{{ID: "ingress-nginx", Version: "4.7.1", Namespaces: []string{"ingress-nginx"}, Source: "chart"}},
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
		{Name: "ingress-nginx", Namespace: "ingress-nginx", ChartName: "ingress-nginx", ChartVersion: "4.7.1", Status: "deployed"},
	}}
	if err := collectAddOns(context.Background(), cs, testRegistry(), &inv); err != nil {
		t.Fatal(err)
	}
	want := []inventory.AddOnInstance{{ID: "ingress-nginx", Version: "4.7.1", Namespaces: []string{"ingress-nginx"}, Source: "chart"}}
	if !reflect.DeepEqual(inv.AddOns, want) {
		t.Errorf("addons = %#v\nwant   %#v", inv.AddOns, want)
	}
	if !reflect.DeepEqual(inv.UnrecognizedImages, []string{"docker.io/library/busybox"}) {
		t.Errorf("unrecognized = %#v, want busybox repo (init container counted)", inv.UnrecognizedImages)
	}
}
