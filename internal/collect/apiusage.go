package collect

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/version"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/metadata"

	"github.com/abd-ulbasit/upgradescope/internal/inventory"
	"github.com/abd-ulbasit/upgradescope/internal/kb"
)

const (
	lastAppliedAnnotation = "kubectl.kubernetes.io/last-applied-configuration"
	// lastAppliedManager is ObjectRef.Manager for an object flagged only by
	// its last-applied annotation (no managedFields entry names the GV).
	lastAppliedManager = "kubectl last-applied"
	// apfAutoUpdateAnnotation marks APF objects the apiserver maintains
	// itself; it rewrites them on every start, whoever touched them last.
	apfAutoUpdateAnnotation = "apf.kubernetes.io/autoupdate-spec"
)

// internalManagers are field managers inside the control plane. Their
// managedFields entries record the version that was current when that
// control-plane release wrote the object (the apiserver's default
// ServiceCIDR, IPAddresses and APF objects; controller-written objects),
// and go stale across upgrades without anyone writing the old version
// again. No user manifest is behind them.
var internalManagers = map[string]bool{
	"kube-apiserver":                               true,
	"kube-controller-manager":                      true,
	"api-priority-and-fairness-config-producer-v1": true,
}

// collectAPIUsage finds objects that someone still writes through a
// deprecated or removed group/version.
//
// The apiserver serves every stored object at every served version of its
// resource, converting on read, so listing a deprecated endpoint returns
// all objects and says nothing about who uses that version; it would also
// make the scanner itself a deprecated-API caller in
// apiserver_requested_deprecated_apis. Instead, each resource with a
// flagged version that the cluster still serves is listed once,
// metadata-only and paged, at a version that is not deprecated (see
// listVersion and replacementList; resources that share storage across
// groups share the LIST), and each object is attributed per flagged entry:
//
//   - the KB entry has no replacement and no non-deprecated version of the
//     resource is served: the type itself goes away, so every object counts;
//   - otherwise only objects authored via the flagged group/version count:
//     some manager's newest managedFields entry (not an internal manager,
//     not the status subresource) names it, or, for an object with no such
//     entries, the last-applied annotation does (see authoringManager).
//
// Objects created by a raw client that leaves neither trace go undetected;
// the deprecated-calls metric covers live callers. APF objects the
// apiserver auto-updates are skipped.
//
// Failures are per-resource: a forbidden or broken endpoint is recorded
// and the remaining resources still run. If at least one resource (or
// none was flagged) succeeded, the aggregate comes back as a partialError
// so the capability stays available with the failures as Reason; if every
// flagged resource failed, the capability degrades fully.
func collectAPIUsage(ctx context.Context, disc discovery.DiscoveryInterface, meta metadata.Interface, lifecycle []kb.APILifecycleEntry, inv *inventory.Inventory) error {
	flagged := map[kb.GVK]kb.APILifecycleEntry{}
	for _, e := range lifecycle {
		if e.Deprecated != nil || e.Removed != nil {
			flagged[kb.GVK{Group: e.Group, Version: e.Version, Kind: e.Kind}] = e
		}
	}

	var failures []string

	groups, lists, err := disc.ServerGroupsAndResources()
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
	preferred := map[string]string{} // group → preferred version
	for _, g := range groups {
		if g != nil {
			preferred[g.Name] = g.PreferredVersion.Version
		}
	}

	// Every served version of each resource, in discovery order.
	byResource := map[schema.GroupResource][]servedVersion{}
	var order []schema.GroupResource
	for _, l := range lists {
		gv, err := schema.ParseGroupVersion(l.GroupVersion)
		if err != nil {
			continue
		}
		for _, r := range l.APIResources {
			if strings.Contains(r.Name, "/") { // subresource
				continue
			}
			gr := schema.GroupResource{Group: gv.Group, Resource: r.Name}
			if _, seen := byResource[gr]; !seen {
				order = append(order, gr)
			}
			_, isFlagged := flagged[kb.GVK{Group: gv.Group, Version: gv.Version, Kind: r.Kind}]
			byResource[gr] = append(byResource[gr], servedVersion{
				version: gv.Version, kind: r.Kind, flagged: isFlagged,
				list: slices.Contains(r.Verbs, "list"),
			})
		}
	}

	// The targets of each LIST, in first-seen order. Two group/resources
	// share one LIST when one is listed at the other's group (below).
	var toList []schema.GroupVersionResource
	targetsOf := map[schema.GroupVersionResource][]usageTarget{}
	for _, gr := range order {
		served := byResource[gr]
		listV, ok := listVersion(served, preferred[gr.Group])
		if !ok {
			continue
		}
		gvr := schema.GroupVersionResource{Group: gr.Group, Version: listV.version, Resource: gr.Resource}
		if listV.flagged {
			for _, s := range served {
				e := flagged[kb.GVK{Group: gr.Group, Version: s.version, Kind: s.kind}]
				if r, ok := replacementList(gr.Resource, e.Replacement, byResource, preferred); ok {
					gvr = r
					break
				}
			}
		}
		if _, seen := targetsOf[gvr]; !seen {
			toList = append(toList, gvr)
		}
		for _, s := range served {
			if !s.flagged {
				continue
			}
			e := flagged[kb.GVK{Group: gr.Group, Version: s.version, Kind: s.kind}]
			targetsOf[gvr] = append(targetsOf[gvr], usageTarget{
				gv: schema.GroupVersion{Group: gr.Group, Version: s.version}.String(),
				// listV is flagged only when every listable served version is.
				allObjects: e.Replacement == nil && listV.flagged,
				usage: inventory.APIUsage{Group: gr.Group, Version: s.version, Kind: s.kind,
					Namespaces: map[string]int{}},
			})
		}
	}

	attempted, succeeded := 0, 0
	var usages []inventory.APIUsage
	for _, gvr := range toList {
		targets := targetsOf[gvr]
		attempted++
		if err := listUsage(ctx, meta, gvr, targets); err != nil {
			failures = append(failures, fmt.Sprintf("list %s %s: %v", gvr.GroupVersion(), gvr.Resource, err))
			continue
		}
		succeeded++
		for _, t := range targets {
			if t.usage.Count > 0 {
				usages = append(usages, t.usage)
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

// servedVersion is one version the cluster serves a resource at.
type servedVersion struct {
	version, kind string
	flagged       bool // the KB marks this group/version/kind deprecated or removed
	list          bool
}

// usageTarget accumulates the usage of one flagged group/version/kind
// while its resource is listed.
type usageTarget struct {
	gv         string // "group/version", or "v1" for core
	allObjects bool   // the type goes away: every object counts
	usage      inventory.APIUsage
}

// listVersion picks the version to list a resource at, or false when none
// is listable or none is flagged (nothing to look for); see bestListable.
// When every served version is flagged it returns a flagged one, and the
// caller first looks for the same objects in the replacement's group
// (replacementList).
func listVersion(served []servedVersion, preferred string) (servedVersion, bool) {
	if !slices.ContainsFunc(served, func(s servedVersion) bool { return s.flagged }) {
		return servedVersion{}, false
	}
	return bestListable(served, preferred)
}

// bestListable picks among the listable served versions: unflagged before
// flagged, then the group's preferred version, then the newest. A flagged
// version comes back only when every listable version is flagged, and
// listing it is unavoidable: the scanner then shows up in
// apiserver_requested_deprecated_apis for that resource (on 1.33+, every
// cluster does for core v1 endpoints and componentstatuses).
func bestListable(served []servedVersion, preferred string) (servedVersion, bool) {
	var listable []servedVersion
	for _, s := range served {
		if s.list {
			listable = append(listable, s)
		}
	}
	if len(listable) == 0 {
		return servedVersion{}, false
	}
	slices.SortStableFunc(listable, func(a, b servedVersion) int {
		switch {
		case a.flagged != b.flagged:
			if a.flagged {
				return 1
			}
			return -1
		case (a.version == preferred) != (b.version == preferred):
			if a.version == preferred {
				return -1
			}
			return 1
		}
		return version.CompareKubeAwareVersionStrings(b.version, a.version) // newest first
	})
	return listable[0], true
}

// replacementList returns where else to list resource when every version
// its own group serves is flagged: the KB replacement's group, if it serves
// the same resource and kind at an unflagged version. extensions/v1beta1
// ingresses on 1.19–1.21 are the same stored objects as networking.k8s.io/v1
// ingresses; listing there keeps the scanner off the deprecated endpoint,
// and managedFields still record the group/version each write went through.
func replacementList(resource string, r *kb.GVK, byResource map[schema.GroupResource][]servedVersion, preferred map[string]string) (schema.GroupVersionResource, bool) {
	if r == nil {
		return schema.GroupVersionResource{}, false
	}
	var same []servedVersion
	for _, s := range byResource[schema.GroupResource{Group: r.Group, Resource: resource}] {
		if s.kind == r.Kind {
			same = append(same, s)
		}
	}
	best, ok := bestListable(same, preferred[r.Group])
	if !ok || best.flagged {
		return schema.GroupVersionResource{}, false
	}
	return schema.GroupVersionResource{Group: r.Group, Version: best.version, Resource: resource}, true
}

// listUsage pages through one resource, metadata-only, and attributes each
// object to the targets it uses.
func listUsage(ctx context.Context, meta metadata.Interface, gvr schema.GroupVersionResource, targets []usageTarget) error {
	opts := metav1.ListOptions{Limit: listPageSize}
	for {
		page, err := meta.Resource(gvr).List(ctx, opts)
		if err != nil {
			return err
		}
		for i := range page.Items {
			m := &page.Items[i]
			if m.Annotations[apfAutoUpdateAnnotation] == "true" {
				continue
			}
			for j := range targets {
				t := &targets[j]
				manager := "" // a type that goes away counts objects, not authors
				if !t.allObjects {
					if manager = authoringManager(m, t.gv); manager == "" {
						continue
					}
				}
				t.usage.Count++
				t.usage.Namespaces[m.Namespace]++
				if len(t.usage.Objects) < inventory.MaxObjectRefs {
					t.usage.Objects = append(t.usage.Objects, inventory.ObjectRef{Namespace: m.Namespace, Name: m.Name, Manager: manager})
				} else {
					t.usage.ObjectsOmitted++
				}
			}
		}
		if page.Continue == "" {
			return nil
		}
		opts.Continue = page.Continue
	}
}

// authoringManager reports who still writes m through group/version gv,
// or "".
//
// managedFields come first. An Update entry's identity includes its
// apiVersion, so a manager that moved to another version keeps its old
// entry for every field its newer writes left alone; only each manager's
// newest entry says what it writes now. Entries are grouped by (manager,
// subresource), skipping internal managers and the status subresource, and
// a manager counts when it has an entry naming gv that is strictly newer
// than all of its entries naming another version (a missing timestamp
// cannot be ordered, so it counts as a tie, and ties clear). The first
// such manager, in managedFields order, is returned.
//
// The last-applied annotation is the fallback only when no entry is left
// to judge by: kubectl client-side apply rewrites it, nothing else does,
// so under any other writer's entries it may be long stale.
func authoringManager(m *metav1.PartialObjectMetadata, gv string) string {
	type key struct{ manager, subresource string }
	var order []key
	byKey := map[key][]metav1.ManagedFieldsEntry{}
	for _, f := range m.ManagedFields {
		if f.Subresource == "status" || internalManagers[f.Manager] {
			continue
		}
		k := key{f.Manager, f.Subresource}
		if _, seen := byKey[k]; !seen {
			order = append(order, k)
		}
		byKey[k] = append(byKey[k], f)
	}
	for _, k := range order {
		if writesNow(byKey[k], gv) {
			return k.manager
		}
	}
	if len(order) > 0 {
		return ""
	}
	if raw, ok := m.Annotations[lastAppliedAnnotation]; ok {
		var applied struct {
			APIVersion string `json:"apiVersion"`
		}
		if json.Unmarshal([]byte(raw), &applied) == nil && applied.APIVersion == gv {
			return lastAppliedManager
		}
	}
	return ""
}

// writesNow reports whether one manager's entries have an entry naming gv
// that is strictly newer than each of its entries naming another version.
func writesNow(entries []metav1.ManagedFieldsEntry, gv string) bool {
	for _, e := range entries {
		if e.APIVersion != gv {
			continue
		}
		newest := true
		for _, o := range entries {
			if o.APIVersion != gv && (e.Time == nil || o.Time == nil || !o.Time.Before(e.Time)) {
				newest = false
				break
			}
		}
		if newest {
			return true
		}
	}
	return false
}
