package collect

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
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

// The annotations that accept findings for one object, recorded on its
// ObjectRef by both the live and the files collector: a comma-separated
// list of finding categories or keys, and why (see internal/suppress).
const (
	IgnoreAnnotation       = "upgradescope.dev/ignore"
	IgnoreReasonAnnotation = "upgradescope.dev/ignore-reason"
)

// internalManagers are field managers inside the control plane. Their
// managedFields entries record the version that was current when that
// control-plane release wrote the object (the apiserver's default
// ServiceCIDR, IPAddresses and APF objects; controller-written objects),
// and go stale across upgrades without anyone writing the old version
// again. No user manifest is behind them. kube-controller-manager and
// kube-scheduler also write their own LeaseCandidates (coordinated leader
// election). A custom scheduler built on the kube-scheduler framework may
// report the field manager "kube-scheduler" too, so its writes through a
// deprecated version are not counted either. Schedulers mostly write
// bindings, Events and Leases through GA versions, so the gap is narrow.
var internalManagers = map[string]bool{
	"kube-apiserver":                               true,
	"kube-controller-manager":                      true,
	"kube-scheduler":                               true,
	"api-priority-and-fairness-config-producer-v1": true,
}

// controlPlaneOwned reports whether only internal managers ever wrote m:
// it has managedFields entries and every one names an internal manager.
// The control plane rewrites such objects itself after an upgrade.
func controlPlaneOwned(m *metav1.PartialObjectMetadata) bool {
	if len(m.ManagedFields) == 0 {
		return false
	}
	for _, f := range m.ManagedFields {
		if !internalManagers[f.Manager] {
			return false
		}
	}
	return true
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
// groups share the LIST). When every version the cluster serves is
// deprecated, the resource is listed there only if the KB schedules a
// removal for it: a kind that is deprecated but never removed (core v1
// Endpoints, ComponentStatus) can only ever yield info findings, which do
// not justify a deprecated request. Those deprecated LISTs that remain
// (policy/v1beta1 PodSecurityPolicy on 1.24) are returned as selfListed,
// "group/version resource", sorted: the scanner's own entries in
// apiserver_requested_deprecated_apis, which the deprecated-calls step
// marks so the engine does not report the scanner as a caller. Discovery
// and the KB alone decide them, so they are the same on every scan.
//
// Each object is attributed per flagged entry:
//
//   - the KB entry has no replacement, the KB records no version of the
//     kind that is neither deprecated nor removed, and no non-deprecated
//     version of the resource is served: the type itself goes away, so
//     every object counts, except objects only internal managers ever
//     wrote (controlPlaneOwned). For a kind upstream has shipped only as
//     alpha or beta (LeaseCandidate), "no surviving version" may just mean
//     the KB does not know its GA yet, and the control plane replaces its
//     own objects across an upgrade either way;
//   - otherwise only objects authored via the flagged group/version count:
//     some manager's newest managedFields entry (not an internal manager,
//     not the status subresource) names it, or, for an object with no such
//     entries, the last-applied annotation does (see authoringManager).
//
// An object with no managedFields entry at all and no last-applied
// annotation (created with no fields: the apiserver drops a manager's
// empty entry) cannot be attributed, and is stored the same through any
// version. It is not counted as use; it goes to inv.APIAuthorshipUnknown,
// which the engine reports as info (authorshipUnknown). The
// deprecated-calls metric covers live callers. APF objects the apiserver
// auto-updates are skipped.
//
// Failures are per-resource: a forbidden or broken endpoint is recorded
// and the remaining resources still run. If at least one resource (or
// none was flagged) succeeded, the aggregate comes back as an incomplete
// partialError, so the capability stays available but Partial, with the
// failures as Reason and, as Skipped, every flagged API a failed LIST or
// a failed group discovery left unchecked (the engine decides from them
// whether the gap can hide a blocker); if every flagged resource failed,
// the capability degrades fully.
func collectAPIUsage(ctx context.Context, disc discovery.DiscoveryInterface, meta metadata.Interface, lifecycle []kb.APILifecycleEntry, inv *inventory.Inventory) (selfListed []string, err error) {
	flagged := map[kb.GVK]kb.APILifecycleEntry{}
	// continues holds the kinds the KB records a version of that is neither
	// deprecated nor removed. They outlive their flagged versions even when
	// no replacement is recorded (ServiceCIDR, ValidatingAdmissionPolicy and
	// the DRA kinds graduated within their own group).
	continues := map[schema.GroupKind]bool{}
	for _, e := range lifecycle {
		if e.Deprecated != nil || e.Removed != nil {
			flagged[kb.GVK{Group: e.Group, Version: e.Version, Kind: e.Kind}] = e
		} else {
			continues[schema.GroupKind{Group: e.Group, Kind: e.Kind}] = true
		}
	}

	var failures []string
	unchecked := map[string]bool{} // flagged APIs not checked, "group/version Kind"

	groups, lists, err := discovery.ToServerResourcesInterfaceWithContext(disc).ServerGroupsAndResourcesWithContext(ctx)
	if err != nil {
		// Partial discovery failure (one broken aggregated API) must not
		// kill the capability; total failure does. Skipped groups are
		// surfaced in the capability reason, never silently dropped, and
		// any flagged API the KB records at one went unchecked.
		var gde *discovery.ErrGroupDiscoveryFailed
		if !errors.As(err, &gde) || lists == nil {
			return nil, fmt.Errorf("discovery: %w", err)
		}
		skipped := make([]string, 0, len(gde.Groups))
		for gv := range gde.Groups {
			skipped = append(skipped, gv.String())
			for k := range flagged {
				if k.Group == gv.Group && k.Version == gv.Version {
					unchecked[apiName(gv.String(), k.Kind)] = true
				}
			}
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
		deprecatedList := listV.flagged
		if listV.flagged {
			for _, s := range served {
				e := flagged[kb.GVK{Group: gr.Group, Version: s.version, Kind: s.kind}]
				if r, ok := replacementList(gr.Resource, e.Replacement, byResource, preferred); ok {
					gvr, deprecatedList = r, false
					break
				}
			}
		}
		if deprecatedList {
			// Listing here is itself a deprecated request. Worth it only
			// for a kind with a removal scheduled, whose objects can block
			// an upgrade; one that is never removed (core v1 Endpoints,
			// ComponentStatus) could only ever yield info findings.
			if !slices.ContainsFunc(served, func(s servedVersion) bool {
				return s.flagged && flagged[kb.GVK{Group: gr.Group, Version: s.version, Kind: s.kind}].Removed != nil
			}) {
				continue
			}
		}
		if _, seen := targetsOf[gvr]; !seen {
			toList = append(toList, gvr)
			if deprecatedList {
				selfListed = append(selfListed, gvr.GroupVersion().String()+" "+gvr.Resource)
			}
		}
		for _, s := range served {
			if !s.flagged {
				continue
			}
			e := flagged[kb.GVK{Group: gr.Group, Version: s.version, Kind: s.kind}]
			targetsOf[gvr] = append(targetsOf[gvr], usageTarget{
				gv: schema.GroupVersion{Group: gr.Group, Version: s.version}.String(),
				// The kind goes away: no replacement, no surviving version in
				// the KB, and none served (listV is flagged only when every
				// listable served version is).
				allObjects: e.Replacement == nil && !continues[schema.GroupKind{Group: gr.Group, Kind: s.kind}] && listV.flagged,
				usage: inventory.APIUsage{Group: gr.Group, Version: s.version, Kind: s.kind,
					Namespaces: map[string]int{}},
				unknown: inventory.APIUsage{Group: gr.Group, Version: s.version, Kind: s.kind,
					Namespaces: map[string]int{}},
			})
		}
	}

	slices.Sort(selfListed)

	attempted, succeeded := 0, 0
	var usages, unknowns []inventory.APIUsage
	for _, gvr := range toList {
		targets := targetsOf[gvr]
		attempted++
		if err := listUsage(ctx, meta, gvr, targets); err != nil {
			failures = append(failures, fmt.Sprintf("list %s %s: %v", gvr.GroupVersion(), gvr.Resource, err))
			for _, t := range targets {
				unchecked[apiName(t.gv, t.usage.Kind)] = true
			}
			continue
		}
		succeeded++
		for _, t := range targets {
			if t.usage.Count > 0 {
				usages = append(usages, t.usage)
			}
			if t.unknown.Count > 0 {
				unknowns = append(unknowns, t.unknown)
			}
		}
	}

	byGVK := func(a, b inventory.APIUsage) int {
		return cmp.Or(strings.Compare(a.Group, b.Group), strings.Compare(a.Version, b.Version), strings.Compare(a.Kind, b.Kind))
	}
	slices.SortFunc(usages, byGVK)
	slices.SortFunc(unknowns, byGVK)
	inv.APIUsage, inv.APIAuthorshipUnknown = usages, unknowns

	if len(failures) == 0 {
		return selfListed, nil
	}
	msg := strings.Join(failures, "; ")
	if attempted > 0 && succeeded == 0 {
		return selfListed, errors.New(msg) // nothing usable — degrade the capability
	}
	return selfListed, partialError{msg: msg, incomplete: true, skipped: slices.Sorted(maps.Keys(unchecked))}
}

// apiName renders a flagged API as CapabilityStatus.Skipped names it:
// "group/version Kind", core as "v1 Kind".
func apiName(gv, kind string) string {
	return gv + " " + kind
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
	// unknown holds the objects of a flagged kind that nothing attributes
	// (see authorshipUnknown), which usage does not count.
	unknown inventory.APIUsage
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
// version comes back only when every listable version is flagged; listing
// it makes the scanner show up in apiserver_requested_deprecated_apis for
// that resource, so collectAPIUsage does that only for kinds being removed
// and reports each such LIST.
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
				if t.allObjects && controlPlaneOwned(m) {
					continue // ...except the control plane's own, which it replaces
				}
				if !t.allObjects {
					manager = authoringManager(m, t.gv)
				}
				u := &t.usage
				if !t.allObjects && manager == "" {
					if !authorshipUnknown(m) {
						continue
					}
					u = &t.unknown
				}
				u.Count++
				u.Namespaces[m.Namespace]++
				if len(u.Objects) < inventory.MaxObjectRefs {
					u.Objects = append(u.Objects, inventory.ObjectRef{
						Namespace: m.Namespace, Name: m.Name, Manager: manager,
						Ignore: m.Annotations[IgnoreAnnotation], IgnoreReason: m.Annotations[IgnoreReasonAnnotation],
					})
				} else {
					u.ObjectsOmitted++
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
// managedFields come first, grouped by (manager, subresource) and skipping
// internal managers and the status subresource. The two operations are
// recorded differently, so they are judged differently:
//
//   - A manager has at most one Apply entry per subresource. Each
//     server-side apply replaces it, field set and apiVersion, so it is
//     the manager's current configuration: an Apply entry naming gv counts.
//   - Update entries are keyed by (manager, apiVersion). A manager that
//     moved to another version, by Update or by switching to server-side
//     apply under the same name, keeps its old entry for every field it
//     still co-owns, and nothing clears it. An Update entry naming gv
//     counts only when it is strictly newer than every entry of its group
//     naming another version, the Apply entry included. A missing
//     timestamp cannot be ordered, so it counts as a tie, and ties clear.
//
// The manager of the first group that counts, in managedFields order, is
// returned.
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

// authorshipUnknown reports whether m has nothing to attribute it by: no
// managedFields entry at all and no last-applied annotation. The apiserver
// prunes a field manager's entry when it owns no fields, so an object
// created with an empty spec (a DeviceClass through resource.k8s.io/v1beta1
// with spec {}) has neither, and is stored the same whichever version
// created it. An object whose entries are only internal managers' or the
// status subresource's is attributed, not unknown.
func authorshipUnknown(m *metav1.PartialObjectMetadata) bool {
	_, applied := m.Annotations[lastAppliedAnnotation]
	return len(m.ManagedFields) == 0 && !applied
}

// writesNow reports whether one manager's entries for one subresource say
// it still writes through gv: its Apply entry names gv, or an Update entry
// naming gv is strictly newer than each of its entries naming another
// version, whatever their operation.
func writesNow(entries []metav1.ManagedFieldsEntry, gv string) bool {
	for _, e := range entries {
		if e.APIVersion != gv {
			continue
		}
		if e.Operation == metav1.ManagedFieldsOperationApply {
			return true
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
