package collect

import (
	"context"
	"fmt"
	"sort"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/kubernetes"

	"github.com/abd-ulbasit/upgradescope/internal/inventory"
)

// collectVersions fills server version, cluster ID (kube-system namespace
// UID), node kubelet versions, and namespaces with team labels. Writes
// are best-effort: fields populated before an error persist even though
// the capability degrades.
func collectVersions(ctx context.Context, disc discovery.DiscoveryInterface, kube kubernetes.Interface, teamLabel string, inv *inventory.Inventory) error {
	sv, err := disc.ServerVersion()
	if err != nil {
		return fmt.Errorf("server version: %w", err)
	}
	inv.ServerVersion = sv.GitVersion

	ks, err := kube.CoreV1().Namespaces().Get(ctx, "kube-system", metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("cluster id (kube-system uid): %w", err)
	}
	inv.ClusterID = string(ks.UID)

	nodeOpts := metav1.ListOptions{Limit: listPageSize}
	for {
		nodes, err := kube.CoreV1().Nodes().List(ctx, nodeOpts)
		if err != nil {
			return fmt.Errorf("list nodes: %w", err)
		}
		for i := range nodes.Items {
			n := &nodes.Items[i]
			inv.Nodes = append(inv.Nodes, inventory.NodeInfo{Name: n.Name, KubeletVersion: n.Status.NodeInfo.KubeletVersion})
		}
		if nodes.Continue == "" {
			break
		}
		nodeOpts.Continue = nodes.Continue
	}
	sort.Slice(inv.Nodes, func(i, j int) bool { return inv.Nodes[i].Name < inv.Nodes[j].Name })

	nsOpts := metav1.ListOptions{Limit: listPageSize}
	for {
		nss, err := kube.CoreV1().Namespaces().List(ctx, nsOpts)
		if err != nil {
			return fmt.Errorf("list namespaces: %w", err)
		}
		for i := range nss.Items {
			ns := &nss.Items[i]
			inv.Namespaces = append(inv.Namespaces, inventory.NamespaceInfo{Name: ns.Name, Team: ns.Labels[teamLabel]})
		}
		if nss.Continue == "" {
			break
		}
		nsOpts.Continue = nss.Continue
	}
	sort.Slice(inv.Namespaces, func(i, j int) bool { return inv.Namespaces[i].Name < inv.Namespaces[j].Name })
	return nil
}
