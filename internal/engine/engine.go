package engine

import (
	"fmt"
	"sort"
	"strings"

	"github.com/abd-ulbasit/upgradescope/internal/inventory"
	"github.com/abd-ulbasit/upgradescope/internal/kb"
)

const (
	deprecationGuideURL = "https://kubernetes.io/docs/reference/using-api/deprecation-guide/"
	skewPolicyURL       = "https://kubernetes.io/releases/version-skew-policy/"
)

type gvkKey struct{ group, version, kind string }

func lifecycleIndex(k kb.KB) map[gvkKey]kb.APILifecycleEntry {
	idx := make(map[gvkKey]kb.APILifecycleEntry, len(k.APILifecycle))
	for _, e := range k.APILifecycle {
		idx[gvkKey{e.Group, e.Version, e.Kind}] = e
	}
	return idx
}

// gvString renders "group/version"; the core group renders as just "version".
func gvString(group, version string) string {
	if group == "" {
		return version
	}
	return group + "/" + version
}

// namespaceBreakdown renders "ns (count)" parts sorted by namespace name and
// returns the sorted namespace names. The cluster-scoped key "" renders as
// "cluster-scoped" in the detail and is excluded from the returned names.
func namespaceBreakdown(counts map[string]int) (detail string, names []string) {
	keys := make([]string, 0, len(counts))
	for ns := range counts {
		keys = append(keys, ns)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, ns := range keys {
		label := ns
		if ns == "" {
			label = "cluster-scoped"
		} else {
			names = append(names, ns)
		}
		parts = append(parts, fmt.Sprintf("%s (%d)", label, counts[ns]))
	}
	return strings.Join(parts, ", "), names
}

// teamsFor maps namespace names to teams via the inventory's namespace team
// labels; result is deduped and sorted.
func teamsFor(namespaces []string, nsInfo []inventory.NamespaceInfo) []string {
	byName := make(map[string]string, len(nsInfo))
	for _, n := range nsInfo {
		byName[n.Name] = n.Team
	}
	seen := map[string]bool{}
	var teams []string
	for _, ns := range namespaces {
		if t := byName[ns]; t != "" && !seen[t] {
			seen[t] = true
			teams = append(teams, t)
		}
	}
	sort.Strings(teams)
	return teams
}

// evalAPIUsage: for each observed deprecated/removed GVK residency,
//   - removed at ≤ target          → blocker, removed-api
//   - removed exactly at target+1  → warning, removed-api
//   - deprecated, removal beyond the window or unset → info, deprecated-api
func evalAPIUsage(inv inventory.Inventory, k kb.KB, target inventory.Version) []Finding {
	idx := lifecycleIndex(k)
	var out []Finding
	for _, u := range inv.APIUsage {
		e, ok := idx[gvkKey{u.Group, u.Version, u.Kind}]
		if !ok {
			continue
		}
		nsDetail, nsNames := namespaceBreakdown(u.Namespaces)
		f := Finding{
			Teams:      teamsFor(nsNames, inv.Namespaces),
			Namespaces: nsNames,
			Citations:  []string{deprecationGuideURL},
		}
		if e.Replacement != nil {
			f.Remediation = fmt.Sprintf("migrate to %s %s",
				gvString(e.Replacement.Group, e.Replacement.Version), e.Replacement.Kind)
		}
		gv := gvString(u.Group, u.Version)
		switch {
		case e.Removed != nil && e.Removed.Compare(target) <= 0:
			f.Category = CatRemovedAPI
			f.Severity = SevBlocker
			f.Title = fmt.Sprintf("%s %s removed in %s (%d objects)", gv, u.Kind, e.Removed, u.Count)
		case e.Removed != nil && e.Removed.Compare(target.Next()) == 0:
			f.Category = CatRemovedAPI
			f.Severity = SevWarning
			f.Title = fmt.Sprintf("%s %s removed in %s (%d objects)", gv, u.Kind, e.Removed, u.Count)
		case e.Deprecated != nil:
			f.Category = CatDeprecatedAPI
			f.Severity = SevInfo
			f.Title = fmt.Sprintf("%s %s deprecated since %s (%d objects)", gv, u.Kind, e.Deprecated, u.Count)
		default:
			continue // KB entry exists but is neither deprecated nor removed
		}
		if nsDetail == "" {
			f.Detail = fmt.Sprintf("%d object(s) still stored/served at this version.", u.Count)
		} else {
			f.Detail = fmt.Sprintf("%d object(s) still stored/served at this version: %s.", u.Count, nsDetail)
		}
		out = append(out, f)
	}
	return out
}
