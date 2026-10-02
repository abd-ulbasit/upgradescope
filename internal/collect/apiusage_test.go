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
	"k8s.io/client-go/metadata"
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

// wroteAt is wrote with the entry's timestamp, minute m of a fixed day.
func wroteAt(manager, apiVersion string, m int) metav1.ManagedFieldsEntry {
	e := wrote(manager, apiVersion)
	e.Time = &metav1.Time{Time: time.Date(2026, 9, 1, 0, m, 0, 0, time.UTC)}
	return e
}

// appliedAt is a server-side apply entry at minute m. A manager has at
// most one Apply entry per subresource: each apply replaces its field set
// and apiVersion.
func appliedAt(manager, apiVersion string, m int) metav1.ManagedFieldsEntry {
	e := wroteAt(manager, apiVersion, m)
	e.Operation = metav1.ManagedFieldsOperationApply
	return e
}

func lastApplied(apiVersion, kind string) map[string]string {
	return map[string]string{
		"kubectl.kubernetes.io/last-applied-configuration": fmt.Sprintf(`{"apiVersion":%q,"kind":%q,"metadata":{"name":"x"},"spec":{}}`, apiVersion, kind),
	}
}

// withAutoUpdate adds apf.kubernetes.io/autoupdate-spec="true" to annotations.
func withAutoUpdate(annotations map[string]string) map[string]string {
	annotations["apf.kubernetes.io/autoupdate-spec"] = "true"
	return annotations
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

// The remediation path: the chart moved to v1 and `helm upgrade` ran. The
// apiserver adds a second helm entry at v1 and keeps the v1beta1 one for
// every field the upgrade did not change, so the blocker must clear on the
// newer entry, not wait for the old one to disappear.
func TestCollectAPIUsageClearsAfterManagerMigrates(t *testing.T) {
	served := []string{"networking.k8s.io/v1", "networking.k8s.io/v1beta1"}
	meta := metaClient(servedAt("Ingress", served,
		obj{namespace: "default", name: "web", managed: []metav1.ManagedFieldsEntry{
			wroteAt("helm", "networking.k8s.io/v1beta1", 1),
			wroteAt("helm", "networking.k8s.io/v1", 2),
		}},
	))
	disc := fakeDiscovery(resources("networking.k8s.io/v1", ingresses), resources("networking.k8s.io/v1beta1", ingresses))

	var inv inventory.Inventory
	if err := collectAPIUsage(context.Background(), disc, meta, ingressLifecycle(), &inv); err != nil {
		t.Fatal(err)
	}
	if len(inv.APIUsage) != 0 {
		t.Errorf("api usage = %#v, want none: helm's newest write is v1", inv.APIUsage)
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

// On 1.19–1.21, extensions/v1beta1 ingresses is its own group/resource
// with only a deprecated version, yet it is the same storage that
// networking.k8s.io serves at v1. Listing it there keeps the scanner off
// the deprecated endpoint (no self-inflicted deprecated-calls row at
// 1.22), and one LIST serves both groups' targets.
func TestCollectAPIUsageListsAtReplacementGroupWhenOwnGroupIsAllDeprecated(t *testing.T) {
	lifecycle := append(ingressLifecycle(), kb.APILifecycleEntry{
		Group: "extensions", Version: "v1beta1", Kind: "Ingress", Introduced: inventory.Version{Major: 1, Minor: 1}, Deprecated: ver(1, 14), Removed: ver(1, 22),
		Replacement: &kb.GVK{Group: "networking.k8s.io", Version: "v1", Kind: "Ingress"},
	})
	meta := metaClient(servedAt("Ingress", []string{"extensions/v1beta1", "networking.k8s.io/v1", "networking.k8s.io/v1beta1"},
		obj{namespace: "default", name: "via-extensions", managed: []metav1.ManagedFieldsEntry{wrote("helm", "extensions/v1beta1")}},
		obj{namespace: "default", name: "via-networking-beta", managed: []metav1.ManagedFieldsEntry{wrote("argocd", "networking.k8s.io/v1beta1")}},
		obj{namespace: "default", name: "via-v1", managed: []metav1.ManagedFieldsEntry{wrote("kubectl-client-side-apply", "networking.k8s.io/v1")}},
	))
	disc := fakeDiscovery(
		resources("extensions/v1beta1", ingresses),
		resources("networking.k8s.io/v1", ingresses),
		resources("networking.k8s.io/v1beta1", ingresses),
	)

	var inv inventory.Inventory
	if err := collectAPIUsage(context.Background(), disc, meta, lifecycle, &inv); err != nil {
		t.Fatal(err)
	}
	want := []inventory.APIUsage{
		{Group: "extensions", Version: "v1beta1", Kind: "Ingress", Count: 1, Namespaces: map[string]int{"default": 1},
			Objects: []inventory.ObjectRef{{Namespace: "default", Name: "via-extensions", Manager: "helm"}}},
		{Group: "networking.k8s.io", Version: "v1beta1", Kind: "Ingress", Count: 1, Namespaces: map[string]int{"default": 1},
			Objects: []inventory.ObjectRef{{Namespace: "default", Name: "via-networking-beta", Manager: "argocd"}}},
	}
	if !reflect.DeepEqual(inv.APIUsage, want) {
		t.Errorf("api usage = %#v\nwant       %#v", inv.APIUsage, want)
	}
	wantLists := []schema.GroupVersionResource{{Group: "networking.k8s.io", Version: "v1", Resource: "ingresses"}}
	if got := listedGVRs(meta); !reflect.DeepEqual(got, wantLists) {
		t.Errorf("listed %v, want exactly %v", got, wantLists)
	}
}

func TestListVersionOrder(t *testing.T) {
	v := func(version string, flagged bool) servedVersion {
		return servedVersion{version: version, kind: "K", flagged: flagged, list: true}
	}
	cases := []struct {
		name      string
		served    []servedVersion
		preferred string
		want      string
	}{
		{"preferred unflagged beats a newer one", []servedVersion{v("v1beta1", true), v("v2", false), v("v1", false)}, "v1", "v1"},
		{"newest unflagged when preferred is absent", []servedVersion{v("v1beta1", true), v("v1", false), v("v2beta1", false), v("v2", false)}, "v3", "v2"},
		{"unflagged beats a flagged preferred", []servedVersion{v("v1beta1", true), v("v1", false)}, "v1beta1", "v1"},
		{"newest flagged when all are flagged", []servedVersion{v("v1alpha1", true), v("v1beta2", true), v("v1beta1", true)}, "", "v1beta2"},
		{"unlistable versions are skipped", []servedVersion{v("v1beta1", true), {version: "v1", kind: "K"}}, "v1", "v1beta1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := listVersion(tc.served, tc.preferred)
			if !ok || got.version != tc.want {
				t.Errorf("listVersion = %q, %v; want %q", got.version, ok, tc.want)
			}
		})
	}
	if _, ok := listVersion([]servedVersion{v("v1", false)}, "v1"); ok {
		t.Error("listVersion with nothing flagged = ok, want nothing to list")
	}
}

// recordingMeta records the ListOptions of every LIST: the fake's list
// action drops Limit and Continue.
type recordingMeta struct {
	metadata.Interface
	opts *[]metav1.ListOptions
}

func (r recordingMeta) Resource(gvr schema.GroupVersionResource) metadata.Getter {
	return recordingGetter{r.Interface.Resource(gvr), r.opts}
}

type recordingGetter struct {
	metadata.Getter
	opts *[]metav1.ListOptions
}

func (g recordingGetter) List(ctx context.Context, opts metav1.ListOptions) (*metav1.PartialObjectMetadataList, error) {
	*g.opts = append(*g.opts, opts)
	return g.Getter.List(ctx, opts)
}

// Namespace records namespaced (and all-namespace) LISTs too.
func (g recordingGetter) Namespace(ns string) metadata.ResourceInterface {
	return recordingResource{g.Getter.Namespace(ns), g.opts}
}

type recordingResource struct {
	metadata.ResourceInterface
	opts *[]metav1.ListOptions
}

func (r recordingResource) List(ctx context.Context, opts metav1.ListOptions) (*metav1.PartialObjectMetadataList, error) {
	*r.opts = append(*r.opts, opts)
	return r.ResourceInterface.List(ctx, opts)
}

func TestCollectAPIUsageFollowsListPagination(t *testing.T) {
	ingress := func(name, manager, apiVersion string) runtime.RawExtension {
		return runtime.RawExtension{Object: &metav1.PartialObjectMetadata{
			TypeMeta:   metav1.TypeMeta{APIVersion: "networking.k8s.io/v1", Kind: "Ingress"},
			ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: name, ManagedFields: []metav1.ManagedFieldsEntry{wrote(manager, apiVersion)}},
		}}
	}
	meta := metaClient()
	calls := 0
	meta.PrependReactor("list", "ingresses", func(k8stesting.Action) (bool, runtime.Object, error) {
		calls++
		switch calls {
		case 1:
			return true, &metav1.List{
				ListMeta: metav1.ListMeta{Continue: "page-2"},
				Items: []runtime.RawExtension{
					ingress("page-1-beta", "helm", "networking.k8s.io/v1beta1"),
					ingress("page-1-v1", "argocd", "networking.k8s.io/v1"),
				},
			}, nil
		default:
			return true, &metav1.List{Items: []runtime.RawExtension{ingress("page-2-beta", "terraform", "networking.k8s.io/v1beta1")}}, nil
		}
	})
	var opts []metav1.ListOptions
	disc := fakeDiscovery(resources("networking.k8s.io/v1", ingresses), resources("networking.k8s.io/v1beta1", ingresses))

	var inv inventory.Inventory
	if err := collectAPIUsage(context.Background(), disc, recordingMeta{meta, &opts}, ingressLifecycle(), &inv); err != nil {
		t.Fatal(err)
	}
	wantOpts := []metav1.ListOptions{{Limit: listPageSize}, {Limit: listPageSize, Continue: "page-2"}}
	if !reflect.DeepEqual(opts, wantOpts) {
		t.Errorf("list options = %+v, want %+v (paged, Continue token followed)", opts, wantOpts)
	}
	want := []inventory.APIUsage{{
		Group: "networking.k8s.io", Version: "v1beta1", Kind: "Ingress", Count: 2,
		Namespaces: map[string]int{"default": 2},
		Objects: []inventory.ObjectRef{
			{Namespace: "default", Name: "page-1-beta", Manager: "helm"},
			{Namespace: "default", Name: "page-2-beta", Manager: "terraform"},
		},
	}}
	if !reflect.DeepEqual(inv.APIUsage, want) {
		t.Errorf("api usage = %#v\nwant       %#v (objects from every page must count)", inv.APIUsage, want)
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

		// An Update entry's identity includes its apiVersion, so a manager
		// that migrated keeps its old entry for every field the new write
		// left alone. Only each manager's newest entry says what it writes.
		{"manager migrated off the version", []metav1.ManagedFieldsEntry{wroteAt("helm", gv, 1), wroteAt("helm", "flowcontrol.apiserver.k8s.io/v1", 2)}, nil, ""},
		{"manager migrated, entries out of order", []metav1.ManagedFieldsEntry{wroteAt("helm", "flowcontrol.apiserver.k8s.io/v1", 2), wroteAt("helm", gv, 1)}, nil, ""},
		{"manager went back to the version", []metav1.ManagedFieldsEntry{wroteAt("helm", "flowcontrol.apiserver.k8s.io/v1", 1), wroteAt("helm", gv, 2)}, nil, "helm"},
		{"equal times prefer the other version", []metav1.ManagedFieldsEntry{wroteAt("helm", gv, 1), wroteAt("helm", "flowcontrol.apiserver.k8s.io/v1", 1)}, nil, ""},
		{"no times prefer the other version", []metav1.ManagedFieldsEntry{wrote("helm", gv), wrote("helm", "flowcontrol.apiserver.k8s.io/v1")}, nil, ""},
		{"an untimed entry ties with a timed one", []metav1.ManagedFieldsEntry{wrote("helm", "flowcontrol.apiserver.k8s.io/v1"), wroteAt("helm", gv, 1)}, nil, ""},
		// Each manager is judged on its own: one tool moving to v1 says
		// nothing about another that still writes the beta.
		{"another manager migrated", []metav1.ManagedFieldsEntry{wroteAt("terraform", gv, 1), wroteAt("helm", "flowcontrol.apiserver.k8s.io/v1", 2)}, nil, "terraform"},
		{"a newer status entry does not clear the main one", []metav1.ManagedFieldsEntry{
			wroteAt("operator", gv, 1),
			{Manager: "operator", APIVersion: "flowcontrol.apiserver.k8s.io/v1", Subresource: "status", Time: wroteAt("", "", 2).Time},
		}, nil, "operator"},
		// Server-side apply: a manager has one Apply entry per subresource,
		// replaced by each apply, so it names the version of the manager's
		// current configuration whatever its Update entries say. An Update
		// entry is keyed by its apiVersion and nothing clears it, so it says
		// the manager writes through gv only while it is newer than every
		// entry naming another version, the Apply entry included.
		{"apply via the version", []metav1.ManagedFieldsEntry{appliedAt("argocd", gv, 1)}, nil, "argocd"},
		{"apply via another version", []metav1.ManagedFieldsEntry{appliedAt("argocd", "flowcontrol.apiserver.k8s.io/v1", 1)}, nil, ""},
		{"a newer update via another version does not clear the apply", []metav1.ManagedFieldsEntry{
			appliedAt("helm", gv, 1), wroteAt("helm", "flowcontrol.apiserver.k8s.io/v1", 2),
		}, nil, "helm"},
		{"an apply via the version newer than an update via another version", []metav1.ManagedFieldsEntry{
			wroteAt("helm", "flowcontrol.apiserver.k8s.io/v1", 1), appliedAt("helm", gv, 2),
		}, nil, "helm"},
		{"a newer update via the version under an apply via another version", []metav1.ManagedFieldsEntry{
			appliedAt("helm", "flowcontrol.apiserver.k8s.io/v1", 1), wroteAt("helm", gv, 2),
		}, nil, "helm"},
		// Helm 3 wrote by Update; Helm 4 applies server-side under the same
		// manager name. The old Update entry stays for every field the apply
		// did not take over.
		{"manager moved from update via the version to apply via another", []metav1.ManagedFieldsEntry{
			wroteAt("helm", gv, 1), appliedAt("helm", "flowcontrol.apiserver.k8s.io/v1", 2),
		}, nil, ""},
		{"an update via the version ties with an apply via another version", []metav1.ManagedFieldsEntry{
			wroteAt("helm", gv, 1), appliedAt("helm", "flowcontrol.apiserver.k8s.io/v1", 1),
		}, nil, ""},
		{"apply and update both via another version", []metav1.ManagedFieldsEntry{
			appliedAt("helm", "flowcontrol.apiserver.k8s.io/v1", 1), wroteAt("helm", "flowcontrol.apiserver.k8s.io/v1", 2),
		}, nil, ""},
		// managedFields from a non-internal writer are newer evidence than
		// the annotation, which only kubectl client-side apply rewrites.
		{"stale last-applied under a v1 manager", []metav1.ManagedFieldsEntry{wrote("helm", "flowcontrol.apiserver.k8s.io/v1")}, lastApplied(gv, "FlowSchema"), ""},
		{"last-applied with only internal managers", []metav1.ManagedFieldsEntry{wrote("kube-controller-manager", "flowcontrol.apiserver.k8s.io/v1")}, lastApplied(gv, "FlowSchema"), "kubectl last-applied"},
		{"last-applied with only status entries", []metav1.ManagedFieldsEntry{{Manager: "operator", APIVersion: "flowcontrol.apiserver.k8s.io/v1", Subresource: "status"}}, lastApplied(gv, "FlowSchema"), "kubectl last-applied"},
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

// The autoupdate annotation alone excludes an APF object: the apiserver
// rewrites its spec on every start, so even an entry from a user tool
// that once wrote it via the beta says nothing about what will break.
// The same object without the annotation is a real authored use.
func TestCollectAPIUsageAPFAutoUpdateAnnotationExcludesObject(t *testing.T) {
	const beta3 = "flowcontrol.apiserver.k8s.io/v1beta3"
	served := []string{"flowcontrol.apiserver.k8s.io/v1", beta3}
	meta := metaClient(servedAt("FlowSchema", served,
		obj{name: "auto-managed",
			annotations: map[string]string{"apf.kubernetes.io/autoupdate-spec": "true"},
			managed:     []metav1.ManagedFieldsEntry{wrote("kubectl-client-side-apply", beta3)}},
		obj{name: "auto-last-applied", annotations: withAutoUpdate(lastApplied(beta3, "FlowSchema"))},
		obj{name: "user-owned",
			annotations: map[string]string{"apf.kubernetes.io/autoupdate-spec": "false"},
			managed:     []metav1.ManagedFieldsEntry{wrote("kubectl-client-side-apply", beta3)}},
	))
	flowschemas := metav1.APIResource{Name: "flowschemas", Kind: "FlowSchema", Verbs: metav1.Verbs{"list"}}
	disc := fakeDiscovery(resources("flowcontrol.apiserver.k8s.io/v1", flowschemas), resources(beta3, flowschemas))
	lifecycle := []kb.APILifecycleEntry{
		{Group: "flowcontrol.apiserver.k8s.io", Version: "v1beta3", Kind: "FlowSchema", Introduced: inventory.Version{Major: 1, Minor: 26}, Deprecated: ver(1, 29), Removed: ver(1, 32),
			Replacement: &kb.GVK{Group: "flowcontrol.apiserver.k8s.io", Version: "v1", Kind: "FlowSchema"}},
	}

	var inv inventory.Inventory
	if err := collectAPIUsage(context.Background(), disc, meta, lifecycle, &inv); err != nil {
		t.Fatal(err)
	}
	want := []inventory.APIUsage{{
		Group: "flowcontrol.apiserver.k8s.io", Version: "v1beta3", Kind: "FlowSchema", Count: 1,
		Namespaces: map[string]int{"": 1},
		Objects:    []inventory.ObjectRef{{Name: "user-owned", Manager: "kubectl-client-side-apply"}},
	}}
	if !reflect.DeepEqual(inv.APIUsage, want) {
		t.Errorf("api usage = %#v\nwant       %#v", inv.APIUsage, want)
	}
}

// A kind with no KB replacement that is also served at a non-deprecated
// version continues there (ServiceCIDR, IPAddress, ValidatingAdmission-
// Policy and the DRA kinds have no replacement recorded): it does not go
// away, so only authorship counts, never residency. Issue #3's 1.34 repro,
// judged by the real KB.
func TestCollectAPIUsageNoReplacementButGAServedCountsOnlyAuthors(t *testing.T) {
	const beta = "networking.k8s.io/v1beta1"
	meta := metaClient(servedAt("ServiceCIDR", []string{"networking.k8s.io/v1", beta},
		obj{name: "kubernetes", managed: []metav1.ManagedFieldsEntry{wrote("kube-apiserver", beta)}},
		obj{name: "via-v1", managed: []metav1.ManagedFieldsEntry{wrote("kubectl-client-side-apply", "networking.k8s.io/v1")}},
		obj{name: "via-beta", managed: []metav1.ManagedFieldsEntry{wrote("terraform", beta)}},
	))
	servicecidrs := metav1.APIResource{Name: "servicecidrs", Kind: "ServiceCIDR", Verbs: metav1.Verbs{"list"}}
	disc := fakeDiscovery(resources("networking.k8s.io/v1", servicecidrs), resources(beta, servicecidrs))

	var inv inventory.Inventory
	if err := collectAPIUsage(context.Background(), disc, meta, loadKB(t).APILifecycle, &inv); err != nil {
		t.Fatal(err)
	}
	want := []inventory.APIUsage{{
		Group: "networking.k8s.io", Version: "v1beta1", Kind: "ServiceCIDR", Count: 1,
		Namespaces: map[string]int{"": 1},
		Objects:    []inventory.ObjectRef{{Name: "via-beta", Manager: "terraform"}},
	}}
	if !reflect.DeepEqual(inv.APIUsage, want) {
		t.Errorf("api usage = %#v\nwant       %#v", inv.APIUsage, want)
	}
}

// loadKB loads the embedded knowledge base: tests of which kinds go away
// must judge by the real lifecycle data, not a hand-built subset of it.
func loadKB(t *testing.T) kb.KB {
	t.Helper()
	k, err := kb.Load()
	if err != nil {
		t.Fatal(err)
	}
	return k
}

// evaluateAt runs the engine on a live inventory whose versions and
// api-usage capabilities are available.
func evaluateAt(inv inventory.Inventory, k kb.KB, server string, target inventory.Version) engine.Report {
	inv.ServerVersion = server
	inv.Capabilities = map[inventory.Capability]inventory.CapabilityStatus{
		inventory.CapAPIUsage: {Available: true},
		inventory.CapVersions: {Available: true},
	}
	return engine.Evaluate(inv, k, target, time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC))
}

// removedAPIFindings returns the report's removed-api findings.
func removedAPIFindings(rep engine.Report) []engine.Finding {
	var out []engine.Finding
	for _, f := range rep.Findings {
		if f.Category == engine.CatRemovedAPI {
			out = append(out, f)
		}
	}
	return out
}

// A 1.32 cluster serves ServiceCIDR and IPAddress only at
// networking.k8s.io/v1beta1 (v1 arrives in 1.33), and the apiserver
// writes the default `kubernetes` ServiceCIDR and the service IPAddresses
// itself. The KB records no replacement for either beta, but both kinds
// continue at v1, so these objects are not blockers for a later upgrade:
// the apiserver rewrites them, nobody's manifest is behind them.
func TestCollectAPIUsageServiceCIDROnlyBetaServedApiserverObjectsAreNotBlockers(t *testing.T) {
	const beta = "networking.k8s.io/v1beta1"
	meta := metaClient(
		servedAt("ServiceCIDR", []string{beta},
			obj{name: "kubernetes", managed: []metav1.ManagedFieldsEntry{wrote("kube-apiserver", beta)}}),
		servedAt("IPAddress", []string{beta},
			obj{name: "10.96.0.1", managed: []metav1.ManagedFieldsEntry{wrote("kube-apiserver", beta)}}),
	)
	disc := fakeDiscovery(
		resources("networking.k8s.io/v1", ingresses),
		resources(beta,
			metav1.APIResource{Name: "servicecidrs", Kind: "ServiceCIDR", Verbs: metav1.Verbs{"list"}},
			metav1.APIResource{Name: "ipaddresses", Kind: "IPAddress", Verbs: metav1.Verbs{"list"}},
		),
	)
	k := loadKB(t)

	var inv inventory.Inventory
	if err := collectAPIUsage(context.Background(), disc, meta, k.APILifecycle, &inv); err != nil {
		t.Fatal(err)
	}
	if len(inv.APIUsage) != 0 {
		t.Errorf("api usage = %#v, want none: only the apiserver wrote these objects", inv.APIUsage)
	}
	rep := evaluateAt(inv, k, "v1.32.5", inventory.Version{Major: 1, Minor: 37})
	if fs := removedAPIFindings(rep); len(fs) != 0 {
		t.Errorf("1.32 → 1.37: removed-api findings %+v (score %d, ready=%v), want none", fs, rep.Score, rep.Ready)
	}
}

// A 1.29 cluster serves ValidatingAdmissionPolicy at v1alpha1 and v1beta1
// (v1 arrives in 1.30), and the policy was written through v1beta1. Both
// served versions are flagged and neither has a KB replacement, but the
// kind continues at v1: the policy must not count under v1alpha1, which
// nobody wrote it through, and so is no v1alpha1 removed-api blocker at 1.32.
func TestCollectAPIUsageVAPWrittenViaBetaIsNotAlphaUsage(t *testing.T) {
	const alpha, beta = "admissionregistration.k8s.io/v1alpha1", "admissionregistration.k8s.io/v1beta1"
	meta := metaClient(servedAt("ValidatingAdmissionPolicy", []string{alpha, beta},
		obj{name: "require-labels",
			managed:     []metav1.ManagedFieldsEntry{wrote("kubectl-client-side-apply", beta)},
			annotations: lastApplied(beta, "ValidatingAdmissionPolicy")},
	))
	vap := func(gv string) *metav1.APIResourceList {
		return resources(gv,
			metav1.APIResource{Name: "validatingadmissionpolicies", Kind: "ValidatingAdmissionPolicy", Verbs: metav1.Verbs{"list"}},
			metav1.APIResource{Name: "validatingadmissionpolicies/status", Kind: "ValidatingAdmissionPolicy", Verbs: metav1.Verbs{"get"}},
			metav1.APIResource{Name: "validatingadmissionpolicybindings", Kind: "ValidatingAdmissionPolicyBinding", Verbs: metav1.Verbs{"list"}},
		)
	}
	disc := fakeDiscovery(
		resources("admissionregistration.k8s.io/v1",
			metav1.APIResource{Name: "validatingwebhookconfigurations", Kind: "ValidatingWebhookConfiguration", Verbs: metav1.Verbs{"list"}}),
		vap(beta),
		vap(alpha),
	)
	k := loadKB(t)

	var inv inventory.Inventory
	if err := collectAPIUsage(context.Background(), disc, meta, k.APILifecycle, &inv); err != nil {
		t.Fatal(err)
	}
	want := []inventory.APIUsage{{
		Group: "admissionregistration.k8s.io", Version: "v1beta1", Kind: "ValidatingAdmissionPolicy", Count: 1,
		Namespaces: map[string]int{"": 1},
		Objects:    []inventory.ObjectRef{{Name: "require-labels", Manager: "kubectl-client-side-apply"}},
	}}
	if !reflect.DeepEqual(inv.APIUsage, want) {
		t.Errorf("api usage = %#v\nwant       %#v", inv.APIUsage, want)
	}
	rep := evaluateAt(inv, k, "v1.29.10", inventory.Version{Major: 1, Minor: 32})
	if fs := removedAPIFindings(rep); len(fs) != 0 {
		t.Errorf("1.29 → 1.32: removed-api findings %+v (score %d, ready=%v), want none", fs, rep.Score, rep.Ready)
	}
}

// policy/v1beta1 PodSecurityPolicy has no surviving version in the KB:
// the type goes away in 1.25, so every stored PSP blocks, however (or by
// whom) it was written.
func TestCollectAPIUsageRealKBPodSecurityPolicyCountsEveryObject(t *testing.T) {
	meta := metaClient(servedAt("PodSecurityPolicy", []string{"policy/v1beta1"},
		obj{name: "restricted", managed: []metav1.ManagedFieldsEntry{wrote("kubectl-client-side-apply", "policy/v1beta1")}},
		obj{name: "privileged"},
		obj{name: "from-controller", managed: []metav1.ManagedFieldsEntry{wrote("kube-controller-manager", "policy/v1beta1")}},
	))
	disc := fakeDiscovery(
		resources("policy/v1", pdbs),
		resources("policy/v1beta1", pdbs, psps),
	)
	k := loadKB(t)

	var inv inventory.Inventory
	if err := collectAPIUsage(context.Background(), disc, meta, k.APILifecycle, &inv); err != nil {
		t.Fatal(err)
	}
	if len(inv.APIUsage) != 1 || inv.APIUsage[0].Kind != "PodSecurityPolicy" || inv.APIUsage[0].Count != 3 {
		t.Fatalf("api usage = %#v, want all 3 PodSecurityPolicies", inv.APIUsage)
	}
	rep := evaluateAt(inv, k, "v1.24.17", inventory.Version{Major: 1, Minor: 25})
	if fs := removedAPIFindings(rep); len(fs) != 1 || fs[0].Severity != engine.SevBlocker {
		t.Errorf("1.24 → 1.25: removed-api findings %+v, want one PodSecurityPolicy blocker", fs)
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
	if !pe.incomplete || !reflect.DeepEqual(pe.skipped, []string{"policy/v1beta1 PodSecurityPolicy"}) {
		t.Errorf("partial = %v, skipped = %q; want incomplete, skipping the flagged API the failed list covered", pe.incomplete, pe.skipped)
	}
	want := []inventory.APIUsage{{
		Group: "networking.k8s.io", Version: "v1beta1", Kind: "Ingress", Count: 1, Namespaces: map[string]int{"default": 1},
		Objects: []inventory.ObjectRef{{Namespace: "default", Name: "web", Manager: "helm"}},
	}}
	if !reflect.DeepEqual(inv.APIUsage, want) {
		t.Errorf("api usage = %#v\nwant     %#v (successes must be kept)", inv.APIUsage, want)
	}
}

// M08/VS-07: a 403 on the ingresses LIST hid a v1beta1-authored Ingress
// and the report read ready. The capability must come back partial, naming
// every flagged API that LIST covered, so the engine can tell the gap
// matters.
func TestCollectAPIUsageForbiddenIngressesIsPartialNamingIngressAPIs(t *testing.T) {
	meta := flaggedObjects()
	meta.PrependReactor("list", "ingresses", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(schema.GroupResource{Group: "networking.k8s.io", Resource: "ingresses"}, "", errors.New("RBAC denied"))
	})

	var inv inventory.Inventory
	err := collectAPIUsage(context.Background(), flaggedDiscovery(), meta, flaggedLifecycle(), &inv)

	var pe partialError
	if !errors.As(err, &pe) || !pe.incomplete {
		t.Fatalf("err = %#v, want an incomplete partialError", err)
	}
	if !strings.Contains(pe.msg, "list networking.k8s.io/v1 ingresses: ") {
		t.Errorf("reason = %q, must name the failed list", pe.msg)
	}
	if want := []string{"networking.k8s.io/v1beta1 Ingress"}; !reflect.DeepEqual(pe.skipped, want) {
		t.Errorf("skipped = %q, want %q", pe.skipped, want)
	}
	if len(inv.APIUsage) != 1 || inv.APIUsage[0].Kind != "PodSecurityPolicy" {
		t.Errorf("api usage = %#v, want the PodSecurityPolicy still counted", inv.APIUsage)
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
	// The group holds no flagged API: incomplete, but nothing it skipped
	// is one the KB tracks.
	if !pe.incomplete || len(pe.skipped) != 0 {
		t.Errorf("partial = %v, skipped = %q; want incomplete, nothing flagged skipped", pe.incomplete, pe.skipped)
	}
	if len(inv.APIUsage) != 2 {
		t.Errorf("api usage = %#v, want both served resources still counted", inv.APIUsage)
	}
}

// M08/VS-08: a forbidden PodSecurityPolicy LIST on 1.24 turned blocked/75
// into ready/100 with no gap. End to end, the partial capability is now a
// required gap at 1.25 (PSP is removed there), and the verdict unknown.
func TestForbiddenPodSecurityPolicyListIsARequiredGapAtRemoval(t *testing.T) {
	meta := metaClient(servedAt("PodSecurityPolicy", []string{"policy/v1beta1"}, obj{name: "privileged"}))
	meta.PrependReactor("list", "podsecuritypolicies", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(schema.GroupResource{Group: "policy", Resource: "podsecuritypolicies"}, "", errors.New("RBAC denied"))
	})
	disc := fakeDiscovery(resources("policy/v1", pdbs), resources("policy/v1beta1", pdbs, psps))
	k := loadKB(t)

	inv := inventory.Inventory{Source: inventory.SourceCluster, ServerVersion: "v1.24.17",
		Capabilities: map[inventory.Capability]inventory.CapabilityStatus{inventory.CapVersions: {Available: true}}}
	runSteps(context.Background(), &inv, []step{{cap: inventory.CapAPIUsage, run: func(ctx context.Context, inv *inventory.Inventory) error {
		return collectAPIUsage(ctx, disc, meta, k.APILifecycle, inv)
	}}})
	k.AddOns = nil // only api-usage is under test

	rep := engine.Evaluate(inv, k, inventory.Version{Major: 1, Minor: 25}, time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC))
	want := []engine.CapabilityGap{{Capability: inventory.CapAPIUsage, Partial: true, Required: true,
		Reason:  inv.Capabilities[inventory.CapAPIUsage].Reason,
		Skipped: []string{"policy/v1beta1 PodSecurityPolicy"}}}
	if rep.Verdict != engine.VerdictUnknown || !reflect.DeepEqual(rep.NotAssessed, want) {
		t.Errorf("verdict %s, notAssessed %+v\nwant unknown, %+v", rep.Verdict, rep.NotAssessed, want)
	}
}

// A group/version whose discovery failed may serve a flagged API: every
// flagged API the KB records at that group/version went unchecked.
func TestCollectAPIUsageDiscoveryFailureSkipsFlaggedAPIsOfThatGroupVersion(t *testing.T) {
	disc := failingGroupDiscovery{fakeDiscovery(resources("networking.k8s.io/v1", ingresses)),
		schema.GroupVersion{Group: "policy", Version: "v1beta1"}}

	var inv inventory.Inventory
	err := collectAPIUsage(context.Background(), disc, flaggedObjects(), flaggedLifecycle(), &inv)

	var pe partialError
	if !errors.As(err, &pe) || !pe.incomplete {
		t.Fatalf("err = %#v, want an incomplete partialError", err)
	}
	if want := []string{"policy/v1beta1 PodSecurityPolicy"}; !reflect.DeepEqual(pe.skipped, want) {
		t.Errorf("skipped = %q, want %q", pe.skipped, want)
	}
}

// failingGroupDiscovery reports one group/version's discovery as failed.
type failingGroupDiscovery struct {
	*discoveryfake.FakeDiscovery
	failed schema.GroupVersion
}

func (p failingGroupDiscovery) ServerGroupsAndResources() ([]*metav1.APIGroup, []*metav1.APIResourceList, error) {
	groups, lists, _ := p.FakeDiscovery.ServerGroupsAndResources()
	return groups, lists, &discovery.ErrGroupDiscoveryFailed{Groups: map[schema.GroupVersion]error{
		p.failed: errors.New("the server is currently unable to handle the request"),
	}}
}
