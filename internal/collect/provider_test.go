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
	"github.com/abd-ulbasit/upgradescope/registry"
)

func providerNode(name, providerID string, labels map[string]string) *corev1.Node {
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name, Labels: labels},
		Spec:       corev1.NodeSpec{ProviderID: providerID},
		Status:     corev1.NodeStatus{NodeInfo: corev1.NodeSystemInfo{KubeletVersion: "v1.34.2"}},
	}
}

// The provider is inferred from signals only the provider produces: the
// control plane's version suffix and the labels its managed node pools put
// on every node. A providerID alone says where a VM runs, not who manages
// the control plane (kubeadm on EC2 has aws://), so it can only contradict
// a claim, never make one. Anything unclear is "other"; a cluster whose
// nodes could not be listed and whose version says nothing is "" (not
// determined), since "other" would be a guess.
func TestCollectVersionsProvider(t *testing.T) {
	type L = map[string]string
	cases := []struct {
		name   string
		server string
		nodes  []*corev1.Node
		denied bool // nodes list forbidden
		want   inventory.Provider
	}{
		{"EKS: version suffix and managed node group", "v1.34.2-eks-3abc123", []*corev1.Node{
			providerNode("n1", "aws:///us-east-1a/i-0123", L{"eks.amazonaws.com/nodegroup": "ng-1"}),
		}, false, inventory.ProviderEKS},
		{"EKS: Karpenter nodes carry no EKS label, the version says it", "v1.34.2-eks-3abc123", []*corev1.Node{
			providerNode("n1", "aws:///us-east-1a/i-0123", L{"karpenter.sh/nodepool": "default"}),
		}, false, inventory.ProviderEKS},
		{"EKS: Fargate and hybrid nodes do not contradict it", "v1.31.4-eks-aeac579", []*corev1.Node{
			providerNode("fargate-ip-10-0-1-2", "aws:///us-east-1a/fargate-ip-10-0-1-2", L{"eks.amazonaws.com/compute-type": "fargate"}),
			providerNode("hybrid-1", "eks-hybrid:///us-east-1/prod/mi-0abc", nil),
		}, false, inventory.ProviderEKS},
		{"EKS: nodes unreadable, the version is still the control plane's own", "v1.34.2-eks-3abc123", nil, true, inventory.ProviderEKS},
		{"EKS Distro / EKS Anywhere build is not the managed service", "v1.29.1-eks-1-29-12", []*corev1.Node{
			providerNode("n1", "vsphere://4237", nil),
		}, false, inventory.ProviderOther},
		{"GKE: version suffix and node pool label", "v1.34.2-gke.1234000", []*corev1.Node{
			providerNode("n1", "gce://proj/us-central1-a/n1", L{"cloud.google.com/gke-nodepool": "default-pool"}),
		}, false, inventory.ProviderGKE},
		{"GKE version on nodes with no providerID (some bare-metal clusters): nothing contradicts it, so it is GKE", "v1.34.2-gke.1234000", []*corev1.Node{
			providerNode("n1", "", nil),
		}, false, inventory.ProviderGKE},
		{"GKE version on providerID-less bare-metal nodes that carry a Google Distributed Cloud label is not GKE", "v1.34.2-gke.1234000", []*corev1.Node{
			providerNode("n1", "", L{"baremetal.cluster.gke.io/node-pool": "node-pool-1"}),
		}, false, inventory.ProviderOther},
		{"GKE-versioned cluster whose nodes run on vSphere is not GKE", "v1.30.2-gke.100", []*corev1.Node{
			providerNode("n1", "vsphere://4237", nil),
		}, false, inventory.ProviderOther},
		{"AKS: no version suffix, the cluster label says it", "v1.34.2", []*corev1.Node{
			providerNode("n1", "azure:///subscriptions/s/resourceGroups/MC_rg/providers/Microsoft.Compute/virtualMachineScaleSets/vmss/virtualMachines/0",
				L{"kubernetes.azure.com/cluster": "MC_rg_aks_eastus"}),
		}, false, inventory.ProviderAKS},
		{"AKS: a virtual node with no providerID does not contradict it", "v1.34.2", []*corev1.Node{
			providerNode("n1", "azure:///subscriptions/s/x", L{"kubernetes.azure.com/cluster": "MC_rg_aks_eastus"}),
			providerNode("virtual-node-aci-linux", "", nil),
		}, false, inventory.ProviderAKS},
		{"kubeadm on Azure VMs: azure:// alone claims nothing", "v1.34.2", []*corev1.Node{
			providerNode("n1", "azure:///subscriptions/s/x", nil),
		}, false, inventory.ProviderOther},
		{"kubeadm on EC2: aws:// alone claims nothing", "v1.34.2", []*corev1.Node{
			providerNode("n1", "aws:///us-east-1a/i-0123", nil),
		}, false, inventory.ProviderOther},
		{"kubeadm on GCE: gce:// alone claims nothing", "v1.34.2", []*corev1.Node{
			providerNode("n1", "gce://proj/us-central1-a/n1", nil),
		}, false, inventory.ProviderOther},
		{"kind", "v1.34.0", []*corev1.Node{
			providerNode("kind-control-plane", "kind://docker/kind/kind-control-plane", nil),
		}, false, inventory.ProviderOther},
		{"vanilla cluster with a bare provider-less node", "v1.34.2", []*corev1.Node{
			providerNode("n1", "", nil),
		}, false, inventory.ProviderOther},
		{"EKS version with AKS node labels: two claims, nothing is guessed", "v1.34.2-eks-3abc123", []*corev1.Node{
			providerNode("n1", "azure:///x", L{"kubernetes.azure.com/cluster": "MC_rg_aks_eastus"}),
		}, false, inventory.ProviderOther},
		{"EKS version on GCE nodes: the providerID contradicts it", "v1.34.2-eks-3abc123", []*corev1.Node{
			providerNode("n1", "gce://proj/z/n1", nil),
		}, false, inventory.ProviderOther},
		{"AKS label on aws:// nodes: the providerID contradicts it", "v1.34.2", []*corev1.Node{
			providerNode("n1", "aws:///us-east-1a/i-0123", L{"kubernetes.azure.com/cluster": "MC_rg_aks_eastus"}),
		}, false, inventory.ProviderOther},
		{"nodes unreadable and no suffix: not determined, not other", "v1.34.2", nil, true, ""},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			objs := []runtime.Object{&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "kube-system", UID: types.UID("uid-1")}}}
			for _, n := range tt.nodes {
				objs = append(objs, n)
			}
			cs := kubefake.NewClientset(objs...)
			disc := cs.Discovery().(*discoveryfake.FakeDiscovery)
			disc.FakedServerVersion = &version.Info{GitVersion: tt.server}
			if tt.denied {
				cs.PrependReactor("list", "nodes", func(k8stesting.Action) (bool, runtime.Object, error) {
					return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: "nodes"}, "", errors.New("RBAC denied"))
				})
			}
			var inv inventory.Inventory
			err := collectVersions(context.Background(), disc, cs, "team", &inv)
			if tt.denied != (err != nil) {
				t.Fatalf("collectVersions error = %v, want one only when nodes are denied (%v)", err, tt.denied)
			}
			if inv.Provider != tt.want {
				t.Errorf("Provider = %q, want %q", inv.Provider, tt.want)
			}
		})
	}
}

// A cluster that has no nodes yet but a provider's version suffix is that
// provider's; one with neither is other, since the (empty) list was read.
func TestCollectVersionsProviderWithoutNodes(t *testing.T) {
	for server, want := range map[string]inventory.Provider{
		"v1.34.2-gke.1234000": inventory.ProviderGKE,
		"v1.34.2":             inventory.ProviderOther,
	} {
		cs := kubefake.NewClientset(&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "kube-system", UID: types.UID("uid-1")}})
		disc := cs.Discovery().(*discoveryfake.FakeDiscovery)
		disc.FakedServerVersion = &version.Info{GitVersion: server}
		var inv inventory.Inventory
		// An empty node list is no error but a gap (#185); the provider is
		// still decided from the (empty) list that was read.
		var pe partialError
		if err := collectVersions(context.Background(), disc, cs, "team", &inv); !errors.As(err, &pe) || pe.msg != noNodesReason {
			t.Fatalf("collectVersions error = %v, want the no-nodes gap", err)
		}
		if inv.Provider != want {
			t.Errorf("%s with no nodes: Provider = %q, want %q", server, inv.Provider, want)
		}
	}
}

// The provider names the inventory can carry are the ones the registry has
// calendars for (plus other).
func TestProviderNamesMatchRegistry(t *testing.T) {
	got := []string{string(inventory.ProviderEKS), string(inventory.ProviderGKE), string(inventory.ProviderAKS)}
	if len(got) != len(registry.ProviderIDs) {
		t.Fatalf("registry.ProviderIDs = %v, inventory names %v: keep them in step", registry.ProviderIDs, got)
	}
	for i, id := range registry.ProviderIDs {
		if got[i] != id {
			t.Errorf("registry.ProviderIDs[%d] = %q, inventory has %q", i, id, got[i])
		}
	}
}

// When the node list fails after its first page, the pages read are not
// evidence: an AKS label on page one could claim a provider that a
// providerID on page two would contradict. Only the version suffix, which
// needs no nodes, still names the provider; otherwise it is undetermined.
func TestCollectVersionsProviderNodeListFailsMidway(t *testing.T) {
	for _, tt := range []struct {
		name   string
		server string
		want   inventory.Provider
	}{
		{"label on the pages read, no suffix", "v1.34.2", ""},
		{"suffix names it whatever was read", "v1.34.2-eks-3abc123", inventory.ProviderEKS},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cs := kubefake.NewClientset(&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "kube-system", UID: types.UID("uid-1")}})
			disc := cs.Discovery().(*discoveryfake.FakeDiscovery)
			disc.FakedServerVersion = &version.Info{GitVersion: tt.server}
			calls := 0
			cs.PrependReactor("list", "nodes", func(k8stesting.Action) (bool, runtime.Object, error) {
				calls++
				if calls > 1 {
					return true, nil, errors.New("etcd timeout")
				}
				return true, &corev1.NodeList{
					ListMeta: metav1.ListMeta{Continue: "page-2"},
					Items:    []corev1.Node{*providerNode("n1", "azure:///x", map[string]string{"kubernetes.azure.com/cluster": "MC_rg_aks_eastus"})},
				}, nil
			})
			var inv inventory.Inventory
			if err := collectVersions(context.Background(), disc, cs, "team", &inv); err == nil {
				t.Fatal("collectVersions succeeded, want the second page's error")
			}
			if inv.Provider != tt.want {
				t.Errorf("Provider = %q, want %q", inv.Provider, tt.want)
			}
		})
	}
}
