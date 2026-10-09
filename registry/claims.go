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
// entry's matchers by replacing it under the same id. MergeConflicts is the
// check for operator entries: the same errors, each with the fix that suits
// it.
func ClaimConflicts(addons []AddOn) []error {
	return claimConflicts(addons, nil)
}

// MergeConflicts returns the claim conflicts of Merge(base, extra), each
// with the fix that suits it. An extra entry of a new id that claims an
// embedded entry's image should replace that entry instead, by its id. An
// extra entry that replaces an embedded one, yet claims an image or chart
// the embedded registry gives to another entry, is nearly always a copy of
// the embedded file from an earlier release, taken before that claim moved:
// a copy of rke2-ingress-nginx.yaml from before #265 claims every
// rancher/nginx-ingress-controller build, RKE1's included, which
// ingress-nginx claims now. Its fix is a fresh copy of the current file
// with the edits re-applied, not a replacement of the other entry too.
func MergeConflicts(base, extra []AddOn) []error {
	embedded := map[string]AddOn{}
	for _, a := range base {
		embedded[a.ID] = a
	}
	isExtra := map[string]bool{}
	for _, e := range extra {
		isExtra[e.ID] = true
	}
	return claimConflicts(Merge(base, extra), func(a, b AddOn, claim string) string {
		mine, other := a, b // mine: the operator's entry; other: an embedded one
		if !isExtra[mine.ID] {
			mine, other = b, a
		}
		if !isExtra[mine.ID] || isExtra[other.ID] {
			return "" // two embedded or two extra entries: the default fix
		}
		old, replaces := embedded[mine.ID]
		if !replaces {
			return ""
		}
		hint := fmt.Sprintf("your %s replaces the embedded entry of that id, and the embedded registry now claims this %s under %s", mine.ID, claim, other.ID)
		if claim == "image" {
			if now := sameRepository(imageClaims(old), imageClaims(mine)); len(now) > 0 {
				hint += fmt.Sprintf(", and the embedded %s claims %s instead", mine.ID, quoteAll(now))
			}
		}
		return hint + fmt.Sprintf(": if your file is a copy of registry/data/%s.yaml from an earlier release, copy the current one again and re-apply your edits; otherwise replace %s too, by using its id", mine.ID, other.ID)
	})
}

// sameRepository returns the matchers of current that name a repository
// one of mine's matchers names: what the current embedded file says where
// an operator's outdated copy says something else.
func sameRepository(current, mine []string) []string {
	var out []string
	for _, c := range current {
		cp, _ := SplitTagPattern(c)
		if slices.ContainsFunc(mine, func(m string) bool {
			mp, _ := SplitTagPattern(m)
			return PathMatches(cp, mp) || PathMatches(mp, cp)
		}) {
			out = append(out, c)
		}
	}
	return out
}

func quoteAll(ss []string) string {
	q := make([]string, len(ss))
	for i, s := range ss {
		q[i] = fmt.Sprintf("%q", s)
	}
	return strings.Join(q, ", ")
}

// claimConflicts is ClaimConflicts with hint, when non-nil, choosing the fix
// an error suggests for a conflict between a and b over an "image" or a
// "chart"; a hint of "" keeps the default fix.
func claimConflicts(addons []AddOn, hint func(a, b AddOn, claim string) string) []error {
	fix := func(a, b AddOn, claim string) string {
		if hint != nil {
			if h := hint(a, b, claim); h != "" {
				return h
			}
		}
		return "replace the embedded entry by using its id"
	}
	var errs []error
	for i, a := range addons {
		for _, b := range addons[i+1:] {
			for _, ma := range imageClaims(a) {
				for _, mb := range imageClaims(b) {
					if matchersOverlap(ma, mb) {
						errs = append(errs, fmt.Errorf("registry: %s image matcher %q and %s image matcher %q claim the same image; an image may belong to one entry only (%s)", a.ID, ma, b.ID, mb, fix(a, b, "image")))
					}
				}
			}
			for _, ca := range a.Matchers.Charts {
				for _, cb := range b.Matchers.Charts {
					if ca == cb {
						errs = append(errs, fmt.Errorf("registry: %s and %s both match chart %q; a chart may belong to one entry only (%s)", a.ID, b.ID, ca, fix(a, b, "chart")))
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
