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

	"github.com/abd-ulbasit/upgradescope/internal/crd"
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

	// #374: a role without the PersistentVolume and StorageClass reads
	// (#362) hides an rbd PV, which blocks 1.31. Lists allowed, the scan
	// says blocked; refused, unknown, with volumes partial and required,
	// and the agent's Ready condition is not True.
	t.Run("persistentvolumes and storageclasses refused", func(t *testing.T) {
		objs := []runtime.Object{testPV("pv-rbd", "data", rbdSource())}
		full := evaluateRBDCluster(t, objs, 0, nil)
		if full.Verdict != engine.VerdictBlocked || !hasBlocker(full, "volume-plugin/rbd") {
			t.Fatalf("lists allowed: verdict %s (findings %+v, gaps %+v), want blocked by volume-plugin/rbd", full.Verdict, full.Findings, full.NotAssessed)
		}
		rep := evaluateRBDCluster(t, objs, 0, func(r string, _ metav1.ListOptions) error { return volumeListForbidden(r) })
		wantVolumesRequired(t, rep, []string{inventory.SkippedStorageClasses, inventory.SkippedPersistentVolumes})
		if rep.Ready || rep.Verdict != engine.VerdictUnknown {
			t.Errorf("lists refused: verdict %s, ready %v, want unknown (findings %+v, gaps %+v)", rep.Verdict, rep.Ready, rep.Findings, rep.NotAssessed)
		}
		st := crd.StatusFromReports([]engine.Report{rep}, rbdServer, "test", time.Now())
		if c := crd.ReadyCondition(st); c.Status != metav1.ConditionUnknown {
			t.Errorf("lists refused: Ready condition = %s %q, want Unknown", c.Status, c.Message)
		}
	})
}

// rbdServer is the kube-apiserver and kubelet version of
// evaluateRBDCluster: one minor before rbd's removal.
const rbdServer = "v1.30.4"

// evaluateRBDCluster collects a 1.30 cluster with every capability's
// clients, a PersistentVolume and StorageClass list serving objs perPage a
// page (0: one page), each list call answered by fail first when it is not
// nil, and evaluates it at 1.31.
func evaluateRBDCluster(t *testing.T, objs []runtime.Object, perPage int, fail func(resource string, o metav1.ListOptions) error) engine.Report {
	t.Helper()
	cs := kubefake.NewClientset(
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "kube-system", UID: types.UID("uid-rbd")}},
		&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "n"}, Status: corev1.NodeStatus{NodeInfo: corev1.NodeSystemInfo{KubeletVersion: rbdServer}}},
	)
	disc := cs.Discovery().(*discoveryfake.FakeDiscovery)
	disc.FakedServerVersion = &version.Info{GitVersion: rbdServer}
	disc.Resources = []*metav1.APIResourceList{
		resources("v1", metav1.APIResource{Name: "pods", Kind: "Pod", Namespaced: true, Verbs: metav1.Verbs{"list"}}),
	}
	srv := serveVolumeLists(cs, perPage, objs...)
	srv.fail = fail
	k := loadKB(t)
	clients := Clients{Kube: cs, Metadata: metaClient(), Discovery: disc, RESTClient: metricsRESTClient(t, ""), APIExtensions: apiextensionsfake.NewClientset()}
	return engine.Evaluate(Collect(context.Background(), clients, k, Options{}), k, inventory.Version{Major: 1, Minor: 31},
		time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC))
}

// volumeListForbidden is the 403 of a PersistentVolume or StorageClass
// list.
func volumeListForbidden(resource string) error {
	g := ""
	if resource == "storageclasses" {
		g = "storage.k8s.io"
	}
	return apierrors.NewForbidden(schema.GroupResource{Group: g, Resource: resource}, "", errors.New("RBAC"))
}

func hasBlocker(r engine.Report, id string) bool {
	for _, f := range r.Findings {
		if f.Key == id && f.Severity == engine.SevBlocker {
			return true
		}
	}
	return false
}

// wantVolumesRequired fails unless r's volumes gap is partial and
// required, skipping skipped.
func wantVolumesRequired(t *testing.T, r engine.Report, skipped []string) {
	t.Helper()
	for _, g := range r.NotAssessed {
		if g.Capability == inventory.CapVolumes {
			if !g.Partial || !g.Required || !reflect.DeepEqual(g.Skipped, skipped) {
				t.Errorf("volumes gap = %+v, want partial and required, skipping %q", g, skipped)
			}
			return
		}
	}
	t.Errorf("no volumes gap in %+v, want one partial and required", r.NotAssessed)
}

// #374: a PersistentVolume list that fails, transiently and not for want
// of a rule, hides the rbd PV the same way: on its first page, or on a
// later one (the PVs listed before the failure are read).
func TestCollectLiveVolumesListFailedIsRequired(t *testing.T) {
	timeout := errors.New("etcdserver: request timed out")
	csi := testPV("pv-csi", "shop", corev1.PersistentVolumeSource{CSI: &corev1.CSIPersistentVolumeSource{Driver: "ebs.csi.aws.com", VolumeHandle: "vol-1"}})
	objs := []runtime.Object{csi, testPV("pv-rbd", "data", rbdSource())}
	for _, tc := range []struct {
		name string
		fail func(resource string, o metav1.ListOptions) error
	}{
		{"timeout on the first page", func(r string, _ metav1.ListOptions) error {
			if r == "persistentvolumes" {
				return timeout
			}
			return nil
		}},
		{"timeout on a later page", func(r string, o metav1.ListOptions) error {
			if r == "persistentvolumes" && o.Continue != "" {
				return timeout
			}
			return nil
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if full := evaluateRBDCluster(t, objs, 1, nil); full.Verdict != engine.VerdictBlocked {
				t.Fatalf("no failure: verdict %s, want blocked (findings %+v)", full.Verdict, full.Findings)
			}
			rep := evaluateRBDCluster(t, objs, 1, tc.fail)
			wantVolumesRequired(t, rep, []string{inventory.SkippedPersistentVolumes})
			if rep.Verdict != engine.VerdictUnknown {
				t.Errorf("verdict %s, want unknown (findings %+v, gaps %+v)", rep.Verdict, rep.Findings, rep.NotAssessed)
			}
		})
	}
}
