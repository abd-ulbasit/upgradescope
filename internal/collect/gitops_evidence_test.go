package collect

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/abd-ulbasit/upgradescope/internal/inventory"
)

// Pods, IngressClasses and Helm releases all unreadable, but a GitOps
// chart was read: that is evidence, so the add-ons capability is partial
// (and says what it was detected from), not unavailable, and the add-on
// the chart names is found.
func TestGitOpsChartKeepsAddOnsPartialWhenPodsAreForbidden(t *testing.T) {
	f := newGitOpsFixture(t, []*metav1.APIResourceList{argoServed()}, []runtime.Object{ingressNginxApp()})
	forbidList(f.kube, "pods")
	forbidList(f.kube, "ingressclasses")
	inv := Collect(context.Background(), f.clients(), loadKB(t), Options{})
	st := inv.Capabilities[inventory.CapAddOns]
	if !st.Available || !st.Partial || !reflect.DeepEqual(st.Skipped, []string{"networking.k8s.io/v1 ingressclasses", inventory.SkippedPods}) {
		t.Fatalf("addons capability = %+v, want available, partial, skipping pods and ingressclasses", st)
	}
	if !strings.Contains(st.Reason, "and GitOps chart sources only") {
		t.Errorf("reason %q does not name the GitOps chart sources as what was read", st.Reason)
	}
	var found []inventory.AddOnInstance
	for _, a := range inv.AddOns {
		if a.ID == "ingress-nginx" {
			found = append(found, a)
		}
	}
	if len(found) != 1 || found[0].Source != "gitops" || !reflect.DeepEqual(found[0].Namespaces, []string{"ingress-nginx"}) {
		t.Errorf("add-ons = %+v, want ingress-nginx from gitops in ingress-nginx", inv.AddOns)
	}
}

// With nothing at all read, the add-ons capability is still unavailable.
func TestGitOpsAbsentLeavesAddOnsUnavailableWhenNothingIsRead(t *testing.T) {
	f := newGitOpsFixture(t, nil, nil)
	forbidList(f.kube, "pods")
	forbidList(f.kube, "ingressclasses")
	inv := Collect(context.Background(), f.clients(), loadKB(t), Options{})
	if st := inv.Capabilities[inventory.CapAddOns]; st.Available {
		t.Errorf("addons capability = %+v, want unavailable", st)
	}
}

// A HelmRelease with a targetNamespace other than its own leaves its
// release Secret in the HelmRelease's namespace (the storage namespace),
// while the chart deploys, and its pods run, in the target. The two are
// reported as two installs, as for any chart with a namespace override;
// the GitOps chart version is not merged into the release's.
func TestGitOpsFluxTargetNamespaceApartFromStorageIsTwoInstalls(t *testing.T) {
	hr := fluxRelease("v2", "flux-system", "ingress-nginx", map[string]any{
		"targetNamespace": "ingress-nginx",
		"chart": map[string]any{"spec": map[string]any{"chart": "ingress-nginx", "version": "4.11.3",
			"sourceRef": map[string]any{"kind": "HelmRepository", "name": "ingress-nginx"}}},
	})
	release := helmSecret(t, helmRev{ns: "flux-system", release: "ingress-nginx-ingress-nginx", rev: 1, status: "deployed",
		chart: "ingress-nginx", chartVersion: "4.11.3", appVersion: "1.11.3"})
	f := newGitOpsFixture(t, []*metav1.APIResourceList{resources("helm.toolkit.fluxcd.io/v2", fluxHelmReleases)},
		[]runtime.Object{hr}, release)
	inv := Collect(context.Background(), f.clients(), loadKB(t), Options{})
	var got []inventory.AddOnInstance
	for _, a := range inv.AddOns {
		if a.ID == "ingress-nginx" {
			got = append(got, a)
		}
	}
	want := []inventory.AddOnInstance{
		{ID: "ingress-nginx", Version: "1.11.3", ChartVersion: "4.11.3", Namespaces: []string{"flux-system"}, Source: "chart"},
		{ID: "ingress-nginx", ChartVersion: "4.11.3", Namespaces: []string{"ingress-nginx"}, Source: "gitops"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("add-ons = %#v\nwant     %#v", got, want)
	}
}

// A Flux whose HelmReleases all deploy to other clusters deploys nothing
// here: no gap on a cluster with no Helm release, though the count of
// those left out is still in the reason.
func TestGitOpsFluxOnlyForeignHelmReleasesIsNoGap(t *testing.T) {
	foreign := fluxRelease("v2", "apps", "remote", map[string]any{
		"kubeConfig": map[string]any{"secretRef": map[string]any{"name": "spoke"}},
		"chart":      map[string]any{"spec": map[string]any{"chart": "cert-manager", "sourceRef": map[string]any{"kind": "HelmRepository", "name": "jetstack"}}},
	})
	f := newGitOpsFixture(t, []*metav1.APIResourceList{resources("helm.toolkit.fluxcd.io/v2", fluxHelmReleases)}, []runtime.Object{foreign})
	_, err := f.helmStep()
	pe := partial(t, err)
	if pe.incomplete || len(pe.skipped) != 0 {
		t.Errorf("partial = %+v, want no gap", pe)
	}
	if !strings.Contains(pe.Error(), "1 Flux chart source(s) deploy to other clusters") {
		t.Errorf("reason %q lacks the count of other clusters' sources", pe.Error())
	}
}

// Likewise HelmReleases whose target is not a namespace name: nothing
// here is deployed by them.
func TestGitOpsFluxOnlyInvalidHelmReleasesIsNoGap(t *testing.T) {
	bad := fluxRelease("v2", "flux-system", "bad", map[string]any{
		"targetNamespace": "Not_A_Namespace",
		"chart":           map[string]any{"spec": map[string]any{"chart": "x", "sourceRef": map[string]any{"kind": "HelmRepository", "name": "x"}}},
	})
	f := newGitOpsFixture(t, []*metav1.APIResourceList{resources("helm.toolkit.fluxcd.io/v2", fluxHelmReleases)}, []runtime.Object{bad})
	_, err := f.helmStep()
	if pe := partial(t, err); pe.incomplete || len(pe.skipped) != 0 {
		t.Errorf("partial = %+v, want no gap", pe)
	}
}

// A foreign HelmRelease beside a local one is still the gap: the local one
// has no release to assess.
func TestGitOpsFluxForeignBesideLocalHelmReleaseIsAGap(t *testing.T) {
	foreign := fluxRelease("v2", "apps", "remote", map[string]any{
		"kubeConfig": map[string]any{"secretRef": map[string]any{"name": "spoke"}},
		"chart":      map[string]any{"spec": map[string]any{"chart": "cert-manager", "sourceRef": map[string]any{"kind": "HelmRepository", "name": "jetstack"}}},
	})
	local := fluxRelease("v2", "flux-system", "podinfo", map[string]any{
		"chart": map[string]any{"spec": map[string]any{"chart": "podinfo", "sourceRef": map[string]any{"kind": "HelmRepository", "name": "podinfo"}}},
	})
	f := newGitOpsFixture(t, []*metav1.APIResourceList{resources("helm.toolkit.fluxcd.io/v2", fluxHelmReleases)}, []runtime.Object{foreign, local})
	_, err := f.helmStep()
	if pe := partial(t, err); !pe.incomplete || !reflect.DeepEqual(pe.skipped, []string{"flux"}) {
		t.Errorf("partial = %+v, want a gap skipping flux", pe)
	}
}

// With Secrets and ConfigMaps both unreadable the helm capability is
// unavailable, but the GitOps charts that were read still feed add-on
// matching, and its reason says so (rbac.gitops.* is on, or nothing would
// have been read).
func TestGitOpsHelmUnavailableReasonSaysTheChartsWereRead(t *testing.T) {
	f := newGitOpsFixture(t, []*metav1.APIResourceList{argoServed()}, []runtime.Object{ingressNginxApp()})
	forbidList(f.meta, "secrets")
	forbidList(f.meta, "configmaps")
	inv, err := f.helmStep()
	var pe partialError
	if err == nil || errors.As(err, &pe) {
		t.Fatalf("err = %v, want a full failure", err)
	}
	for _, want := range []string{"secrets not read", "1 GitOps chart source(s) were read and still feed add-on detection"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("reason %q lacks %q", err.Error(), want)
		}
	}
	if len(inv.GitOpsCharts) != 1 {
		t.Errorf("gitops charts = %#v, want the Application's chart", inv.GitOpsCharts)
	}
	// Nothing was read of GitOps: the reason is what it was.
	f = newGitOpsFixture(t, nil, nil)
	forbidList(f.meta, "secrets")
	forbidList(f.meta, "configmaps")
	if _, err := f.helmStep(); err == nil || strings.Contains(err.Error(), "GitOps") {
		t.Errorf("err = %v, want a failure that does not mention GitOps", err)
	}
}
