package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"slices"
	"strings"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/validate/content"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	discoveryfake "k8s.io/client-go/discovery/fake"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	kubefake "k8s.io/client-go/kubernetes/fake"
	metadatafake "k8s.io/client-go/metadata/fake"
	ktesting "k8s.io/client-go/testing"

	"github.com/abd-ulbasit/upgradescope/internal/collect"
	"github.com/abd-ulbasit/upgradescope/internal/inventory"
	"github.com/abd-ulbasit/upgradescope/internal/kb"
)

func gitopsListKinds() map[schema.GroupVersionResource]string {
	return map[schema.GroupVersionResource]string{
		argoApplicationGVR: "ApplicationList",
		fluxHelmReleaseGVR: "HelmReleaseList",
		fluxOCIRepoGVR:     "OCIRepositoryList",
	}
}

func newDyn(objs ...runtime.Object) *dynamicfake.FakeDynamicClient {
	return dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), gitopsListKinds(), objs...)
}

func served(gv, resource, kind string) *metav1.APIResourceList {
	return &metav1.APIResourceList{GroupVersion: gv, APIResources: []metav1.APIResource{
		{Name: resource, Kind: kind, Namespaced: true, Verbs: metav1.Verbs{"get", "list"}},
	}}
}

func nestedMap(t *testing.T, o map[string]any, path ...string) map[string]any {
	t.Helper()
	v, found, err := unstructured.NestedMap(o, path...)
	if err != nil || !found {
		t.Fatalf("%s is not an object (found %v, err %v) in %v", strings.Join(path, "."), found, err, o)
	}
	return v
}

func TestArgoApplicationsAreSingleAndMultiSource(t *testing.T) {
	cfg := config{Namespaces: 7, ArgoApps: 20, Seed: 1}
	single, multi := 0, 0
	for i := range cfg.ArgoApps {
		app := argoApplicationObject(i, cfg)
		if app.GetAPIVersion() != "argoproj.io/v1alpha1" || app.GetKind() != "Application" || app.GetName() == "" {
			t.Fatalf("app %d: %s %s %q", i, app.GetAPIVersion(), app.GetKind(), app.GetName())
		}
		if app.GetNamespace() != nsName(i%7) || app.GetLabels()[benchLabel] != "true" {
			t.Errorf("app %d: namespace %q labels %v, want a bench namespace and the bench label", i, app.GetNamespace(), app.GetLabels())
		}
		spec := nestedMap(t, app.Object, "spec")
		dest := nestedMap(t, spec, "destination")
		if dest["server"] != "https://kubernetes.default.svc" || len(content.IsDNS1123Label(dest["namespace"].(string))) != 0 {
			t.Errorf("app %d: destination %v, want this cluster and a namespace name (the collector skips anything else)", i, dest)
		}
		if _, ok := spec["sources"]; ok {
			multi++
			if _, both := spec["source"]; both {
				t.Errorf("app %d has source and sources: Argo CD ignores source beside sources", i)
			}
			srcs := spec["sources"].([]any)
			if len(srcs) != 2 {
				t.Fatalf("app %d: %d sources, want a chart and a values repository", i, len(srcs))
			}
			chart := srcs[0].(map[string]any)
			values := srcs[1].(map[string]any)
			if chart["chart"] == "" || chart["chart"] == nil || values["chart"] != nil || values["ref"] != "values" {
				t.Errorf("app %d: sources %v, want the first a chart and the second a chartless ref", i, srcs)
			}
		} else {
			single++
			src := nestedMap(t, spec, "source")
			if src["chart"] == nil || src["repoURL"] == nil || src["targetRevision"] == nil {
				t.Errorf("app %d: source %v, want chart, repoURL and targetRevision", i, src)
			}
		}
		if _, found, _ := unstructured.NestedSlice(app.Object, "status", "resources"); !found {
			t.Errorf("app %d has no status.resources: its list cost would be understated", i)
		}
	}
	if single != 10 || multi != 10 {
		t.Errorf("%d single-source and %d multi-source Applications of 20, want 10 each", single, multi)
	}
}

func TestFluxHelmReleasesUseChartSourcesAndChartRefs(t *testing.T) {
	cfg := config{Namespaces: 7, FluxHelmReleases: 20, Seed: 1}
	sources, refs := 0, 0
	names := map[string]bool{}
	for i := range cfg.FluxHelmReleases {
		o := fluxHelmReleaseObjects(i, cfg)
		hr := o.release
		if hr.GetAPIVersion() != "helm.toolkit.fluxcd.io/v2" || hr.GetKind() != "HelmRelease" || hr.GetNamespace() != nsName(i%7) {
			t.Fatalf("hr %d: %s %s in %q", i, hr.GetAPIVersion(), hr.GetKind(), hr.GetNamespace())
		}
		spec := nestedMap(t, hr.Object, "spec")
		if spec["kubeConfig"] != nil {
			t.Errorf("hr %d has a kubeConfig: the collector skips it as another cluster's", i)
		}
		if spec["interval"] == nil || o.status == nil {
			t.Errorf("hr %d: interval %v status %v", i, spec["interval"], o.status)
		}
		if ref, ok := spec["chartRef"].(map[string]any); ok {
			refs++
			if spec["chart"] != nil {
				t.Errorf("hr %d has chart and chartRef", i)
			}
			if o.oci == nil || ref["kind"] != "OCIRepository" || ref["name"] != o.oci.GetName() {
				t.Fatalf("hr %d: chartRef %v with OCIRepository %v, want one of that name", i, ref, o.oci)
			}
			if o.oci.GetNamespace() != hr.GetNamespace() || o.oci.GetAPIVersion() != "source.toolkit.fluxcd.io/v1" {
				t.Errorf("hr %d: OCIRepository %s in %q: the chartRef names none, so it must share the HelmRelease's namespace", i, o.oci.GetAPIVersion(), o.oci.GetNamespace())
			}
			url, _, _ := unstructured.NestedString(o.oci.Object, "spec", "url")
			tag, _, _ := unstructured.NestedString(o.oci.Object, "spec", "ref", "tag")
			if !strings.HasPrefix(url, "oci://") || tag == "" {
				t.Errorf("hr %d: OCIRepository url %q tag %q, want an oci:// URL and a tag", i, url, tag)
			}
			if names[o.oci.GetNamespace()+"/"+o.oci.GetName()] {
				t.Errorf("OCIRepository %s/%s is shared: each chartRef must have its own (as many to read as chartRefs)", o.oci.GetNamespace(), o.oci.GetName())
			}
			names[o.oci.GetNamespace()+"/"+o.oci.GetName()] = true
		} else {
			sources++
			cs := nestedMap(t, spec, "chart", "spec")
			sr := nestedMap(t, cs, "sourceRef")
			if cs["chart"] == nil || cs["version"] == nil || sr["kind"] != "HelmRepository" || o.oci != nil {
				t.Errorf("hr %d: chart spec %v, want a chart, version and HelmRepository sourceRef and no OCIRepository", i, cs)
			}
		}
	}
	if sources != 10 || refs != 10 {
		t.Errorf("%d chart-source and %d chartRef HelmReleases of 20, want 10 each", sources, refs)
	}
}

func TestGitOpsCounts(t *testing.T) {
	got := countGitOps(config{ArgoApps: 5, FluxHelmReleases: 7})
	want := gitopsCounts{ArgoApplications: 5, ArgoMultiSource: 2, FluxHelmReleases: 7, FluxChartRefs: 3, OCIRepositories: 3, ExpectedCharts: 12}
	if got != want {
		t.Errorf("countGitOps = %+v, want %+v", got, want)
	}
	if got := countGitOps(config{}); got != (gitopsCounts{}) {
		t.Errorf("countGitOps of the default config = %+v, want nothing", got)
	}
}

// The fill is opt-in: a config that names none makes no request at all, so
// the existing fill levels (and what they measured) are unchanged.
func TestGitOpsFillIsOffByDefault(t *testing.T) {
	dyn := newDyn()
	dyn.PrependReactor("*", "*", func(a ktesting.Action) (bool, runtime.Object, error) {
		t.Errorf("unexpected %s of %s with the GitOps fill off", a.GetVerb(), a.GetResource().Resource)
		return true, nil, errors.New("unexpected")
	})
	sum, err := seedGitOps(context.Background(), dyn, config{Namespaces: 3, Nodes: 5, Pods: 5, HelmReleases: 5, Seed: 1}, 4, io.Discard)
	if err != nil || sum != (gitopsSummary{}) {
		t.Errorf("seedGitOps = %+v, %v; want an empty summary and no error", sum, err)
	}
}

func TestSeedGitOpsCreatesEveryObjectAndIsRerunnable(t *testing.T) {
	cfg := config{Namespaces: 3, ArgoApps: 6, FluxHelmReleases: 7, Seed: 1}
	dyn := newDyn()
	var log bytes.Buffer
	var sum gitopsSummary
	for pass := range 2 { // the second pass meets what the first made
		var err error
		if sum, err = seedGitOps(context.Background(), dyn, cfg, 4, &log); err != nil {
			t.Fatalf("pass %d: %v", pass, err)
		}
	}
	count := func(gvr schema.GroupVersionResource) int {
		l, err := dyn.Resource(gvr).Namespace(metav1.NamespaceAll).List(context.Background(), metav1.ListOptions{})
		if err != nil {
			t.Fatal(err)
		}
		return len(l.Items)
	}
	if n := count(argoApplicationGVR); n != 6 {
		t.Errorf("%d Applications, want 6", n)
	}
	if n := count(fluxHelmReleaseGVR); n != 7 {
		t.Errorf("%d HelmReleases, want 7", n)
	}
	if n := count(fluxOCIRepoGVR); n != 3 { // HelmReleases 1, 3 and 5 use a chartRef
		t.Errorf("%d OCIRepositories, want 3", n)
	}
	if sum.ArgoApplications != 6 || sum.ArgoMultiSource != 3 || sum.FluxHelmReleases != 7 || sum.FluxChartRefs != 3 || sum.OCIRepositories != 3 || sum.ExpectedCharts != 13 {
		t.Errorf("summary %+v", sum)
	}
	if sum.ArgoApplicationAvgBytes < 1000 || sum.FluxHelmReleaseAvgBytes < 1000 || sum.OCIRepositoryAvgBytes < 500 {
		t.Errorf("average object sizes %d, %d, %d bytes: want the status counted in", sum.ArgoApplicationAvgBytes, sum.FluxHelmReleaseAvgBytes, sum.OCIRepositoryAvgBytes)
	}
	// The status is a subresource: it is set in a second request.
	hr, err := dyn.Resource(fluxHelmReleaseGVR).Namespace(nsName(0)).Get(context.Background(), fluxReleaseName(0), metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, found, _ := unstructured.NestedSlice(hr.Object, "status", "history"); !found {
		t.Errorf("HelmRelease has no status.history after seeding: %v", hr.Object["status"])
	}
}

func TestSeedGitOpsStopsAtTheFirstFailure(t *testing.T) {
	dyn := newDyn()
	boom := errors.New("etcd is full")
	dyn.PrependReactor("create", "applications", func(ktesting.Action) (bool, runtime.Object, error) { return true, nil, boom })
	_, err := seedGitOps(context.Background(), dyn, config{Namespaces: 1, ArgoApps: 10, FluxHelmReleases: 10}, 4, io.Discard)
	if !errors.Is(err, boom) || !strings.Contains(err.Error(), "argo applications") {
		t.Fatalf("err = %v, want the Application create failure named", err)
	}
	l, _ := dyn.Resource(fluxHelmReleaseGVR).Namespace("").List(context.Background(), metav1.ListOptions{})
	if len(l.Items) != 0 {
		t.Errorf("%d HelmReleases created after the Applications failed, want none", len(l.Items))
	}
}

// What the harness must be able to claim: the objects are the ones the
// agent's GitOps collector reads. This seeds a fake apiserver and runs the
// real collection against it with the Argo CD and Flux resources served, as
// the lab's CRDs make them.
func TestTheCollectorReadsEveryGitOpsObject(t *testing.T) {
	cfg := config{Namespaces: 5, ArgoApps: 30, FluxHelmReleases: 30, Seed: 1}
	dyn := newDyn()
	if _, err := seedGitOps(context.Background(), dyn, cfg, 4, io.Discard); err != nil {
		t.Fatal(err)
	}
	// One Helm release, so that "no releases" does not add a Flux gap of its
	// own (the lab always has them at a fill above zero).
	rel, err := helmReleaseSecrets(0, config{Namespaces: 5, HelmRevisions: 1, Seed: 1})
	if err != nil {
		t.Fatal(err)
	}
	secret := rel.secrets[0]
	kube := kubefake.NewClientset(secret)
	kube.Discovery().(*discoveryfake.FakeDiscovery).Resources = []*metav1.APIResourceList{
		served("argoproj.io/v1alpha1", "applications", "Application"),
		served("helm.toolkit.fluxcd.io/v2", "helmreleases", "HelmRelease"),
		served("source.toolkit.fluxcd.io/v1", "ocirepositories", "OCIRepository"),
	}
	k, err := kb.Load()
	if err != nil {
		t.Fatal(err)
	}
	scheme := runtime.NewScheme()
	if err := metav1.AddMetaToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	meta := &metav1.PartialObjectMetadata{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Secret"}, ObjectMeta: secret.ObjectMeta}
	inv := collect.Collect(context.Background(), collect.Clients{
		Kube: kube, Metadata: metadatafake.NewSimpleMetadataClient(scheme, meta), Discovery: kube.Discovery(), Dynamic: dyn,
	}, k, collect.Options{})
	counts := countGitOps(cfg)
	if len(inv.GitOpsCharts) != counts.ExpectedCharts {
		t.Fatalf("the collector read %d GitOps charts, want %d (every Application and HelmRelease)", len(inv.GitOpsCharts), counts.ExpectedCharts)
	}
	byTool := map[string]int{}
	named := 0
	for _, c := range inv.GitOpsCharts {
		byTool[c.Tool]++
		if c.Chart == "" || c.Version == "" || c.Repo == "" || c.Target == "" {
			t.Errorf("chart %+v is missing a field the matcher uses", c)
		}
		if c.Chart == "ingress-nginx" {
			named++
		}
	}
	if byTool[inventory.GitOpsArgoCD] != 30 || byTool[inventory.GitOpsFlux] != 30 {
		t.Errorf("charts by tool = %v, want 30 each", byTool)
	}
	if named == 0 {
		t.Error("no ingress-nginx chart: the add-on charts of the fill are not reaching the inventory")
	}
	helm := inv.Capabilities[inventory.CapHelm]
	if strings.Contains(helm.Reason, "not resolved") || strings.Contains(helm.Reason, "forbidden") {
		t.Errorf("helm capability reason %q: a chartRef did not resolve, or a list failed", helm.Reason)
	}
	// Argo CD renders with helm template, so its charts are the one gap the
	// collector names; the harness (agent.sh, the agent benchmark) expects it.
	if !slices.Equal(helm.Skipped, []string{inventory.GitOpsArgoCD}) {
		t.Errorf("helm skipped = %v, want only argocd", helm.Skipped)
	}
}

// A loaded lab's etcd answers "request timed out" to a create now and then
// (seen at 2,000 fake nodes beside KWOK's heartbeats, #233): the GitOps fill
// retries those, and gives up on anything that is not transient.
func TestSeedGitOpsRetriesTransientFailures(t *testing.T) {
	old := retryDelay
	retryDelay = 0
	t.Cleanup(func() { retryDelay = old })
	cfg := config{Namespaces: 2, ArgoApps: 3, FluxHelmReleases: 2, Seed: 1}
	dyn := newDyn()
	fails := map[string]int{}
	timeout := errors.New("etcdserver: request timed out")
	dyn.PrependReactor("*", "*", func(a ktesting.Action) (bool, runtime.Object, error) {
		if a.GetVerb() != "create" && a.GetVerb() != "update" {
			return false, nil, nil
		}
		key := a.GetVerb() + "/" + a.GetResource().Resource + "/" + a.GetSubresource()
		if fails[key] < 2 { // each kind of call fails twice, then works
			fails[key]++
			return true, nil, timeout
		}
		return false, nil, nil
	})
	if _, err := seedGitOps(context.Background(), dyn, cfg, 1, io.Discard); err != nil {
		t.Fatalf("transient failures were not retried: %v", err)
	}
	l, _ := dyn.Resource(argoApplicationGVR).Namespace("").List(context.Background(), metav1.ListOptions{})
	if len(l.Items) != 3 {
		t.Errorf("%d Applications after the retries, want 3", len(l.Items))
	}

	for _, msg := range []string{`Post "https://lab:16438/apis/x": http2: client connection lost`, "read tcp: connection reset by peer", "unexpected EOF"} {
		if !transient(errors.New(msg)) {
			t.Errorf("%q is not retried: a dropped connection is cured by a retry", msg)
		}
	}
	if transient(errors.New("the server could not find the requested resource")) {
		t.Error("a missing CRD is retried: it will not appear")
	}

	dyn = newDyn()
	denied := apierrors.NewForbidden(schema.GroupResource{Resource: "applications"}, "x", errors.New("no"))
	calls := 0
	dyn.PrependReactor("create", "applications", func(ktesting.Action) (bool, runtime.Object, error) { calls++; return true, nil, denied })
	_, err := seedGitOps(context.Background(), dyn, config{Namespaces: 1, ArgoApps: 1}, 1, io.Discard)
	if !apierrors.IsForbidden(err) || calls != 1 {
		t.Errorf("a forbidden create: err %v after %d calls, want it returned at once", err, calls)
	}
}

// A create that times out at the server may still have gone through: the
// retry then meets AlreadyExists, and the object must not be left without the
// status its size and the collector's reading depend on (#233).
func TestCreateWithStatusFillsAnObjectThatHasNone(t *testing.T) {
	cfg := config{Namespaces: 2, FluxHelmReleases: 2, Seed: 1}
	dyn := newDyn()
	ctx := context.Background()
	o := fluxHelmReleaseObjects(0, cfg)
	res := dyn.Resource(fluxHelmReleaseGVR).Namespace(o.release.GetNamespace())

	// The server made the object but the client was told it timed out: the
	// first create fails after storing, the retry finds it.
	timedOut := false
	dyn.PrependReactor("create", "helmreleases", func(a ktesting.Action) (bool, runtime.Object, error) {
		if timedOut {
			return false, nil, nil
		}
		timedOut = true
		obj := a.(ktesting.CreateAction).GetObject()
		if err := dyn.Tracker().Create(fluxHelmReleaseGVR, obj, o.release.GetNamespace()); err != nil {
			t.Fatal(err)
		}
		return true, nil, errors.New("etcdserver: request timed out")
	})
	old := retryDelay
	retryDelay = 0
	t.Cleanup(func() { retryDelay = old })

	if err := createWithStatus(ctx, res, o.release, o.status); err != nil {
		t.Fatal(err)
	}
	got, err := res.Get(ctx, o.release.GetName(), metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, found, _ := unstructured.NestedSlice(got.Object, "status", "history"); !found {
		t.Errorf("an object whose create timed out but went through has no status: %v", got.Object["status"])
	}

	// An object that has a status keeps it: a rerun does not rewrite it.
	kept := fluxHelmReleaseObjects(1, cfg)
	existing := kept.release.DeepCopy()
	existing.Object["status"] = map[string]any{"observedGeneration": int64(7)}
	res1 := dyn.Resource(fluxHelmReleaseGVR).Namespace(kept.release.GetNamespace())
	if _, err := res1.Create(ctx, existing, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := createWithStatus(ctx, res1, kept.release, kept.status); err != nil {
		t.Fatal(err)
	}
	got, _ = res1.Get(ctx, kept.release.GetName(), metav1.GetOptions{})
	if g, _, _ := unstructured.NestedInt64(got.Object, "status", "observedGeneration"); g != 7 {
		t.Errorf("an object that had a status was rewritten: %v", got.Object["status"])
	}
}

// An UpdateStatus that timed out at the client but was applied makes its
// retry fail with a Conflict (the resourceVersion moved). That is not a
// failure of the fill: the object has its status, so the call reads it again
// and is done, and an object that still has none gets another try.
func TestCreateWithStatusSurvivesAConflictAfterATimedOutStatusUpdate(t *testing.T) {
	old := retryDelay
	retryDelay = 0
	t.Cleanup(func() { retryDelay = old })
	cfg := config{Namespaces: 2, FluxHelmReleases: 2, Seed: 1}
	ctx := context.Background()

	dyn := newDyn()
	o := fluxHelmReleaseObjects(0, cfg)
	ns := o.release.GetNamespace()
	res := dyn.Resource(fluxHelmReleaseGVR).Namespace(ns)
	updates := 0
	dyn.PrependReactor("update", "helmreleases", func(a ktesting.Action) (bool, runtime.Object, error) {
		updates++
		if updates == 1 { // applied, but the answer was lost
			if err := dyn.Tracker().Update(fluxHelmReleaseGVR, a.(ktesting.UpdateAction).GetObject(), ns); err != nil {
				t.Fatal(err)
			}
			return true, nil, errors.New("etcdserver: request timed out")
		}
		return true, nil, apierrors.NewConflict(schema.GroupResource{Resource: "helmreleases"}, o.release.GetName(), errors.New("the object has been modified"))
	})
	if err := createWithStatus(ctx, res, o.release, o.status); err != nil {
		t.Fatalf("a conflict after an applied status update aborted the fill: %v", err)
	}
	got, err := res.Get(ctx, o.release.GetName(), metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, found, _ := unstructured.NestedSlice(got.Object, "status", "history"); !found {
		t.Errorf("the status is missing after the timed-out update: %v", got.Object["status"])
	}
	if updates != 2 {
		t.Errorf("%d status updates, want 2 (the timed-out one and its retry)", updates)
	}

	// A conflict that did not apply the status is tried again, and ends.
	dyn = newDyn()
	o = fluxHelmReleaseObjects(1, cfg)
	res = dyn.Resource(fluxHelmReleaseGVR).Namespace(o.release.GetNamespace())
	conflicts := 0
	dyn.PrependReactor("update", "helmreleases", func(ktesting.Action) (bool, runtime.Object, error) {
		if conflicts < 1 {
			conflicts++
			return true, nil, apierrors.NewConflict(schema.GroupResource{Resource: "helmreleases"}, o.release.GetName(), errors.New("stale"))
		}
		return false, nil, nil
	})
	if err := createWithStatus(ctx, res, o.release, o.status); err != nil {
		t.Fatalf("a one-off conflict was not retried: %v", err)
	}
	got, _ = res.Get(ctx, o.release.GetName(), metav1.GetOptions{})
	if _, found, _ := unstructured.NestedSlice(got.Object, "status", "history"); !found {
		t.Errorf("no status after a retried conflict: %v", got.Object["status"])
	}
}
