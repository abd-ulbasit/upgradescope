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
	corev1 "k8s.io/api/core/v1"
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
	gitops             []inventory.GitOpsChart // charts Argo CD and Flux deploy
	ingressControllers []string                // IngressClass spec.controller values
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
// runs first), and the registry. A list that fails does not discard the
// rest (#199): without pods, add-ons are matched from Helm releases and
// IngressClasses; without IngressClasses (an apiserver before 1.19 has
// none, which is not a failure) from pods and releases. A failure leaves
// the capability partial, naming what went unread, unless nothing else was
// read either (no releases, no GitOps chart sources, no IngressClasses):
// then it is not assessed.
// Pods are what finds an add-on installed any other way, so
// inventory.SkippedPods keeps the gap required; IngressClasses only add
// evidence.
func collectAddOns(ctx context.Context, kube kubernetes.Interface, addons []registry.AddOn, inv *inventory.Inventory) error {
	return collectAddOnsFrom(ctx, kube, addons, inv, &kubeSystemPods{}, nil)
}

// kubeSystemPods is what the add-ons capability takes from the versions
// capability's list of the kube-system pods (#227): their images and
// labels, once versions has listed all of them. The control-plane
// components are found in that list, and at 2,000 nodes kube-system holds
// about 4,000 pods (a CNI and a kube-proxy pod per node), so listing them
// again in the all-namespaces list read that share of the response twice a
// tick. Where versions did not list them (it failed before or in
// the list, or ran no step), read is false and the add-ons list them
// themselves.
type kubeSystemPods struct {
	read bool
	ev   addOnEvidence // images and labelled only
}

// podContainerImages lists a pod's init-container and container images.
func podContainerImages(p *corev1.Pod) []string {
	var images []string
	for _, c := range p.Spec.InitContainers {
		images = append(images, c.Image)
	}
	for _, c := range p.Spec.Containers {
		images = append(images, c.Image)
	}
	return images
}

// collectAddOnsFrom is collectAddOns taking the kube-system pods from sysPods
// when versions listed them, and then listing the other namespaces only:
// the server leaves kube-system out of the response, and a server that did
// not would still not count its pods twice. A list of the other namespaces
// that fails on its first page is a failure of the pods exactly as before:
// the kube-system evidence is dropped, so the gap reason, the add-ons and
// the unavailable check are those of a list of every pod that failed. A
// later page failing keeps the pages read, as it always did. A server or
// proxy that rejects the field selector with a 400 is asked once more
// without it, and its kube-system pods are skipped here.
func collectAddOnsFrom(ctx context.Context, kube kubernetes.Interface, addons []registry.AddOn, inv *inventory.Inventory, sysPods *kubeSystemPods, pass *PodPassCache) error {
	ev := addOnEvidence{releases: inv.HelmReleases, gitops: inv.GitOpsCharts}
	var failures, skipped []string
	var podErr error
	classesRead := false
	opts := metav1.ListOptions{Limit: listPageSize} // then sized by pageLimit
	if sysPods.read {
		ev.images, ev.labelled = sysPods.ev.images, sysPods.ev.labelled
		opts.FieldSelector = "metadata.namespace!=" + metav1.NamespaceSystem
	}
	// The pods outside kube-system are listed only every few collections,
	// and reused in between (PodPassCache, #228); the kube-system pods are
	// versions' list of this collection, so only a collection that has it
	// can do without the pass.
	listPods := true
	sig := installSignature(inv.HelmReleases, inv.GitOpsCharts, addons)
	if sysPods.read {
		if images, labelled, age, ok := pass.reuse(sig); ok {
			ev.images, ev.labelled = slices.Concat(ev.images, images), slices.Concat(ev.labelled, labelled)
			inv.AddOnEvidenceAgeSeconds = age
			listPods = false
		}
	}
	passStart := pass.clock()
	if listPods {
		// The held pass is of no use from here on, whatever the list does:
		// drop it before reading the new one, so the peak is the new pass
		// alone and never the old and the new together, and a pass that
		// fails part-way leaves nothing held.
		pass.forget()
	}
	for listPods {
		pods, err := kube.CoreV1().Pods(metav1.NamespaceAll).List(ctx, opts)
		if err != nil && opts.Continue == "" && opts.FieldSelector != "" && apierrors.IsBadRequest(err) {
			// A server or proxy that refuses the field selector: list every
			// pod as before, skipping kube-system's below.
			opts.FieldSelector = ""
			continue
		}
		if err != nil {
			podErr = err
			if opts.Continue == "" {
				ev.images, ev.labelled = nil, nil // no pod was read, kube-system's included
			}
			failures = append(failures, fmt.Sprintf("list pods: %v", err))
			skipped = append(skipped, inventory.SkippedPods)
			break
		}
		// Extract images and labels per page so only those are retained —
		// never the accumulated PodList of a large cluster.
		largest := 0
		for i := range pods.Items {
			p := &pods.Items[i]
			largest = max(largest, p.Size())
			if sysPods.read && p.Namespace == metav1.NamespaceSystem {
				continue // already counted from versions' list
			}
			ev.addPod(p.Namespace, p.Labels, podContainerImages(p))
		}
		if pods.Continue == "" {
			break
		}
		opts.Continue, opts.Limit = pods.Continue, pageLimit(largest, podPageSize)
	}
	if listPods {
		// Only a pass that read every pod is kept; a failed or partial one
		// drops the earlier evidence too, and the next collection lists
		// every pod again.
		if podErr == nil {
			pass.record(ev, passStart, sig)
		} else {
			pass.forget()
		}
	}
	opts = metav1.ListOptions{Limit: listPageSize}
	for {
		classes, err := kube.NetworkingV1().IngressClasses().List(ctx, opts)
		if err != nil {
			if !apierrors.IsNotFound(err) {
				failures = append(failures, fmt.Sprintf("list ingressclasses: %v", err))
				skipped = append(skipped, "networking.k8s.io/v1 ingressclasses")
			}
			break
		}
		classesRead = true
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
	if podErr != nil && len(ev.releases) == 0 && len(ev.gitops) == 0 && !classesRead {
		return fmt.Errorf("list pods: %w", podErr) // nothing was read: not assessed, not partial
	}
	if len(failures) == 0 {
		return nil
	}
	var read []string // what the add-ons were detected from
	if podErr == nil {
		read = append(read, "pods")
	}
	if len(ev.releases) > 0 || inv.Capabilities[inventory.CapHelm].Available {
		read = append(read, "Helm releases")
	}
	if len(ev.gitops) > 0 {
		read = append(read, "GitOps chart sources")
	}
	if classesRead {
		read = append(read, "IngressClasses")
	}
	slices.Sort(skipped)
	return partialError{incomplete: true, skipped: skipped,
		msg: fmt.Sprintf("%s; add-ons were detected from %s only", strings.Join(failures, "; "), strings.Join(read, " and "))}
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

// imageMatches applies one registry image matcher. A provider build
// (registry.ProviderBuildPrefixes: GKE's and AKS's own builds of Calico,
// Cilium, Istio, …) follows the provider's support policy, so host-less
// upstream matchers never claim it; only a matcher naming the provider
// location does, on the full reference or a mirror path ending with it. A
// matcher with a tag pattern (registry.SplitTagPattern) matches only the
// tags it names.
func imageMatches(ref imageRef, matcher string) bool {
	matcher, pattern := registry.SplitTagPattern(matcher)
	if pattern != "" && !registry.TagMatches(ref.tag, pattern) {
		return false
	}
	full := ref.host + "/" + ref.path
	if registry.IsProviderBuild(matcher) {
		return registry.PathMatches(full, matcher)
	}
	return !registry.IsProviderBuild(full) && registry.PathMatches(ref.path, matcher)
}

// versionRe finds a version anywhere in an image tag or chart appVersion:
// "nginx-1.9.4-hardened1" → "1.9.4". A semver pre-release ("-rc.1") is kept;
// distro and build suffixes ("-debian-12-r0", "-eksbuild.4") are not.
var versionRe = regexp.MustCompile(`\d+\.\d+(\.\d+)?(-(alpha|beta|rc)(\.?\d+)*)?`)

// versionFromTag is the single normalization point for every value that
// lands in AddOnInstance.Version, so registry cycles, compat ranges and
// findings compare against one uniform form; "" when the tag carries no
// version ("latest", a digest-only reference), or none that is one: a
// pod's creator chooses the tag, and a "version" of 17 KiB of digits is
// not a release of anything, but would make the server refuse the
// inventory (#268), so one over maxVersionBytes reads as no version.
func versionFromTag(tag string) string {
	v := versionRe.FindString(tag)
	if len(v) > maxVersionBytes {
		return ""
	}
	return v
}

// maxVersionBytes is the longest version versionFromTag and
// exactChartVersion return: well past any real one ("1.31.1-rc.1" is 11
// bytes), well under the limit the server puts on a string.
const maxVersionBytes = 128

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

// releaseLine is a detected version's release line, its first two
// components: "1.31.1" → "1.31". versionFromTag guarantees there are two.
func releaseLine(v string) string {
	major, rest, _ := strings.Cut(v, ".")
	minor, _, _ := strings.Cut(rest, ".")
	minor, _, _ = strings.Cut(minor, "-")
	return major + "." + minor
}

// exactChartVersion returns a chart version as a single version without a
// leading "v", or "" when it is a constraint or a tag: a GitOps resource's
// chart version, unlike a Helm release's, is whatever its author wrote.
func exactChartVersion(v string) string {
	v = strings.TrimPrefix(v, "v")
	if len(v) > maxVersionBytes {
		return ""
	}
	if _, err := semver.StrictNewVersion(v); err != nil {
		return ""
	}
	return v
}

// olderVersion returns the older of two versions, ignoring "": the
// conservative pick when the pods or releases of one install disagree.
func olderVersion(cur, v string) string {
	if v != "" && (cur == "" || versionLess(v, cur)) {
		return v
	}
	return cur
}

// imageClaim is an add-on an image belongs to, and the add-on's version
// the image gives.
type imageClaim struct {
	id, version string
	// byTag: the claim is the tag's own version, not a component image's
	// product line, so a claim without one may take its pod's label
	// version (matchAddOns).
	byTag bool
}

// imageClaims returns the add-ons whose image matchers claim ref, in
// registry order, each with the version ref gives it: the tag's, or, for a
// component image (registry.ComponentImage), the product line that ships
// the tag's line ("" when the entry does not map it). A matcher with a
// tag pattern claims ref ahead of path-only matchers: when one matches,
// only the entries matching that way claim it, so RKE2's "-hardenedN"
// build is not also upstream ingress-nginx's (#265).
func imageClaims(ref imageRef, addons []registry.AddOn) []imageClaim {
	var tagged, plain []imageClaim
	tagVersion := versionFromTag(ref.tag)
	for _, a := range addons {
		byTag, byPath := false, false
		for _, m := range a.Matchers.Images {
			if imageMatches(ref, m) {
				_, pattern := registry.SplitTagPattern(m)
				byTag, byPath = byTag || pattern != "", byPath || pattern == ""
			}
		}
		switch {
		case byTag:
			tagged = append(tagged, imageClaim{a.ID, tagVersion, true})
		case byPath:
			plain = append(plain, imageClaim{a.ID, tagVersion, true})
		default:
			if i := slices.IndexFunc(a.Matchers.Components, func(c registry.ComponentImage) bool { return imageMatches(ref, c.Image) }); i >= 0 {
				plain = append(plain, imageClaim{a.ID, a.Matchers.Components[i].ProductLine(tagVersion), false})
			}
		}
	}
	if len(tagged) > 0 {
		return tagged
	}
	return plain
}

// untaggedClaim settles an image without a tag (a digest-only reference),
// which no tag pattern can match, so that a repository two products share
// (RKE2's "-hardenedN" and RKE1's "-rancherN" builds of
// rancher/nginx-ingress-controller, #265) is not given to the path-only
// entry by default when the evidence says otherwise: the first of named
// (the entries the pod's own labels name, then those a Helm release in its
// namespace names) with a tag-qualified matcher of ref's repository claims
// it, at that entry's version in named. ok is false when none does, and
// the image keeps its imageClaims.
func untaggedClaim(ref imageRef, named []imageClaim, addons []registry.AddOn) (claim imageClaim, ok bool) {
	if ref.tag != "" {
		return imageClaim{}, false
	}
	for _, n := range named {
		i := slices.IndexFunc(addons, func(a registry.AddOn) bool { return a.ID == n.id })
		if i >= 0 && slices.ContainsFunc(addons[i].Matchers.Images, func(m string) bool {
			path, pattern := registry.SplitTagPattern(m)
			return pattern != "" && imageMatches(ref, path)
		}) {
			return n, true
		}
	}
	return imageClaim{}, false
}

// imageAddOns returns the IDs of the add-ons whose image matchers claim
// ref (imageClaims), in registry order.
func imageAddOns(ref imageRef, addons []registry.AddOn) []string {
	var ids []string
	for _, c := range imageClaims(ref, addons) {
		ids = append(ids, c.id)
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
// namespace (two where pods are off a Helm release's release line, see
// below), sorted by ID, namespace and version, and the deduped, sorted
// list of image repos no image matcher claims (registry gap visibility —
// never findings, spec §9; an add-on found from labels may still run one).
//
// Evidence, strongest first, each a Source:
//   - "chart": a Helm release of a chart matcher; its appVersion is the
//     install's version.
//   - "image": an image matcher; the version is the tag's, or for a
//     component image the product line its tag's line ships in. An image
//     without a tag goes to the entry its pod's labels, or a Helm release
//     in its namespace, name when that entry has a tag-qualified matcher
//     of its repository (untaggedClaim). An image whose tag names no
//     version (a digest, ":latest") takes the version label of its pod
//     when the pod's labels name the same add-on (labelAddOn), the oldest
//     where pods differ; a tag is never overridden, and a component
//     image's line is not guessed.
//   - "labels": the pod's labels name the add-on (see labelAddOn), for a
//     pod none of whose images that add-on's matchers claim but one of
//     which no matcher claims at all: the container the labels are about
//     (an injected sidecar matching another add-on does not stop it; a
//     vendor build (vendorBuilds) whose image matched its own entry does,
//     whatever unmatched sidecars run beside it). A pod running a
//     provider build (registry.IsProviderBuild) is never claimed through
//     its labels: its support follows the provider, not upstream (#110).
//   - "gitops": a chart reference of an Argo CD Application or Flux
//     HelmRelease (inventory.GitOpsChart) naming a chart matcher. It
//     gives the namespace the chart deploys into and the chart version
//     (evidence only), but no app version: that comes from the pods
//     running it, so a product retired as a whole (ingress-nginx) is
//     still end-of-life, and a per-release-line product gets no
//     lifecycle verdict without a running pod to read a version from.
//   - "ingressclass": an IngressClass whose controller names the add-on
//     (ingressClassAddOns) when nothing else found it nor another
//     controller that can serve the class. It is cluster-scoped (no
//     namespace) and has no version, so a product retired as a whole
//     (ingress-nginx) is still end-of-life, and a per-release-line product
//     gets no lifecycle verdict.
//
// Each namespace is its own install, judged at its own version: within
// one, the oldest version wins, and a Helm release's appVersion over the
// image tags and labels on its release line (releaseLine: pods a patch
// behind the release, or without a version, are the release's). Image
// tags and labels on another line are a second install in the namespace,
// judged like a namespace without a release at their oldest version, so an
// istioctl canary revision running an older istio/pilot beside a newer
// istiod Helm release in istio-system is judged on its own line (#165).
// Neither crosses namespaces, so a mesh mid-upgrade or a newer release
// elsewhere cannot hide an older install, nor lend its version to one.
// Across namespaces the other way round: a release whose pods run in
// another namespace (a chart with a namespace override) is reported twice,
// the release's namespace at its appVersion and the pods' namespace at
// their image tag, which may not track the app version.
func matchAddOns(ev addOnEvidence, addons []registry.AddOn) ([]inventory.AddOnInstance, []string) {
	type evidence struct {
		source  string // "image" | "labels" | "chart" | "gitops" | "ingressclass"
		version string // app version
		chart   string // chart version, chart and gitops evidence only (see below)
	}
	type install struct{ id, ns string }
	byInstall := map[install][]evidence{}
	unmatched := map[string]bool{}

	// What names the add-on of an image without a tag (untaggedClaim): its
	// pod's labels, then the Helm releases of its namespace.
	labelNamed := map[nsImage][]imageClaim{}
	for _, p := range ev.labelled {
		if id, version := labelAddOn(p.Labels, addons); id != "" {
			for _, img := range p.Images {
				k := nsImage{p.Namespace, img}
				labelNamed[k] = append(labelNamed[k], imageClaim{id, version, true})
			}
		}
	}
	releaseNamed := map[string][]imageClaim{}
	for _, rel := range ev.releases {
		for _, a := range addons {
			if slices.Contains(a.Matchers.Charts, rel.ChartName) {
				releaseNamed[rel.Namespace] = append(releaseNamed[rel.Namespace], imageClaim{a.ID, "", true})
			}
		}
	}

	for _, img := range ev.images {
		ref := parseImage(img.Image)
		claims := imageClaims(ref, addons)
		if c, ok := untaggedClaim(ref, append(slices.Clone(labelNamed[img]), releaseNamed[img.Namespace]...), addons); ok {
			claims = []imageClaim{c}
		}
		for _, c := range claims {
			// An image that names no version (a digest, ":latest") is the
			// version its pod's labels give the same add-on (#301), the
			// oldest where pods sharing the image differ. A tag always
			// decides; a component image's line is never guessed.
			if c.version == "" && c.byTag {
				for _, n := range labelNamed[img] {
					if n.id == c.id {
						c.version = olderVersion(c.version, n.version)
					}
				}
			}
			in := install{c.id, img.Namespace}
			byInstall[in] = append(byInstall[in], evidence{source: "image", version: c.version})
		}
		if len(claims) == 0 {
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

	// A GitOps chart reference names the add-on and where it deploys, but
	// no app version: the version comes from the pods running it, if any.
	// Its chart version is evidence only, and only when it is one version:
	// the resource's spelling is often a constraint ("4.*", ">=4.0.0"),
	// which says what may be installed, not what is.
	for _, g := range ev.gitops {
		for _, a := range addons {
			if slices.Contains(a.Matchers.Charts, g.Chart) {
				in := install{a.ID, g.Target}
				byInstall[in] = append(byInstall[in], evidence{source: "gitops", chart: exactChartVersion(g.Version)})
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

	strength := map[string]int{"ingressclass": 1, "gitops": 2, "labels": 3, "image": 4, "chart": 5}
	instance := func(in install, evs []evidence) inventory.AddOnInstance {
		inst := inventory.AddOnInstance{ID: in.id}
		var podVersion, appVersion, releaseChart, gitopsChart string
		for _, e := range evs {
			if strength[e.source] > strength[inst.Source] {
				inst.Source = e.source
			}
			if e.source == "chart" {
				appVersion = olderVersion(appVersion, e.version)
			} else {
				podVersion = olderVersion(podVersion, e.version)
			}
			if e.source == "gitops" {
				gitopsChart = olderVersion(gitopsChart, e.chart)
			} else {
				releaseChart = olderVersion(releaseChart, e.chart)
			}
		}
		// What a Helm release records is the chart version installed; a
		// GitOps resource's is what it asks for, so it only fills a gap.
		inst.ChartVersion = cmp.Or(releaseChart, gitopsChart)
		// "" is a namespace too: a manifest object's left unset.
		if inst.Source != "ingressclass" {
			inst.Namespaces = []string{in.ns}
		}
		// A release's appVersion is authoritative; a chart without one
		// falls back to the image tag or labels, never to the chart version.
		inst.Version = cmp.Or(appVersion, podVersion)
		return inst
	}
	var out []inventory.AddOnInstance
	for in, evs := range byInstall {
		// The releases' appVersions speak for the pods on their release
		// lines; a pod version on another line is a second install here.
		lines := map[string]bool{}
		for _, e := range evs {
			if e.source == "chart" && e.version != "" {
				lines[releaseLine(e.version)] = true
			}
		}
		var agree, other []evidence
		for _, e := range evs {
			if len(lines) > 0 && e.source != "chart" && e.version != "" && !lines[releaseLine(e.version)] {
				other = append(other, e)
			} else {
				agree = append(agree, e)
			}
		}
		out = append(out, instance(in, agree))
		if len(other) > 0 {
			out = append(out, instance(in, other))
		}
	}
	sort.Slice(out, func(i, j int) bool {
		return cmp.Or(cmp.Compare(out[i].ID, out[j].ID), slices.Compare(out[i].Namespaces, out[j].Namespaces),
			cmp.Compare(out[i].Version, out[j].Version), cmp.Compare(out[i].Source, out[j].Source)) < 0
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
