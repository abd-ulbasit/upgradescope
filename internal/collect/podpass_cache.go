package collect

import (
	"slices"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Defaults of the agent's --pod-pass-every and --pod-pass-max-age (#228).
const (
	// DefaultPodPassEvery is the default of --pod-pass-every: at the default
	// interval of 10 minutes, a full pod pass about every 30 minutes.
	DefaultPodPassEvery = 3
	// DefaultPodPassMaxAge is the default of --pod-pass-max-age.
	DefaultPodPassMaxAge = time.Hour
)

// PodPassCache lets a long-running caller (the agent) read the pods outside
// kube-system only every few collections, and detect add-ons in between
// from the evidence the last full pass read (#228).
//
// The pods are most of what a steady collection costs on a large cluster:
// at 2,001 nodes and about 14,000 pods, 16 of 31 requests and 79% of the
// bytes, decoded whole to read their images and labels. The cluster-wide
// list is the cost, and a watch or informer is excluded by design (AG-01),
// so the list is made less often. A collection reuses the last full pass
// while fewer than Every collections have run since it (the pass itself
// is the first of them) and it is less than MaxAge old, and otherwise
// lists every pod again. So an add-on installed or upgraded right after a
// pass is reported as it was for at most Every-1 collections, and never for
// longer than MaxAge: the age of what was reused is left in
// inventory.AddOnEvidenceAgeSeconds.
//
// What is reused is the images and labels of the pods outside kube-system,
// the evidence matchAddOns reads, never a verdict: the registry is applied
// to it every time, so a knowledge-base update takes effect at once. Not
// reused, read in every collection: the kube-system pods (the versions
// capability lists them for the control-plane components and kube-proxy
// skew, and the add-ons take their evidence from that list, #227), Helm
// releases, GitOps resources and IngressClasses.
//
// A full pass is forced, whatever the counts say, when
//   - the cache holds no complete pass (it is new, or the last pass failed
//     or read only some of its pages: a failed or partial pass is never
//     reused, and the evidence of an earlier one is dropped with it);
//   - the versions capability did not list the kube-system pods in this
//     collection, because the add-ons would then need them from the pass,
//     and the pass does not hold them.
//
// The cache holds the distinct (namespace, image) pairs and the distinct
// labelled pods among the pods outside kube-system, never the pods, so it
// is as big as the cluster's variety of images, at most what one pass
// retains while it matches.
//
// Every <= 1 reuses nothing, as a nil *PodPassCache does, which is what a
// one-shot scan wants. Not safe for concurrent use; one collection at a
// time.
type PodPassCache struct {
	every  int
	maxAge time.Duration
	now    func() time.Time

	have   bool      // a complete pass is held
	at     time.Time // when it began
	since  int       // collections that reused it
	images []nsImage
	labels []labelledPod
}

// NewPodPassCache returns an empty cache that reuses a full pass for the
// collections after it, up to every-1 of them, none that is maxAge or more
// after it began. every of 1 or less reuses nothing; a maxAge of 0 or less
// is no limit but the count.
func NewPodPassCache(every int, maxAge time.Duration) *PodPassCache {
	return &PodPassCache{every: every, maxAge: maxAge, now: time.Now}
}

// clock is the time a pass began at; the zero time for a nil cache.
func (c *PodPassCache) clock() time.Time {
	if c == nil {
		return time.Time{}
	}
	return c.now()
}

func (c *PodPassCache) enabled() bool { return c != nil && c.every > 1 }

// Held reports whether a complete pass is held, for tests and diagnostics.
func (c *PodPassCache) Held() bool { return c != nil && c.have }

// reuse reports whether this collection should reuse the held pass, and if
// so returns its evidence and age (whole seconds, at least 1). The caller
// has already established that the kube-system pods were listed by
// versions. A reuse counts toward Every.
func (c *PodPassCache) reuse() (images []nsImage, labelled []labelledPod, ageSeconds int64, ok bool) {
	if !c.enabled() || !c.have {
		return nil, nil, 0, false
	}
	age := c.now().Sub(c.at)
	if c.since+1 >= c.every || (c.maxAge > 0 && age >= c.maxAge) {
		return nil, nil, 0, false
	}
	c.since++
	return c.images, c.labels, max(1, int64(age/time.Second)), true
}

// forget drops the held pass: the next collection lists every pod.
func (c *PodPassCache) forget() {
	if c == nil {
		return
	}
	*c = PodPassCache{every: c.every, maxAge: c.maxAge, now: c.now}
}

// record keeps the evidence of a full pass that began at start and read
// every pod, dropping the kube-system pods' (every collection reads those)
// and, as matchAddOns reads a repeat the same as the first, repeats of a
// pair or a labelled pod already seen.
func (c *PodPassCache) record(ev addOnEvidence, start time.Time) {
	if !c.enabled() {
		return
	}
	c.forget()
	seenImage := map[nsImage]bool{}
	for _, img := range ev.images {
		if img.Namespace == metav1.NamespaceSystem || seenImage[img] {
			continue
		}
		seenImage[img] = true
		c.images = append(c.images, img)
	}
	seenPod := map[string]bool{}
	for _, p := range ev.labelled {
		if p.Namespace == metav1.NamespaceSystem {
			continue
		}
		key := strings.Join(slices.Concat([]string{p.Namespace, p.Labels.chart, p.Labels.name, p.Labels.version, p.Labels.partOf}, p.Images), "\x00")
		if seenPod[key] {
			continue
		}
		seenPod[key] = true
		c.labels = append(c.labels, labelledPod{Namespace: p.Namespace, Labels: p.Labels, Images: slices.Clone(p.Images)})
	}
	c.have, c.at = true, start
}
