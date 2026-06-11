package collect

import (
	"context"
	"fmt"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/kubernetes"

	"github.com/abd-ulbasit/upgradescope/internal/inventory"
)

// collectVersions fills server version, cluster ID (kube-system namespace
// UID), node kubelet versions, namespaces with team labels, and observed
// control-plane component versions. Writes are best-effort: fields
// populated before an error persist even though the capability degrades.
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

	return collectControlPlane(ctx, kube, inv)
}

// controlPlaneComponents are the components detected from kube-system pods.
// kubeadm static pods carry component=<name> labels; the pod-name prefix is
// the fallback. kube-proxy runs as a DaemonSet and carries k8s-app=kube-proxy.
var controlPlaneComponents = []string{
	"kube-apiserver", "kube-controller-manager", "kube-scheduler", "kube-proxy",
}

// collectControlPlane fills inv.ControlPlane from kube-system pods: the
// component version is the pod's image tag (the container whose image repo
// basename equals the component name), normalized (build suffix after "-"/"+"
// stripped) and kept only if inventory.ParseVersion accepts it. The result is
// (Component, Version)-deduped and sorted.
//
// Managed control planes (EKS, GKE, AKS, ...) run the apiserver, controller
// manager, and scheduler outside the cluster: no matching pods exist, which
// is NOT an error — inv.ControlPlane stays empty and the engine emits no
// control-plane skew findings.
func collectControlPlane(ctx context.Context, kube kubernetes.Interface, inv *inventory.Inventory) error {
	seen := map[inventory.ComponentVersion]bool{}
	podOpts := metav1.ListOptions{Limit: listPageSize}
	for {
		pods, err := kube.CoreV1().Pods("kube-system").List(ctx, podOpts)
		if err != nil {
			return fmt.Errorf("list kube-system pods: %w", err)
		}
		for i := range pods.Items {
			p := &pods.Items[i]
			comp := classifyControlPlanePod(p.Name, p.Labels)
			if comp == "" {
				continue
			}
			tag, ok := componentImageTag(p.Spec.Containers, comp)
			if !ok {
				continue // no matching container or unparseable tag — skip, never error
			}
			seen[inventory.ComponentVersion{Component: comp, Version: tag}] = true
		}
		if pods.Continue == "" {
			break
		}
		podOpts.Continue = pods.Continue
	}
	for cv := range seen {
		inv.ControlPlane = append(inv.ControlPlane, cv)
	}
	sort.Slice(inv.ControlPlane, func(i, j int) bool {
		a, b := inv.ControlPlane[i], inv.ControlPlane[j]
		if a.Component != b.Component {
			return a.Component < b.Component
		}
		return a.Version < b.Version
	})
	return nil
}

// classifyControlPlanePod maps a kube-system pod to a control-plane
// component via the kubeadm component label, the kube-proxy DaemonSet's
// k8s-app label, or the pod-name prefix; "" means not a control-plane pod.
func classifyControlPlanePod(name string, labels map[string]string) string {
	for _, comp := range controlPlaneComponents {
		if labels["component"] == comp || strings.HasPrefix(name, comp+"-") {
			return comp
		}
	}
	if labels["k8s-app"] == "kube-proxy" {
		return "kube-proxy"
	}
	return ""
}

// componentImageTag extracts the version tag for comp from the container
// whose image repo basename is comp (e.g. ".../eks/kube-proxy:v1.33.0").
// Build suffixes ("v1.33.0-eksbuild.1", "+fips") are stripped; the tag is
// returned only if inventory.ParseVersion accepts the normalized form.
func componentImageTag(containers []corev1.Container, comp string) (string, bool) {
	for _, c := range containers {
		repo, tag := splitImage(c.Image)
		if repo != comp && !strings.HasSuffix(repo, "/"+comp) {
			continue
		}
		if i := strings.IndexAny(tag, "-+"); i >= 0 {
			tag = tag[:i]
		}
		if _, err := inventory.ParseVersion(tag); err != nil {
			continue
		}
		return tag, true
	}
	return "", false
}
