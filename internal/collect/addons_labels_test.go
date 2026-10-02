package collect

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	kubefake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/abd-ulbasit/upgradescope/internal/engine"
	"github.com/abd-ulbasit/upgradescope/internal/inventory"
	"github.com/abd-ulbasit/upgradescope/internal/kb"
	"github.com/abd-ulbasit/upgradescope/registry"
)

// nginxLabels are the labels the upstream ingress-nginx chart puts on its
// controller pods; Argo CD and kustomize renders keep them.
var nginxLabels = map[string]string{
	"helm.sh/chart":                "ingress-nginx-4.11.2",
	"app.kubernetes.io/name":       "ingress-nginx",
	"app.kubernetes.io/version":    "1.11.2",
	"app.kubernetes.io/part-of":    "ingress-nginx",
	"app.kubernetes.io/component":  "controller",
	"app.kubernetes.io/managed-by": "Helm",
}

// A mirror path no matcher knows: only labels or an IngressClass can
// tell it is ingress-nginx.
const unmatchedNginx = "corp.example/edge/nginx-controller:2024.06"

func labelledPodOf(ns string, labels map[string]string, images ...string) labelledPod {
	return labelledPod{Namespace: ns, Labels: appLabelsOf(labels), Images: images}
}

func TestMatchAddOnsFromLabelsAndIngressClass(t *testing.T) {
	addons := append(testRegistry(), registry.AddOn{ID: "rke2-ingress-nginx", Matchers: registry.Matchers{
		Images: []string{"rancher/nginx-ingress-controller"}, Charts: []string{"rke2-ingress-nginx"},
	}}, registry.AddOn{ID: "traefik", Matchers: registry.Matchers{
		Images: []string{"library/traefik"}, Charts: []string{"traefik"},
	}})
	cases := []struct {
		name string
		ev   addOnEvidence
		want []inventory.AddOnInstance
	}{
		{
			name: "chart and name labels: version from app.kubernetes.io/version, never the chart's",
			ev:   addOnEvidence{labelled: []labelledPod{labelledPodOf("edge", nginxLabels, unmatchedNginx)}},
			want: []inventory.AddOnInstance{{ID: "ingress-nginx", Version: "1.11.2", Namespaces: []string{"edge"}, Source: "labels"}},
		},
		{
			name: "app.kubernetes.io/name and version alone",
			ev: addOnEvidence{labelled: []labelledPod{labelledPodOf("edge", map[string]string{
				"app.kubernetes.io/name": "ingress-nginx", "app.kubernetes.io/version": "v1.10.1"}, unmatchedNginx)}},
			want: []inventory.AddOnInstance{{ID: "ingress-nginx", Version: "1.10.1", Namespaces: []string{"edge"}, Source: "labels"}},
		},
		{
			// helm.sh/chart carries the chart version, which registry
			// cycles never speak.
			name: "helm.sh/chart alone: version unknown",
			ev: addOnEvidence{labelled: []labelledPod{labelledPodOf("edge", map[string]string{
				"helm.sh/chart": "ingress-nginx-4.11.2"}, unmatchedNginx)}},
			want: []inventory.AddOnInstance{{ID: "ingress-nginx", Namespaces: []string{"edge"}, Source: "labels"}},
		},
		{
			// A component's version need not be the product's.
			name: "part-of alone names the add-on but not its version",
			ev: addOnEvidence{labelled: []labelledPod{labelledPodOf("mesh", map[string]string{
				"app.kubernetes.io/name": "mesh-gateway", "app.kubernetes.io/part-of": "istio", "app.kubernetes.io/version": "1.27.3"},
				"corp.example/mesh/gateway:1.27.3")}},
			want: []inventory.AddOnInstance{{ID: "istio", Namespaces: []string{"mesh"}, Source: "labels"}},
		},
		{
			name: "a chart matcher names the add-on through the name label (istiod → istio)",
			ev: addOnEvidence{labelled: []labelledPod{labelledPodOf("istio-system", map[string]string{
				"app.kubernetes.io/name": "istiod", "app.kubernetes.io/version": "1.27.3"}, "corp.example/istio/pilot-fips:1.27.3")}},
			want: []inventory.AddOnInstance{{ID: "istio", Version: "1.27.3", Namespaces: []string{"istio-system"}, Source: "labels"}},
		},
		{
			name: "the pod's own image matched the add-on: image evidence only",
			ev: addOnEvidence{
				images:   []nsImage{{"edge", "registry.k8s.io/ingress-nginx/controller:v1.9.4"}},
				labelled: []labelledPod{labelledPodOf("edge", nginxLabels, "registry.k8s.io/ingress-nginx/controller:v1.9.4")},
			},
			want: []inventory.AddOnInstance{{ID: "ingress-nginx", Version: "1.9.4", Namespaces: []string{"edge"}, Source: "image"}},
		},
		{
			// A vendor build whose labels name upstream: the image says
			// what runs, and no container is left for the labels to name.
			name: "every image matched another add-on: labels add nothing",
			ev: addOnEvidence{
				images:   []nsImage{{"kube-system", "rancher/nginx-ingress-controller:nginx-1.9.4-hardened1"}},
				labelled: []labelledPod{labelledPodOf("kube-system", nginxLabels, "rancher/nginx-ingress-controller:nginx-1.9.4-hardened1")},
			},
			want: []inventory.AddOnInstance{{ID: "rke2-ingress-nginx", Version: "1.9.4", Namespaces: []string{"kube-system"}, Source: "image"}},
		},
		{
			// A nameOverride or relabel can leave upstream's labels on a
			// vendor build; an unmatched sidecar is not the controller.
			name: "a vendor build with an unmatched sidecar: labels naming upstream add nothing",
			ev: addOnEvidence{
				images: []nsImage{{"kube-system", "rancher/nginx-ingress-controller:nginx-1.9.4-hardened1"}, {"kube-system", "corp.example/log-shipper:2.1"}},
				labelled: []labelledPod{labelledPodOf("kube-system", nginxLabels,
					"rancher/nginx-ingress-controller:nginx-1.9.4-hardened1", "corp.example/log-shipper:2.1")},
			},
			want: []inventory.AddOnInstance{{ID: "rke2-ingress-nginx", Version: "1.9.4", Namespaces: []string{"kube-system"}, Source: "image"}},
		},
		{
			name: "an injected sidecar does not hide the labelled controller",
			ev: addOnEvidence{
				images:   []nsImage{{"edge", unmatchedNginx}, {"edge", "istio/proxyv2:1.31.1"}},
				labelled: []labelledPod{labelledPodOf("edge", nginxLabels, unmatchedNginx, "istio/proxyv2:1.31.1")},
			},
			want: []inventory.AddOnInstance{
				{ID: "ingress-nginx", Version: "1.11.2", Namespaces: []string{"edge"}, Source: "labels"},
				{ID: "istio", Version: "1.31.1", Namespaces: []string{"edge"}, Source: "image"},
			},
		},
		{
			// #110: provider builds follow the provider's support policy.
			name: "a provider build is never claimed through its labels",
			ev: addOnEvidence{labelled: []labelledPod{labelledPodOf("kube-system", map[string]string{
				"app.kubernetes.io/name": "cilium", "app.kubernetes.io/version": "1.13.0"}, "mcr.microsoft.com/oss/cilium/cilium:1.13.0")}},
		},
		{
			name: "a Helm release's appVersion wins over labels on its release line",
			ev: addOnEvidence{
				labelled: []labelledPod{labelledPodOf("edge", nginxLabels, unmatchedNginx)},
				releases: []inventory.HelmRelease{{Name: "edge", Namespace: "edge", ChartName: "ingress-nginx", ChartVersion: "4.11.3", AppVersion: "1.11.3"}},
			},
			want: []inventory.AddOnInstance{{ID: "ingress-nginx", Version: "1.11.3", ChartVersion: "4.11.3", Namespaces: []string{"edge"}, Source: "chart"}},
		},
		{
			name: "labels on another release line than the Helm release are their own install (#165)",
			ev: addOnEvidence{
				labelled: []labelledPod{labelledPodOf("edge", nginxLabels, unmatchedNginx)},
				releases: []inventory.HelmRelease{{Name: "edge", Namespace: "edge", ChartName: "ingress-nginx", ChartVersion: "4.10.0", AppVersion: "1.10.0"}},
			},
			want: []inventory.AddOnInstance{
				{ID: "ingress-nginx", Version: "1.10.0", ChartVersion: "4.10.0", Namespaces: []string{"edge"}, Source: "chart"},
				{ID: "ingress-nginx", Version: "1.11.2", Namespaces: []string{"edge"}, Source: "labels"},
			},
		},
		{
			name: "labels that name no add-on",
			ev: addOnEvidence{labelled: []labelledPod{labelledPodOf("shop", map[string]string{
				"helm.sh/chart": "shop-1.2.3", "app.kubernetes.io/name": "shop", "app.kubernetes.io/version": "1.2.3"}, "corp.example/shop:1.2.3")}},
		},
		{
			name: "IngressClass alone: present, version unknown, cluster-scoped",
			ev:   addOnEvidence{ingressControllers: []string{"k8s.io/ingress-nginx"}},
			want: []inventory.AddOnInstance{{ID: "ingress-nginx", Source: "ingressclass"}},
		},
		{
			name: "IngressClass adds nothing when the controller was detected otherwise",
			ev: addOnEvidence{
				images:             []nsImage{{"edge", "registry.k8s.io/ingress-nginx/controller:v1.9.4"}},
				ingressControllers: []string{"k8s.io/ingress-nginx"},
			},
			want: []inventory.AddOnInstance{{ID: "ingress-nginx", Version: "1.9.4", Namespaces: []string{"edge"}, Source: "image"}},
		},
		{
			// RKE2's chart keeps upstream's controller name.
			name: "IngressClass adds nothing when a vendor build claiming its controller runs",
			ev: addOnEvidence{
				images:             []nsImage{{"kube-system", "rancher/nginx-ingress-controller:nginx-1.9.4-hardened1"}},
				ingressControllers: []string{"k8s.io/ingress-nginx"},
			},
			want: []inventory.AddOnInstance{{ID: "rke2-ingress-nginx", Version: "1.9.4", Namespaces: []string{"kube-system"}, Source: "image"}},
		},
		{
			// Traefik's Kubernetes Ingress NGINX provider (v3.6.2+) serves
			// IngressClasses of controller k8s.io/ingress-nginx by default:
			// the migration off the retired controller keeps the class.
			name: "IngressClass adds nothing when Traefik, which can serve it, runs",
			ev: addOnEvidence{
				images:             []nsImage{{"traefik", "traefik:v3.6.2"}},
				ingressControllers: []string{"k8s.io/ingress-nginx"},
			},
			want: []inventory.AddOnInstance{{ID: "traefik", Version: "3.6.2", Namespaces: []string{"traefik"}, Source: "image"}},
		},
		{
			name: "other ingress controllers name no add-on",
			ev:   addOnEvidence{ingressControllers: []string{"k8s.io/ingress-gce", "traefik.io/ingress-controller"}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, _ := matchAddOns(tc.ev, addons)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("addons = %#v\nwant   %#v", got, tc.want)
			}
		})
	}
}

func TestChartName(t *testing.T) {
	cases := map[string]string{
		"ingress-nginx-4.11.2":        "ingress-nginx",
		"cert-manager-v1.15.3":        "cert-manager",
		"rke2-ingress-nginx-4.10.401": "rke2-ingress-nginx",
		"ingress-nginx-4.12.0-beta.0": "ingress-nginx",
		"kube-state-metrics-5.25.1":   "kube-state-metrics",
		"argo-cd-7.6.8_build.1":       "argo-cd", // "+" becomes "_" in a label
		"no-version":                  "",
		"":                            "",
	}
	for label, want := range cases {
		if got := chartName(label); got != want {
			t.Errorf("chartName(%q) = %q, want %q", label, got, want)
		}
	}
}

// End to end against the embedded registry (#18): an ingress-nginx install
// no image matcher recognises is still blocked, from its labels alone or
// from its IngressClass alone, while a label-detected per-cycle product is
// judged by its labelled version and a provider build by nothing upstream.
func TestLabelAndIngressClassDetectionVerdicts(t *testing.T) {
	addons, err := registry.Load()
	if err != nil {
		t.Fatal(err)
	}
	k := kb.KB{AddOns: addons, Skew: kb.DefaultSkewPolicy(), MaxKnownK8s: inventory.Version{Major: 1, Minor: 99}}
	now := time.Date(2026, 10, 2, 0, 0, 0, 0, time.UTC)
	istio := func(version string) map[string]string {
		return map[string]string{"app.kubernetes.io/name": "istiod", "app.kubernetes.io/version": version}
	}
	cases := []struct {
		name        string
		ev          addOnEvidence
		wantKey     string // a finding key the report must contain; "" for none
		wantBlocker bool
	}{
		{"ingress-nginx from labels alone", addOnEvidence{labelled: []labelledPod{labelledPodOf("edge", nginxLabels, unmatchedNginx)}},
			"eol-addon/ingress-nginx", true},
		{"ingress-nginx from IngressClass alone", addOnEvidence{ingressControllers: []string{"k8s.io/ingress-nginx"}},
			"eol-addon/ingress-nginx", true},
		{"Traefik serving the retained ingress-nginx IngressClass: no ingress-nginx finding",
			addOnEvidence{images: []nsImage{{"traefik", "traefik:v3.7.1"}}, ingressControllers: []string{"k8s.io/ingress-nginx"}},
			"", false},
		{"Istio 1.27 from labels: its ended release line blocks",
			addOnEvidence{labelled: []labelledPod{labelledPodOf("istio-system", istio("1.27.3"), "corp.example/mesh/istiod-fips:1.27.3")}},
			"eol-addon/istio/1.27", true},
		{"Istio 1.31 from labels: supported", addOnEvidence{labelled: []labelledPod{labelledPodOf("istio-system", istio("1.31.1"), "corp.example/mesh/istiod-fips:1.31.1")}},
			"", false},
		{"Istio from part-of alone: version unknown, no data, never a blocker",
			addOnEvidence{labelled: []labelledPod{labelledPodOf("mesh", map[string]string{
				"app.kubernetes.io/part-of": "istio", "app.kubernetes.io/version": "1.20.0"}, "corp.example/mesh/gateway:1.20.0")}},
			"addon-no-data/istio", false},
		{"AKS Cilium labelled cilium: the provider build is not judged upstream",
			addOnEvidence{labelled: []labelledPod{labelledPodOf("kube-system", map[string]string{
				"app.kubernetes.io/name": "cilium-agent", "app.kubernetes.io/part-of": "cilium", "helm.sh/chart": "cilium-1.13.0",
				"app.kubernetes.io/version": "1.13.0"}, "mcr.microsoft.com/oss/cilium/cilium:1.13.0")}},
			"", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			detected, _ := matchAddOns(tc.ev, addons)
			rep := engine.Evaluate(inventory.Inventory{AddOns: detected}, k, inventory.Version{Major: 1, Minor: 35}, now)
			blocker := slices.ContainsFunc(rep.Findings, func(f engine.Finding) bool { return f.Severity == engine.SevBlocker })
			if blocker != tc.wantBlocker {
				t.Errorf("blocker = %v, want %v (detected %+v, findings %+v)", blocker, tc.wantBlocker, detected, rep.Findings)
			}
			if tc.wantKey != "" && !slices.ContainsFunc(rep.Findings, func(f engine.Finding) bool { return f.Key == tc.wantKey }) {
				t.Errorf("findings %+v missing %s", rep.Findings, tc.wantKey)
			}
			if tc.wantKey == "" && len(rep.Findings) > 0 {
				t.Errorf("want no findings, got %+v", rep.Findings)
			}
		})
	}
}

func TestCollectAddOnsReadsLabelsAndIngressClasses(t *testing.T) {
	cs := kubefake.NewClientset(
		&corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: "controller-abc", Namespace: "edge", Labels: nginxLabels},
			Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "controller", Image: unmatchedNginx}}},
		},
		&networkingv1.IngressClass{ObjectMeta: metav1.ObjectMeta{Name: "nginx"}, Spec: networkingv1.IngressClassSpec{Controller: "k8s.io/ingress-nginx"}},
	)
	var inv inventory.Inventory
	if err := collectAddOns(context.Background(), cs, testRegistry(), &inv); err != nil {
		t.Fatal(err)
	}
	want := []inventory.AddOnInstance{{ID: "ingress-nginx", Version: "1.11.2", Namespaces: []string{"edge"}, Source: "labels"}}
	if !reflect.DeepEqual(inv.AddOns, want) {
		t.Errorf("addons = %#v\nwant   %#v", inv.AddOns, want)
	}
	// The labels found the add-on; the image is still one no matcher knows.
	if !reflect.DeepEqual(inv.UnrecognizedImages, []string{"corp.example/edge/nginx-controller"}) {
		t.Errorf("unrecognized = %#v", inv.UnrecognizedImages)
	}

	cs = kubefake.NewClientset(&networkingv1.IngressClass{ObjectMeta: metav1.ObjectMeta{Name: "nginx"},
		Spec: networkingv1.IngressClassSpec{Controller: "k8s.io/ingress-nginx"}})
	inv = inventory.Inventory{}
	if err := collectAddOns(context.Background(), cs, testRegistry(), &inv); err != nil {
		t.Fatal(err)
	}
	if want := []inventory.AddOnInstance{{ID: "ingress-nginx", Source: "ingressclass"}}; !reflect.DeepEqual(inv.AddOns, want) {
		t.Errorf("IngressClass only: addons = %#v, want %#v", inv.AddOns, want)
	}
}

// IngressClasses are supplementary evidence: a role that cannot list them
// still assesses add-ons from pods, as a partial capability naming what it
// skipped; an apiserver without networking.k8s.io/v1 IngressClass (before
// 1.19) has none to read.
func TestCollectAddOnsIngressClassesUnreadable(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "c", Namespace: "edge"},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "c", Image: "registry.k8s.io/ingress-nginx/controller:v1.9.4"}}},
	}
	for name, tc := range map[string]struct {
		err     error
		partial bool
	}{
		"forbidden": {apierrors.NewForbidden(schema.GroupResource{Group: "networking.k8s.io", Resource: "ingressclasses"}, "", errors.New("RBAC")), true},
		"not found": {apierrors.NewNotFound(schema.GroupResource{Group: "networking.k8s.io", Resource: "ingressclasses"}, ""), false},
	} {
		t.Run(name, func(t *testing.T) {
			cs := kubefake.NewClientset(pod)
			cs.PrependReactor("list", "ingressclasses", func(k8stesting.Action) (bool, runtime.Object, error) {
				return true, nil, tc.err
			})
			inv := inventory.Inventory{Capabilities: map[inventory.Capability]inventory.CapabilityStatus{}}
			runSteps(context.Background(), &inv, []step{{cap: inventory.CapAddOns, run: func(ctx context.Context, inv *inventory.Inventory) error {
				return collectAddOns(ctx, cs, testRegistry(), inv)
			}}})
			st := inv.Capabilities[inventory.CapAddOns]
			if !st.Available || st.Partial != tc.partial {
				t.Errorf("addons capability = %+v, want available, partial %v", st, tc.partial)
			}
			if tc.partial && !reflect.DeepEqual(st.Skipped, []string{"networking.k8s.io/v1 ingressclasses"}) {
				t.Errorf("skipped = %v", st.Skipped)
			}
			if len(inv.AddOns) != 1 || inv.AddOns[0].ID != "ingress-nginx" {
				t.Errorf("addons = %+v, want the pod's ingress-nginx", inv.AddOns)
			}
		})
	}
}
