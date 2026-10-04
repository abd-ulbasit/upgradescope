package collect

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/version"
	discoveryfake "k8s.io/client-go/discovery/fake"
	kubefake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/abd-ulbasit/upgradescope/internal/inventory"
)

// #227: the versions capability lists the kube-system pods for the
// control-plane components and the add-ons capability lists every pod, so a
// kube-system pod was read twice a tick. The add-ons keep the images and
// labels of the kube-system pods versions read and list the other
// namespaces only.

// podServer is a fake API server's pod lists: it serves pods the way the
// real one does, by namespace and by the metadata.namespace field selector
// (which the fake tracker ignores), and records every request and every
// pod it sent.
type podServer struct {
	perPage int // pod objects per page; 0: all in one
	// failList, when set, fails a request after it has recorded it.
	failList func(call int, namespace string, o metav1.ListOptions) error

	calls  []podListCall
	served map[string]int // namespace/name → times sent
}

type podListCall struct {
	namespace, fieldSelector, cont string
}

func servePods(cs *kubefake.Clientset, pods ...*corev1.Pod) *podServer {
	s := &podServer{served: map[string]int{}}
	cs.PrependReactor("list", "pods", func(a k8stesting.Action) (bool, runtime.Object, error) {
		o := a.(interface{ GetListOptions() metav1.ListOptions }).GetListOptions()
		s.calls = append(s.calls, podListCall{a.GetNamespace(), o.FieldSelector, o.Continue})
		if s.failList != nil {
			if err := s.failList(len(s.calls), a.GetNamespace(), o); err != nil {
				return true, nil, err
			}
		}
		var match []corev1.Pod
		for _, p := range pods {
			if ns := a.GetNamespace(); ns != "" && p.Namespace != ns {
				continue
			}
			if o.FieldSelector == "metadata.namespace!=kube-system" && p.Namespace == "kube-system" {
				continue
			}
			match = append(match, *p)
		}
		start := 0
		if o.Continue != "" {
			start, _ = strconv.Atoi(o.Continue)
		}
		list := &corev1.PodList{}
		end := len(match)
		if s.perPage > 0 && start+s.perPage < end {
			end = start + s.perPage
			list.Continue = strconv.Itoa(end)
		}
		list.Items = match[start:end]
		for _, p := range list.Items {
			s.served[p.Namespace+"/"+p.Name]++
		}
		return true, list, nil
	})
	return s
}

func podFixture(objs ...runtime.Object) (*kubefake.Clientset, *discoveryfake.FakeDiscovery) {
	cs := kubefake.NewClientset(append([]runtime.Object{
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "kube-system", UID: types.UID("uid-1")}},
		testNode(),
	}, objs...)...)
	disc := cs.Discovery().(*discoveryfake.FakeDiscovery)
	disc.FakedServerVersion = &version.Info{GitVersion: "v1.34.2"}
	return cs, disc
}

func appPod(name, image string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "ingress-nginx"},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "c", Image: image}}},
	}
}

func addOnIDs(inv inventory.Inventory) []string {
	var ids []string
	for _, a := range inv.AddOns {
		ids = append(ids, a.ID)
	}
	slices.Sort(ids)
	return ids
}

func tickPods() []*corev1.Pod {
	return []*corev1.Pod{
		cpPod("kube-apiserver-cp", map[string]string{"component": "kube-apiserver"}, "registry.k8s.io/kube-apiserver:v1.34.2"),
		cpPodOn("node-a", "kube-proxy-a", map[string]string{"k8s-app": "kube-proxy"}, "registry.k8s.io/kube-proxy:v1.34.2"),
		cpPod("cilium-x", nil, "quay.io/cilium/cilium:v1.14.0"),
		appPod("nginx-1", "registry.k8s.io/ingress-nginx/controller:v1.9.4"),
	}
}

func wantTickControlPlane() []inventory.ComponentVersion {
	return []inventory.ComponentVersion{
		{Component: "kube-apiserver", Version: "v1.34.2"},
		{Component: "kube-proxy", Version: "v1.34.2", Node: "node-a"},
	}
}

func TestCollectReadsEachPodOncePerTick(t *testing.T) {
	for _, perPage := range []int{0, 1} { // one page, then a page per pod
		cs, disc := podFixture()
		srv := servePods(cs, tickPods()...)
		srv.perPage = perPage

		inv := Collect(context.Background(), Clients{Kube: cs, Discovery: disc}, loadKB(t), Options{})

		for pod, n := range srv.served {
			if n != 1 {
				t.Errorf("perPage %d: pod %s sent %d times in one tick, want once (requests %+v)", perPage, pod, n, srv.calls)
			}
		}
		if len(srv.served) != 4 {
			t.Errorf("perPage %d: %d pods sent, want all 4: %v", perPage, len(srv.served), srv.served)
		}
		for _, c := range srv.calls {
			if c.namespace == "" && c.fieldSelector != "metadata.namespace!=kube-system" {
				t.Errorf("perPage %d: the cluster-wide list %+v includes kube-system, whose pods versions already read", perPage, c)
			}
		}
		if st := inv.Capabilities[inventory.CapVersions]; !st.Available || st.Partial {
			t.Errorf("perPage %d: versions capability = %+v, want available and complete", perPage, st)
		}
		if !reflect.DeepEqual(inv.ControlPlane, wantTickControlPlane()) {
			t.Errorf("perPage %d: ControlPlane = %+v, want %+v", perPage, inv.ControlPlane, wantTickControlPlane())
		}
		if got := addOnIDs(inv); !reflect.DeepEqual(got, []string{"cilium", "ingress-nginx"}) {
			t.Errorf("perPage %d: add-ons = %v, want cilium (a kube-system pod) and ingress-nginx", perPage, got)
		}
		if st := inv.Capabilities[inventory.CapAddOns]; !st.Available || st.Partial {
			t.Errorf("perPage %d: addons capability = %+v, want available and complete", perPage, st)
		}
	}
}

// When versions could not read the kube-system pods, the add-ons list them
// with the rest: they are not read twice, but not at all by versions.
func TestCollectKubeSystemPodsForbiddenAddOnsListThemItself(t *testing.T) {
	cs, disc := podFixture()
	srv := servePods(cs, tickPods()...)
	srv.failList = func(_ int, ns string, _ metav1.ListOptions) error {
		if ns == "kube-system" {
			return apierrors.NewForbidden(schema.GroupResource{Resource: "pods"}, "", errors.New("RBAC denied"))
		}
		return nil
	}

	inv := Collect(context.Background(), Clients{Kube: cs, Discovery: disc}, loadKB(t), Options{})

	if st := inv.Capabilities[inventory.CapVersions]; st.Available || !strings.HasPrefix(st.Reason, "list kube-system pods: ") {
		t.Errorf("versions capability = %+v, want unavailable with a list kube-system pods reason", st)
	}
	if len(inv.ControlPlane) != 0 {
		t.Errorf("ControlPlane = %+v, want none", inv.ControlPlane)
	}
	if st := inv.Capabilities[inventory.CapAddOns]; !st.Available || st.Partial {
		t.Errorf("addons capability = %+v, want available and complete", st)
	}
	if got := addOnIDs(inv); !reflect.DeepEqual(got, []string{"cilium", "ingress-nginx"}) {
		t.Errorf("add-ons = %v, want cilium too: its kube-system pod was read by the add-ons", got)
	}
	if last := srv.calls[len(srv.calls)-1]; last.namespace != "" || last.fieldSelector != "" {
		t.Errorf("last pod list = %+v, want the full cluster-wide list, kube-system not excluded", last)
	}
}

// A kube-system list that fails on a later page leaves versions
// unavailable with nothing recorded, and the add-ons with none of its
// pages: they list every pod themselves, so no kube-system pod goes unread.
func TestCollectKubeSystemPodsFailingOnLaterPage(t *testing.T) {
	cs, disc := podFixture()
	srv := servePods(cs, tickPods()...)
	srv.perPage = 1
	srv.failList = func(_ int, ns string, o metav1.ListOptions) error {
		if ns == "kube-system" && o.Continue != "" {
			return errors.New("etcdserver: request timed out")
		}
		return nil
	}

	inv := Collect(context.Background(), Clients{Kube: cs, Discovery: disc}, loadKB(t), Options{})

	if st := inv.Capabilities[inventory.CapVersions]; st.Available || !strings.HasPrefix(st.Reason, "list kube-system pods: ") {
		t.Errorf("versions capability = %+v, want unavailable with a list kube-system pods reason", st)
	}
	if len(inv.ControlPlane) != 0 {
		t.Errorf("ControlPlane = %+v, want none: the failed list recorded nothing", inv.ControlPlane)
	}
	if got := addOnIDs(inv); !reflect.DeepEqual(got, []string{"cilium", "ingress-nginx"}) {
		t.Errorf("add-ons = %v, want cilium and ingress-nginx", got)
	}
	for pod, n := range srv.served {
		if strings.HasPrefix(pod, "kube-system/") && n > 2 {
			t.Errorf("pod %s sent %d times, want at most the failed attempt's page and the add-ons' read", pod, n)
		}
	}
}

// The role may read the kube-system pods and not the others (the audit's
// narrow role, #122): versions is unaffected, and the add-ons are partial
// exactly as when they listed every pod and that list failed: no pod was
// read, so the add-ons come from the IngressClasses alone and the reason
// says so.
func TestCollectOtherNamespacesForbiddenKeepsVersionsAndAddOnGapSemantics(t *testing.T) {
	class := &networkingv1.IngressClass{
		ObjectMeta: metav1.ObjectMeta{Name: "nginx"},
		Spec:       networkingv1.IngressClassSpec{Controller: "k8s.io/ingress-nginx"},
	}
	cs, disc := podFixture(class)
	srv := servePods(cs, tickPods()...)
	srv.failList = func(_ int, ns string, _ metav1.ListOptions) error {
		if ns == "" {
			return apierrors.NewForbidden(schema.GroupResource{Resource: "pods"}, "", errors.New("RBAC denied"))
		}
		return nil
	}

	inv := Collect(context.Background(), Clients{Kube: cs, Discovery: disc}, loadKB(t), Options{})

	if st := inv.Capabilities[inventory.CapVersions]; !st.Available || st.Partial {
		t.Errorf("versions capability = %+v, want available and complete", st)
	}
	if !reflect.DeepEqual(inv.ControlPlane, wantTickControlPlane()) {
		t.Errorf("ControlPlane = %+v, want %+v", inv.ControlPlane, wantTickControlPlane())
	}
	st := inv.Capabilities[inventory.CapAddOns]
	if !st.Available || !st.Partial || !reflect.DeepEqual(st.Skipped, []string{inventory.SkippedPods}) || !strings.HasPrefix(st.Reason, "list pods: ") {
		t.Errorf("addons capability = %+v, want partial, skipping pods, the reason the list error", st)
	}
	if !strings.HasSuffix(st.Reason, "add-ons were detected from IngressClasses only") {
		t.Errorf("addons reason = %q, want the unchanged one: from IngressClasses only", st.Reason)
	}
	if got := addOnIDs(inv); !reflect.DeepEqual(got, []string{"ingress-nginx"}) {
		t.Errorf("add-ons = %v, want ingress-nginx (IngressClass) alone: the failed list read no pod", got)
	}
}

// Every add-on source unreadable: the pods of the other namespaces and the
// IngressClasses are forbidden, and there are no Helm releases. The add-ons
// capability is not assessed and holds no add-on although versions read the
// kube-system pods: the engine reads inv.AddOns without asking whether the
// capability is available.
func TestCollectNothingReadForAddOnsLeavesNoAddOns(t *testing.T) {
	cs, disc := podFixture()
	srv := servePods(cs, tickPods()...)
	srv.failList = func(_ int, ns string, _ metav1.ListOptions) error {
		if ns == "" {
			return apierrors.NewForbidden(schema.GroupResource{Resource: "pods"}, "", errors.New("RBAC denied"))
		}
		return nil
	}
	cs.PrependReactor("list", "ingressclasses", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(schema.GroupResource{Group: "networking.k8s.io", Resource: "ingressclasses"}, "", errors.New("RBAC denied"))
	})

	inv := Collect(context.Background(), Clients{Kube: cs, Discovery: disc}, loadKB(t), Options{})

	if st := inv.Capabilities[inventory.CapVersions]; !st.Available || st.Partial {
		t.Errorf("versions capability = %+v, want available and complete", st)
	}
	st := inv.Capabilities[inventory.CapAddOns]
	if st.Available || !strings.HasPrefix(st.Reason, "list pods: ") {
		t.Errorf("addons capability = %+v, want unavailable with the list pods reason", st)
	}
	if len(inv.AddOns) != 0 {
		t.Errorf("add-ons = %v, want none for a capability that was not assessed", addOnIDs(inv))
	}
}

// A server or proxy may refuse the metadata.namespace field selector with a
// 400. The add-ons then list every pod, as before the shared read, and skip
// the kube-system pods client-side: the add-ons are those of main.
func TestCollectFieldSelectorRejectedFallsBackToUnfilteredList(t *testing.T) {
	cs, disc := podFixture()
	srv := servePods(cs, tickPods()...)
	srv.perPage = 1
	srv.failList = func(_ int, _ string, o metav1.ListOptions) error {
		if o.FieldSelector != "" {
			return apierrors.NewBadRequest("field label not supported: metadata.namespace")
		}
		return nil
	}

	inv := Collect(context.Background(), Clients{Kube: cs, Discovery: disc}, loadKB(t), Options{})

	if st := inv.Capabilities[inventory.CapVersions]; !st.Available || st.Partial {
		t.Errorf("versions capability = %+v, want available and complete", st)
	}
	if !reflect.DeepEqual(inv.ControlPlane, wantTickControlPlane()) {
		t.Errorf("ControlPlane = %+v, want %+v", inv.ControlPlane, wantTickControlPlane())
	}
	if st := inv.Capabilities[inventory.CapAddOns]; !st.Available || st.Partial {
		t.Errorf("addons capability = %+v, want available and complete", st)
	}
	if got := addOnIDs(inv); !reflect.DeepEqual(got, []string{"cilium", "ingress-nginx"}) {
		t.Errorf("add-ons = %v, want cilium and ingress-nginx as on main", got)
	}
	var selected, retried int
	for _, c := range srv.calls {
		if c.namespace != "" {
			continue
		}
		if c.fieldSelector != "" {
			selected++
		} else {
			retried++
		}
	}
	if selected != 1 || retried != 4 {
		t.Errorf("cluster-wide requests = %d with the selector, %d without (%+v), want one refused, then every pod in 4 unfiltered pages", selected, retried, srv.calls)
	}
}

// The other-namespaces list failing on a later page keeps what was read: the
// kube-system evidence from versions and the pages already listed. The
// capability is partial, with the unchanged "list pods" reason.
func TestCollectOtherNamespacesFailingOnLaterPageKeepsEvidence(t *testing.T) {
	cs, disc := podFixture()
	srv := servePods(cs, append(tickPods(), appPod("nginx-2", "example.com/app:v1"))...)
	srv.perPage = 1
	srv.failList = func(_ int, ns string, o metav1.ListOptions) error {
		if ns == "" && o.Continue != "" {
			return errors.New("etcdserver: request timed out")
		}
		return nil
	}

	inv := Collect(context.Background(), Clients{Kube: cs, Discovery: disc}, loadKB(t), Options{})

	if st := inv.Capabilities[inventory.CapVersions]; !st.Available || st.Partial {
		t.Errorf("versions capability = %+v, want available and complete", st)
	}
	st := inv.Capabilities[inventory.CapAddOns]
	if !st.Available || !st.Partial || !reflect.DeepEqual(st.Skipped, []string{inventory.SkippedPods}) {
		t.Errorf("addons capability = %+v, want partial, skipping pods", st)
	}
	if !strings.HasPrefix(st.Reason, "list pods: ") || !strings.Contains(st.Reason, "etcdserver: request timed out") {
		t.Errorf("addons reason = %q, want the unchanged list pods reason", st.Reason)
	}
	if got := addOnIDs(inv); !reflect.DeepEqual(got, []string{"cilium", "ingress-nginx"}) {
		t.Errorf("add-ons = %v, want cilium (kube-system, from versions) and ingress-nginx (the page read before the failure)", got)
	}
}
