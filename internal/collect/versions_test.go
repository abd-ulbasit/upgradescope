package collect

import (
	"context"
	"errors"
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
