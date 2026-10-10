package collect

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apiextensionsfake "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset/fake"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/version"
	discoveryfake "k8s.io/client-go/discovery/fake"
	kubefake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/abd-ulbasit/upgradescope/internal/engine"
	"github.com/abd-ulbasit/upgradescope/internal/inventory"
)

// Issue #122 (M08/VS-06, VS-07; M10/RB-07): a role narrower than the
// chart's (rbac.create=false with a hand-written role) must degrade
// visibly. A cluster running EOL ingress-nginx with an Ingress written via
// a removed API is blocked under the full role; with pods, Secrets and
// ingresses denied the scan cannot see either, and must say unknown,
// naming every gap, never ready.
func TestCollectNarrowRoleIsNeverReady(t *testing.T) {
	forbidden := func(resource string) k8stesting.ReactionFunc {
		return func(k8stesting.Action) (bool, runtime.Object, error) {
			return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: resource}, "", errors.New("RBAC"))
		}
	}
	const server = "v1.33.4"
	clients := func(narrow bool) Clients {
		cs := kubefake.NewClientset(
			&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "kube-system", UID: types.UID("uid-narrow")}},
			&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "n"}, Status: corev1.NodeStatus{NodeInfo: corev1.NodeSystemInfo{KubeletVersion: server}}},
			&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "ingress-nginx", Name: "controller"},
				Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "c", Image: "registry.k8s.io/ingress-nginx/controller:v1.8.1"}}}},
		)
		disc := cs.Discovery().(*discoveryfake.FakeDiscovery)
		disc.FakedServerVersion = &version.Info{GitVersion: server}
		disc.Resources = []*metav1.APIResourceList{
			resources("v1", metav1.APIResource{Name: "pods", Kind: "Pod", Namespaced: true, Verbs: metav1.Verbs{"list"}}),
			resources("networking.k8s.io/v1", ingresses),
			resources("networking.k8s.io/v1beta1", ingresses),
			resources("policy/v1", pdbs),
			resources("policy/v1beta1", pdbs),
		}
		meta := metaClient(servedAt("Ingress", []string{"networking.k8s.io/v1", "networking.k8s.io/v1beta1"},
			obj{namespace: "shop", name: "web", managed: []metav1.ManagedFieldsEntry{wrote("helm", "networking.k8s.io/v1beta1")}}))
		if narrow {
			cs.PrependReactor("list", "pods", func(a k8stesting.Action) (bool, runtime.Object, error) {
				if a.GetNamespace() != "" { // kube-system pods stay readable, as in the audit
					return false, nil, nil
				}
				return forbidden("pods")(a)
			})
			meta.PrependReactor("list", "secrets", forbidden("secrets"))
			meta.PrependReactor("list", "ingresses", forbidden("ingresses"))
		}
		return Clients{Kube: cs, Metadata: meta, Discovery: disc, RESTClient: metricsRESTClient(t, ""), APIExtensions: apiextensionsfake.NewClientset()}
	}
	k := loadKB(t)
	target := inventory.Version{Major: 1, Minor: 34}
	now := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)

	full := engine.Evaluate(Collect(context.Background(), clients(false), k, Options{}), k, target, now)
	if full.Verdict != engine.VerdictBlocked {
		t.Fatalf("full role: verdict %s (findings %+v, gaps %+v), want blocked", full.Verdict, full.Findings, full.NotAssessed)
	}

	rep := engine.Evaluate(Collect(context.Background(), clients(true), k, Options{}), k, target, now)
	if rep.Verdict != engine.VerdictUnknown {
		t.Errorf("narrow role: verdict %s, want unknown (findings %+v)", rep.Verdict, rep.Findings)
	}
	type gap struct {
		capability        inventory.Capability
		partial, required bool
	}
	var got []gap
	for _, g := range rep.NotAssessed {
		got = append(got, gap{g.Capability, g.Partial, g.Required})
	}
	want := []gap{
		{inventory.CapAddOns, true, true},    // pods denied: add-ons judged from Helm releases and IngressClasses only
		{inventory.CapAPIUsage, true, true},  // ingresses denied: a removed API went unchecked
		{inventory.CapHelm, false, false},    // Secrets denied
		{inventory.CapVolumes, false, false}, // pods denied: in-tree volume plugins not checked (optional)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("narrow role gaps = %+v\nwant                %+v\n(%+v)", got, want, rep.NotAssessed)
	}
}
