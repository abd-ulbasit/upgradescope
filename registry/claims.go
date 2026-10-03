package registry

import (
	"fmt"
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

// matchersOverlap reports whether some repository path is claimed by both
// image matchers. Provider builds are claimed only by matchers naming the
// provider location, so a provider matcher never overlaps a host-less one.
func matchersOverlap(a, b string) bool {
	if IsProviderBuild(a) != IsProviderBuild(b) {
		return false
	}
	return PathMatches(a, b) || PathMatches(b, a)
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
			for _, ma := range a.Matchers.Images {
				for _, mb := range b.Matchers.Images {
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
		}
	}
	return errs
}
