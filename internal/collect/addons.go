package collect

import (
	"cmp"
	"context"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"sort"
	"strings"

	"github.com/Masterminds/semver/v3"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/abd-ulbasit/upgradescope/internal/inventory"
	"github.com/abd-ulbasit/upgradescope/registry"
)

// nsImage is one container image observed in a namespace.
type nsImage struct {
	Namespace string
	Image     string
}

// appLabels are the pod labels that can name an add-on (#18): Helm's
// chart label and the recommended app.kubernetes.io labels, which charts
// installed without the Helm SDK (Argo CD's helm template, kustomize,
// kubectl apply of a render) keep.
type appLabels struct {
	chart   string // helm.sh/chart: "<chart name>-<chart version>"
	name    string // app.kubernetes.io/name
	version string // app.kubernetes.io/version
	partOf  string // app.kubernetes.io/part-of
}

func appLabelsOf(labels map[string]string) appLabels {
	return appLabels{
		chart:   labels["helm.sh/chart"],
		name:    labels["app.kubernetes.io/name"],
		version: labels["app.kubernetes.io/version"],
		partOf:  labels["app.kubernetes.io/part-of"],
	}
}

// labelledPod is a pod (or, in files mode, a pod template) carrying
// appLabels, with its container and init-container images.
type labelledPod struct {
	Namespace string
	Labels    appLabels
	Images    []string
}

// addOnEvidence is what add-on detection reads.
type addOnEvidence struct {
	images             []nsImage     // every container and init-container image
	labelled           []labelledPod // the pods among them that carry appLabels
	releases           []inventory.HelmRelease
	ingressControllers []string // IngressClass spec.controller values
}

// addPod adds one pod's (or pod template's) images, and the pod to
// ev.labelled when its labels could name an add-on.
func (ev *addOnEvidence) addPod(namespace string, labels map[string]string, images []string) {
	for _, img := range images {
		ev.images = append(ev.images, nsImage{Namespace: namespace, Image: img})
	}
	if l := appLabelsOf(labels); l != (appLabels{}) {
		ev.labelled = append(ev.labelled, labelledPod{Namespace: namespace, Labels: l, Images: images})
	}
}

// collectAddOns lists pod images (containers + init containers) and
// labels, and IngressClass controllers, and runs the pure matcher over
// them, already-collected Helm releases (inv.HelmReleases — the helm step
// runs first), and the registry. IngressClasses only add evidence, so a
// role that cannot list them leaves add-ons assessed from pods, partially;
// an apiserver without networking.k8s.io/v1 (before 1.19) has none.
func collectAddOns(ctx context.Context, kube kubernetes.Interface, addons []registry.AddOn, inv *inventory.Inventory) error {
	ev := addOnEvidence{releases: inv.HelmReleases}
	opts := metav1.ListOptions{Limit: listPageSize}
	for {
		pods, err := kube.CoreV1().Pods(metav1.NamespaceAll).List(ctx, opts)
		if err != nil {
			return fmt.Errorf("list pods: %w", err)
		}
		// Extract images and labels per page so only those are retained —
		// never the accumulated PodList of a large cluster.
		for i := range pods.Items {
			p := &pods.Items[i]
			var images []string
			for _, c := range p.Spec.InitContainers {
				images = append(images, c.Image)
			}
			for _, c := range p.Spec.Containers {
				images = append(images, c.Image)
			}
			ev.addPod(p.Namespace, p.Labels, images)
		}
		if pods.Continue == "" {
			break
		}
		opts.Continue = pods.Continue
	}
	var classErr error
	opts = metav1.ListOptions{Limit: listPageSize}
	for {
		classes, err := kube.NetworkingV1().IngressClasses().List(ctx, opts)
		if err != nil {
			if !apierrors.IsNotFound(err) {
				classErr = partialError{incomplete: true, skipped: []string{"networking.k8s.io/v1 ingressclasses"},
					msg: fmt.Sprintf("list ingressclasses: %v; add-ons were detected from pods and Helm releases only", err)}
			}
			break
		}
		for _, c := range classes.Items {
			ev.ingressControllers = append(ev.ingressControllers, c.Spec.Controller)
		}
		if classes.Continue == "" {
			break
		}
		opts.Continue = classes.Continue
	}
	var unrec []string
	inv.AddOns, unrec = matchAddOns(ev, addons)
	setUnrecognized(inv, unrec)
	return classErr
}

// splitImage strips digest then tag:
// "reg:5000/repo/app:v1.2@sha256:…" → ("reg:5000/repo/app", "v1.2").
// The tag colon must come after the last slash so registry ports survive.
func splitImage(image string) (repo, tag string) {
	if at := strings.Index(image, "@"); at >= 0 {
		image = image[:at]
	}
	slash := strings.LastIndex(image, "/")
	if colon := strings.LastIndex(image, ":"); colon > slash {
		return image[:colon], image[colon+1:]
	}
	return image, ""
}

// imageRef is a container image reference normalised for matching: the
// registry host (Docker Hub spelled "docker.io"), the repository path below
// it ("library/" added for Docker Hub official images) and the tag.
type imageRef struct {
	host, path, tag string
}

// parseImage normalises an image reference the way the container runtime
// resolves it: the first segment is a registry host only when it contains
// "." or ":" or is "localhost"; otherwise the image lives on Docker Hub.
// The digest is dropped.
func parseImage(image string) imageRef {
	repo, tag := splitImage(image)
	host, path, ok := strings.Cut(repo, "/")
	if !ok || (!strings.ContainsAny(host, ".:") && host != "localhost") {
		host, path = "docker.io", repo
	}
	switch host {
	case "index.docker.io", "registry-1.docker.io":
		host = "docker.io"
	}
	if host == "docker.io" && !strings.Contains(path, "/") {
		path = "library/" + path
	}
	return imageRef{host: host, path: path, tag: tag}
}

// pathMatches reports whether the repository path ends with the matcher on
// whole segments, so "ingress-nginx/controller" matches the canonical path
// and every mirror or pull-through-cache path that keeps it as a suffix
// ("registry-k8s-io/ingress-nginx/controller").
func pathMatches(path, matcher string) bool {
	return path == matcher || strings.HasSuffix(path, "/"+matcher)
}

// imageMatches applies one registry image matcher. A provider build
// (registry.ProviderBuildPrefixes: GKE's and AKS's own builds of Calico,
// Cilium, Istio, …) follows the provider's support policy, so host-less
// upstream matchers never claim it; only a matcher naming the provider
// location does, on the full reference or a mirror path ending with it.
func imageMatches(ref imageRef, matcher string) bool {
	full := ref.host + "/" + ref.path
	if registry.IsProviderBuild(matcher) {
		return pathMatches(full, matcher)
	}
	return !registry.IsProviderBuild(full) && pathMatches(ref.path, matcher)
}

// versionRe finds a version anywhere in an image tag or chart appVersion:
// "nginx-1.9.4-hardened1" → "1.9.4". A semver pre-release ("-rc.1") is kept;
// distro and build suffixes ("-debian-12-r0", "-eksbuild.4") are not.
var versionRe = regexp.MustCompile(`\d+\.\d+(\.\d+)?(-(alpha|beta|rc)(\.?\d+)*)?`)

// versionFromTag is the single normalization point for every value that
// lands in AddOnInstance.Version, so registry cycles, compat ranges and
// findings compare against one uniform form; "" when the tag carries no
// version ("latest", a digest-only reference).
func versionFromTag(tag string) string {
	return versionRe.FindString(tag)
}

// versionLess orders detected versions for the conservative-oldest pick:
// semver compare when both sides parse ("1.9.4" < "1.10.0"), falling back
// to lexicographic for unparseable versions. Plain string `<` would rank
// "1.10.0" before "1.9.4" and mask the older install's EOL risk.
func versionLess(a, b string) bool {
	av, aerr := semver.NewVersion(a)
	bv, berr := semver.NewVersion(b)
	if aerr == nil && berr == nil {
		return av.LessThan(bv)
	}
	return a < b
}

// olderVersion returns the older of two versions, ignoring "": the
// conservative pick when the pods or releases of one install disagree.
func olderVersion(cur, v string) string {
	if v != "" && (cur == "" || versionLess(v, cur)) {
		return v
	}
	return cur
}

// imageAddOns returns the IDs of the add-ons whose image matchers claim
// ref, in registry order.
func imageAddOns(ref imageRef, addons []registry.AddOn) []string {
	var ids []string
	for _, a := range addons {
		if slices.ContainsFunc(a.Matchers.Images, func(m string) bool { return imageMatches(ref, m) }) {
			ids = append(ids, a.ID)
		}
	}
	return ids
}

// chartVersionRe finds where the chart version starts in a helm.sh/chart
// label value ("<name>-<version>", "+" written as "_"): the first "-"
// followed by a version.
var chartVersionRe = regexp.MustCompile(`-v?\d+(\.\d+)*([-_.+]|$)`)

// chartName returns the chart name in a helm.sh/chart label value
// ("cert-manager-v1.15.3" → "cert-manager"), "" when it carries no version.
func chartName(label string) string {
	loc := chartVersionRe.FindStringIndex(label)
	if loc == nil || loc[0] == 0 {
		return ""
	}
	return label[:loc[0]]
}

// labelAddOn returns the add-on a pod's labels name, and its version when
// the labels give one that is trustworthy (#18). The add-on is the first
// one named by, in order: app.kubernetes.io/name (what the pod is), the
// helm.sh/chart chart name, app.kubernetes.io/part-of. A label names an
// add-on when it equals its registry ID or one of its chart matchers. The
// version is app.kubernetes.io/version, normalised like an image tag, and
// only when app.kubernetes.io/name named the add-on, or the chart did and
// the pod has no name label: a component of a larger app (part-of, or a
// chart's other workloads) may carry its own version. A chart version is
// never used: registry cycles speak app versions.
func labelAddOn(l appLabels, addons []registry.AddOn) (id, version string) {
	names := func(a registry.AddOn, v string) bool {
		return v != "" && (v == a.ID || slices.Contains(a.Matchers.Charts, v))
	}
	chart := chartName(l.chart)
	for _, label := range []string{l.name, chart, l.partOf} {
		for _, a := range addons {
			if !names(a, label) {
				continue
			}
			if names(a, l.name) || l.name == "" && names(a, chart) {
				version = versionFromTag(l.version)
			}
			return a.ID, version
		}
	}
	return "", ""
}

// vendorBuilds maps an add-on to the registry entries for vendor builds of
// it, which may keep its labels and controller name (RKE2's chart keeps
// the controller name) but follow the vendor's lifecycle.
var vendorBuilds = map[string][]string{
	"ingress-nginx": {"rke2-ingress-nginx", "aks-app-routing-nginx"},
}

// ingressClassAddOns maps an IngressClass spec.controller to the add-on it
// names, followed by the add-ons that can also serve a class of that
// controller name: its vendor builds, and Traefik, whose Kubernetes Ingress
// NGINX provider (v3.6.2+) serves k8s.io/ingress-nginx classes by default,
// the migration path off the retired controller that keeps the class. The
// IngressClass is evidence only when none of them was detected otherwise,
// deliberately: with one of them running, the class most likely belongs to
// it, and an upstream controller beside it that no matcher or label names
// is missed (its image is still listed as unrecognized).
var ingressClassAddOns = map[string][]string{
	"k8s.io/ingress-nginx": {"ingress-nginx", "rke2-ingress-nginx", "aks-app-routing-nginx", "traefik"},
}

// matchAddOns is pure: pod images and labels + helm releases + IngressClass
// controllers + registry → detected add-on instances, one per add-on and
// namespace, sorted by ID then namespace, and the deduped, sorted
// list of image repos no image matcher claims (registry gap visibility —
// never findings, spec §9; an add-on found from labels may still run one).
//
// Evidence, strongest first, each a Source:
//   - "chart": a Helm release of a chart matcher; its appVersion is the
//     install's version.
//   - "image": an image matcher; the version is the tag's.
//   - "labels": the pod's labels name the add-on (see labelAddOn), for a
//     pod none of whose images that add-on's matchers claim but one of
//     which no matcher claims at all: the container the labels are about
//     (an injected sidecar matching another add-on does not stop it; a
//     vendor build (vendorBuilds) whose image matched its own entry does,
//     whatever unmatched sidecars run beside it). A pod running a
//     provider build (registry.IsProviderBuild) is never claimed through
//     its labels: its support follows the provider, not upstream (#110).
//   - "ingressclass": an IngressClass whose controller names the add-on
//     (ingressClassAddOns) when nothing else found it nor another
//     controller that can serve the class. It is cluster-scoped (no
//     namespace) and has no version, so a product retired as a whole
//     (ingress-nginx) is still end-of-life, and a per-release-line product
//     gets no lifecycle verdict.
//
// Each namespace is its own install, judged at its own version: within
// one, the oldest version wins, and a Helm release's appVersion over image
// tags and labels (sidecars and stale pods lag the release). Neither
// crosses namespaces, so a mesh mid-upgrade or a newer release elsewhere
// cannot hide an older install, nor lend its version to one. The limit is
// within a namespace: an istioctl canary revision running an older
// istio/pilot beside a newer istiod Helm release in istio-system is
// reported at the release's version, and the older revision is not judged.
// Across namespaces the other way round: a release whose pods run in
// another namespace (a chart with a namespace override) is reported twice,
// the release's namespace at its appVersion and the pods' namespace at
// their image tag, which may not track the app version.
func matchAddOns(ev addOnEvidence, addons []registry.AddOn) ([]inventory.AddOnInstance, []string) {
	type evidence struct {
		source  string // "image" | "labels" | "chart" | "ingressclass"
		version string // app version
		chart   string // chart version, chart evidence only
	}
	type install struct{ id, ns string }
	byInstall := map[install][]evidence{}
	unmatched := map[string]bool{}

	for _, img := range ev.images {
		ref := parseImage(img.Image)
		ids := imageAddOns(ref, addons)
		for _, id := range ids {
			in := install{id, img.Namespace}
			byInstall[in] = append(byInstall[in], evidence{source: "image", version: versionFromTag(ref.tag)})
		}
		if len(ids) == 0 {
			unmatched[ref.host+"/"+ref.path] = true
		}
	}

	for _, p := range ev.labelled {
		id, version := labelAddOn(p.Labels, addons)
		if id == "" {
			continue
		}
		claimed, unclaimed, provider := false, false, false
		for _, img := range p.Images {
			ref := parseImage(img)
			ids := imageAddOns(ref, addons)
			claimed = claimed || slices.Contains(ids, id) ||
				slices.ContainsFunc(ids, func(v string) bool { return slices.Contains(vendorBuilds[id], v) })
			unclaimed = unclaimed || len(ids) == 0
			provider = provider || registry.IsProviderBuild(ref.host+"/"+ref.path)
		}
		if claimed || !unclaimed || provider {
			continue
		}
		in := install{id, p.Namespace}
		byInstall[in] = append(byInstall[in], evidence{source: "labels", version: version})
	}

	for _, rel := range ev.releases {
		for _, a := range addons {
			for _, chart := range a.Matchers.Charts {
				if rel.ChartName == chart {
					in := install{a.ID, rel.Namespace}
					byInstall[in] = append(byInstall[in], evidence{
						source:  "chart",
						version: versionFromTag(rel.AppVersion),
						chart:   strings.TrimPrefix(rel.ChartVersion, "v"),
					})
				}
			}
		}
	}

	detected := map[string]bool{}
	for in := range byInstall {
		detected[in.id] = true
	}
	for _, c := range ev.ingressControllers {
		ids := ingressClassAddOns[c]
		if len(ids) == 0 || slices.ContainsFunc(ids, func(id string) bool { return detected[id] }) ||
			!slices.ContainsFunc(addons, func(a registry.AddOn) bool { return a.ID == ids[0] }) {
			continue
		}
		in := install{ids[0], ""}
		byInstall[in] = append(byInstall[in], evidence{source: "ingressclass"})
	}

	strength := map[string]int{"ingressclass": 1, "labels": 2, "image": 3, "chart": 4}
	var out []inventory.AddOnInstance
	for in, evs := range byInstall {
		inst := inventory.AddOnInstance{ID: in.id}
		var podVersion, appVersion string
		for _, e := range evs {
			if strength[e.source] > strength[inst.Source] {
				inst.Source = e.source
			}
			if e.source == "chart" {
				appVersion = olderVersion(appVersion, e.version)
				inst.ChartVersion = olderVersion(inst.ChartVersion, e.chart)
			} else {
				podVersion = olderVersion(podVersion, e.version)
			}
		}
		// "" is a namespace too: a manifest object's left unset.
		if inst.Source != "ingressclass" {
			inst.Namespaces = []string{in.ns}
		}
		// A release's appVersion is authoritative; a chart without one
		// falls back to the image tag or labels, never to the chart version.
		inst.Version = cmp.Or(appVersion, podVersion)
		out = append(out, inst)
	}
	sort.Slice(out, func(i, j int) bool {
		return cmp.Or(cmp.Compare(out[i].ID, out[j].ID), slices.Compare(out[i].Namespaces, out[j].Namespaces)) < 0
	})

	return out, slices.Sorted(maps.Keys(unmatched))
}

// setUnrecognized records sorted unrecognized image repos in inv, capped
// at inventory.MaxUnrecognizedImages, counting the ones the cap drops.
func setUnrecognized(inv *inventory.Inventory, repos []string) {
	inv.UnrecognizedImages, inv.UnrecognizedImagesOmitted = repos, 0
	if n := len(repos) - inventory.MaxUnrecognizedImages; n > 0 {
		inv.UnrecognizedImages, inv.UnrecognizedImagesOmitted = repos[:inventory.MaxUnrecognizedImages], n
	}
}
