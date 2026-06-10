package collect

import (
	"context"
	"fmt"
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

// repoMatches reports whether repo equals the matcher or sits beneath it,
// e.g. "registry.k8s.io/ingress-nginx" matches ".../ingress-nginx/controller".
func repoMatches(repo, matcher string) bool {
	return repo == matcher || strings.HasPrefix(repo, matcher+"/")
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
		repo, tag := splitImage(img.Image)
		matched := false
		for _, a := range addons {
			for _, m := range a.Matchers.Images {
				if repoMatches(repo, m) {
					byID[a.ID] = append(byID[a.ID], evidence{
						source:  "image",
						version: strings.TrimPrefix(tag, "v"),
						ns:      img.Namespace,
					})
					matched = true
					break
				}
			}
		}
		if !matched {
			unmatched[repo] = true
		}
	}

	for _, rel := range releases {
		for _, a := range addons {
			for _, chart := range a.Matchers.Charts {
				if rel.ChartName == chart {
					byID[a.ID] = append(byID[a.ID], evidence{source: "chart", version: rel.ChartVersion, ns: rel.Namespace})
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
