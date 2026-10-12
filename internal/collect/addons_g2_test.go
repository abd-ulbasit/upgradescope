package collect

import (
	"testing"

	"github.com/abd-ulbasit/upgradescope/internal/inventory"
	"github.com/abd-ulbasit/upgradescope/registry"
)

// The images and charts of the networking and mesh entries added with the
// Kubernetes compatibility ranges (Linkerd, AWS Load Balancer Controller),
// read through the real registry. The tags are the shapes the registries
// publish (ghcr.io/linkerd tag lists, public.ecr.aws/eks tag list, read on
// 2026-10-12): Linkerd's open-source images carry "edge-YY.M.N" tags (and
// "stable-2.N.P" up to 2.14), the controller "vMAJOR.MINOR.PATCH" with
// optional "-linux_amd64" style suffixes.
func TestMatchAddOnsMeshAndLoadBalancerImages(t *testing.T) {
	addons, err := registry.Load()
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		image, wantID, wantVersion string
	}{
		{"cr.l5d.io/linkerd/controller:stable-2.14.10", "linkerd", "2.14.10"},
		{"ghcr.io/linkerd/proxy:stable-2.14.9", "linkerd", "2.14.9"},
		{"ghcr.io/linkerd/policy-controller:stable-2.14.10", "linkerd", "2.14.10"},
		{"cr.l5d.io/linkerd/proxy:edge-26.10.1", "linkerd", "26.10.1"},
		{"harbor.corp.example/ghcr/linkerd/controller:edge-26.9.3", "linkerd", "26.9.3"},
		{"cr.l5d.io/linkerd/debug:edge-26.8.4", "linkerd", "26.8.4"},
		// proxy-init and cni-plugin version themselves (v2.4.10, v1.7.0).
		{"cr.l5d.io/linkerd/proxy-init:v2.4.10", "", ""},
		{"cr.l5d.io/linkerd/cni-plugin:v1.7.0", "", ""},

		{"public.ecr.aws/eks/aws-load-balancer-controller:v3.6.0", "aws-load-balancer-controller", "3.6.0"},
		{"public.ecr.aws/eks/aws-load-balancer-controller:v2.14.1-linux_amd64", "aws-load-balancer-controller", "2.14.1"},
		{"602401143452.dkr.ecr.us-west-2.amazonaws.com/amazon/aws-load-balancer-controller:v2.13.2", "aws-load-balancer-controller", "2.13.2"},
		{"harbor.corp.example/ecr-public/eks/aws-load-balancer-controller:v2.8.3", "aws-load-balancer-controller", "2.8.3"},
		{"docker.io/amazon/aws-alb-ingress-controller:v1.1.9", "", ""}, // the v1 ALB Ingress Controller, a different product
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

func TestMatchAddOnsMeshAndLoadBalancerCharts(t *testing.T) {
	addons, err := registry.Load()
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ chart, appVersion, wantID, wantVersion string }{
		{"linkerd-control-plane", "stable-2.14.9", "linkerd", "2.14.9"},
		{"linkerd-control-plane", "edge-26.9.3", "linkerd", "26.9.3"},
		{"linkerd2", "stable-2.9.5", "linkerd", "2.9.5"},
		{"aws-load-balancer-controller", "v2.13.2", "aws-load-balancer-controller", "2.13.2"},
	} {
		rel := inventory.HelmRelease{Name: tc.chart, Namespace: "ns", ChartName: tc.chart, ChartVersion: "1.0.0", AppVersion: tc.appVersion, Status: "deployed"}
		got, _ := matchAddOns(addOnEvidence{releases: []inventory.HelmRelease{rel}}, addons)
		if len(got) != 1 || got[0].ID != tc.wantID || got[0].Source != "chart" || got[0].Version != tc.wantVersion {
			t.Errorf("chart %s %s: got %+v, want %s %s from the chart", tc.chart, tc.appVersion, got, tc.wantID, tc.wantVersion)
		}
	}
}
