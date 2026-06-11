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
		&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-b"}, Status: corev1.NodeStatus{NodeInfo: corev1.NodeSystemInfo{KubeletVersion: "v1.33.1"}}},
		&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-a"}, Status: corev1.NodeStatus{NodeInfo: corev1.NodeSystemInfo{KubeletVersion: "v1.34.2"}}},
	)
	disc := cs.Discovery().(*discoveryfake.FakeDiscovery)
	disc.FakedServerVersion = &version.Info{GitVersion: "v1.34.2"}

	var inv inventory.Inventory
	if err := collectVersions(context.Background(), disc, cs, "team", &inv); err != nil {
		t.Fatal(err)
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
		// unparseable tag → skipped, not an error.
		cpPod("kube-scheduler-cp2", nil, "registry.k8s.io/kube-scheduler:latest"),
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
		{Component: "kube-controller-manager", Version: "v1.34.2"},
		{Component: "kube-proxy", Version: "v1.33.0"},
		{Component: "kube-proxy", Version: "v1.34.2"},
		{Component: "kube-scheduler", Version: "v1.34.2"},
	}
	if !reflect.DeepEqual(inv.ControlPlane, want) {
		t.Errorf("ControlPlane =\n%+v\nwant sorted+deduped\n%+v", inv.ControlPlane, want)
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
