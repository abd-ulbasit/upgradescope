package collect

import (
	"context"
	"errors"
	"reflect"
	"strings"
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

// testNode is a listed node: with none, the versions capability is partial
// (#174), which tests of the control-plane pods are not about.
func testNode() *corev1.Node {
	return &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-a"}, Status: corev1.NodeStatus{NodeInfo: corev1.NodeSystemInfo{KubeletVersion: "v1.34.2"}}}
}

func TestCollectVersionsControlPlane(t *testing.T) {
	cs := kubefake.NewClientset(
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "kube-system", UID: types.UID("uid-123")}},
		testNode(),
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
// labelled as the component that runs a vendor image of another name (a
// wrapper image) — leaves the versions capability partial, naming the
// components and the first such pod, rather than being dropped silently
// (#169). Skipped names only the components whose skew upstream would have
// told (the scheduler's latest and digest-only images), not kube-proxy's
// vendor image, so only that part is a required gap, and the reason names
// the first pod of a required component. The versions that were read are
// still recorded.
func TestCollectVersionsUnreadableControlPlaneVersionIsPartial(t *testing.T) {
	cs := kubefake.NewClientset(
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "kube-system", UID: types.UID("uid-123")}},
		testNode(),
		cpPod("kube-apiserver-cp1", map[string]string{"component": "kube-apiserver"}, "registry.k8s.io/kube-apiserver:v1.34.2"),
		cpPod("kube-scheduler-cp2", nil, "registry.k8s.io/kube-scheduler:latest"),
		cpPod("kube-scheduler-cp1", map[string]string{"component": "kube-scheduler"}, "registry.k8s.io/kube-scheduler@sha256:4f8bd1ec8bb2e6a5fcd4b4bbc6e4d5b0fdcb7f0f8a1c8c3f63b5f7f2b1a3c9d1"),
		cpPod("kube-proxy-abc12", map[string]string{"k8s-app": "kube-proxy"}, "registry.example.com/platform/proxy-wrapper:2.1"),
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
	// The reason names the first pod of a required component, the one to
	// fix, not the alphabetically earlier vendor kube-proxy.
	const wantMsg = "version not read from 3 control-plane pod(s) (kube-proxy, kube-scheduler), first kube-system/kube-scheduler-cp1: " +
		"kube-scheduler image registry.k8s.io/kube-scheduler@sha256:4f8bd1ec8bb2e6a5fcd4b4bbc6e4d5b0fdcb7f0f8a1c8c3f63b5f7f2b1a3c9d1 has no version tag; their skew was not evaluated"
	if !pe.incomplete || !reflect.DeepEqual(pe.skipped, []string{"kube-scheduler"}) || pe.msg != wantMsg {
		t.Errorf("partial = %v, skipped = %q, reason =\n%q\nwant incomplete, [kube-scheduler],\n%q", pe.incomplete, pe.skipped, pe.msg, wantMsg)
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

// RKE2 runs every control-plane component and kube-proxy from one image,
// rancher/hardened-kubernetes, tagged with the Kubernetes version: its
// static pods' versions are read, so its skew is judged rather than every
// component being a gap.
func TestCollectVersionsRKE2HardenedKubernetes(t *testing.T) {
	const image = "docker.io/rancher/hardened-kubernetes:v1.34.2-rke2r1-build20260101"
	cs := kubefake.NewClientset(
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "kube-system", UID: types.UID("uid-123")}},
		testNode(),
		cpPod("kube-apiserver-cp1", map[string]string{"component": "kube-apiserver"}, image),
		cpPod("kube-controller-manager-cp1", map[string]string{"component": "kube-controller-manager"}, image),
		cpPod("kube-scheduler-cp1", map[string]string{"component": "kube-scheduler"}, image),
		cpPod("kube-proxy-worker1", map[string]string{"component": "kube-proxy"}, "docker.io/rancher/hardened-kubernetes:v1.33.6-rke2r1-build20251101"),
	)
	disc := cs.Discovery().(*discoveryfake.FakeDiscovery)
	disc.FakedServerVersion = &version.Info{GitVersion: "v1.34.2+rke2r1"}
	var inv inventory.Inventory
	if err := collectVersions(context.Background(), disc, cs, "team", &inv); err != nil {
		t.Fatalf("err = %v, want every version read", err)
	}
	want := []inventory.ComponentVersion{
		{Component: "kube-apiserver", Version: "v1.34.2"},
		{Component: "kube-controller-manager", Version: "v1.34.2"},
		{Component: "kube-proxy", Version: "v1.33.6"},
		{Component: "kube-scheduler", Version: "v1.34.2"},
	}
	if !reflect.DeepEqual(inv.ControlPlane, want) {
		t.Errorf("ControlPlane = %+v, want %+v", inv.ControlPlane, want)
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
		{"docker.io/rancher/hardened-kubernetes:v1.34.2-rke2r1-build20260101", "v1.34.2", ""},
		{"projects.registry.vmware.com/tkg/kube-scheduler:v1.28.7_vmware.1", "v1.28.7", ""}, // TKG: Docker tags cannot hold '+'
		{"registry.k8s.io/kube-scheduler@sha256:abc", "", "kube-scheduler image registry.k8s.io/kube-scheduler@sha256:abc has no version tag"},
		{"registry.k8s.io/kube-scheduler:latest", "", `kube-scheduler image registry.k8s.io/kube-scheduler:latest has tag "latest", not a version`},
		{"registry.k8s.io/kube-scheduler", "", "kube-scheduler image registry.k8s.io/kube-scheduler has no version tag"},
		{"example.com/kube-scheduler-extender:v1.0.0", "", ""},
		{"example.com/kube-scheduler-mips:v1.0.0", "", ""},
		{"k8s.gcr.io/hyperkube:v1.18.20", "v1.18.20", ""}, // pre-1.19: one image ran every component
		{"gcr.io/google-containers/hyperkube-amd64:v1.15.12", "v1.15.12", ""},
		// scheduler-plugins' kube-scheduler carries its own version, whose
		// minor is the Kubernetes minor it is compiled with: v0.29.7 is
		// built on v1.29.7, and a three-digit patch (v0.18.800) changed
		// plugin code only, so only the minor is read.
		{"registry.k8s.io/scheduler-plugins/kube-scheduler:v0.29.7", "v1.29.7", ""},
		{"registry.k8s.io/scheduler-plugins/kube-scheduler:v0.31.8-rc.1", "v1.31.8", ""},
		{"registry.k8s.io/scheduler-plugins/kube-scheduler:v0.18.800", "v1.18.0", ""},
		{"registry.k8s.io/scheduler-plugins/kube-scheduler@sha256:abc", "", "kube-scheduler image registry.k8s.io/scheduler-plugins/kube-scheduler@sha256:abc has no version tag"},
		{"registry.k8s.io/scheduler-plugins/kube-scheduler:latest", "", `kube-scheduler image registry.k8s.io/scheduler-plugins/kube-scheduler:latest has tag "latest", not a scheduler-plugins version (v0.<minor>.<patch>)`},
		{"registry.k8s.io/scheduler-plugins/kube-scheduler:v1.0.0", "", `kube-scheduler image registry.k8s.io/scheduler-plugins/kube-scheduler:v1.0.0 has tag "v1.0.0", not a scheduler-plugins version (v0.<minor>.<patch>)`},
		// No Kubernetes release is 0.x: a v0 kube-scheduler tag is a
		// scheduler-plugins build, also when a mirror dropped that path.
		{"myreg.example.com/kube-scheduler:v0.31.8", "v1.31.8", ""},
	} {
		tag, why := componentImageTag([]corev1.Container{{Image: tc.image}}, "kube-scheduler")
		if tag != tc.tag || why != tc.why {
			t.Errorf("%s: tag %q, why %q; want %q, %q", tc.image, tag, why, tc.tag, tc.why)
		}
	}
}

// Any other component tagged 0.x is no Kubernetes version (#169 review):
// it reads as unreadable, not as Kubernetes 0.x, which would be a false
// skew blocker.
func TestComponentImageTagRejectsMajorZero(t *testing.T) {
	tag, why := componentImageTag([]corev1.Container{{Image: "myreg.example.com/kube-proxy:v0.31.8"}}, "kube-proxy")
	want := `kube-proxy image myreg.example.com/kube-proxy:v0.31.8 has tag "v0.31.8", not a Kubernetes version (no release is 0.x)`
	if tag != "" || why != want {
		t.Errorf("tag %q, why %q; want \"\", %q", tag, why, want)
	}
}

func TestCollectVersionsManagedClusterEmptyControlPlane(t *testing.T) {
	// EKS/GKE-style managed control plane: no apiserver/cm/scheduler pods in
	// kube-system (here, not even kube-proxy). Must yield an empty slice and
	// no error.
	cs := kubefake.NewClientset(
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "kube-system", UID: types.UID("uid-123")}},
		testNode(),
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

// Nodes, namespaces and kube-system pods are listed in pages, following the
// Continue token, so a large cluster never returns one unbounded list
// (PF-02 in docs/claims.md): listPageSize objects, and for nodes and pods,
// after the first page, as many as pageLimit allows.
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
	// The second page of nodes and pods is sized by the first (pageLimit):
	// one small object, so as many as the page may hold.
	next := map[string]int64{"nodes": nodePageSize, "namespaces": listPageSize, "pods": podPageSize}
	for resource := range pages {
		want := []metav1.ListOptions{{Limit: listPageSize}, {Limit: next[resource], Continue: "page-2"}}
		if !reflect.DeepEqual(opts[resource], want) {
			t.Errorf("%s list options = %+v, want %+v (paged, Continue token followed)", resource, opts[resource], want)
		}
	}
	if len(inv.Nodes) != 2 || len(inv.Namespaces) != 2 || len(inv.ControlPlane) != 2 {
		t.Errorf("nodes %+v, namespaces %+v, control plane %+v: want 2 of each (items from every page count)", inv.Nodes, inv.Namespaces, inv.ControlPlane)
	}
}

// cpPodOn is cpPod scheduled on node.
func cpPodOn(node, name string, labels map[string]string, image string) *corev1.Pod {
	p := cpPod(name, labels, image)
	p.Spec.NodeName = node
	return p
}

// A kube-proxy is recorded with the node it runs on and is not deduped
// across nodes, so the engine can pair it with that node's kubelet (#148).
// Every other component keeps the (Component, Version) dedupe, whatever
// node its pod is on.
func TestCollectVersionsKubeProxyRecordsNodeNotDeduped(t *testing.T) {
	proxy := map[string]string{"k8s-app": "kube-proxy"}
	cs := kubefake.NewClientset(
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "kube-system", UID: types.UID("uid-123")}},
		&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-a"}, Status: corev1.NodeStatus{NodeInfo: corev1.NodeSystemInfo{KubeletVersion: "v1.34.2"}}},
		cpPodOn("node-b", "kube-proxy-bbbbb", proxy, "registry.k8s.io/kube-proxy:v1.33.0"),
		cpPodOn("node-a", "kube-proxy-aaaaa", proxy, "registry.k8s.io/kube-proxy:v1.33.0"),
		cpPodOn("node-c", "kube-proxy-ccccc", proxy, "registry.k8s.io/kube-proxy:v1.34.2"),
		cpPod("kube-proxy-unscheduled", proxy, "registry.k8s.io/kube-proxy:v1.34.2"), // no node yet
		cpPodOn("node-a", "kube-apiserver-a", map[string]string{"component": "kube-apiserver"}, "registry.k8s.io/kube-apiserver:v1.34.2"),
		cpPodOn("node-b", "kube-apiserver-b", map[string]string{"component": "kube-apiserver"}, "registry.k8s.io/kube-apiserver:v1.34.2"),
	)
	disc := cs.Discovery().(*discoveryfake.FakeDiscovery)
	disc.FakedServerVersion = &version.Info{GitVersion: "v1.34.2"}

	var inv inventory.Inventory
	if err := collectVersions(context.Background(), disc, cs, "team", &inv); err != nil {
		t.Fatal(err)
	}
	want := []inventory.ComponentVersion{
		{Component: "kube-apiserver", Version: "v1.34.2"},
		{Component: "kube-proxy", Version: "v1.33.0", Node: "node-a"},
		{Component: "kube-proxy", Version: "v1.33.0", Node: "node-b"},
		{Component: "kube-proxy", Version: "v1.34.2"},
		{Component: "kube-proxy", Version: "v1.34.2", Node: "node-c"},
	}
	if !reflect.DeepEqual(inv.ControlPlane, want) {
		t.Errorf("ControlPlane =\n%+v\nwant\n%+v", inv.ControlPlane, want)
	}
}

// No Node listed means the kubelet-skew and node-runtime checks had
// nothing to check: the versions capability is partial, naming them, and
// that gap is required (a forbidden Node list makes versions unavailable,
// which is required for a cluster), so the verdict cannot be ready on it
// (#174). What was read is still recorded.
func TestCollectVersionsNoNodesIsPartial(t *testing.T) {
	cs := kubefake.NewClientset(
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "kube-system", UID: types.UID("uid-123")}},
		cpPod("kube-apiserver-cp1", map[string]string{"component": "kube-apiserver"}, "registry.k8s.io/kube-apiserver:v1.34.2"),
	)
	disc := cs.Discovery().(*discoveryfake.FakeDiscovery)
	disc.FakedServerVersion = &version.Info{GitVersion: "v1.34.2"}

	var inv inventory.Inventory
	err := collectVersions(context.Background(), disc, cs, "team", &inv)
	var pe partialError
	if !errors.As(err, &pe) {
		t.Fatalf("err = %v, want a partialError", err)
	}
	const wantMsg = "no nodes listed: kubelet skew and node runtimes not assessed"
	if !pe.incomplete || !reflect.DeepEqual(pe.skipped, []string{"nodes"}) || pe.msg != wantMsg {
		t.Errorf("partial = %v, skipped = %q, reason = %q; want incomplete, [nodes], %q", pe.incomplete, pe.skipped, pe.msg, wantMsg)
	}
	if inv.ServerVersion != "v1.34.2" || len(inv.ControlPlane) != 1 {
		t.Errorf("what was read must persist: %+v", inv)
	}

	got := Collect(context.Background(), Clients{Kube: cs, Discovery: disc}, kb.KB{}, Options{}).Capabilities[inventory.CapVersions]
	if want := (inventory.CapabilityStatus{Available: true, Reason: wantMsg, Partial: true, Skipped: []string{"nodes"}}); !reflect.DeepEqual(got, want) {
		t.Errorf("versions capability = %+v, want %+v", got, want)
	}
}

// An empty node list and an unreadable control-plane pod are both
// reported: one reason, one Skipped listing both.
func TestCollectVersionsNoNodesAndUnreadablePodReportBoth(t *testing.T) {
	cs := kubefake.NewClientset(
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "kube-system", UID: types.UID("uid-123")}},
		cpPod("kube-scheduler-cp1", map[string]string{"component": "kube-scheduler"}, "registry.k8s.io/kube-scheduler:latest"),
	)
	disc := cs.Discovery().(*discoveryfake.FakeDiscovery)
	disc.FakedServerVersion = &version.Info{GitVersion: "v1.34.2"}

	var inv inventory.Inventory
	err := collectVersions(context.Background(), disc, cs, "team", &inv)
	var pe partialError
	if !errors.As(err, &pe) {
		t.Fatalf("err = %v, want a partialError", err)
	}
	if !pe.incomplete || !reflect.DeepEqual(pe.skipped, []string{"kube-scheduler", "nodes"}) {
		t.Errorf("partial = %v, skipped = %q, want incomplete, [kube-scheduler nodes]", pe.incomplete, pe.skipped)
	}
	for _, part := range []string{"version not read from 1 control-plane pod(s) (kube-scheduler)", "no nodes listed: kubelet skew and node runtimes not assessed"} {
		if !strings.Contains(pe.msg, part) {
			t.Errorf("reason %q lacks %q", pe.msg, part)
		}
	}
}
