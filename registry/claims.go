package registry

import (
	"fmt"
	"path"
	"slices"
	"strings"
)

// AnyPrefix marks an image matcher that opts into matching its final segment
// under any registry host or prefix: "*/etcd" is etcd, bare or behind a
// kubeadm imageRepository, a Harbor proxy cache or any other mirror. It is
// for a distinctive name only; validateImageMatcher refuses generic ones.
const AnyPrefix = "*/"

// PathMatches reports whether a normalised repository path is claimed by an
// image matcher. A matcher of two or more segments is a suffix on whole
// segments, so "ingress-nginx/controller" matches the canonical path and
// every mirror or pull-through-cache path that keeps it
// ("registry-k8s-io/ingress-nginx/controller"). A one-segment matcher is the
// repository itself, exactly: a bare "controller" or "operator" never claims
// another product's repository of that name. A matcher written "*/name"
// (AnyPrefix) is the explicit opt-in to the suffix match on that one segment.
func PathMatches(path, matcher string) bool {
	if name, ok := strings.CutPrefix(matcher, AnyPrefix); ok {
		return path == name || strings.HasSuffix(path, "/"+name)
	}
	if !strings.Contains(matcher, "/") {
		return path == matcher
	}
	return path == matcher || strings.HasSuffix(path, "/"+matcher)
}

// SplitTagPattern splits an image matcher into its repository path and
// its tag pattern, "" when it has none: "rancher/nginx-ingress-controller:*-hardened*"
// is the path rancher/nginx-ingress-controller and the pattern
// "*-hardened*". A tag pattern tells apart builds of different products
// that one repository publishes and only their tags name (RKE2's
// "-hardenedN" and RKE1's "-rancherN" builds of ingress-nginx, #265).
func SplitTagPattern(matcher string) (path, tagPattern string) {
	path, tagPattern, _ = strings.Cut(matcher, ":")
	return path, tagPattern
}

// TagMatches reports whether an image tag matches a tag pattern, in which
// "*" stands for any run of characters: "*-hardened*" matches
// "v1.12.6-hardened1" and "nginx-1.9.4-hardened1". An image without a tag
// matches no pattern.
func TagMatches(tag, pattern string) bool {
	if tag == "" {
		return false
	}
	ok, err := path.Match(pattern, tag) // tags hold no "/", so "*" spans the whole tag
	return err == nil && ok
}

// matchersOverlap reports whether some image is claimed by both image
// matchers. Provider builds are claimed only by matchers naming the
// provider location, so a provider matcher never overlaps a host-less one.
// A tag-qualified matcher claims its tags ahead of a path-only matcher (see
// SplitTagPattern), so the two never overlap; two tag-qualified matchers of
// one repository are taken to overlap whatever their patterns.
func matchersOverlap(a, b string) bool {
	a, ta := SplitTagPattern(a)
	b, tb := SplitTagPattern(b)
	if IsProviderBuild(a) != IsProviderBuild(b) || (ta == "") != (tb == "") {
		return false
	}
	return PathMatches(a, b) || PathMatches(b, a)
}

// imageClaims are the image matchers of an entry: its images and its
// component images.
func imageClaims(a AddOn) []string {
	claims := slices.Clone(a.Matchers.Images)
	for _, c := range a.Matchers.Components {
		claims = append(claims, c.Image)
	}
	return claims
}

// ClaimConflicts returns one error for each image repository or chart that
// two entries both claim. The embedded registry has none (tested); this
// guards operator-supplied entries (--registry-dir), which would otherwise
// make one image judged by two entries, with a false blocker when the
// operator's entry is the wrong one. An extra entry takes over an embedded
// entry's matchers by replacing it under the same id.
func ClaimConflicts(addons []AddOn) []error {
	var errs []error
	for i, a := range addons {
		for _, b := range addons[i+1:] {
			for _, ma := range imageClaims(a) {
				for _, mb := range imageClaims(b) {
					if matchersOverlap(ma, mb) {
						errs = append(errs, fmt.Errorf("registry: %s image matcher %q and %s image matcher %q claim the same image; an image may belong to one entry only (replace the embedded entry by using its id)", a.ID, ma, b.ID, mb))
					}
				}
			}
			for _, ca := range a.Matchers.Charts {
				for _, cb := range b.Matchers.Charts {
					if ca == cb {
						errs = append(errs, fmt.Errorf("registry: %s and %s both match chart %q; a chart may belong to one entry only (replace the embedded entry by using its id)", a.ID, b.ID, ca))
					}
				}
			}
			// A pod's app.kubernetes.io/name label names an add-on by its id
			// or by a chart name alike, so one entry's id equal to another's
			// chart would let a single label name two add-ons.
			for _, pair := range [][2]AddOn{{a, b}, {b, a}} {
				id, other := pair[0].ID, pair[1]
				if slices.Contains(other.Matchers.Charts, id) {
					errs = append(errs, fmt.Errorf("registry: the id %q of %s is also a chart name of %s; a label may name one entry only (rename the entry or drop the chart)", id, pair[0].ID, other.ID))
				}
			}
		}
	}
	return errs
}
