package collect

import (
	"context"
	"errors"
	"reflect"
	"testing"

	corev1 "k8s.io/api/core/v1"
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
	"github.com/abd-ulbasit/upgradescope/internal/kb"
)

func TestCollectVersions(t *testing.T) {
	cs := kubefake.NewClientset(
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "kube-system", UID: types.UID("uid-123")}},
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "payments", Labels: map[string]string{"team": "fintech"}}},
		&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-b"}, Status: corev1.NodeStatus{NodeInfo: corev1.NodeSystemInfo{KubeletVersion: "v1.33.1", ContainerRuntimeVersion: "containerd://1.7.27"}}},
		&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-a"}, Status: corev1.NodeStatus{NodeInfo: corev1.NodeSystemInfo{KubeletVersion: "v1.34.2", ContainerRuntimeVersion: "containerd://2.0.5"}}},
	)
	disc := cs.Discovery().(*discoveryfake.FakeDiscovery)
	disc.FakedServerVersion = &version.Info{GitVersion: "v1.34.2"}

	var inv inventory.Inventory
	if err := collectVersions(context.Background(), disc, cs, "team", &inv); err != nil {
		t.Fatal(err)
	}
	if want := []inventory.NodeInfo{
		{Name: "node-a", KubeletVersion: "v1.34.2", ContainerRuntime: "containerd://2.0.5"},
		{Name: "node-b", KubeletVersion: "v1.33.1", ContainerRuntime: "containerd://1.7.27"},
	}; !reflect.DeepEqual(inv.Nodes, want) {
		t.Errorf("Nodes = %+v, want %+v (container runtime collected)", inv.Nodes, want)
	}
	if inv.ServerVersion != "v1.34.2" {
		t.Errorf("ServerVersion = %q, want v1.34.2", inv.ServerVersion)
	}
	if inv.ClusterID != "uid-123" {
		t.Errorf("ClusterID = %q, want kube-system UID uid-123", inv.ClusterID)
	}
	if len(inv.Nodes) != 2 || inv.Nodes[0].Name != "node-a" || inv.Nodes[1].KubeletVersion != "v1.33.1" {
		t.Errorf("Nodes = %+v, want sorted [node-a node-b]", inv.Nodes)
	}
	if len(inv.Namespaces) != 2 || inv.Namespaces[0].Name != "kube-system" {
		t.Fatalf("Namespaces = %+v, want sorted [kube-system payments]", inv.Namespaces)
	}
	if inv.Namespaces[1].Team != "fintech" {
		t.Errorf("payments team = %q, want fintech (from label %q)", inv.Namespaces[1].Team, "team")
	}
}

// cpPod builds a kube-system pod with the given name, labels, and container image.
func cpPod(name string, labels map[string]string, image string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "kube-system", Labels: labels},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "c", Image: image}}},
	}
}

func TestCollectVersionsControlPlane(t *testing.T) {
	cs := kubefake.NewClientset(
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "kube-system", UID: types.UID("uid-123")}},
		// kubeadm static pods carry component=<name> labels.
		cpPod("kube-apiserver-cp1", map[string]string{"component": "kube-apiserver"}, "registry.k8s.io/kube-apiserver:v1.34.2"),
		cpPod("kube-apiserver-cp2", map[string]string{"component": "kube-apiserver"}, "registry.k8s.io/kube-apiserver:v1.34.2"), // duplicate (component, version) → deduped
		cpPod("kube-apiserver-cp3", map[string]string{"component": "kube-apiserver"}, "registry.k8s.io/kube-apiserver:v1.33.0"), // second distinct version survives
		cpPod("kube-controller-manager-cp1", map[string]string{"component": "kube-controller-manager"}, "registry.k8s.io/kube-controller-manager:v1.34.2"),
		// no component label → matched by pod-name prefix.
		cpPod("kube-scheduler-cp1", nil, "registry.k8s.io/kube-scheduler:v1.34.2"),
		// kube-proxy DaemonSet pods carry k8s-app=kube-proxy, not component.
		cpPod("kube-proxy-abc12", map[string]string{"k8s-app": "kube-proxy"}, "registry.k8s.io/kube-proxy:v1.34.2"),
		// EKS-style build-suffix tag normalizes to a parseable version.
		cpPod("kube-proxy-def34", map[string]string{"k8s-app": "kube-proxy"}, "602401143452.dkr.ecr.us-east-1.amazonaws.com/eks/kube-proxy:v1.33.0-eksbuild.1"),
		// per-architecture image names (GKE's kube-proxy, older kubeadm) → read (#169).
		cpPod("kube-proxy-gke-pool-1-abcd", map[string]string{"component": "kube-proxy"}, "gke.gcr.io/kube-proxy-amd64:v1.32.0-gke.1000"),
		cpPod("kube-controller-manager-cp2", map[string]string{"component": "kube-controller-manager"}, "k8s.gcr.io/kube-controller-manager-arm64:v1.33.1"),
		// a pod named like a component running another image → not that component, silently.
		cpPod("kube-scheduler-extender-x1", nil, "example.com/kube-scheduler-extender:v1.0.0"),
		cpPod("kube-proxy-sidecar-x1", nil, "example.com/kube-proxy-sidecar:v2.0.0"),
		// unrelated kube-system pod → ignored.
		cpPod("coredns-12345", map[string]string{"k8s-app": "kube-dns"}, "registry.k8s.io/coredns/coredns:v1.11.1"),
	)
	disc := cs.Discovery().(*discoveryfake.FakeDiscovery)
	disc.FakedServerVersion = &version.Info{GitVersion: "v1.34.2"}

	var inv inventory.Inventory
	if err := collectVersions(context.Background(), disc, cs, "team", &inv); err != nil {
		t.Fatal(err)
	}
	want := []inventory.ComponentVersion{
		{Component: "kube-apiserver", Version: "v1.33.0"},
		{Component: "kube-apiserver", Version: "v1.34.2"},
		{Component: "kube-controller-manager", Version: "v1.33.1"},
		{Component: "kube-controller-manager", Version: "v1.34.2"},
		{Component: "kube-proxy", Version: "v1.32.0"},
		{Component: "kube-proxy", Version: "v1.33.0"},
		{Component: "kube-proxy", Version: "v1.34.2"},
		{Component: "kube-scheduler", Version: "v1.34.2"},
	}
	if !reflect.DeepEqual(inv.ControlPlane, want) {
		t.Errorf("ControlPlane =\n%+v\nwant sorted+deduped\n%+v", inv.ControlPlane, want)
	}
}

// A control-plane or kube-proxy pod whose version cannot be read — a
// digest-only or non-version tag on the component's image, or a pod
// labelled as the component that runs no image of that name (RKE2's
// hardened-kubernetes) — leaves the versions capability partial, naming
// the components and the first such pod, rather than being dropped
// silently (#169). The versions that were read are still recorded.
func TestCollectVersionsUnreadableControlPlaneVersionIsPartial(t *testing.T) {
	cs := kubefake.NewClientset(
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "kube-system", UID: types.UID("uid-123")}},
		cpPod("kube-apiserver-cp1", map[string]string{"component": "kube-apiserver"}, "registry.k8s.io/kube-apiserver:v1.34.2"),
		cpPod("kube-scheduler-cp2", nil, "registry.k8s.io/kube-scheduler:latest"),
		cpPod("kube-scheduler-cp1", map[string]string{"component": "kube-scheduler"}, "registry.k8s.io/kube-scheduler@sha256:4f8bd1ec8bb2e6a5fcd4b4bbc6e4d5b0fdcb7f0f8a1c8c3f63b5f7f2b1a3c9d1"),
		cpPod("kube-proxy-abc12", map[string]string{"k8s-app": "kube-proxy"}, "docker.io/rancher/hardened-kubernetes:v1.34.2-rke2r1-build20260101"),
		// named like a component, no label, another image: not that component, no gap.
		cpPod("kube-scheduler-extender-x1", nil, "example.com/kube-scheduler-extender:v1.0.0"),
	)
	disc := cs.Discovery().(*discoveryfake.FakeDiscovery)
	disc.FakedServerVersion = &version.Info{GitVersion: "v1.34.2"}

	var inv inventory.Inventory
	err := collectVersions(context.Background(), disc, cs, "team", &inv)
	var pe partialError
	if !errors.As(err, &pe) {
		t.Fatalf("err = %v, want a partialError", err)
	}
	const wantMsg = "version not read from 3 control-plane pod(s) (kube-proxy, kube-scheduler), first kube-system/kube-proxy-abc12: " +
		"labelled kube-proxy but runs no kube-proxy image (docker.io/rancher/hardened-kubernetes:v1.34.2-rke2r1-build20260101); their skew was not evaluated"
	if !pe.incomplete || !reflect.DeepEqual(pe.skipped, []string{"kube-proxy", "kube-scheduler"}) || pe.msg != wantMsg {
		t.Errorf("partial = %v, skipped = %q, reason =\n%q\nwant incomplete, [kube-proxy kube-scheduler],\n%q", pe.incomplete, pe.skipped, pe.msg, wantMsg)
	}
	if want := []inventory.ComponentVersion{{Component: "kube-apiserver", Version: "v1.34.2"}}; !reflect.DeepEqual(inv.ControlPlane, want) {
		t.Errorf("ControlPlane = %+v, want %+v", inv.ControlPlane, want)
	}

	// Collect surfaces it as a partial capability, still available.
	got := Collect(context.Background(), Clients{Kube: cs, Discovery: disc}, kb.KB{}, Options{}).Capabilities[inventory.CapVersions]
	if !got.Available || !got.Partial || got.Reason != wantMsg {
		t.Errorf("versions capability = %+v, want available, partial, with the reason", got)
	}
}

// The reasons a pod's version is unreadable name the image.
func TestComponentImageTagUnreadableReasons(t *testing.T) {
	for _, tc := range []struct {
		image, tag, why string
	}{
		{"registry.k8s.io/kube-scheduler:v1.34.2", "v1.34.2", ""},
		{"registry.k8s.io/kube-scheduler:v1.34.2@sha256:abc", "v1.34.2", ""},
		{"gke.gcr.io/kube-scheduler-amd64:v1.32.0-gke.1000", "v1.32.0", ""},
		{"registry.k8s.io/kube-scheduler@sha256:abc", "", "kube-scheduler image registry.k8s.io/kube-scheduler@sha256:abc has no version tag"},
		{"registry.k8s.io/kube-scheduler:latest", "", `kube-scheduler image registry.k8s.io/kube-scheduler:latest has tag "latest", not a version`},
		{"registry.k8s.io/kube-scheduler", "", "kube-scheduler image registry.k8s.io/kube-scheduler has no version tag"},
		{"example.com/kube-scheduler-extender:v1.0.0", "", ""},
		{"example.com/kube-scheduler-mips:v1.0.0", "", ""},
	} {
		tag, why := componentImageTag([]corev1.Container{{Image: tc.image}}, "kube-scheduler")
		if tag != tc.tag || why != tc.why {
			t.Errorf("%s: tag %q, why %q; want %q, %q", tc.image, tag, why, tc.tag, tc.why)
		}
	}
}

func TestCollectVersionsManagedClusterEmptyControlPlane(t *testing.T) {
	// EKS/GKE-style managed control plane: no apiserver/cm/scheduler pods in
	// kube-system (here, not even kube-proxy). Must yield an empty slice and
	// no error.
	cs := kubefake.NewClientset(
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "kube-system", UID: types.UID("uid-123")}},
		cpPod("coredns-12345", map[string]string{"k8s-app": "kube-dns"}, "registry.k8s.io/coredns/coredns:v1.11.1"),
	)
	disc := cs.Discovery().(*discoveryfake.FakeDiscovery)
	disc.FakedServerVersion = &version.Info{GitVersion: "v1.34.2"}

	var inv inventory.Inventory
	if err := collectVersions(context.Background(), disc, cs, "team", &inv); err != nil {
		t.Fatal(err)
	}
	if len(inv.ControlPlane) != 0 {
		t.Errorf("ControlPlane = %+v, want empty for managed control planes", inv.ControlPlane)
	}
}

func TestCollectVersionsControlPlanePodsForbiddenKeepsEarlierFields(t *testing.T) {
	cs := kubefake.NewClientset(
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "kube-system", UID: types.UID("uid-123")}},
		&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-a"}, Status: corev1.NodeStatus{NodeInfo: corev1.NodeSystemInfo{KubeletVersion: "v1.34.2"}}},
	)
	disc := cs.Discovery().(*discoveryfake.FakeDiscovery)
	disc.FakedServerVersion = &version.Info{GitVersion: "v1.34.2"}
	cs.PrependReactor("list", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: "pods"}, "", errors.New("RBAC denied"))
	})

	var inv inventory.Inventory
	if err := collectVersions(context.Background(), disc, cs, "team", &inv); err == nil {
		t.Fatal("want error when kube-system pods list is forbidden")
	}
	if inv.ClusterID != "uid-123" || len(inv.Nodes) != 1 || len(inv.Namespaces) != 1 {
		t.Errorf("fields gathered before the failure must persist: %+v", inv)
	}
}

func TestCollectVersionsNodesForbiddenKeepsEarlierFields(t *testing.T) {
	cs := kubefake.NewClientset(
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "kube-system", UID: types.UID("uid-123")}},
	)
	disc := cs.Discovery().(*discoveryfake.FakeDiscovery)
	disc.FakedServerVersion = &version.Info{GitVersion: "v1.34.2"}
	cs.PrependReactor("list", "nodes", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: "nodes"}, "", errors.New("RBAC denied"))
	})

	var inv inventory.Inventory
	if err := collectVersions(context.Background(), disc, cs, "team", &inv); err == nil {
		t.Fatal("want error when nodes list is forbidden")
	}
	if inv.ClusterID != "uid-123" || inv.ServerVersion != "v1.34.2" {
		t.Errorf("best-effort fields gathered before the failure must persist: %+v", inv)
	}
}

func TestCollectWiresVersionsCapability(t *testing.T) {
	cs := kubefake.NewClientset(&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "kube-system", UID: types.UID("uid-1")}})
	disc := cs.Discovery().(*discoveryfake.FakeDiscovery)
	disc.FakedServerVersion = &version.Info{GitVersion: "v1.34.0"}

	inv := Collect(context.Background(), Clients{Kube: cs, Discovery: disc}, kb.KB{}, Options{})
	if got := inv.Capabilities[inventory.CapVersions]; !got.Available {
		t.Errorf("versions capability = %+v, want available", got)
	}
}

// Nodes, namespaces and kube-system pods are listed in pages of
// listPageSize, following the Continue token, so a large cluster never
// returns one unbounded list (PF-02 in docs/claims.md).
func TestCollectVersionsFollowsListPagination(t *testing.T) {
	cs := kubefake.NewClientset(&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "kube-system", UID: types.UID("uid-123")}})
	disc := cs.Discovery().(*discoveryfake.FakeDiscovery)
	disc.FakedServerVersion = &version.Info{GitVersion: "v1.34.2"}
	node := func(name string) corev1.Node { return corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name}} }
	ns := func(name string) corev1.Namespace { return corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}} }
	pages := map[string][2]runtime.Object{
		"nodes": {
			&corev1.NodeList{ListMeta: metav1.ListMeta{Continue: "page-2"}, Items: []corev1.Node{node("node-a")}},
			&corev1.NodeList{Items: []corev1.Node{node("node-b")}},
		},
		"namespaces": {
			&corev1.NamespaceList{ListMeta: metav1.ListMeta{Continue: "page-2"}, Items: []corev1.Namespace{ns("apps")}},
			&corev1.NamespaceList{Items: []corev1.Namespace{ns("kube-system")}},
		},
		"pods": {
			&corev1.PodList{ListMeta: metav1.ListMeta{Continue: "page-2"}, Items: []corev1.Pod{*cpPod("kube-apiserver-cp", map[string]string{"component": "kube-apiserver"}, "registry.k8s.io/kube-apiserver:v1.34.2")}},
			&corev1.PodList{Items: []corev1.Pod{*cpPod("kube-scheduler-cp", map[string]string{"component": "kube-scheduler"}, "registry.k8s.io/kube-scheduler:v1.34.2")}},
		},
	}
	opts := map[string][]metav1.ListOptions{}
	for resource, page := range pages {
		cs.PrependReactor("list", resource, func(a k8stesting.Action) (bool, runtime.Object, error) {
			o := a.(interface{ GetListOptions() metav1.ListOptions }).GetListOptions()
			opts[resource] = append(opts[resource], o)
			if o.Continue == "" {
				return true, page[0], nil
			}
			return true, page[1], nil
		})
	}

	var inv inventory.Inventory
	if err := collectVersions(context.Background(), disc, cs, "team", &inv); err != nil {
		t.Fatal(err)
	}
	want := []metav1.ListOptions{{Limit: listPageSize}, {Limit: listPageSize, Continue: "page-2"}}
	for resource := range pages {
		if !reflect.DeepEqual(opts[resource], want) {
			t.Errorf("%s list options = %+v, want %+v (paged, Continue token followed)", resource, opts[resource], want)
		}
	}
	if len(inv.Nodes) != 2 || len(inv.Namespaces) != 2 || len(inv.ControlPlane) != 2 {
		t.Errorf("nodes %+v, namespaces %+v, control plane %+v: want 2 of each (items from every page count)", inv.Nodes, inv.Namespaces, inv.ControlPlane)
	}
}
