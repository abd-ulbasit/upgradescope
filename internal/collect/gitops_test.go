package collect

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	discoveryfake "k8s.io/client-go/discovery/fake"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	kubefake "k8s.io/client-go/kubernetes/fake"
	metadatafake "k8s.io/client-go/metadata/fake"
	clienttesting "k8s.io/client-go/testing"

	"github.com/abd-ulbasit/upgradescope/internal/engine"
	"github.com/abd-ulbasit/upgradescope/internal/inventory"
)

// #70: charts deployed by Argo CD and Flux leave no Helm release Secret
// the Helm collector can read (Argo CD renders with helm template), so
// their chart references are read from the tools' own resources, and a
// cluster that shows the tools but no release records is a gap, not clean.

var (
	argoApplications = metav1.APIResource{Name: "applications", Kind: "Application", Namespaced: true, Verbs: metav1.Verbs{"get", "list"}}
	fluxHelmReleases = metav1.APIResource{Name: "helmreleases", Kind: "HelmRelease", Namespaced: true, Verbs: metav1.Verbs{"get", "list"}}
	fluxOCIRepos     = metav1.APIResource{Name: "ocirepositories", Kind: "OCIRepository", Namespaced: true, Verbs: metav1.Verbs{"get", "list"}}

	argoGVR = schema.GroupVersionResource{Group: "argoproj.io", Version: "v1alpha1", Resource: "applications"}
)

func fluxGVR(version string) schema.GroupVersionResource {
	return schema.GroupVersionResource{Group: "helm.toolkit.fluxcd.io", Version: version, Resource: "helmreleases"}
}

func cr(apiVersion, kind, ns, name string, spec map[string]any) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": apiVersion, "kind": kind,
		"metadata": map[string]any{"namespace": ns, "name": name},
		"spec":     spec,
	}}
}

// inCluster is an Application destination on the scanned cluster.
func inCluster(ns string) map[string]any {
	return map[string]any{"server": "https://kubernetes.default.svc", "namespace": ns}
}

func argoApp(name string, spec map[string]any) *unstructured.Unstructured {
	return cr("argoproj.io/v1alpha1", "Application", "argocd", name, spec)
}

func fluxRelease(version, ns, name string, spec map[string]any) *unstructured.Unstructured {
	return cr("helm.toolkit.fluxcd.io/"+version, "HelmRelease", ns, name, spec)
}

// ingressNginxApp is an Argo CD Application of the retired ingress-nginx
// chart, deployed to the scanned cluster.
func ingressNginxApp() *unstructured.Unstructured {
	return argoApp("ingress-nginx", map[string]any{
		"destination": inCluster("ingress-nginx"),
		"source": map[string]any{
			"repoURL": "https://kubernetes.github.io/ingress-nginx", "chart": "ingress-nginx", "targetRevision": "4.11.3",
		},
	})
}

// gitopsFixture is a cluster with the given served API resources, custom
// resources (served by a dynamic fake) and plain objects.
type gitopsFixture struct {
	disc *discoveryfake.FakeDiscovery
	dyn  *dynamicfake.FakeDynamicClient
	kube *kubefake.Clientset
	meta *metadatafake.FakeMetadataClient
}

func newGitOpsFixture(t *testing.T, served []*metav1.APIResourceList, crs []runtime.Object, objs ...runtime.Object) gitopsFixture {
	t.Helper()
	kube, meta := helmClients(t, objs...)
	return gitopsFixture{
		disc: fakeDiscovery(served...),
		dyn:  dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), gitopsListKinds(), crs...),
		kube: kube,
		meta: meta,
	}
}

// gitopsListKinds are the list kinds of every resource the collector may
// ask a dynamic fake to list.
func gitopsListKinds() map[schema.GroupVersionResource]string {
	kinds := map[schema.GroupVersionResource]string{argoGVR: "ApplicationList"}
	for _, v := range []string{"v2", "v2beta2", "v2beta1"} {
		kinds[fluxGVR(v)] = "HelmReleaseList"
	}
	for _, v := range []string{"v1", "v1beta2"} {
		kinds[schema.GroupVersionResource{Group: "source.toolkit.fluxcd.io", Version: v, Resource: "ocirepositories"}] = "OCIRepositoryList"
	}
	return kinds
}

func (f gitopsFixture) clients() Clients {
	return Clients{Kube: f.kube, Metadata: f.meta, Discovery: f.disc, Dynamic: f.dyn}
}

func (f gitopsFixture) helmStep() (inventory.Inventory, error) {
	var inv inventory.Inventory
	err := collectHelmStep(context.Background(), f.clients(), nil, &inv)
	return inv, err
}

func forbidList(c interface {
	PrependReactor(verb, resource string, reaction clienttesting.ReactionFunc)
}, resource string) {
	c.PrependReactor("list", resource, func(clienttesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: resource}, "", errors.New("RBAC"))
	})
}

func argoServed() *metav1.APIResourceList { return resources("argoproj.io/v1alpha1", argoApplications) }

// partial unpacks the capability outcome of a step that kept its data.
func partial(t *testing.T, err error) partialError {
	t.Helper()
	var pe partialError
	if !errors.As(err, &pe) {
		t.Fatalf("err = %v, want a partialError (capability available)", err)
	}
	return pe
}

func TestGitOpsArgoApplicationSingleSource(t *testing.T) {
	f := newGitOpsFixture(t, []*metav1.APIResourceList{argoServed()}, []runtime.Object{ingressNginxApp()},
		helmSecret(t, helmRev{ns: "kube-system", release: "dns", rev: 1, status: "deployed", chart: "coredns", chartVersion: "1.0.0"}))
	inv, err := f.helmStep()
	pe := partial(t, err)
	want := []inventory.GitOpsChart{{
		Tool: inventory.GitOpsArgoCD, Name: "ingress-nginx", Namespace: "argocd", Target: "ingress-nginx",
		Chart: "ingress-nginx", Version: "4.11.3", Repo: "https://kubernetes.github.io/ingress-nginx",
	}}
	if !reflect.DeepEqual(inv.GitOpsCharts, want) {
		t.Errorf("gitops charts = %#v\nwant            %#v", inv.GitOpsCharts, want)
	}
	// Helm releases exist and the Application was read: nothing is missing.
	if pe.incomplete || len(pe.skipped) != 0 || !strings.Contains(pe.Error(), "1 via Argo CD") {
		t.Errorf("partial = %+v, want a complete note counting 1 chart via Argo CD", pe)
	}
}

func TestGitOpsArgoApplicationMultipleSources(t *testing.T) {
	app := argoApp("platform", map[string]any{
		"destination": inCluster("platform"),
		"sources": []any{
			map[string]any{"repoURL": "https://github.com/acme/config.git", "targetRevision": "main", "ref": "values"}, // no chart
			map[string]any{"repoURL": "https://kubernetes.github.io/ingress-nginx", "chart": "ingress-nginx", "targetRevision": "4.*"},
			map[string]any{"repoURL": "registry-1.docker.io/bitnamicharts", "chart": "redis", "targetRevision": "20.1.0"},
		},
	})
	f := newGitOpsFixture(t, []*metav1.APIResourceList{argoServed()}, []runtime.Object{app})
	inv, err := f.helmStep()
	partial(t, err)
	want := []inventory.GitOpsChart{
		{Tool: "argocd", Name: "platform", Namespace: "argocd", Target: "platform", Chart: "ingress-nginx", Version: "4.*", Repo: "https://kubernetes.github.io/ingress-nginx"},
		{Tool: "argocd", Name: "platform", Namespace: "argocd", Target: "platform", Chart: "redis", Version: "20.1.0", Repo: "registry-1.docker.io/bitnamicharts"},
	}
	if !reflect.DeepEqual(inv.GitOpsCharts, want) {
		t.Errorf("gitops charts = %#v\nwant            %#v", inv.GitOpsCharts, want)
	}
}

// An Application that deploys to another cluster (a hub managing spokes)
// says nothing about this one.
func TestGitOpsArgoSkipsOtherClusters(t *testing.T) {
	spoke := argoApp("spoke-ingress", map[string]any{
		"destination": map[string]any{"server": "https://spoke.example.com:6443", "namespace": "ingress-nginx"},
		"source":      map[string]any{"repoURL": "https://kubernetes.github.io/ingress-nginx", "chart": "ingress-nginx", "targetRevision": "4.11.3"},
	})
	named := argoApp("named-local", map[string]any{
		"destination": map[string]any{"name": "in-cluster", "namespace": "cert-manager"},
		"source":      map[string]any{"repoURL": "https://charts.jetstack.io", "chart": "cert-manager", "targetRevision": "v1.15.3"},
	})
	f := newGitOpsFixture(t, []*metav1.APIResourceList{argoServed()}, []runtime.Object{spoke, named},
		helmSecret(t, helmRev{ns: "a", release: "r", rev: 1, status: "deployed", chart: "x", chartVersion: "1.0.0"}))
	inv, err := f.helmStep()
	pe := partial(t, err)
	if len(inv.GitOpsCharts) != 1 || inv.GitOpsCharts[0].Chart != "cert-manager" || inv.GitOpsCharts[0].Target != "cert-manager" {
		t.Errorf("gitops charts = %#v, want only the in-cluster cert-manager", inv.GitOpsCharts)
	}
	if !strings.Contains(pe.Error(), "1 Argo CD chart source(s) deploy to other clusters") {
		t.Errorf("reason %q does not count the source for another cluster", pe.Error())
	}
}

func TestGitOpsFluxHelmRelease(t *testing.T) {
	hr := fluxRelease("v2", "flux-system", "ingress-nginx", map[string]any{
		"targetNamespace": "ingress-nginx",
		"chart": map[string]any{"spec": map[string]any{
			"chart": "ingress-nginx", "version": "4.x",
			"sourceRef": map[string]any{"kind": "HelmRepository", "name": "ingress-nginx", "namespace": "flux-system"},
		}},
	})
	elsewhere := fluxRelease("v2", "apps", "remote", map[string]any{ // another cluster
		"kubeConfig": map[string]any{"secretRef": map[string]any{"name": "spoke"}},
		"chart":      map[string]any{"spec": map[string]any{"chart": "cert-manager", "sourceRef": map[string]any{"kind": "HelmRepository", "name": "jetstack"}}},
	})
	f := newGitOpsFixture(t, []*metav1.APIResourceList{resources("helm.toolkit.fluxcd.io/v2", fluxHelmReleases)},
		[]runtime.Object{hr, elsewhere},
		helmSecret(t, helmRev{ns: "a", release: "r", rev: 1, status: "deployed", chart: "x", chartVersion: "1.0.0"}))
	inv, err := f.helmStep()
	partial(t, err)
	want := []inventory.GitOpsChart{{
		Tool: inventory.GitOpsFlux, Name: "ingress-nginx", Namespace: "flux-system", Target: "ingress-nginx",
		Chart: "ingress-nginx", Version: "4.x", Repo: "HelmRepository/flux-system/ingress-nginx",
	}}
	if !reflect.DeepEqual(inv.GitOpsCharts, want) {
		t.Errorf("gitops charts = %#v\nwant            %#v", inv.GitOpsCharts, want)
	}
}

// Flux before 2.3 serves only the beta APIs; the newest served version
// is the one read, and the v2 API absent from discovery is not asked for.
func TestGitOpsFluxReadsTheNewestServedAPIVersion(t *testing.T) {
	hr := fluxRelease("v2beta2", "apps", "web", map[string]any{
		"chart": map[string]any{"spec": map[string]any{"chart": "traefik", "version": "30.0.0",
			"sourceRef": map[string]any{"kind": "HelmRepository", "name": "traefik", "namespace": "apps"}}},
	})
	f := newGitOpsFixture(t, []*metav1.APIResourceList{
		resources("helm.toolkit.fluxcd.io/v2beta1", fluxHelmReleases),
		resources("helm.toolkit.fluxcd.io/v2beta2", fluxHelmReleases),
	}, []runtime.Object{hr})
	var asked []schema.GroupVersionResource
	f.dyn.PrependReactor("list", "helmreleases", func(a clienttesting.Action) (bool, runtime.Object, error) {
		asked = append(asked, a.GetResource())
		return false, nil, nil
	})
	inv, err := f.helmStep()
	partial(t, err)
	if len(inv.GitOpsCharts) != 1 || inv.GitOpsCharts[0].Chart != "traefik" || inv.GitOpsCharts[0].Target != "apps" {
		t.Errorf("gitops charts = %#v, want the traefik release, targeting its own namespace", inv.GitOpsCharts)
	}
	if !reflect.DeepEqual(asked, []schema.GroupVersionResource{fluxGVR("v2beta2")}) {
		t.Errorf("listed %v, want only helm.toolkit.fluxcd.io/v2beta2", asked)
	}
}

// A HelmRelease may name its chart through an OCIRepository (chartRef);
// the chart is the last segment of its URL and the version its tag.
func TestGitOpsFluxChartRefOCIRepository(t *testing.T) {
	hr := fluxRelease("v2", "flux-system", "ingress-nginx", map[string]any{
		"chartRef": map[string]any{"kind": "OCIRepository", "name": "ingress-nginx-chart"},
	})
	unreadable := fluxRelease("v2", "flux-system", "podinfo", map[string]any{
		"chartRef": map[string]any{"kind": "HelmChart", "name": "flux-system-podinfo"},
	})
	repo := cr("source.toolkit.fluxcd.io/v1", "OCIRepository", "flux-system", "ingress-nginx-chart", map[string]any{
		"url": "oci://ghcr.io/acme/charts/ingress-nginx/", "ref": map[string]any{"tag": "4.11.3"},
	})
	f := newGitOpsFixture(t, []*metav1.APIResourceList{
		resources("helm.toolkit.fluxcd.io/v2", fluxHelmReleases),
		resources("source.toolkit.fluxcd.io/v1", fluxOCIRepos),
	}, []runtime.Object{hr, unreadable, repo},
		helmSecret(t, helmRev{ns: "a", release: "r", rev: 1, status: "deployed", chart: "x", chartVersion: "1.0.0"}))
	inv, err := f.helmStep()
	pe := partial(t, err)
	want := []inventory.GitOpsChart{{
		Tool: "flux", Name: "ingress-nginx", Namespace: "flux-system", Target: "flux-system",
		Chart: "ingress-nginx", Version: "4.11.3", Repo: "oci://ghcr.io/acme/charts/ingress-nginx/",
	}}
	if !reflect.DeepEqual(inv.GitOpsCharts, want) {
		t.Errorf("gitops charts = %#v\nwant            %#v", inv.GitOpsCharts, want)
	}
	// The HelmChart reference could not be resolved: say so, as a gap.
	if !pe.incomplete || !reflect.DeepEqual(pe.skipped, []string{"flux"}) || !strings.Contains(pe.Error(), "1 HelmRelease chartRef(s) not resolved") {
		t.Errorf("partial = %+v, want incomplete, skipping flux, naming the unresolved chartRef", pe)
	}
}

// The gap (#70): no Helm release anywhere, but the tools' CRDs are served.
func TestGitOpsMarkersWithoutHelmReleasesAreAGap(t *testing.T) {
	for _, tc := range []struct {
		name    string
		served  *metav1.APIResourceList
		tool    string
		mention string
	}{
		{"argo cd", argoServed(), "argocd", "Argo CD"},
		{"flux", resources("helm.toolkit.fluxcd.io/v2", fluxHelmReleases), "flux", "Flux"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newGitOpsFixture(t, []*metav1.APIResourceList{tc.served}, nil)
			_, err := f.helmStep()
			pe := partial(t, err)
			if !pe.incomplete || !reflect.DeepEqual(pe.skipped, []string{tc.tool}) {
				t.Errorf("partial = %+v, want incomplete, skipping %s", pe, tc.tool)
			}
			for _, want := range []string{"helm releases: 0 via secrets, 0 via configmaps", "no Helm releases read, but " + tc.mention + " is present", "not assessed"} {
				if !strings.Contains(pe.Error(), want) {
					t.Errorf("reason %q lacks %q", pe.Error(), want)
				}
			}
		})
	}
}

// Argo CD's helm template leaves no release object: say why.
func TestGitOpsArgoGapExplainsHelmTemplate(t *testing.T) {
	f := newGitOpsFixture(t, []*metav1.APIResourceList{argoServed()}, []runtime.Object{ingressNginxApp()})
	inv, err := f.helmStep()
	pe := partial(t, err)
	if !strings.Contains(pe.Error(), "helm template") {
		t.Errorf("reason %q does not explain that Argo CD leaves no Helm release", pe.Error())
	}
	if len(inv.GitOpsCharts) != 1 {
		t.Errorf("gitops charts = %#v, want the Application's chart read despite the gap", inv.GitOpsCharts)
	}
}

// Tracking labels and annotations on workloads show a tool whose CRDs the
// scan cannot see (an Argo CD in another cluster managing this one).
func TestGitOpsWorkloadMarkersWithoutHelmReleasesAreAGap(t *testing.T) {
	deploy := func(name string, labels, annotations map[string]string) runtime.Object {
		return &metav1.PartialObjectMetadata{
			TypeMeta:   metav1.TypeMeta{APIVersion: "apps/v1", Kind: "Deployment"},
			ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: name, Labels: labels, Annotations: annotations},
		}
	}
	for _, tc := range []struct {
		name string
		obj  runtime.Object
		tool string // "" for no gap
	}{
		{"argo cd tracking annotation", deploy("web", nil, map[string]string{"argocd.argoproj.io/tracking-id": "shop:apps/Deployment:shop/web"}), "argocd"},
		{"argo cd instance label", deploy("web", map[string]string{"argocd.argoproj.io/instance": "shop"}, nil), "argocd"},
		{"flux helm-controller label", deploy("web", map[string]string{"helm.toolkit.fluxcd.io/name": "web"}, nil), "flux"},
		{"flux kustomize label is not a helm marker", deploy("web", map[string]string{"kustomize.toolkit.fluxcd.io/name": "apps"}, nil), ""},
		{"generic instance label is not a marker", deploy("web", map[string]string{"app.kubernetes.io/instance": "web"}, nil), ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newGitOpsFixture(t, nil, nil)
			f.meta = helmMetaWith(t, tc.obj)
			inv, err := f.helmStep()
			pe := partial(t, err)
			if len(inv.GitOpsCharts) != 0 {
				t.Errorf("gitops charts = %#v, want none: the CRDs are not served", inv.GitOpsCharts)
			}
			if tc.tool == "" {
				if pe.incomplete || len(pe.skipped) != 0 {
					t.Errorf("partial = %+v, want a complete helm capability", pe)
				}
				return
			}
			if !pe.incomplete || !reflect.DeepEqual(pe.skipped, []string{tc.tool}) || !strings.Contains(pe.Error(), "workloads carry") {
				t.Errorf("partial = %+v, want incomplete, skipping %s, naming the workload markers", pe, tc.tool)
			}
		})
	}
}

// helmMetaWith serves objs (metadata only) as a metadata client does.
func helmMetaWith(t *testing.T, objs ...runtime.Object) *metadatafake.FakeMetadataClient {
	t.Helper()
	return metaClient(objs)
}

// A cluster with Helm releases does not list its workloads for markers:
// the gap is only for clusters with no release records.
func TestGitOpsWorkloadsAreNotListedWhenHelmReleasesExist(t *testing.T) {
	f := newGitOpsFixture(t, nil, nil, helmSecret(t, helmRev{ns: "a", release: "r", rev: 1, status: "deployed", chart: "x", chartVersion: "1.0.0"}))
	f.meta.PrependReactor("list", "deployments", func(clienttesting.Action) (bool, runtime.Object, error) {
		t.Error("listed deployments although a Helm release was read")
		return false, nil, nil
	})
	_, err := f.helmStep()
	if pe := partial(t, err); pe.incomplete {
		t.Errorf("partial = %+v, want complete", pe)
	}
}

// A role that cannot list workloads cannot show their markers; that is not
// a gap of its own (a restricted user's role may lack list on workloads;
// the chart's grants it), and the CRD markers still work.
func TestGitOpsUnreadableWorkloadsAreNotAGap(t *testing.T) {
	f := newGitOpsFixture(t, nil, nil)
	for _, r := range []string{"deployments", "statefulsets", "daemonsets"} {
		forbidList(f.meta, r)
	}
	_, err := f.helmStep()
	if pe := partial(t, err); pe.incomplete || len(pe.skipped) != 0 {
		t.Errorf("partial = %+v, want complete", pe)
	}
}

// Neither tool: no GitOps note at all, the helm reason is what it was.
func TestGitOpsAbsentChangesNothing(t *testing.T) {
	f := newGitOpsFixture(t, nil, nil)
	inv, err := f.helmStep()
	pe := partial(t, err)
	if pe.incomplete || pe.Error() != "helm releases: 0 via secrets, 0 via configmaps" || inv.GitOpsCharts != nil {
		t.Errorf("partial = %+v, charts %v; want the plain helm note and no charts", pe, inv.GitOpsCharts)
	}
}

// Forbidden custom resources (the chart's rbac.gitops.* off) are a named
// gap, never a failure of the capability, whatever else was read.
func TestGitOpsForbiddenDegradesToPartial(t *testing.T) {
	for _, withReleases := range []bool{true, false} {
		f := newGitOpsFixture(t, []*metav1.APIResourceList{argoServed(), resources("helm.toolkit.fluxcd.io/v2", fluxHelmReleases)}, nil)
		if withReleases {
			f = newGitOpsFixture(t, []*metav1.APIResourceList{argoServed(), resources("helm.toolkit.fluxcd.io/v2", fluxHelmReleases)}, nil,
				helmSecret(t, helmRev{ns: "a", release: "r", rev: 1, status: "deployed", chart: "x", chartVersion: "1.0.0"}))
		}
		forbidList(f.dyn, "applications")
		forbidList(f.dyn, "helmreleases")
		inv, err := f.helmStep()
		pe := partial(t, err)
		if !pe.incomplete || !reflect.DeepEqual(pe.skipped, []string{"argocd", "flux"}) {
			t.Errorf("releases=%v: partial = %+v, want incomplete, skipping argocd and flux", withReleases, pe)
		}
		for _, want := range []string{"Argo CD chart sources not read: list applications: ", "Flux chart sources not read: list helmreleases: ", "forbidden"} {
			if !strings.Contains(pe.Error(), want) {
				t.Errorf("releases=%v: reason %q lacks %q", withReleases, pe.Error(), want)
			}
		}
		if inv.GitOpsCharts != nil {
			t.Errorf("gitops charts = %v, want none", inv.GitOpsCharts)
		}
	}
}

// One tool unreadable does not hide the other's charts.
func TestGitOpsForbiddenToolKeepsTheOthersCharts(t *testing.T) {
	hr := fluxRelease("v2", "flux-system", "ingress-nginx", map[string]any{
		"chart": map[string]any{"spec": map[string]any{"chart": "ingress-nginx", "version": "4.11.3",
			"sourceRef": map[string]any{"kind": "HelmRepository", "name": "ingress-nginx"}}},
	})
	f := newGitOpsFixture(t, []*metav1.APIResourceList{argoServed(), resources("helm.toolkit.fluxcd.io/v2", fluxHelmReleases)},
		[]runtime.Object{hr}, helmSecret(t, helmRev{ns: "a", release: "r", rev: 1, status: "deployed", chart: "x", chartVersion: "1.0.0"}))
	forbidList(f.dyn, "applications")
	inv, err := f.helmStep()
	pe := partial(t, err)
	if len(inv.GitOpsCharts) != 1 || inv.GitOpsCharts[0].Tool != "flux" {
		t.Errorf("gitops charts = %#v, want the Flux chart", inv.GitOpsCharts)
	}
	if !pe.incomplete || !reflect.DeepEqual(pe.skipped, []string{"argocd"}) {
		t.Errorf("partial = %+v, want incomplete, skipping only argocd", pe)
	}
}

// A CRD that discovery lists with no such resource, or a group at a
// version no better than not served, is "not served": no error, no read.
func TestGitOpsNotServedIsNotAFailure(t *testing.T) {
	f := newGitOpsFixture(t, []*metav1.APIResourceList{
		resources("argoproj.io/v1alpha1", metav1.APIResource{Name: "appprojects", Kind: "AppProject", Namespaced: true, Verbs: metav1.Verbs{"get", "list"}}),
		resources("helm.toolkit.fluxcd.io/v1", fluxHelmReleases), // a version this code does not know
	}, nil, helmSecret(t, helmRev{ns: "a", release: "r", rev: 1, status: "deployed", chart: "x", chartVersion: "1.0.0"}))
	f.dyn.PrependReactor("list", "*", func(a clienttesting.Action) (bool, runtime.Object, error) {
		t.Errorf("listed %v although neither resource is served", a.GetResource())
		return false, nil, nil
	})
	_, err := f.helmStep()
	if pe := partial(t, err); pe.incomplete || len(pe.skipped) != 0 {
		t.Errorf("partial = %+v, want complete", pe)
	}
}

// Custom resources are listed in pages, not whole.
func TestGitOpsListsInPages(t *testing.T) {
	calls := 0
	f := newGitOpsFixture(t, []*metav1.APIResourceList{argoServed()}, nil,
		helmSecret(t, helmRev{ns: "a", release: "r", rev: 1, status: "deployed", chart: "x", chartVersion: "1.0.0"}))
	f.dyn.PrependReactor("list", "applications", func(a clienttesting.Action) (bool, runtime.Object, error) {
		page := func(name, cont string) runtime.Object {
			l := &unstructured.UnstructuredList{Object: map[string]any{"apiVersion": "argoproj.io/v1alpha1", "kind": "ApplicationList"}}
			l.Items = []unstructured.Unstructured{*argoApp(name, map[string]any{
				"destination": inCluster("ns"),
				"source":      map[string]any{"repoURL": "https://charts.example.com", "chart": name, "targetRevision": "1.0.0"},
			})}
			l.SetContinue(cont)
			return l
		}
		if calls++; calls == 1 { // the fake does not pass Limit or Continue on
			return true, page("first", "next"), nil
		}
		return true, page("second", ""), nil
	})
	inv, err := f.helmStep()
	partial(t, err)
	if len(inv.GitOpsCharts) != 2 || inv.GitOpsCharts[0].Chart != "first" || inv.GitOpsCharts[1].Chart != "second" {
		t.Errorf("gitops charts = %#v, want both pages", inv.GitOpsCharts)
	}
	if calls != 2 {
		t.Errorf("listed %d times, want 2 pages", calls)
	}
}

// Chart references are free text of a custom resource anyone with write
// access to one namespace can create: one that cannot be a chart name is
// not recorded.
func TestGitOpsIgnoresImplausibleChartNames(t *testing.T) {
	app := argoApp("odd", map[string]any{
		"destination": inCluster("ns"),
		"sources": []any{
			map[string]any{"repoURL": "https://charts.example.com", "chart": strings.Repeat("c", 254), "targetRevision": "1"},
			map[string]any{"repoURL": "https://charts.example.com", "chart": int64(42), "targetRevision": "1"},
			map[string]any{"repoURL": "https://charts.example.com", "chart": "ok", "targetRevision": float64(1.1)},
		},
	})
	f := newGitOpsFixture(t, []*metav1.APIResourceList{argoServed()}, []runtime.Object{app})
	inv, err := f.helmStep()
	partial(t, err)
	if len(inv.GitOpsCharts) != 1 || inv.GitOpsCharts[0].Chart != "ok" || inv.GitOpsCharts[0].Version != "" {
		t.Errorf("gitops charts = %#v, want only chart ok, with no version", inv.GitOpsCharts)
	}
}

// The Helm capability stays unavailable when its own reads failed; the
// GitOps charts are still collected, for add-on matching.
func TestGitOpsKeepsHelmUnavailable(t *testing.T) {
	f := newGitOpsFixture(t, []*metav1.APIResourceList{argoServed()}, []runtime.Object{ingressNginxApp()})
	forbidList(f.meta, "secrets")
	forbidList(f.meta, "configmaps")
	inv, err := f.helmStep()
	var pe partialError
	if err == nil || errors.As(err, &pe) || !strings.Contains(err.Error(), "secrets not read") {
		t.Errorf("err = %v, want a full failure", err)
	}
	if len(inv.GitOpsCharts) != 1 {
		t.Errorf("gitops charts = %#v, want the Application's chart", inv.GitOpsCharts)
	}
}

// Without a dynamic client the custom resources are not read, but the
// markers still make the gap.
func TestGitOpsWithoutDynamicClientStillReportsTheGap(t *testing.T) {
	f := newGitOpsFixture(t, []*metav1.APIResourceList{argoServed()}, []runtime.Object{ingressNginxApp()})
	c := f.clients()
	c.Dynamic = nil
	var inv inventory.Inventory
	err := collectHelmStep(context.Background(), c, nil, &inv)
	pe := partial(t, err)
	if !pe.incomplete || !reflect.DeepEqual(pe.skipped, []string{"argocd"}) || inv.GitOpsCharts != nil {
		t.Errorf("partial = %+v, charts %v; want the gap and no charts", pe, inv.GitOpsCharts)
	}
}

// End to end: an EOL ingress-nginx deployed by Argo CD (and by Flux), with
// no Helm release and no ingress-nginx pod the scan can see, is found and
// is a blocker, and the report shows the Helm checks as not assessed.
func TestGitOpsDeployedIngressNginxIsFoundEndToEnd(t *testing.T) {
	flux := fluxRelease("v2", "flux-system", "ingress-nginx", map[string]any{
		"targetNamespace": "ingress-nginx",
		"chart": map[string]any{"spec": map[string]any{"chart": "ingress-nginx", "version": "4.11.3",
			"sourceRef": map[string]any{"kind": "HelmRepository", "name": "ingress-nginx"}}},
	})
	for _, tc := range []struct {
		name   string
		served *metav1.APIResourceList
		cr     *unstructured.Unstructured
		tool   string
	}{
		{"argo cd", argoServed(), ingressNginxApp(), "argocd"},
		{"flux", resources("helm.toolkit.fluxcd.io/v2", fluxHelmReleases), flux, "flux"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newGitOpsFixture(t, []*metav1.APIResourceList{tc.served}, []runtime.Object{tc.cr})
			k := loadKB(t)
			inv := Collect(context.Background(), f.clients(), k, Options{})
			if st := inv.Capabilities[inventory.CapHelm]; !st.Available || !st.Partial || !reflect.DeepEqual(st.Skipped, []string{tc.tool}) {
				t.Errorf("helm capability = %+v, want available, partial, skipping %s", st, tc.tool)
			}
			var found []inventory.AddOnInstance
			for _, a := range inv.AddOns {
				if a.ID == "ingress-nginx" {
					found = append(found, a)
				}
			}
			if len(found) != 1 || found[0].Source != "gitops" || !reflect.DeepEqual(found[0].Namespaces, []string{"ingress-nginx"}) || found[0].ChartVersion != "4.11.3" {
				t.Fatalf("add-ons = %+v, want ingress-nginx from gitops in ingress-nginx at chart 4.11.3", inv.AddOns)
			}
			rep := engine.Evaluate(inv, k, inventory.Version{Major: 1, Minor: 34}, time.Date(2026, 10, 3, 0, 0, 0, 0, time.UTC))
			var eol bool
			for _, fnd := range rep.Findings {
				eol = eol || fnd.Category == engine.CatEOLAddon && fnd.Severity == engine.SevBlocker && fnd.Key == "eol-addon/ingress-nginx"
			}
			if !eol {
				t.Errorf("findings = %+v, want an end-of-life ingress-nginx blocker", rep.Findings)
			}
			var gap bool
			for _, g := range rep.NotAssessed {
				gap = gap || g.Capability == inventory.CapHelm && g.Partial && strings.Contains(g.Reason, "not assessed")
			}
			if !gap {
				t.Errorf("not assessed = %+v, want the partial helm gap", rep.NotAssessed)
			}
		})
	}
}

// destination.namespace and targetNamespace are free text the apiserver
// does not validate as namespaces. A reference to one that is not a
// namespace name is dropped (and counted), so the inventory it would have
// poisoned still passes the server's identifier check and the cluster is
// still reported.
func TestGitOpsInvalidTargetNamespaceIsDroppedNotPushed(t *testing.T) {
	badArgo := argoApp("bad", map[string]any{
		"destination": inCluster("Not_A_Namespace"),
		"source":      map[string]any{"repoURL": "https://kubernetes.github.io/ingress-nginx", "chart": "ingress-nginx", "targetRevision": "4.11.3"},
	})
	badFlux := fluxRelease("v2", "flux-system", "bad", map[string]any{
		"targetNamespace": "Not_A_Namespace",
		"chart":           map[string]any{"spec": map[string]any{"chart": "ingress-nginx", "version": "4.11.3", "sourceRef": map[string]any{"kind": "HelmRepository", "name": "x"}}},
	})
	good := argoApp("good", map[string]any{
		"destination": inCluster("cert-manager"),
		"source":      map[string]any{"repoURL": "https://charts.jetstack.io", "chart": "cert-manager", "targetRevision": "1.15.3"},
	})
	f := newGitOpsFixture(t, []*metav1.APIResourceList{argoServed(), resources("helm.toolkit.fluxcd.io/v2", fluxHelmReleases)},
		[]runtime.Object{badArgo, badFlux, good})
	inv := Collect(context.Background(), f.clients(), loadKB(t), Options{})
	if err := inv.ValidateIdentifiers(); err != nil {
		t.Fatalf("inventory with a free-text target namespace does not validate: %v", err)
	}
	if len(inv.GitOpsCharts) != 1 || inv.GitOpsCharts[0].Chart != "cert-manager" {
		t.Errorf("gitops charts = %#v, want only cert-manager", inv.GitOpsCharts)
	}
	for _, a := range inv.AddOns {
		if a.ID == "ingress-nginx" {
			t.Errorf("ingress-nginx add-on %+v found through a reference to an impossible namespace", a)
		}
	}
	reason := inv.Capabilities[inventory.CapHelm].Reason
	for _, want := range []string{"1 Argo CD chart source(s)", "1 Flux chart source(s)", "not a namespace name"} {
		if !strings.Contains(reason, want) {
			t.Errorf("helm reason %q lacks %q", reason, want)
		}
	}
}

// A HelmRelease's constraint ("4.*") is not a chart version. The chart
// version of the Helm release Flux's helm-controller leaves wins; without
// a release, only an exact version is shown.
func TestGitOpsChartVersionIsEvidenceOnlyWhenExact(t *testing.T) {
	hr := func(version string) *unstructured.Unstructured {
		return fluxRelease("v2", "flux-system", "ingress-nginx", map[string]any{
			"targetNamespace": "ingress-nginx",
			"chart": map[string]any{"spec": map[string]any{"chart": "ingress-nginx", "version": version,
				"sourceRef": map[string]any{"kind": "HelmRepository", "name": "ingress-nginx"}}},
		})
	}
	release := helmSecret(t, helmRev{ns: "ingress-nginx", release: "ingress-nginx", rev: 1, status: "deployed",
		chart: "ingress-nginx", chartVersion: "4.11.3", appVersion: "1.11.3"})
	for _, tc := range []struct {
		name, constraint string
		withRelease      bool
		want             string
	}{
		{"release secret beats a wildcard constraint", "4.*", true, "4.11.3"},
		{"release secret beats a range", ">=4.0.0 <5.0.0", true, "4.11.3"},
		{"release secret beats an older exact version", "4.0.0", true, "4.11.3"},
		{"a wildcard alone is no version", "4.*", false, ""},
		{"a range alone is no version", ">=4.0.0 <5.0.0", false, ""},
		{"an exact version alone is shown", "v4.11.3", false, "4.11.3"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var objs []runtime.Object
			if tc.withRelease {
				objs = append(objs, release)
			}
			f := newGitOpsFixture(t, []*metav1.APIResourceList{resources("helm.toolkit.fluxcd.io/v2", fluxHelmReleases)},
				[]runtime.Object{hr(tc.constraint)}, objs...)
			inv := Collect(context.Background(), f.clients(), loadKB(t), Options{})
			var found []inventory.AddOnInstance
			for _, a := range inv.AddOns {
				if a.ID == "ingress-nginx" {
					found = append(found, a)
				}
			}
			if len(found) != 1 || found[0].ChartVersion != tc.want {
				t.Fatalf("add-ons = %+v, want one ingress-nginx at chart version %q", found, tc.want)
			}
			if tc.withRelease && found[0].Source != "chart" {
				t.Errorf("source = %q, want chart", found[0].Source)
			}
		})
	}
}

// Application.spec.sources, when set, replaces spec.source: Argo CD
// ignores a stale source beside it.
func TestGitOpsArgoSourcesReplaceSource(t *testing.T) {
	app := argoApp("both", map[string]any{
		"destination": inCluster("platform"),
		"source":      map[string]any{"repoURL": "https://kubernetes.github.io/ingress-nginx", "chart": "ingress-nginx", "targetRevision": "4.11.3"},
		"sources": []any{
			map[string]any{"repoURL": "https://charts.jetstack.io", "chart": "cert-manager", "targetRevision": "1.15.3"},
		},
	})
	f := newGitOpsFixture(t, []*metav1.APIResourceList{argoServed()}, []runtime.Object{app})
	inv, err := f.helmStep()
	partial(t, err)
	if len(inv.GitOpsCharts) != 1 || inv.GitOpsCharts[0].Chart != "cert-manager" {
		t.Errorf("gitops charts = %#v, want only the spec.sources chart", inv.GitOpsCharts)
	}
}

// A chart from a GitRepository or Bucket is a path in it, whose last
// element is the chart's directory; a HelmRepository's is a name.
func TestGitOpsFluxChartPathFromGitRepository(t *testing.T) {
	git := fluxRelease("v2", "flux-system", "ingress-nginx", map[string]any{
		"chart": map[string]any{"spec": map[string]any{"chart": "./charts/ingress-nginx",
			"sourceRef": map[string]any{"kind": "GitRepository", "name": "platform"}}},
	})
	f := newGitOpsFixture(t, []*metav1.APIResourceList{resources("helm.toolkit.fluxcd.io/v2", fluxHelmReleases)}, []runtime.Object{git})
	inv, err := f.helmStep()
	partial(t, err)
	if len(inv.GitOpsCharts) != 1 || inv.GitOpsCharts[0].Chart != "ingress-nginx" || inv.GitOpsCharts[0].Repo != "GitRepository/flux-system/platform" {
		t.Errorf("gitops charts = %#v, want ingress-nginx from the GitRepository", inv.GitOpsCharts)
	}
}

// The in-cluster API server has more than one spelling.
func TestArgoInClusterSpellings(t *testing.T) {
	for server, want := range map[string]bool{
		"https://kubernetes.default.svc":                    true,
		"https://kubernetes.default.svc/":                   true,
		"https://kubernetes.default.svc:443":                true,
		"https://kubernetes.default.svc.cluster.local":      true,
		"https://kubernetes.default.svc.cluster.local:443/": true,
		"https://kubernetes.default":                        false,
		"https://spoke.example.com:6443":                    false,
		"https://kubernetes.default.svc.evil.example":       false,
	} {
		if got := argoInCluster(map[string]any{"server": server}); got != want {
			t.Errorf("argoInCluster(%q) = %v, want %v", server, got, want)
		}
	}
}

// An OCIRepository that cannot be read says why, not only that a
// chartRef was left unresolved.
func TestGitOpsFluxOCIRepositoryErrorIsInTheReason(t *testing.T) {
	hr := fluxRelease("v2", "flux-system", "ingress-nginx", map[string]any{
		"chartRef": map[string]any{"kind": "OCIRepository", "name": "ingress-nginx-chart"},
	})
	f := newGitOpsFixture(t, []*metav1.APIResourceList{
		resources("helm.toolkit.fluxcd.io/v2", fluxHelmReleases),
		resources("source.toolkit.fluxcd.io/v1", fluxOCIRepos),
	}, []runtime.Object{hr}, helmSecret(t, helmRev{ns: "a", release: "r", rev: 1, status: "deployed", chart: "x", chartVersion: "1.0.0"}))
	f.dyn.PrependReactor("get", "ocirepositories", func(clienttesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: "ocirepositories"}, "ingress-nginx-chart", errors.New("RBAC"))
	})
	_, err := f.helmStep()
	pe := partial(t, err)
	if !strings.Contains(pe.Error(), "1 HelmRelease chartRef(s) not resolved") || !strings.Contains(pe.Error(), "forbidden") {
		t.Errorf("reason %q lacks the unresolved chartRef and the forbidden read", pe.Error())
	}
}

// When discovery itself fails nothing is known of the tools, and the
// reason says that, rather than claiming their sources were unread.
func TestGitOpsDiscoveryFailureIsWorded(t *testing.T) {
	f := newGitOpsFixture(t, nil, nil, helmSecret(t, helmRev{ns: "a", release: "r", rev: 1, status: "deployed", chart: "x", chartVersion: "1.0.0"}))
	f.disc.PrependReactor("get", "group", func(clienttesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("discovery down")
	})
	_, err := f.helmStep()
	pe := partial(t, err)
	for _, want := range []string{"discovery down", "not known whether Argo CD is installed", "not known whether Flux is installed"} {
		if !strings.Contains(pe.Error(), want) {
			t.Errorf("reason %q lacks %q", pe.Error(), want)
		}
	}
	if strings.Contains(pe.Error(), "chart sources not read") {
		t.Errorf("reason %q claims chart sources were unread though neither tool is known to exist", pe.Error())
	}
}
