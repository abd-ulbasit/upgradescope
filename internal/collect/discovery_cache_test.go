package collect

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apiextensionsfake "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset/fake"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/version"
	discoveryfake "k8s.io/client-go/discovery/fake"
	kubefake "k8s.io/client-go/kubernetes/fake"
	metadatafake "k8s.io/client-go/metadata/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/abd-ulbasit/upgradescope/internal/inventory"
	"github.com/abd-ulbasit/upgradescope/internal/kb"
)

// discoveryTicks is a cluster for Collect with a DiscoveryCache: its
// discovery serves argoproj.io (so GitOps detection asks for groups and
// for that group's resources, as api-usage asks for everything), and the
// returned func counts the group and resource discovery requests made
// since it was last called.
func discoveryTicks(t *testing.T) (Clients, *discoveryfake.FakeDiscovery, *apiextensionsfake.Clientset, func() int) {
	t.Helper()
	cs := kubefake.NewClientset()
	disc := cs.Discovery().(*discoveryfake.FakeDiscovery)
	disc.FakedServerVersion = &version.Info{GitVersion: "v1.35.2"}
	disc.Resources = []*metav1.APIResourceList{
		{GroupVersion: "v1", APIResources: []metav1.APIResource{{Name: "pods", Kind: "Pod", Namespaced: true, Verbs: metav1.Verbs{"list"}}}},
		{GroupVersion: "argoproj.io/v1alpha1", APIResources: []metav1.APIResource{{Name: "applications", Kind: "Application", Namespaced: true, Verbs: metav1.Verbs{"list"}}}},
	}
	scheme := runtime.NewScheme()
	utilruntime.Must(metav1.AddMetaToScheme(scheme))
	ext := apiextensionsfake.NewClientset()
	c := Clients{Kube: cs, Discovery: disc, Metadata: metadatafake.NewSimpleMetadataClient(scheme), APIExtensions: ext}
	asked := func() int {
		n := 0
		for _, a := range disc.Actions() {
			if r := a.GetResource().Resource; r == "group" || r == "resource" {
				n++
			}
		}
		disc.ClearActions()
		return n
	}
	return c, disc, ext, asked
}

func collectWith(t *testing.T, c Clients, dc *DiscoveryCache) inventory.Inventory {
	t.Helper()
	k, err := kb.Load()
	if err != nil {
		t.Fatal(err)
	}
	return Collect(context.Background(), c, k, Options{DiscoveryCache: dc})
}

// A steady tick reads discovery from the cache, and the inventory is the
// one discovery read live gives (#228).
func TestDiscoveryCacheServesSteadyTicks(t *testing.T) {
	c, _, _, asked := discoveryTicks(t)
	live := collectWith(t, c, nil)
	if asked() == 0 {
		t.Fatal("the fixture asks for no discovery")
	}
	dc := NewDiscoveryCache()
	first := collectWith(t, c, dc)
	if asked() == 0 {
		t.Fatal("the first tick asked for no discovery")
	}
	for tick := 2; tick <= 3; tick++ {
		inv := collectWith(t, c, dc)
		if n := asked(); n != 0 {
			t.Errorf("tick %d asked for groups or resources %d times, want 0", tick, n)
		}
		for _, cap := range []inventory.Capability{inventory.CapAPIUsage, inventory.CapHelm} {
			if !reflect.DeepEqual(inv.Capabilities[cap], live.Capabilities[cap]) || !reflect.DeepEqual(first.Capabilities[cap], live.Capabilities[cap]) {
				t.Errorf("tick %d: %s = %+v, live discovery gives %+v", tick, cap, inv.Capabilities[cap], live.Capabilities[cap])
			}
		}
	}
}

// What drops the cache: an apiserver of another version (seen before any
// step reads discovery, in the same tick), a CRD change (the tick after),
// a CRD list that fails (the tick after), and age.
func TestDiscoveryCacheStaleness(t *testing.T) {
	crd := &apiextensionsv1.CustomResourceDefinition{
		ObjectMeta: metav1.ObjectMeta{Name: "widgets.example.com"},
		Spec: apiextensionsv1.CustomResourceDefinitionSpec{Group: "example.com", Names: apiextensionsv1.CustomResourceDefinitionNames{Plural: "widgets", Kind: "Widget"},
			Versions: []apiextensionsv1.CustomResourceDefinitionVersion{{Name: "v1", Served: true, Storage: true}}},
	}
	now := time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name   string
		change func(*testing.T, *discoveryfake.FakeDiscovery, *apiextensionsfake.Clientset)
		// asked on the tick of the change and on the one after it
		thisTick, nextTick bool
	}{
		{"nothing changes", func(*testing.T, *discoveryfake.FakeDiscovery, *apiextensionsfake.Clientset) {}, false, false},
		{"the apiserver is upgraded", func(_ *testing.T, d *discoveryfake.FakeDiscovery, _ *apiextensionsfake.Clientset) {
			d.FakedServerVersion = &version.Info{GitVersion: "v1.36.0"}
		}, true, false},
		{"a CRD is installed", func(t *testing.T, _ *discoveryfake.FakeDiscovery, ext *apiextensionsfake.Clientset) {
			if _, err := ext.ApiextensionsV1().CustomResourceDefinitions().Create(context.Background(), crd, metav1.CreateOptions{}); err != nil {
				t.Fatal(err)
			}
		}, false, true},
		{"the CRDs cannot be listed", func(_ *testing.T, _ *discoveryfake.FakeDiscovery, ext *apiextensionsfake.Clientset) {
			ext.PrependReactor("list", "customresourcedefinitions", func(k8stesting.Action) (bool, runtime.Object, error) {
				return true, nil, errors.New("forbidden")
			})
		}, false, true},
		{"an hour passes", func(*testing.T, *discoveryfake.FakeDiscovery, *apiextensionsfake.Clientset) {
			now = now.Add(DiscoveryMaxAge)
		}, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, disc, ext, asked := discoveryTicks(t)
			dc := NewDiscoveryCache()
			dc.now = func() time.Time { return now }
			collectWith(t, c, dc)
			collectWith(t, c, dc) // steady: the CRDs of the tick it was filled in are known
			if n := asked(); n == 0 {
				t.Fatal("the first tick asked for no discovery")
			}
			tc.change(t, disc, ext)
			collectWith(t, c, dc)
			if got := asked() > 0; got != tc.thisTick {
				t.Errorf("the tick of the change asked for discovery: %v, want %v", got, tc.thisTick)
			}
			collectWith(t, c, dc)
			if got := asked() > 0; got != tc.nextTick {
				t.Errorf("the tick after the change asked for discovery: %v, want %v", got, tc.nextTick)
			}
		})
	}
}

// An answer with an error, even a partial one, is not kept.
func TestDiscoveryCacheKeepsNoFailure(t *testing.T) {
	c, disc, _, asked := discoveryTicks(t)
	disc.PrependReactor("get", "resource", func(a k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("aggregated API down")
	})
	dc := NewDiscoveryCache()
	for tick := 1; tick <= 2; tick++ {
		inv := collectWith(t, c, dc)
		if asked() == 0 {
			t.Errorf("tick %d asked for no discovery: a failed answer was kept", tick)
		}
		if st := inv.Capabilities[inventory.CapAPIUsage]; st.Available && !st.Partial {
			t.Errorf("tick %d: api-usage %+v, want the discovery failure reported", tick, st)
		}
	}
}
