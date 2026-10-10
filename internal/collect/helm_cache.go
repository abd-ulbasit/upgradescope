package collect

import (
	"crypto/sha256"
	"fmt"
	"maps"
	"slices"

	"k8s.io/apimachinery/pkg/types"

	"github.com/abd-ulbasit/upgradescope/internal/inventory"
)

// HelmCache keeps what a long-running caller (the agent) decoded from each
// Helm release's installed revision, so a tick fetches only the storage
// objects it has not decoded yet instead of one per release (#71).
//
// An entry is keyed by the object's namespace, name, UID and
// resourceVersion as the metadata-only list reports them, so an object that
// was replaced or written again is read again and nothing else is. What is
// kept is the few fields a release contributes to the inventory, never the
// payload, and an entry is dropped as soon as its object is no longer a
// release's installed revision, so the cache is as big as the cluster's
// release list and not its history. Entries also depend on which API
// versions the knowledge base flags (they record the manifest objects at
// those), so the cache empties itself when that set differs.
//
// A fetch that failed leaves nothing behind. A payload that cannot be
// decoded, or a manifest that cannot be fully parsed, is remembered as such
// and reported on every tick in the same words without being downloaded
// again: it cannot get better until the object changes.
//
// The zero value is not usable, and a nil *HelmCache is: it remembers
// nothing, which is what a one-shot scan wants. Not safe for concurrent use;
// one collection at a time.
type HelmCache struct {
	entries map[helmCacheKey]helmCacheEntry
	flagged string // fingerprint of the knowledge base's flagged APIs the entries were parsed against
}

// NewHelmCache returns an empty cache.
func NewHelmCache() *HelmCache {
	return &HelmCache{entries: map[helmCacheKey]helmCacheEntry{}}
}

// Len is the number of releases held.
func (c *HelmCache) Len() int {
	if c == nil {
		return 0
	}
	return len(c.entries)
}

type helmCacheKey struct {
	driver            int
	namespace, object string
	uid               types.UID
	rv                string
}

// cacheable reports whether the key identifies one version of the object.
// A list that returned no UID or resourceVersion (an apiserver always sets
// both) leaves nothing to tell an old payload from a new one.
func (k helmCacheKey) cacheable() bool { return k.uid != "" && k.rv != "" }

// helmCacheEntry is one release's contribution to the inventory.
type helmCacheEntry struct {
	chartName, chartVersion, appVersion, kubeVersion string
	status                                           string // the release document's own, used when the object has no status label
	apis                                             []inventory.APIUsage
	decodeErr, parseErr                              string // "" when the payload decoded, or the manifest parsed in full
}

// decodeHelmEntry decodes one stored payload into what the inventory keeps
// of it. Its errors are text, reported as they always were, and a release
// that fails to decode or parse still yields an entry.
func decodeHelmEntry(data []byte, flagged map[gvk]bool) helmCacheEntry {
	return decodeHelmEntryWith(data, flagged, false)
}

// decodeHelmEntryWith is decodeHelmEntry, optionally with every manifest
// document parsed twice, as before #285: the reference the tests hold the
// single parse to.
func decodeHelmEntryWith(data []byte, flagged map[gvk]bool, reparse bool) helmCacheEntry {
	doc, err := decodeHelmRelease(data)
	if err != nil {
		return helmCacheEntry{decodeErr: err.Error()}
	}
	// The chart's metadata is free text anyone who can plant a release
	// writes, and every string of an inventory is held to a limit: a
	// release whose chart metadata is over it is not decodable, which the
	// capability names (a kubeVersion cut to the limit would be a narrower
	// constraint, not the chart's).
	m := doc.Chart.Metadata
	for _, s := range []string{m.Name, m.Version, m.AppVersion, m.KubeVersion, doc.Info.Status} {
		if len(s) > inventory.MaxStringBytes {
			return helmCacheEntry{decodeErr: fmt.Sprintf("chart metadata has a value over %d bytes, which is not recorded", inventory.MaxStringBytes)}
		}
	}
	apis, err := manifestAPIsWith(doc.Manifest, flagged, reparse)
	e := helmCacheEntry{
		chartName: doc.Chart.Metadata.Name, chartVersion: doc.Chart.Metadata.Version,
		appVersion: doc.Chart.Metadata.AppVersion, kubeVersion: doc.Chart.Metadata.KubeVersion,
		status: doc.Info.Status, apis: apis,
	}
	if err != nil {
		e.parseErr = err.Error()
	}
	return e
}

// begin starts a collection against the flagged APIs with this fingerprint,
// emptying the cache if its entries were parsed against another set.
func (c *HelmCache) begin(flagged string) {
	if c == nil {
		return
	}
	if c.flagged != flagged {
		clear(c.entries)
		c.flagged = flagged
	}
}

func (c *HelmCache) get(k helmCacheKey) (helmCacheEntry, bool) {
	if c == nil || !k.cacheable() {
		return helmCacheEntry{}, false
	}
	e, ok := c.entries[k]
	return e, ok
}

func (c *HelmCache) put(k helmCacheKey, e helmCacheEntry) {
	if c == nil || !k.cacheable() {
		return
	}
	c.entries[k] = e
}

// prune drops every entry that was not seen in this collection, except the
// entries of a driver whose list failed (keep reports false for it): they
// are not known to be gone.
func (c *HelmCache) prune(seen map[helmCacheKey]bool, keep func(driver int) bool) {
	if c == nil {
		return
	}
	for k := range c.entries {
		if !seen[k] && keep(k.driver) {
			delete(c.entries, k)
		}
	}
}

// flaggedFingerprint identifies a set of flagged API kinds.
func flaggedFingerprint(flagged map[gvk]bool) string {
	keys := make([]string, 0, len(flagged))
	for g := range flagged {
		keys = append(keys, fmt.Sprintf("%s/%s/%s", g.group, g.version, g.kind))
	}
	slices.Sort(keys)
	h := sha256.New()
	for _, k := range keys {
		fmt.Fprintln(h, k)
	}
	return fmt.Sprintf("%x", h.Sum(nil))
}

// cloneAPIUsage copies rows so the inventory handed to the caller shares
// nothing with the cache.
func cloneAPIUsage(rows []inventory.APIUsage) []inventory.APIUsage {
	if rows == nil {
		return nil
	}
	out := make([]inventory.APIUsage, len(rows))
	for i, r := range rows {
		r.Namespaces = maps.Clone(r.Namespaces)
		r.Objects = slices.Clone(r.Objects)
		out[i] = r
	}
	return out
}
