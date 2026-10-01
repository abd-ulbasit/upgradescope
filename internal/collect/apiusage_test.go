package collect

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/discovery"
	discoveryfake "k8s.io/client-go/discovery/fake"
	kubefake "k8s.io/client-go/kubernetes/fake"
	metadatafake "k8s.io/client-go/metadata/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/abd-ulbasit/upgradescope/internal/engine"
	"github.com/abd-ulbasit/upgradescope/internal/inventory"
	"github.com/abd-ulbasit/upgradescope/internal/kb"
)

func ver(major, minor int) *inventory.Version {
	return &inventory.Version{Major: major, Minor: minor}
}

// obj is one stored object. The apiserver serves every stored object at
// every version of its resource (converted), so servedAt copies it to each
// served version: a collector that lists a deprecated endpoint sees all of
// them, exactly as on a real cluster.
type obj struct {
	namespace, name string
	managed         []metav1.ManagedFieldsEntry
	annotations     map[string]string
}

func wrote(manager, apiVersion string) metav1.ManagedFieldsEntry {
	return metav1.ManagedFieldsEntry{Manager: manager, Operation: metav1.ManagedFieldsOperationUpdate, APIVersion: apiVersion}
}

func lastApplied(apiVersion, kind string) map[string]string {
	return map[string]string{
		"kubectl.kubernetes.io/last-applied-configuration": fmt.Sprintf(`{"apiVersion":%q,"kind":%q,"metadata":{"name":"x"},"spec":{}}`, apiVersion, kind),
	}
}

func servedAt(kind string, apiVersions []string, objs ...obj) []runtime.Object {
	var out []runtime.Object
	for _, av := range apiVersions {
		for _, o := range objs {
			out = append(out, &metav1.PartialObjectMetadata{
				TypeMeta: metav1.TypeMeta{APIVersion: av, Kind: kind},
				ObjectMeta: metav1.ObjectMeta{
					Namespace: o.namespace, Name: o.name,
					ManagedFields: o.managed, Annotations: o.annotations,
				},
			})
		}
	}
	return out
}

func metaClient(objs ...[]runtime.Object) *metadatafake.FakeMetadataClient {
	scheme := runtime.NewScheme()
	utilruntime.Must(metav1.AddMetaToScheme(scheme))
	var all []runtime.Object
	for _, o := range objs {
		all = append(all, o...)
	}
	return metadatafake.NewSimpleMetadataClient(scheme, all...)
}

// fakeDiscovery serves lists in order; the fake reports the first version
// listed for a group as its preferred version.
func fakeDiscovery(lists ...*metav1.APIResourceList) *discoveryfake.FakeDiscovery {
	disc := kubefake.NewClientset().Discovery().(*discoveryfake.FakeDiscovery)
	disc.Resources = lists
	return disc
}

func resources(gv string, rs ...metav1.APIResource) *metav1.APIResourceList {
	return &metav1.APIResourceList{GroupVersion: gv, APIResources: rs}
}

var (
	ingresses = metav1.APIResource{Name: "ingresses", Kind: "Ingress", Namespaced: true, Verbs: metav1.Verbs{"get", "list"}}
	psps      = metav1.APIResource{Name: "podsecuritypolicies", Kind: "PodSecurityPolicy", Verbs: metav1.Verbs{"list"}}
	pdbs      = metav1.APIResource{Name: "poddisruptionbudgets", Kind: "PodDisruptionBudget", Namespaced: true, Verbs: metav1.Verbs{"list"}}
)

// ingressLifecycle: networking.k8s.io/v1beta1 Ingress is removed and
// continues as v1 (it has a replacement), so only authorship counts.
func ingressLifecycle() []kb.APILifecycleEntry {
	return []kb.APILifecycleEntry{
		{Group: "networking.k8s.io", Version: "v1beta1", Kind: "Ingress", Introduced: inventory.Version{Major: 1, Minor: 14}, Deprecated: ver(1, 19), Removed: ver(1, 22),
			Replacement: &kb.GVK{Group: "networking.k8s.io", Version: "v1", Kind: "Ingress"}},
		{Group: "networking.k8s.io", Version: "v1", Kind: "Ingress", Introduced: inventory.Version{Major: 1, Minor: 19}},
		// PodSecurityPolicy has no replacement: the type itself goes away.
		{Group: "policy", Version: "v1beta1", Kind: "PodSecurityPolicy", Introduced: inventory.Version{Major: 1, Minor: 10}, Deprecated: ver(1, 21), Removed: ver(1, 25)},
	}
}

// listedGVRs returns every GVR the collector LISTed, in call order.
func listedGVRs(c *metadatafake.FakeMetadataClient) []schema.GroupVersionResource {
	var out []schema.GroupVersionResource
	for _, a := range c.Actions() {
		if a.GetVerb() == "list" {
			out = append(out, a.GetResource())
		}
	}
	return out
}

func TestCollectAPIUsageFlagsOnlyObjectsAuthoredViaDeprecatedVersion(t *testing.T) {
	served := []string{"networking.k8s.io/v1", "networking.k8s.io/v1beta1"}
	meta := metaClient(servedAt("Ingress", served,
		obj{namespace: "default", name: "via-v1", managed: []metav1.ManagedFieldsEntry{wrote("kubectl-client-side-apply", "networking.k8s.io/v1")}},
		obj{namespace: "default", name: "via-beta-helm", managed: []metav1.ManagedFieldsEntry{wrote("helm", "networking.k8s.io/v1beta1")}},
		obj{namespace: "prod", name: "via-beta-last-applied", annotations: lastApplied("networking.k8s.io/v1beta1", "Ingress")},
		// Only the status subresource was written at v1beta1 (an ingress
		// controller): the spec still comes from a v1 manifest.
		obj{namespace: "prod", name: "status-only", managed: []metav1.ManagedFieldsEntry{
			wrote("argocd", "networking.k8s.io/v1"),
			{Manager: "nginx-ingress-controller", Operation: metav1.ManagedFieldsOperationUpdate, APIVersion: "networking.k8s.io/v1beta1", Subresource: "status"},
		}},
		// A control-plane component wrote it while the beta was current.
		obj{namespace: "prod", name: "internal", managed: []metav1.ManagedFieldsEntry{wrote("kube-controller-manager", "networking.k8s.io/v1beta1")}},
	))
	disc := fakeDiscovery(
		resources("networking.k8s.io/v1", ingresses),
		resources("networking.k8s.io/v1beta1", ingresses),
	)

	var inv inventory.Inventory
	if err := collectAPIUsage(context.Background(), disc, meta, ingressLifecycle(), &inv); err != nil {
		t.Fatal(err)
	}
	want := []inventory.APIUsage{{
		Group: "networking.k8s.io", Version: "v1beta1", Kind: "Ingress", Count: 2,
		Namespaces: map[string]int{"default": 1, "prod": 1},
		Objects: []inventory.ObjectRef{
			{Namespace: "default", Name: "via-beta-helm", Manager: "helm"},
			{Namespace: "prod", Name: "via-beta-last-applied", Manager: "kubectl last-applied"},
		},
	}}
	if !reflect.DeepEqual(inv.APIUsage, want) {
		t.Errorf("api usage = %#v\nwant       %#v", inv.APIUsage, want)
	}
	wantLists := []schema.GroupVersionResource{{Group: "networking.k8s.io", Version: "v1", Resource: "ingresses"}}
	if got := listedGVRs(meta); !reflect.DeepEqual(got, wantLists) {
		t.Errorf("listed %v, want exactly %v (never the deprecated endpoint)", got, wantLists)
	}
}

func TestCollectAPIUsageObjectWrittenViaGAVersionIsNotAFinding(t *testing.T) {
	served := []string{"networking.k8s.io/v1", "networking.k8s.io/v1beta1"}
	meta := metaClient(servedAt("Ingress", served,
		obj{namespace: "default", name: "web", managed: []metav1.ManagedFieldsEntry{wrote("kubectl-client-side-apply", "networking.k8s.io/v1")},
			annotations: lastApplied("networking.k8s.io/v1", "Ingress")},
	))
	disc := fakeDiscovery(resources("networking.k8s.io/v1", ingresses), resources("networking.k8s.io/v1beta1", ingresses))

	var inv inventory.Inventory
	if err := collectAPIUsage(context.Background(), disc, meta, ingressLifecycle(), &inv); err != nil {
		t.Fatal(err)
	}
	if len(inv.APIUsage) != 0 {
		t.Errorf("api usage = %#v, want none: the object was written via v1 and is only served at v1beta1", inv.APIUsage)
	}
}

// The preferred version is a per-group choice; when it is the deprecated
// version of this resource, a served non-deprecated version still wins.
func TestCollectAPIUsageNeverListsDeprecatedVersionWhenAnotherIsServed(t *testing.T) {
	served := []string{"networking.k8s.io/v1beta1", "networking.k8s.io/v1"}
	meta := metaClient(servedAt("Ingress", served,
		obj{namespace: "default", name: "web", managed: []metav1.ManagedFieldsEntry{wrote("helm", "networking.k8s.io/v1beta1")}},
	))
	disc := fakeDiscovery( // v1beta1 first: the fake reports it as preferred
		resources("networking.k8s.io/v1beta1", ingresses),
		resources("networking.k8s.io/v1", ingresses),
	)

	var inv inventory.Inventory
	if err := collectAPIUsage(context.Background(), disc, meta, ingressLifecycle(), &inv); err != nil {
		t.Fatal(err)
	}
	wantLists := []schema.GroupVersionResource{{Group: "networking.k8s.io", Version: "v1", Resource: "ingresses"}}
	if got := listedGVRs(meta); !reflect.DeepEqual(got, wantLists) {
		t.Errorf("listed %v, want exactly %v", got, wantLists)
	}
	if len(inv.APIUsage) != 1 || inv.APIUsage[0].Count != 1 {
		t.Errorf("api usage = %#v, want the v1beta1-authored object", inv.APIUsage)
	}
}

// A kind whose type goes away (no replacement, no other served version):
// every existing object is a real blocker, however it was written.
func TestCollectAPIUsageTypeRemovedKindCountsEveryObject(t *testing.T) {
	meta := metaClient(servedAt("PodSecurityPolicy", []string{"policy/v1beta1"},
		obj{name: "restricted", managed: []metav1.ManagedFieldsEntry{wrote("kubectl-client-side-apply", "policy/v1beta1")}},
		obj{name: "privileged"},
	))
	disc := fakeDiscovery( // policy's preferred version (v1) does not serve PSPs
		resources("policy/v1", pdbs),
		resources("policy/v1beta1", pdbs, psps),
	)

	var inv inventory.Inventory
	if err := collectAPIUsage(context.Background(), disc, meta, ingressLifecycle(), &inv); err != nil {
		t.Fatal(err)
	}
	want := []inventory.APIUsage{{
		Group: "policy", Version: "v1beta1", Kind: "PodSecurityPolicy", Count: 2,
		Namespaces: map[string]int{"": 2},
		Objects:    []inventory.ObjectRef{{Name: "privileged"}, {Name: "restricted"}},
	}}
	if !reflect.DeepEqual(inv.APIUsage, want) {
		t.Errorf("api usage = %#v\nwant       %#v", inv.APIUsage, want)
	}
	// Every served version of PSPs is deprecated, so listing v1beta1 is
	// unavoidable; poddisruptionbudgets are not flagged and never listed.
	wantLists := []schema.GroupVersionResource{{Group: "policy", Version: "v1beta1", Resource: "podsecuritypolicies"}}
	if got := listedGVRs(meta); !reflect.DeepEqual(got, wantLists) {
		t.Errorf("listed %v, want exactly %v", got, wantLists)
	}
}

func TestCollectAPIUsageCapsObjectRefs(t *testing.T) {
	var objs []obj
	for i := range inventory.MaxObjectRefs + 2 {
		objs = append(objs, obj{name: fmt.Sprintf("psp-%03d", i)})
	}
	meta := metaClient(servedAt("PodSecurityPolicy", []string{"policy/v1beta1"}, objs...))
	disc := fakeDiscovery(resources("policy/v1beta1", psps))

	var inv inventory.Inventory
	if err := collectAPIUsage(context.Background(), disc, meta, ingressLifecycle(), &inv); err != nil {
		t.Fatal(err)
	}
	if len(inv.APIUsage) != 1 {
		t.Fatalf("api usage = %#v", inv.APIUsage)
	}
	u := inv.APIUsage[0]
	if u.Count != inventory.MaxObjectRefs+2 || len(u.Objects) != inventory.MaxObjectRefs || u.ObjectsOmitted != 2 {
		t.Errorf("count=%d objects=%d omitted=%d, want %d/%d/2", u.Count, len(u.Objects), u.ObjectsOmitted, inventory.MaxObjectRefs+2, inventory.MaxObjectRefs)
	}
}

func TestAuthoringManagerIgnoresInternalManagersAndStatusEntries(t *testing.T) {
	const gv = "flowcontrol.apiserver.k8s.io/v1beta3"
	cases := []struct {
		name    string
		managed []metav1.ManagedFieldsEntry
		annot   map[string]string
		want    string
	}{
		{"user manager", []metav1.ManagedFieldsEntry{wrote("terraform", gv)}, nil, "terraform"},
		{"kube-apiserver", []metav1.ManagedFieldsEntry{wrote("kube-apiserver", gv)}, nil, ""},
		{"kube-controller-manager", []metav1.ManagedFieldsEntry{wrote("kube-controller-manager", gv)}, nil, ""},
		{"apf config producer", []metav1.ManagedFieldsEntry{wrote("api-priority-and-fairness-config-producer-v1", gv)}, nil, ""},
		{"status subresource", []metav1.ManagedFieldsEntry{{Manager: "operator", APIVersion: gv, Subresource: "status"}}, nil, ""},
		{"other version", []metav1.ManagedFieldsEntry{wrote("terraform", "flowcontrol.apiserver.k8s.io/v1")}, nil, ""},
		{"last-applied", nil, lastApplied(gv, "FlowSchema"), "kubectl last-applied"},
		{"managed fields win over last-applied", []metav1.ManagedFieldsEntry{wrote("kubectl-client-side-apply", gv)}, lastApplied(gv, "FlowSchema"), "kubectl-client-side-apply"},
		{"unparseable last-applied", nil, map[string]string{"kubectl.kubernetes.io/last-applied-configuration": "{"}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := metav1.PartialObjectMetadata{ObjectMeta: metav1.ObjectMeta{ManagedFields: tc.managed, Annotations: tc.annot}}
			if got := authoringManager(&m, gv); got != tc.want {
				t.Errorf("authoringManager = %q, want %q", got, tc.want)
			}
		})
	}
}

// Issue #3's headline case: on a default 1.29–1.31 cluster the apiserver
// keeps flowcontrol v1beta3 served and maintains its own bootstrap APF
// objects. They must not become removed-API blockers for a 1.32 upgrade.
func TestCollectAPIUsageAPFBootstrapObjectsAreNotBlockers(t *testing.T) {
	const beta3 = "flowcontrol.apiserver.k8s.io/v1beta3"
	served := []string{"flowcontrol.apiserver.k8s.io/v1", beta3}
	bootstrap := func(name string) obj {
		return obj{name: name,
			annotations: map[string]string{"apf.kubernetes.io/autoupdate-spec": "true"},
			managed:     []metav1.ManagedFieldsEntry{wrote("api-priority-and-fairness-config-producer-v1", beta3)}}
	}
	meta := metaClient(
		servedAt("FlowSchema", served, bootstrap("exempt"), bootstrap("system-leader-election"), bootstrap("catch-all")),
		servedAt("PriorityLevelConfiguration", served, bootstrap("exempt"), bootstrap("workload-low"), bootstrap("catch-all")),
	)
	apf := func(gv string) *metav1.APIResourceList {
		return resources(gv,
			metav1.APIResource{Name: "flowschemas", Kind: "FlowSchema", Verbs: metav1.Verbs{"list"}},
			metav1.APIResource{Name: "flowschemas/status", Kind: "FlowSchema", Verbs: metav1.Verbs{"get"}},
			metav1.APIResource{Name: "prioritylevelconfigurations", Kind: "PriorityLevelConfiguration", Verbs: metav1.Verbs{"list"}},
		)
	}
	disc := fakeDiscovery(apf("flowcontrol.apiserver.k8s.io/v1"), apf(beta3))
	k, err := kb.Load()
	if err != nil {
		t.Fatal(err)
	}

	inv := inventory.Inventory{
		ServerVersion: "v1.31.4",
		Capabilities:  map[inventory.Capability]inventory.CapabilityStatus{},
	}
	if err := collectAPIUsage(context.Background(), disc, meta, k.APILifecycle, &inv); err != nil {
		t.Fatal(err)
	}
	if len(inv.APIUsage) != 0 {
		t.Errorf("api usage = %#v, want none for apiserver-maintained APF objects", inv.APIUsage)
	}
	for _, gvr := range listedGVRs(meta) {
		if gvr.Version != "v1" {
			t.Errorf("listed %v: the scanner must not call the deprecated endpoint", gvr)
		}
	}

	inv.Capabilities[inventory.CapAPIUsage] = inventory.CapabilityStatus{Available: true}
	inv.Capabilities[inventory.CapVersions] = inventory.CapabilityStatus{Available: true}
	rep := engine.Evaluate(inv, k, inventory.Version{Major: 1, Minor: 32}, time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC))
	if !rep.Ready {
		t.Errorf("1.31 → 1.32 with only bootstrap APF objects: ready=false, findings %+v", rep.Findings)
	}
}

// flaggedLifecycle: both Ingress and PodSecurityPolicy deprecated/removed.
func flaggedLifecycle() []kb.APILifecycleEntry { return ingressLifecycle() }

func flaggedDiscovery() *discoveryfake.FakeDiscovery {
	return fakeDiscovery(
		resources("networking.k8s.io/v1", ingresses),
		resources("networking.k8s.io/v1beta1", ingresses),
		resources("policy/v1beta1", psps),
	)
}

func flaggedObjects() *metadatafake.FakeMetadataClient {
	return metaClient(
		servedAt("Ingress", []string{"networking.k8s.io/v1", "networking.k8s.io/v1beta1"},
			obj{namespace: "default", name: "web", managed: []metav1.ManagedFieldsEntry{wrote("helm", "networking.k8s.io/v1beta1")}}),
		servedAt("PodSecurityPolicy", []string{"policy/v1beta1"}, obj{name: "restricted"}),
	)
}

func TestCollectAPIUsagePartialFailureKeepsSuccesses(t *testing.T) {
	meta := flaggedObjects()
	meta.PrependReactor("list", "podsecuritypolicies", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: "podsecuritypolicies"}, "", errors.New("RBAC denied"))
	})

	var inv inventory.Inventory
	err := collectAPIUsage(context.Background(), flaggedDiscovery(), meta, flaggedLifecycle(), &inv)

	var pe partialError
	if !errors.As(err, &pe) {
		t.Fatalf("err = %v, want partialError (capability stays available)", err)
	}
	if !strings.Contains(err.Error(), "podsecuritypolicies") {
		t.Errorf("reason = %q, must name the failed resource", err.Error())
	}
	want := []inventory.APIUsage{{
		Group: "networking.k8s.io", Version: "v1beta1", Kind: "Ingress", Count: 1, Namespaces: map[string]int{"default": 1},
		Objects: []inventory.ObjectRef{{Namespace: "default", Name: "web", Manager: "helm"}},
	}}
	if !reflect.DeepEqual(inv.APIUsage, want) {
		t.Errorf("api usage = %#v\nwant     %#v (successes must be kept)", inv.APIUsage, want)
	}
}

func TestCollectAPIUsageAllResourcesFailedDegrades(t *testing.T) {
	meta := flaggedObjects()
	for _, res := range []string{"ingresses", "podsecuritypolicies"} {
		meta.PrependReactor("list", res, func(k8stesting.Action) (bool, runtime.Object, error) {
			return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: res}, "", errors.New("RBAC denied"))
		})
	}

	var inv inventory.Inventory
	err := collectAPIUsage(context.Background(), flaggedDiscovery(), meta, flaggedLifecycle(), &inv)
	if err == nil {
		t.Fatal("want error when every flagged resource fails")
	}
	var pe partialError
	if errors.As(err, &pe) {
		t.Errorf("err = %v, want hard error (nothing succeeded), not partial", err)
	}
}

// partialDiscovery simulates one broken aggregated API group: lists are
// returned alongside an ErrGroupDiscoveryFailed.
type partialDiscovery struct {
	*discoveryfake.FakeDiscovery
}

func (p partialDiscovery) ServerGroupsAndResources() ([]*metav1.APIGroup, []*metav1.APIResourceList, error) {
	groups, lists, _ := p.FakeDiscovery.ServerGroupsAndResources()
	return groups, lists, &discovery.ErrGroupDiscoveryFailed{Groups: map[schema.GroupVersion]error{
		{Group: "metrics.k8s.io", Version: "v1beta1"}: errors.New("the server is currently unable to handle the request"),
	}}
}

func TestCollectAPIUsagePartialDiscoverySurfacesSkippedGroups(t *testing.T) {
	var inv inventory.Inventory
	err := collectAPIUsage(context.Background(), partialDiscovery{flaggedDiscovery()}, flaggedObjects(), flaggedLifecycle(), &inv)

	var pe partialError
	if !errors.As(err, &pe) {
		t.Fatalf("err = %v, want partialError surfacing skipped groups", err)
	}
	if !strings.Contains(err.Error(), "metrics.k8s.io/v1beta1") {
		t.Errorf("reason = %q, must name the skipped group", err.Error())
	}
	if len(inv.APIUsage) != 2 {
		t.Errorf("api usage = %#v, want both served resources still counted", inv.APIUsage)
	}
}
