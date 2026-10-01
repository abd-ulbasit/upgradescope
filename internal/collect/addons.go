package collect

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/Masterminds/semver/v3"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/abd-ulbasit/upgradescope/internal/inventory"
	"github.com/abd-ulbasit/upgradescope/registry"
)

const maxUnrecognizedImages = 200

// nsImage is one container image observed in a namespace.
type nsImage struct {
	Namespace string
	Image     string
}

// collectAddOns lists pod images (containers + init containers) and runs
// the pure matcher over images, already-collected Helm releases
// (inv.HelmReleases — the helm step runs first), and the registry.
func collectAddOns(ctx context.Context, kube kubernetes.Interface, addons []registry.AddOn, inv *inventory.Inventory) error {
	var images []nsImage
	opts := metav1.ListOptions{Limit: listPageSize}
	for {
		pods, err := kube.CoreV1().Pods(metav1.NamespaceAll).List(ctx, opts)
		if err != nil {
			return fmt.Errorf("list pods: %w", err)
		}
		// Extract (namespace, image) pairs per page so only the pairs are
		// retained — never the accumulated PodList of a large cluster.
		for i := range pods.Items {
			p := &pods.Items[i]
			for _, c := range p.Spec.InitContainers {
				images = append(images, nsImage{Namespace: p.Namespace, Image: c.Image})
			}
			for _, c := range p.Spec.Containers {
				images = append(images, nsImage{Namespace: p.Namespace, Image: c.Image})
			}
		}
		if pods.Continue == "" {
			break
		}
		opts.Continue = pods.Continue
	}
	inv.AddOns, inv.UnrecognizedImages = matchAddOns(images, inv.HelmReleases, addons)
	return nil
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

// versionLess orders detected versions for the conservative-oldest merge:
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

// matchAddOns is pure: images + helm releases + registry → detected
// add-on instances (deduped by ID; chart evidence preferred) and the
// deduped, sorted, capped list of unmatched image repos (registry gap
// visibility — never findings, spec §9).
func matchAddOns(images []nsImage, releases []inventory.HelmRelease, addons []registry.AddOn) ([]inventory.AddOnInstance, []string) {
	type evidence struct {
		source  string // "image" | "chart"
		version string
		ns      string
	}
	byID := map[string][]evidence{}
	unmatched := map[string]bool{}

	for _, img := range images {
		ref := parseImage(img.Image)
		matched := false
		for _, a := range addons {
			for _, m := range a.Matchers.Images {
				if pathMatches(ref.path, m) {
					byID[a.ID] = append(byID[a.ID], evidence{
						source:  "image",
						version: versionFromTag(ref.tag),
						ns:      img.Namespace,
					})
					matched = true
					break
				}
			}
		}
		if !matched {
			unmatched[ref.host+"/"+ref.path] = true
		}
	}

	for _, rel := range releases {
		for _, a := range addons {
			for _, chart := range a.Matchers.Charts {
				if rel.ChartName == chart {
					byID[a.ID] = append(byID[a.ID], evidence{source: "chart", version: strings.TrimPrefix(rel.ChartVersion, "v"), ns: rel.Namespace})
				}
			}
		}
	}

	var out []inventory.AddOnInstance
	for id, evs := range byID {
		inst := inventory.AddOnInstance{ID: id, Source: "image"}
		nsSet := map[string]bool{}
		for _, e := range evs {
			nsSet[e.ns] = true
			switch {
			case e.source == "chart" && inst.Source != "chart":
				inst.Source, inst.Version = "chart", e.version
			case e.source == inst.Source && e.version != "" && (inst.Version == "" || versionLess(e.version, inst.Version)):
				inst.Version = e.version
			}
		}
		for ns := range nsSet {
			inst.Namespaces = append(inst.Namespaces, ns)
		}
		sort.Strings(inst.Namespaces)
		out = append(out, inst)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })

	var unrec []string
	for repo := range unmatched {
		unrec = append(unrec, repo)
	}
	sort.Strings(unrec)
	if len(unrec) > maxUnrecognizedImages {
		unrec = unrec[:maxUnrecognizedImages]
	}
	return out, unrec
}
