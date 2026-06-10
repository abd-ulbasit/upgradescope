package collect

import (
	"context"
	"reflect"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	discoveryfake "k8s.io/client-go/discovery/fake"
	kubefake "k8s.io/client-go/kubernetes/fake"
	metadatafake "k8s.io/client-go/metadata/fake"

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
		{Group: "apps", Version: "v1", Kind: "Deployment", Introduced: inventory.Version{Major: 1, Minor: 9}}, // served but not deprecated: skipped
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
