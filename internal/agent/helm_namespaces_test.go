package agent

import (
	"context"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	metadatafake "k8s.io/client-go/metadata/fake"
	k8stesting "k8s.io/client-go/testing"
)

// Config.HelmNamespaces (--helm-namespaces, #344) reaches the collector: a
// tick lists Helm's storage in those namespaces only, never across the
// cluster, and reports the helm capability partial, saying so. This pins
// the wiring; the collector's rules are tested in internal/collect.
func TestTickReadsHelmReleasesOnlyInHelmNamespaces(t *testing.T) {
	clients := fakeClients(t, "v1.35.2")
	scheme := runtime.NewScheme()
	utilruntime.Must(metav1.AddMetaToScheme(scheme))
	meta := metadatafake.NewSimpleMetadataClient(scheme)
	clients.Metadata = meta
	cfg := Config{HelmNamespaces: []string{"ingress-nginx", "kube-system"}}
	if err := cfg.applyDefaults(); err != nil {
		t.Fatal(err)
	}
	r := newRunner(clients, fakeDyn(), mustKB(t), cfg)
	r.runTick(context.Background())

	listed := map[string]bool{}
	for _, a := range meta.Actions() {
		l, ok := a.(k8stesting.ListAction)
		if !ok {
			continue
		}
		res := l.GetResource().Resource
		if res != "secrets" && res != "configmaps" {
			continue
		}
		if l.GetNamespace() == "" {
			t.Errorf("cluster-wide list of %s, want only the Helm namespaces", res)
		}
		listed[res+" "+l.GetNamespace()] = true
	}
	for _, want := range []string{"secrets ingress-nginx", "secrets kube-system", "configmaps ingress-nginx", "configmaps kube-system"} {
		if !listed[want] {
			t.Errorf("no list of %s; lists = %v", want, listed)
		}
	}
	st := r.last.caps["helm"]
	if !st.Available || !st.Partial || !strings.Contains(st.Reason, "Helm releases read only in namespaces ingress-nginx, kube-system") {
		t.Errorf("helm capability = %+v, want available and partial, naming the namespaces read", st)
	}
}
