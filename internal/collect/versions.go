package collect

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/kubernetes"

	"github.com/abd-ulbasit/upgradescope/internal/inventory"
)

// collectVersions fills server version, cluster ID (kube-system namespace
// UID), node kubelet and container runtime versions, namespaces with team
// labels, and observed control-plane component versions. Writes are
// best-effort: fields populated before an error persist even though the
// capability degrades.
func collectVersions(ctx context.Context, disc discovery.DiscoveryInterface, kube kubernetes.Interface, teamLabel string, inv *inventory.Inventory) error {
	sv, err := discovery.ToServerVersionInterfaceWithContext(disc).ServerVersionWithContext(ctx)
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
			inv.Nodes = append(inv.Nodes, inventory.NodeInfo{
				Name:             n.Name,
				KubeletVersion:   n.Status.NodeInfo.KubeletVersion,
				ContainerRuntime: n.Status.NodeInfo.ContainerRuntimeVersion,
			})
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
// component version is the pod's image tag (the container whose image is
// the component's, see componentImageTag), normalized (build suffix after
// "-"/"+" stripped) and kept only if inventory.ParseVersion accepts it. The
// result is (Component, Version)-deduped and sorted.
//
// A component pod whose version cannot be read is never dropped silently
// (#169): its skew cannot be judged, so the capability is returned partial
// (a partialError) naming the components, the number of such pods and the
// first one by name, and the versions that were read are still recorded.
// That is a pod running the component's image under a tag that is not a
// version (digest-only, "latest"), or a pod labelled as the component
// (component=, k8s-app=kube-proxy) that runs no image of its name or of
// every component (a wrapper image). A pod only named like a component
// that runs another image (kube-scheduler-extender) is not that component.
//
// Managed control planes (EKS, GKE, AKS, ...) run the apiserver, controller
// manager, and scheduler outside the cluster: no matching pods exist, which
// is NOT an error — inv.ControlPlane stays empty and the engine emits no
// control-plane skew findings.
func collectControlPlane(ctx context.Context, kube kubernetes.Interface, inv *inventory.Inventory) error {
	seen := map[inventory.ComponentVersion]bool{}
	unread := map[string]string{} // pod name → why its version was not read
	unreadComps := map[string]bool{}
	podOpts := metav1.ListOptions{Limit: listPageSize}
	for {
		pods, err := kube.CoreV1().Pods("kube-system").List(ctx, podOpts)
		if err != nil {
			return fmt.Errorf("list kube-system pods: %w", err)
		}
		for i := range pods.Items {
			p := &pods.Items[i]
			comp, labelled := classifyControlPlanePod(p.Name, p.Labels)
			if comp == "" {
				continue
			}
			tag, why := componentImageTag(p.Spec.Containers, comp)
			if why == "" && tag == "" && labelled {
				why = fmt.Sprintf("labelled %s but runs no %s image (%s)", comp, comp, podImages(p.Spec.Containers))
			}
			switch {
			case tag != "":
				seen[inventory.ComponentVersion{Component: comp, Version: tag}] = true
			case why != "":
				unread[p.Name] = why
				unreadComps[comp] = true
			}
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
	if len(unread) == 0 {
		return nil
	}
	first := slices.Min(slices.Collect(maps.Keys(unread)))
	comps := slices.Sorted(maps.Keys(unreadComps))
	return partialError{
		msg: fmt.Sprintf("version not read from %d control-plane pod(s) (%s), first kube-system/%s: %s; their skew was not evaluated",
			len(unread), strings.Join(comps, ", "), first, unread[first]),
		incomplete: true,
		skipped:    comps,
	}
}

// classifyControlPlanePod maps a kube-system pod to a control-plane
// component via the kubeadm component label, the kube-proxy DaemonSet's
// k8s-app label, or the pod-name prefix; "" means not a control-plane pod.
// labelled is true when a label, not only the name, said so.
func classifyControlPlanePod(name string, labels map[string]string) (comp string, labelled bool) {
	for _, comp := range controlPlaneComponents {
		if labels["component"] == comp {
			return comp, true
		}
	}
	if labels["k8s-app"] == "kube-proxy" {
		return "kube-proxy", true
	}
	for _, comp := range controlPlaneComponents {
		if strings.HasPrefix(name, comp+"-") {
			return comp, false
		}
	}
	return "", false
}

// componentArches are the architecture suffixes of per-architecture image
// names ("gke.gcr.io/kube-proxy-amd64", "k8s.gcr.io/kube-scheduler-arm64"),
// which GKE and older kubeadm releases use.
var componentArches = []string{"amd64", "arm64", "arm", "ppc64le", "s390x"}

// allComponentsImages are images that run every control-plane component
// and kube-proxy, tagged with the Kubernetes version: RKE2's static pods
// all run "docker.io/rancher/hardened-kubernetes:v1.34.2-rke2r1-build...".
var allComponentsImages = []string{"hardened-kubernetes"}

// componentImageTag extracts the version tag for comp from the container
// whose image repo basename is comp, comp-<arch> or an image of every
// component (e.g. ".../eks/kube-proxy:v1.33.0",
// "gke.gcr.io/kube-proxy-amd64:v1.32.0-gke.1000", RKE2's
// "rancher/hardened-kubernetes:v1.34.2-rke2r1-build20260101").
// Build suffixes ("v1.33.0-eksbuild.1", "+fips") are stripped; the tag is
// returned only if inventory.ParseVersion accepts the normalized form.
// When comp's image is there but no tag parses, why says so, naming the
// first such image; both are empty when no container runs comp's image.
func componentImageTag(containers []corev1.Container, comp string) (tag, why string) {
	for _, c := range containers {
		repo, t := splitImage(c.Image)
		base := repo[strings.LastIndex(repo, "/")+1:]
		arch, ok := strings.CutPrefix(base, comp+"-")
		if base != comp && !(ok && slices.Contains(componentArches, arch)) && !slices.Contains(allComponentsImages, base) {
			continue
		}
		v := t
		if i := strings.IndexAny(v, "-+"); i >= 0 {
			v = v[:i]
		}
		if _, err := inventory.ParseVersion(v); err == nil {
			return v, ""
		}
		if why != "" {
			continue
		}
		if t == "" {
			why = fmt.Sprintf("%s image %s has no version tag", comp, c.Image)
		} else {
			why = fmt.Sprintf("%s image %s has tag %q, not a version", comp, c.Image, t)
		}
	}
	return "", why
}

// podImages lists a pod's container images for a reason, comma-separated.
func podImages(containers []corev1.Container) string {
	images := make([]string, len(containers))
	for i, c := range containers {
		images[i] = c.Image
	}
	return strings.Join(images, ", ")
}
