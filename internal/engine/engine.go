package engine

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/abd-ulbasit/upgradescope/internal/inventory"
	"github.com/abd-ulbasit/upgradescope/internal/kb"
	"github.com/abd-ulbasit/upgradescope/registry"
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

// evalDeprecatedCalls turns apiserver_requested_deprecated_apis rows into
// findings: removal ≤ target → blocker; removal == target+1 → warning;
// otherwise (incl. missing/unparseable removedRelease) → info.
func evalDeprecatedCalls(inv inventory.Inventory, target inventory.Version) []Finding {
	var out []Finding
	for _, c := range inv.DeprecatedCalls {
		res := c.Resource
		if c.Subresource != "" {
			res += "/" + c.Subresource
		}
		gv := gvString(c.Group, c.Version)
		f := Finding{Category: CatDeprecatedAPIInUse, Citations: []string{deprecationGuideURL}}
		removed, perr := inventory.ParseVersion(c.RemovedRelease)
		if c.RemovedRelease == "" || perr != nil {
			f.Severity = SevInfo
			f.Title = fmt.Sprintf("clients still requesting %s %s (deprecated)", gv, res)
			f.Detail = "apiserver_requested_deprecated_apis reports active clients for this API since the last apiserver restart; no removal release is recorded."
			out = append(out, f)
			continue
		}
		f.Title = fmt.Sprintf("clients still requesting %s %s (removed in %s)", gv, res, removed)
		f.Detail = fmt.Sprintf("apiserver_requested_deprecated_apis reports active clients for this API since the last apiserver restart; it is removed in %s.", removed)
		switch {
		case removed.Compare(target) <= 0:
			f.Severity = SevBlocker
		case removed.Compare(target.Next()) == 0:
			f.Severity = SevWarning
		default:
			f.Severity = SevInfo
		}
		out = append(out, f)
	}
	return out
}

// evalAddOns: registry lookup by instance ID.
//   - support.status == "eol" OR eol_date ≤ now            → blocker, eol-addon
//   - eol_date in (now, now+90d]                           → warning, eol-approaching
//   - version matches a Compat range with K8sMax < target  → blocker, chart-incompat
//
// An instance with no detected version produces no compat finding; an ID
// absent from the registry produces nothing.
func evalAddOns(inv inventory.Inventory, k kb.KB, target inventory.Version, now time.Time) []Finding {
	byID := make(map[string]registry.AddOn, len(k.AddOns))
	for _, a := range k.AddOns {
		byID[a.ID] = a
	}
	var out []Finding
	for _, inst := range inv.AddOns {
		a, ok := byID[inst.ID]
		if !ok {
			continue
		}
		ns := append([]string(nil), inst.Namespaces...)
		sort.Strings(ns)
		teams := teamsFor(ns, inv.Namespaces)
		ver := inst.Version
		if ver == "" {
			ver = "(unknown)"
		}
		located := fmt.Sprintf("Detected %s version %s via %s in namespace(s): %s.",
			a.DisplayName, ver, inst.Source, strings.Join(ns, ", "))

		var eolDate time.Time
		hasDate := false
		if a.Support.EOLDate != "" {
			if d, err := time.Parse("2006-01-02", a.Support.EOLDate); err == nil {
				eolDate, hasDate = d, true
			}
		}
		switch {
		case a.Support.Status == "eol" || (hasDate && !eolDate.After(now)):
			title := fmt.Sprintf("%s is end-of-life", a.DisplayName)
			if a.Support.EOLDate != "" {
				title = fmt.Sprintf("%s is end-of-life since %s", a.DisplayName, a.Support.EOLDate)
			}
			out = append(out, Finding{
				Category: CatEOLAddon, Severity: SevBlocker,
				Title:       title,
				Detail:      located + " Upstream support has ended.",
				Teams:       teams,
				Namespaces:  ns,
				Remediation: a.Recommendation,
				Citations:   append([]string(nil), a.Support.Citations...),
			})
		case hasDate && eolDate.After(now) && !eolDate.After(now.Add(90*24*time.Hour)):
			out = append(out, Finding{
				Category: CatEOLApproaching, Severity: SevWarning,
				Title:       fmt.Sprintf("%s reaches end-of-life on %s", a.DisplayName, a.Support.EOLDate),
				Detail:      located + fmt.Sprintf(" Upstream support ends on %s.", a.Support.EOLDate),
				Teams:       teams,
				Namespaces:  ns,
				Remediation: a.Recommendation,
				Citations:   append([]string(nil), a.Support.Citations...),
			})
		}

		if inst.Version == "" {
			continue // cannot match a compat range without a detected version
		}
		for _, c := range a.Compat {
			if !matchesRange(inst.Version, c.Range) {
				continue
			}
			kmax, err := inventory.ParseVersion(c.K8sMax)
			if err == nil && kmax.Compare(target) < 0 {
				out = append(out, Finding{
					Category: CatChartIncompat, Severity: SevBlocker,
					Title:       fmt.Sprintf("%s %s supports Kubernetes up to %s (target %s)", a.DisplayName, inst.Version, c.K8sMax, target),
					Detail:      fmt.Sprintf("Installed version %s matches compatibility range %q, which supports Kubernetes %s through %s.", inst.Version, c.Range, c.K8sMin, c.K8sMax),
					Teams:       teams,
					Namespaces:  ns,
					Remediation: a.Recommendation,
					Citations:   append([]string(nil), c.Citations...),
				})
			}
			break // first matching range wins
		}
	}
	return out
}

// matchesRange is a minimal semver-constraint matcher (stdlib only): clauses
// separated by spaces/commas, each ">=x.y.z", "<=x.y.z", ">x.y.z", "<x.y.z",
// or "=x.y.z" (bare versions mean "="). Pre-release/build suffixes on the
// candidate version are trimmed. An empty constraint never matches.
func matchesRange(version, constraint string) bool {
	if strings.TrimSpace(constraint) == "" {
		return false
	}
	v, ok := parseSemver(version)
	if !ok {
		return false
	}
	clauses := strings.FieldsFunc(constraint, func(r rune) bool { return r == ',' || r == ' ' })
	for _, clause := range clauses {
		op, rest := "=", clause
		for _, o := range []string{">=", "<=", ">", "<", "="} {
			if strings.HasPrefix(clause, o) {
				op, rest = o, strings.TrimPrefix(clause, o)
				break
			}
		}
		w, ok := parseSemver(rest)
		if !ok {
			return false
		}
		c := compareSemver(v, w)
		switch op {
		case ">=":
			if c < 0 {
				return false
			}
		case "<=":
			if c > 0 {
				return false
			}
		case ">":
			if c <= 0 {
				return false
			}
		case "<":
			if c >= 0 {
				return false
			}
		case "=":
			if c != 0 {
				return false
			}
		}
	}
	return true
}

type semver [3]int

func parseSemver(s string) (semver, bool) {
	s = strings.TrimPrefix(strings.TrimSpace(s), "v")
	if i := strings.IndexAny(s, "-+"); i >= 0 {
		s = s[:i] // drop pre-release/build metadata
	}
	parts := strings.Split(s, ".")
	if len(parts) == 0 || len(parts) > 3 || s == "" {
		return semver{}, false
	}
	var v semver
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return semver{}, false
		}
		v[i] = n
	}
	return v, true
}

func compareSemver(a, b semver) int {
	for i := 0; i < 3; i++ {
		if a[i] != b[i] {
			if a[i] < b[i] {
				return -1
			}
			return 1
		}
	}
	return 0
}

// evalSkew evaluates ONLY the kubelet rule in P1: kubelets more than
// policy.KubeletMaxBehind minors behind the CURRENT control plane → warning
// (skew is about today); kubelets that WOULD exceed the limit after the
// control plane reaches target → blocker. KubectlMaxSkew and
// APIServerHASpread are skipped in P1 — the scan collector gathers no kubectl
// or per-apiserver version data (re-added in P2 agent mode).
func evalSkew(inv inventory.Inventory, k kb.KB, target inventory.Version) []Finding {
	server, err := inventory.ParseVersion(inv.ServerVersion)
	if err != nil {
		return nil // versions capability degraded; surfaced via NotAssessed
	}
	maxBehind := k.Skew.KubeletMaxBehind
	var nowBad, postBad []string
	for _, n := range inv.Nodes {
		kv, err := inventory.ParseVersion(n.KubeletVersion)
		if err != nil {
			continue
		}
		entry := fmt.Sprintf("%s (%s)", n.Name, n.KubeletVersion)
		if minorsBehind(server, kv) > maxBehind {
			nowBad = append(nowBad, entry)
		}
		if minorsBehind(target, kv) > maxBehind {
			postBad = append(postBad, entry)
		}
	}
	sort.Strings(nowBad)
	sort.Strings(postBad)
	var out []Finding
	if len(postBad) > 0 {
		out = append(out, Finding{
			Category: CatVersionSkew, Severity: SevBlocker,
			Title:     fmt.Sprintf("%d node(s) would exceed kubelet version skew after upgrading to %s", len(postBad), target),
			Detail:    fmt.Sprintf("After upgrading the control plane to %s these nodes would be more than %d minor versions behind: %s.", target, maxBehind, strings.Join(postBad, ", ")),
			Citations: []string{skewPolicyURL},
		})
	}
	if len(nowBad) > 0 {
		out = append(out, Finding{
			Category: CatVersionSkew, Severity: SevWarning,
			Title:     fmt.Sprintf("%d node(s) exceed kubelet version skew vs control plane %s", len(nowBad), server),
			Detail:    fmt.Sprintf("Nodes more than %d minor versions behind: %s.", maxBehind, strings.Join(nowBad, ", ")),
			Citations: []string{skewPolicyURL},
		})
	}
	return out
}

// minorsBehind flattens (major, minor) so cross-major comparisons stay sane.
func minorsBehind(ctrl, kubelet inventory.Version) int {
	return (ctrl.Major*1000 + ctrl.Minor) - (kubelet.Major*1000 + kubelet.Minor)
}

// evalKBStale: control plane newer than anything the embedded KB knows about
// → the scan itself may be missing removals → warning.
func evalKBStale(inv inventory.Inventory, k kb.KB) []Finding {
	server, err := inventory.ParseVersion(inv.ServerVersion)
	if err != nil || server.Compare(k.MaxKnownK8s) <= 0 {
		return nil
	}
	return []Finding{{
		Category: CatKBStale, Severity: SevWarning,
		Title:  fmt.Sprintf("knowledge base does not cover Kubernetes %s (newest known: %s)", server, k.MaxKnownK8s),
		Detail: fmt.Sprintf("Findings may be incomplete; regenerate the knowledge base (kb version %s).", k.Version),
	}}
}
