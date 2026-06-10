package collect

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

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

	"github.com/abd-ulbasit/upgradescope/internal/inventory"
	"github.com/abd-ulbasit/upgradescope/internal/kb"
)

func pom(apiVersion, kind, namespace, name string) *metav1.PartialObjectMetadata {
	return &metav1.PartialObjectMetadata{
		TypeMeta:   metav1.TypeMeta{APIVersion: apiVersion, Kind: kind},
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name},
	}
}

func ver(major, minor int) *inventory.Version {
	return &inventory.Version{Major: major, Minor: minor}
}

func TestCollectAPIUsage(t *testing.T) {
	scheme := runtime.NewScheme()
	utilruntime.Must(metav1.AddMetaToScheme(scheme))
	metaClient := metadatafake.NewSimpleMetadataClient(scheme,
		pom("networking.k8s.io/v1beta1", "Ingress", "default", "web"),
		pom("networking.k8s.io/v1beta1", "Ingress", "default", "api"),
		pom("networking.k8s.io/v1beta1", "Ingress", "prod", "shop"),
		pom("policy/v1beta1", "PodSecurityPolicy", "", "restricted"),
	)

	cs := kubefake.NewClientset()
	disc := cs.Discovery().(*discoveryfake.FakeDiscovery)
	disc.Resources = []*metav1.APIResourceList{
		{GroupVersion: "networking.k8s.io/v1beta1", APIResources: []metav1.APIResource{
			{Name: "ingresses", Kind: "Ingress", Namespaced: true, Verbs: metav1.Verbs{"get", "list"}},
			{Name: "ingresses/status", Kind: "Ingress", Namespaced: true, Verbs: metav1.Verbs{"update"}}, // subresource: skipped
		}},
		{GroupVersion: "policy/v1beta1", APIResources: []metav1.APIResource{
			{Name: "podsecuritypolicies", Kind: "PodSecurityPolicy", Namespaced: false, Verbs: metav1.Verbs{"list"}},
		}},
		{GroupVersion: "apps/v1", APIResources: []metav1.APIResource{
			{Name: "deployments", Kind: "Deployment", Namespaced: true, Verbs: metav1.Verbs{"list"}},
		}},
	}

	lifecycle := []kb.APILifecycleEntry{
		{Group: "networking.k8s.io", Version: "v1beta1", Kind: "Ingress", Introduced: inventory.Version{Major: 1, Minor: 14}, Deprecated: ver(1, 19), Removed: ver(1, 22)},
		{Group: "policy", Version: "v1beta1", Kind: "PodSecurityPolicy", Introduced: inventory.Version{Major: 1, Minor: 10}, Deprecated: ver(1, 21), Removed: ver(1, 25)},
		{Group: "extensions", Version: "v1beta1", Kind: "Ingress", Introduced: inventory.Version{Major: 1, Minor: 1}, Deprecated: ver(1, 14), Removed: ver(1, 22)}, // not served: skipped
		{Group: "apps", Version: "v1", Kind: "Deployment", Introduced: inventory.Version{Major: 1, Minor: 9}},                                                      // served but not deprecated: skipped
	}

	var inv inventory.Inventory
	if err := collectAPIUsage(context.Background(), disc, metaClient, lifecycle, &inv); err != nil {
		t.Fatal(err)
	}
	want := []inventory.APIUsage{
		{Group: "networking.k8s.io", Version: "v1beta1", Kind: "Ingress", Count: 3, Namespaces: map[string]int{"default": 2, "prod": 1}},
		{Group: "policy", Version: "v1beta1", Kind: "PodSecurityPolicy", Count: 1, Namespaces: map[string]int{"": 1}},
	}
	if !reflect.DeepEqual(inv.APIUsage, want) {
		t.Errorf("api usage = %#v\nwant     %#v", inv.APIUsage, want)
	}
}

// flaggedLifecycle: both Ingress and PodSecurityPolicy deprecated/removed.
func flaggedLifecycle() []kb.APILifecycleEntry {
	return []kb.APILifecycleEntry{
		{Group: "networking.k8s.io", Version: "v1beta1", Kind: "Ingress", Introduced: inventory.Version{Major: 1, Minor: 14}, Deprecated: ver(1, 19), Removed: ver(1, 22)},
		{Group: "policy", Version: "v1beta1", Kind: "PodSecurityPolicy", Introduced: inventory.Version{Major: 1, Minor: 10}, Deprecated: ver(1, 21), Removed: ver(1, 25)},
	}
}

func flaggedDiscovery() *discoveryfake.FakeDiscovery {
	cs := kubefake.NewClientset()
	disc := cs.Discovery().(*discoveryfake.FakeDiscovery)
	disc.Resources = []*metav1.APIResourceList{
		{GroupVersion: "networking.k8s.io/v1beta1", APIResources: []metav1.APIResource{
			{Name: "ingresses", Kind: "Ingress", Namespaced: true, Verbs: metav1.Verbs{"get", "list"}},
		}},
		{GroupVersion: "policy/v1beta1", APIResources: []metav1.APIResource{
			{Name: "podsecuritypolicies", Kind: "PodSecurityPolicy", Namespaced: false, Verbs: metav1.Verbs{"list"}},
		}},
	}
	return disc
}

func TestCollectAPIUsagePartialFailureKeepsSuccesses(t *testing.T) {
	scheme := runtime.NewScheme()
	utilruntime.Must(metav1.AddMetaToScheme(scheme))
	metaClient := metadatafake.NewSimpleMetadataClient(scheme,
		pom("networking.k8s.io/v1beta1", "Ingress", "default", "web"),
		pom("policy/v1beta1", "PodSecurityPolicy", "", "restricted"),
	)
	metaClient.PrependReactor("list", "podsecuritypolicies", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: "podsecuritypolicies"}, "", errors.New("RBAC denied"))
	})

	var inv inventory.Inventory
	err := collectAPIUsage(context.Background(), flaggedDiscovery(), metaClient, flaggedLifecycle(), &inv)

	var pe partialError
	if !errors.As(err, &pe) {
		t.Fatalf("err = %v, want partialError (capability stays available)", err)
	}
	if !strings.Contains(err.Error(), "podsecuritypolicies") {
		t.Errorf("reason = %q, must name the failed resource", err.Error())
	}
	want := []inventory.APIUsage{
		{Group: "networking.k8s.io", Version: "v1beta1", Kind: "Ingress", Count: 1, Namespaces: map[string]int{"default": 1}},
	}
	if !reflect.DeepEqual(inv.APIUsage, want) {
		t.Errorf("api usage = %#v\nwant     %#v (successes must be kept)", inv.APIUsage, want)
	}
}

func TestCollectAPIUsageAllResourcesFailedDegrades(t *testing.T) {
	scheme := runtime.NewScheme()
	utilruntime.Must(metav1.AddMetaToScheme(scheme))
	metaClient := metadatafake.NewSimpleMetadataClient(scheme)
	for _, res := range []string{"ingresses", "podsecuritypolicies"} {
		metaClient.PrependReactor("list", res, func(k8stesting.Action) (bool, runtime.Object, error) {
			return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: res}, "", errors.New("RBAC denied"))
		})
	}

	var inv inventory.Inventory
	err := collectAPIUsage(context.Background(), flaggedDiscovery(), metaClient, flaggedLifecycle(), &inv)
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
	scheme := runtime.NewScheme()
	utilruntime.Must(metav1.AddMetaToScheme(scheme))
	metaClient := metadatafake.NewSimpleMetadataClient(scheme,
		pom("networking.k8s.io/v1beta1", "Ingress", "default", "web"),
		pom("policy/v1beta1", "PodSecurityPolicy", "", "restricted"),
	)

	var inv inventory.Inventory
	err := collectAPIUsage(context.Background(), partialDiscovery{flaggedDiscovery()}, metaClient, flaggedLifecycle(), &inv)

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
