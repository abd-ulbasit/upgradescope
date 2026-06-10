package collect

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/metadata"

	"github.com/abd-ulbasit/upgradescope/internal/inventory"
	"github.com/abd-ulbasit/upgradescope/internal/kb"
)

// collectAPIUsage counts objects resident at deprecated/removed
// group/versions. It lists each flagged-and-served GV at that exact
// endpoint with the metadata client (paged, metadata-only) — converting
// reads would hide residency.
//
// Failures are per-resource: a forbidden or broken endpoint is recorded
// and the remaining resources still run. If at least one resource (or
// none was flagged) succeeded, the aggregate comes back as a partialError
// so the capability stays available with the failures as Reason; if every
// flagged resource failed, the capability degrades fully.
func collectAPIUsage(ctx context.Context, disc discovery.DiscoveryInterface, meta metadata.Interface, lifecycle []kb.APILifecycleEntry, inv *inventory.Inventory) error {
	flagged := map[string]bool{} // "group/version/kind"
	for _, e := range lifecycle {
		if e.Deprecated != nil || e.Removed != nil {
			flagged[e.Group+"/"+e.Version+"/"+e.Kind] = true
		}
	}

	var failures []string

	_, lists, err := disc.ServerGroupsAndResources()
	if err != nil {
		// Partial discovery failure (one broken aggregated API) must not
		// kill the capability; total failure does. Skipped groups are
		// surfaced in the capability reason, never silently dropped.
		var gde *discovery.ErrGroupDiscoveryFailed
		if !errors.As(err, &gde) || lists == nil {
			return fmt.Errorf("discovery: %w", err)
		}
		skipped := make([]string, 0, len(gde.Groups))
		for gv := range gde.Groups {
			skipped = append(skipped, gv.String())
		}
		sort.Strings(skipped)
		failures = append(failures, fmt.Sprintf("discovery: groups %s skipped", strings.Join(skipped, ",")))
	}

	attempted, succeeded := 0, 0
	var usages []inventory.APIUsage
	for _, l := range lists {
		gv, err := schema.ParseGroupVersion(l.GroupVersion)
		if err != nil {
			continue
		}
		for _, r := range l.APIResources {
			if strings.Contains(r.Name, "/") { // subresource
				continue
			}
			if !flagged[gv.Group+"/"+gv.Version+"/"+r.Kind] {
				continue
			}
			if !slices.Contains(r.Verbs, "list") {
				continue
			}
			attempted++
			u, err := listGVUsage(ctx, meta, gv, r.Name, r.Kind)
			if err != nil {
				failures = append(failures, fmt.Sprintf("list %s %s: %v", l.GroupVersion, r.Name, err))
				continue
			}
			succeeded++
			if u.Count > 0 {
				usages = append(usages, u)
			}
		}
	}

	sort.Slice(usages, func(i, j int) bool {
		a, b := usages[i], usages[j]
		if a.Group != b.Group {
			return a.Group < b.Group
		}
		if a.Version != b.Version {
			return a.Version < b.Version
		}
		return a.Kind < b.Kind
	})
	inv.APIUsage = usages

	if len(failures) == 0 {
		return nil
	}
	msg := strings.Join(failures, "; ")
	if attempted > 0 && succeeded == 0 {
		return errors.New(msg) // nothing usable — degrade the capability
	}
	return partialError{msg: "partial: " + msg}
}

// listGVUsage pages through one resource at one exact group/version and
// counts residency per namespace (cluster-scoped → key "").
func listGVUsage(ctx context.Context, meta metadata.Interface, gv schema.GroupVersion, resource, kind string) (inventory.APIUsage, error) {
	u := inventory.APIUsage{Group: gv.Group, Version: gv.Version, Kind: kind, Namespaces: map[string]int{}}
	opts := metav1.ListOptions{Limit: listPageSize}
	for {
		page, err := meta.Resource(gv.WithResource(resource)).List(ctx, opts)
		if err != nil {
			return inventory.APIUsage{}, err
		}
		for i := range page.Items {
			u.Namespaces[page.Items[i].Namespace]++
			u.Count++
		}
		if page.Continue == "" {
			break
		}
		opts.Continue = page.Continue
	}
	return u, nil
}
