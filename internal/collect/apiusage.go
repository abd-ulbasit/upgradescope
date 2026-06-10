package collect

import (
	"context"
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

const apiUsagePageSize = 500

// collectAPIUsage counts objects resident at deprecated/removed
// group/versions. It lists each flagged-and-served GV at that exact
// endpoint with the metadata client (paged, metadata-only) — converting
// reads would hide residency.
func collectAPIUsage(ctx context.Context, disc discovery.DiscoveryInterface, meta metadata.Interface, lifecycle []kb.APILifecycleEntry, inv *inventory.Inventory) error {
	flagged := map[string]bool{} // "group/version/kind"
	for _, e := range lifecycle {
		if e.Deprecated != nil || e.Removed != nil {
			flagged[e.Group+"/"+e.Version+"/"+e.Kind] = true
		}
	}

	_, lists, err := disc.ServerGroupsAndResources()
	if err != nil {
		// Partial discovery failure (one broken aggregated API) must not
		// kill the capability; total failure does.
		if !discovery.IsGroupDiscoveryFailedError(err) || lists == nil {
			return fmt.Errorf("discovery: %w", err)
		}
	}

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
			u := inventory.APIUsage{Group: gv.Group, Version: gv.Version, Kind: r.Kind, Namespaces: map[string]int{}}
			opts := metav1.ListOptions{Limit: apiUsagePageSize}
			for {
				page, err := meta.Resource(gv.WithResource(r.Name)).List(ctx, opts)
				if err != nil {
					return fmt.Errorf("list %s %s: %w", l.GroupVersion, r.Name, err)
				}
				for i := range page.Items {
					u.Namespaces[page.Items[i].Namespace]++ // cluster-scoped → key ""
					u.Count++
				}
				if page.Continue == "" {
					break
				}
				opts.Continue = page.Continue
			}
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
	return nil
}
