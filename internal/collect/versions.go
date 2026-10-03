package collect

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"sort"
	"strconv"
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

	err = collectControlPlane(ctx, kube, inv)
	if len(inv.Nodes) > 0 {
		return err
	}
	return withNoNodes(err)
}

// noNodesReason is the reason of a versions capability that listed no Node.
const noNodesReason = "no nodes listed: kubelet skew and node runtimes not assessed"

// withNoNodes adds the empty Node list (#174) to err, collectControlPlane's
// outcome. An empty list is no error, but the kubelet-skew and node-runtime
// checks then had nothing to check, which must read as not assessed, not as
// passing: the capability is partial, "nodes" in Skipped. Skipped is what
// makes the engine require the gap, as it does for a forbidden Node list,
// which makes versions unavailable, a required gap too: a kubelet past the
// skew policy would be a blocker. An error that is not a partialError (the
// kube-system pods could not be listed) stands, the capability then being
// unavailable.
func withNoNodes(err error) error {
	gap := partialError{msg: noNodesReason, incomplete: true, skipped: []string{"nodes"}}
	if err == nil {
		return gap
	}
	var pe partialError
	if !errors.As(err, &pe) {
		return err
	}
	return partialError{
		msg:        pe.msg + "; " + gap.msg,
		incomplete: true,
		skipped:    slices.Sorted(slices.Values(append(slices.Clone(pe.skipped), gap.skipped...))),
	}
}

// controlPlaneComponents are the components detected from kube-system pods.
// kubeadm static pods carry component=<name> labels, kOps and Talos ones
// k8s-app=<name>; the pod-name prefix is the fallback. kube-proxy runs as a
// DaemonSet and carries k8s-app=kube-proxy.
var controlPlaneComponents = []string{
	"kube-apiserver", "kube-controller-manager", "kube-scheduler", "kube-proxy",
}

// collectControlPlane fills inv.ControlPlane from kube-system pods: the
// component version is the pod's image tag (the container whose image is
// the component's, see componentImageTag), normalized (build suffix after
// "-"/"+" stripped) and kept only if inventory.ParseVersion accepts it. The
// result is (Component, Version)-deduped and sorted, except kube-proxy,
// which is kept per node (the pod's spec.nodeName) so the engine can pair
// it with that node's kubelet (#148).
//
// A component pod whose version cannot be read is never dropped silently
// (#169): its skew cannot be judged, so the capability is returned partial
// (a partialError) naming the components, the number of such pods and the
// first one by name (the first of a Skipped component when there is one,
// the pod to fix), and the versions that were read are still recorded.
// That is a pod running the component's image under a tag that is not a
// version (digest-only, "latest"), or a pod labelled as the component
// (component= or k8s-app=<component>) that runs a vendor image: none of the
// component's name or of every component (a wrapper image, OKE's
// oke-public-kube-proxy). A pod only named like a component that runs
// another image (kube-scheduler-extender) is not that component.
//
// Skipped names only the components whose skew upstream would have told,
// which the engine requires: one whose upstream-named image carries no
// version tag, and a kube-apiserver, kube-controller-manager or
// kube-scheduler pod whose version is not read for any reason (a
// self-hosted control plane runs upstream images; a vendor one there is a
// deliberate replacement whose skew still matters). A kube-proxy pod on a
// vendor image is named in the reason only: platforms ship it that way,
// managing and upgrading it themselves (OKE pins oke-public-kube-proxy by
// digest), and upstream's image would not have told its version either.
// Partial with an empty Skipped is then an optional, disclosed gap.
//
// Managed control planes (EKS, GKE, AKS, ...) run the apiserver, controller
// manager, and scheduler outside the cluster: no matching pods exist, which
// is NOT an error — inv.ControlPlane stays empty and the engine emits no
// control-plane skew findings.
func collectControlPlane(ctx context.Context, kube kubernetes.Interface, inv *inventory.Inventory) error {
	seen := map[inventory.ComponentVersion]bool{}
	unread := map[string]string{} // pod name → why its version was not read
	unreadComps := map[string]bool{}
	requiredComps := map[string]bool{} // the Skipped ones, see above
	var requiredPods []string          // the unread pods of requiredComps
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
			required := why != "" // an upstream-named image with no version tag
			if why == "" && tag == "" && labelled {
				why = fmt.Sprintf("labelled %s but runs a vendor image whose version is not read (%s)", comp, podImages(p.Spec.Containers))
				required = comp != "kube-proxy"
			}
			switch {
			case tag != "":
				cv := inventory.ComponentVersion{Component: comp, Version: tag}
				if comp == "kube-proxy" {
					cv.Node = p.Spec.NodeName
				}
				seen[cv] = true
			case why != "":
				unread[p.Name] = why
				unreadComps[comp] = true
				if required {
					requiredComps[comp] = true
					requiredPods = append(requiredPods, p.Name)
				}
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
		if a.Version != b.Version {
			return a.Version < b.Version
		}
		return a.Node < b.Node
	})
	if len(unread) == 0 {
		return nil
	}
	// Name the pod to fix: the first of a required component, else the
	// first of all.
	var first string
	var skipped []string
	if len(requiredPods) > 0 {
		first = slices.Min(requiredPods)
		skipped = slices.Sorted(maps.Keys(requiredComps))
	} else {
		first = slices.Min(slices.Collect(maps.Keys(unread)))
	}
	comps := slices.Sorted(maps.Keys(unreadComps))
	return partialError{
		msg: fmt.Sprintf("version not read from %d control-plane pod(s) (%s), first kube-system/%s: %s; their skew was not evaluated",
			len(unread), strings.Join(comps, ", "), first, unread[first]),
		incomplete: true,
		skipped:    skipped,
	}
}

// classifyControlPlanePod maps a kube-system pod to a control-plane
// component via the kubeadm component label, the k8s-app label (the
// kube-proxy DaemonSet's, and kOps' and Talos' static pods'), or the
// pod-name prefix; "" means not a control-plane pod. labelled is true when
// a label, not only the name, said so.
func classifyControlPlanePod(name string, labels map[string]string) (comp string, labelled bool) {
	for _, key := range []string{"component", "k8s-app"} {
		if slices.Contains(controlPlaneComponents, labels[key]) {
			return labels[key], true
		}
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
// all run "docker.io/rancher/hardened-kubernetes:v1.34.2-rke2r1-build...",
// and clusters before 1.19 could run "k8s.gcr.io/hyperkube:v1.18.20"
// (per-architecture too, "hyperkube-amd64").
var allComponentsImages = []string{"hardened-kubernetes", "hyperkube"}

// schedulerPluginsPath is the registry path element under which
// kubernetes-sigs/scheduler-plugins publishes its build of kube-scheduler
// ("registry.k8s.io/scheduler-plugins/kube-scheduler:v0.29.7"), which its
// documented single-scheduler install swaps into the kube-scheduler static
// pod. The tag is that project's version, not Kubernetes': see
// schedulerPluginsVersion.
const schedulerPluginsPath = "scheduler-plugins"

// schedulerPluginsVersion maps a scheduler-plugins tag, suffix stripped, to
// the Kubernetes version it is compiled with: "the minor version of the
// scheduler-plugins matches the minor version of the k8s client packages
// that it is compiled with" (its README), and a one- or two-digit patch is
// the Kubernetes patch (v0.29.7 is built on v1.29.7). A three-digit patch
// (v0.18.800) changed plugin code only, so only the minor is known
// (v1.18.0). ok is false for anything but v0.<minor>.<patch>.
func schedulerPluginsVersion(tag string) (string, bool) {
	parts := strings.Split(strings.TrimPrefix(tag, "v"), ".")
	if len(parts) != 3 || parts[0] != "0" {
		return "", false
	}
	n := make([]int, 2)
	for i, p := range parts[1:] {
		v, err := strconv.Atoi(p)
		if err != nil || v < 0 || p != strconv.Itoa(v) {
			return "", false
		}
		n[i] = v
	}
	if n[1] >= 100 {
		n[1] = 0
	}
	return fmt.Sprintf("v1.%d.%d", n[0], n[1]), true
}

// imageOf reports whether an image repo basename is name or a
// per-architecture build of it (name-<arch>).
func imageOf(base, name string) bool {
	arch, ok := strings.CutPrefix(base, name+"-")
	return base == name || ok && slices.Contains(componentArches, arch)
}

// componentImageTag extracts the version tag for comp from the container
// whose image repo basename is comp, comp-<arch> or an image of every
// component (e.g. ".../eks/kube-proxy:v1.33.0",
// "gke.gcr.io/kube-proxy-amd64:v1.32.0-gke.1000", RKE2's
// "rancher/hardened-kubernetes:v1.34.2-rke2r1-build20260101"); one under
// schedulerPluginsPath is read through schedulerPluginsVersion.
// Build suffixes ("v1.33.0-eksbuild.1", "+fips", and VMware TKG's
// "v1.28.7_vmware.1", where "_" stands for the "+" a tag cannot hold) are
// stripped; the tag is returned only if inventory.ParseVersion accepts the
// normalized form.
// When comp's image is there but no tag parses, why says so, naming the
// first such image; both are empty when no container runs comp's image.
func componentImageTag(containers []corev1.Container, comp string) (tag, why string) {
	for _, c := range containers {
		repo, t := splitImage(c.Image)
		path := strings.Split(repo, "/")
		base := path[len(path)-1]
		ofAll := slices.ContainsFunc(allComponentsImages, func(all string) bool { return imageOf(base, all) })
		if !imageOf(base, comp) && !ofAll {
			continue
		}
		plugins := slices.Contains(path[:len(path)-1], schedulerPluginsPath)
		v := t
		if i := strings.IndexAny(v, "-+_"); i >= 0 {
			v = v[:i]
		}
		// No Kubernetes release is 0.x. A 0.x kube-scheduler is a
		// scheduler-plugins build, also through a mirror that dropped its
		// path; any other 0.x tag is unreadable, not a version to judge.
		pv, perr := inventory.ParseVersion(v)
		zero := perr == nil && pv.Major == 0
		if plugins || (zero && comp == "kube-scheduler") {
			if k, ok := schedulerPluginsVersion(v); ok {
				return k, ""
			}
		} else if perr == nil && !zero {
			return v, ""
		}
		if why != "" {
			continue
		}
		switch {
		case t == "":
			why = fmt.Sprintf("%s image %s has no version tag", comp, c.Image)
		case plugins:
			why = fmt.Sprintf("%s image %s has tag %q, not a scheduler-plugins version (v0.<minor>.<patch>)", comp, c.Image, t)
		case zero:
			why = fmt.Sprintf("%s image %s has tag %q, not a Kubernetes version (no release is 0.x)", comp, c.Image, t)
		default:
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
