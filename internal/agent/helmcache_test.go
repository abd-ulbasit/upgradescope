package agent

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apiextensionsfake "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset/fake"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	discoveryfake "k8s.io/client-go/discovery/fake"
	kubefake "k8s.io/client-go/kubernetes/fake"
	metadatafake "k8s.io/client-go/metadata/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/abd-ulbasit/upgradescope/internal/collect"
)

// The agent keeps the Helm releases it decoded across ticks (#71): a tick
// fetches the Secrets that are new or changed, not one per release. This
// pins the wiring; the cache's own rules are tested in internal/collect.
func TestTicksFetchHelmReleasesOnlyWhenTheyChange(t *testing.T) {
	release := func(name string, rv string) *corev1.Secret {
		doc, _ := json.Marshal(map[string]any{
			"info":  map[string]any{"status": "deployed"},
			"chart": map[string]any{"metadata": map[string]any{"name": name, "version": "1.0.0"}},
		})
		var gz bytes.Buffer
		zw := gzip.NewWriter(&gz)
		zw.Write(doc)
		zw.Close()
		return &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name: "sh.helm.release.v1." + name + ".v1", Namespace: "apps", UID: types.UID("uid-" + name), ResourceVersion: rv,
				Labels: map[string]string{"owner": "helm", "name": name, "status": "deployed", "version": "1"},
			},
			Type: "helm.sh/release.v1",
			Data: map[string][]byte{"release": []byte(base64.StdEncoding.EncodeToString(gz.Bytes()))},
		}
	}
	secrets := []*corev1.Secret{release("web", "10"), release("db", "11")}
	var objs []runtime.Object
	var metas []runtime.Object
	for _, s := range secrets {
		objs = append(objs, s)
		metas = append(metas, &metav1.PartialObjectMetadata{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Secret"}, ObjectMeta: s.ObjectMeta})
	}
	scheme := runtime.NewScheme()
	utilruntime.Must(metav1.AddMetaToScheme(scheme))
	cs := kubefake.NewClientset(objs...)
	cs.Discovery().(*discoveryfake.FakeDiscovery).FakedServerVersion = fakeClients(t, "v1.35.2").Discovery.(*discoveryfake.FakeDiscovery).FakedServerVersion
	clients := collect.Clients{Kube: cs, Discovery: cs.Discovery(), Metadata: metadatafake.NewSimpleMetadataClient(scheme, metas...)}

	cfg := Config{}
	if err := cfg.applyDefaults(); err != nil {
		t.Fatal(err)
	}
	r := newRunner(clients, fakeDyn(), mustKB(t), cfg)

	secretGets := func() int {
		n := 0
		for _, a := range cs.Actions() {
			if g, ok := a.(k8stesting.GetAction); ok && g.GetResource().Resource == "secrets" {
				n++
			}
		}
		cs.ClearActions()
		return n
	}
	for tick, want := range []int{2, 0, 0} {
		r.runTick(context.Background())
		if got := secretGets(); got != want {
			t.Errorf("tick %d fetched %d Helm Secrets, want %d", tick+1, got, want)
		}
		if got := len(r.last.reports); got == 0 {
			t.Fatalf("tick %d evaluated nothing", tick+1)
		}
		if n := fmt.Sprint(r.last.caps["helm"].Available); n != "true" {
			t.Errorf("tick %d: helm capability %+v", tick+1, r.last.caps["helm"])
		}
	}
}

// The agent keeps API discovery across ticks (#228): a steady tick asks the
// apiserver for its version, never again for its groups and resources. This
// pins the wiring; the staleness rules are tested in internal/collect.
func TestTicksAskForDiscoveryOnce(t *testing.T) {
	clients := fakeClients(t, "v1.35.2")
	scheme := runtime.NewScheme()
	utilruntime.Must(metav1.AddMetaToScheme(scheme))
	clients.Metadata = metadatafake.NewSimpleMetadataClient(scheme)
	clients.APIExtensions = apiextensionsfake.NewClientset() // a CRD list read: the cache is kept
	disc := clients.Discovery.(*discoveryfake.FakeDiscovery)
	disc.Resources = []*metav1.APIResourceList{{GroupVersion: "v1", APIResources: []metav1.APIResource{{Name: "pods", Kind: "Pod", Namespaced: true, Verbs: metav1.Verbs{"list"}}}}}
	cfg := Config{}
	if err := cfg.applyDefaults(); err != nil {
		t.Fatal(err)
	}
	r := newRunner(clients, fakeDyn(), mustKB(t), cfg)
	asked := func() (groups, version int) {
		for _, a := range disc.Actions() {
			switch a.GetResource().Resource {
			case "group", "resource":
				groups++
			case "version":
				version++
			}
		}
		disc.ClearActions()
		return groups, version
	}
	for tick := range 3 {
		r.runTick(context.Background())
		groups, version := asked()
		if tick == 0 && groups == 0 || tick > 0 && groups != 0 {
			t.Errorf("tick %d asked for groups or resources %d times, want them asked on the first tick only", tick+1, groups)
		}
		if version == 0 {
			t.Errorf("tick %d did not ask for the server version: it is never cached", tick+1)
		}
	}
}
