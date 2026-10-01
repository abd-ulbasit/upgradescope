package engine

import (
	"cmp"
	"fmt"
	"maps"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Masterminds/semver/v3"

	"github.com/abd-ulbasit/upgradescope/internal/inventory"
	"github.com/abd-ulbasit/upgradescope/internal/kb"
	"github.com/abd-ulbasit/upgradescope/registry"
)

const (
	deprecationGuideURL = "https://kubernetes.io/docs/reference/using-api/deprecation-guide/"
	skewPolicyURL       = "https://kubernetes.io/releases/version-skew-policy/"
)

// pluralObjects renders an object count with grammatical number:
// "1 object", "3 objects".
func pluralObjects(n int) string {
	if n == 1 {
		return "1 object"
	}
	return fmt.Sprintf("%d objects", n)
}

// gvString renders "group/version"; the core group renders as just "version".
func gvString(group, version string) string {
	if group == "" {
		return version
	}
	return group + "/" + version
}

// keyGroup renders an API group for Finding.Key; the core group's empty
// string becomes "core" so keys never contain an empty path segment.
func keyGroup(group string) string {
	if group == "" {
		return "core"
	}
	return group
}

// namespaceBreakdown renders "ns (count)" parts sorted by namespace name and
// returns the sorted namespace names. The empty key "" renders as
// emptyLabel in the detail and is excluded from the returned names.
func namespaceBreakdown(counts map[string]int, emptyLabel string) (detail string, names []string) {
	keys := make([]string, 0, len(counts))
	for ns := range counts {
		keys = append(keys, ns)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, ns := range keys {
		label := ns
		if ns == "" {
			label = emptyLabel
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
	idx := kb.NewIndex(k.APILifecycle)
	var out []Finding
	for _, u := range inv.APIUsage {
		e, ok := idx.Lookup(u.Group, u.Version, u.Kind)
		if !ok {
			continue
		}
		// Manifest objects (refs read from text carry a line) are proposed
		// state, not stored objects, and an empty namespace there means
		// metadata.namespace is unset — helm template output usually omits
		// it — not that the object is cluster-scoped.
		manifests := len(u.Objects) > 0 && u.Objects[0].Line > 0
		emptyNS := "cluster-scoped"
		if manifests {
			emptyNS = "namespace unset"
		}
		nsDetail, nsNames := namespaceBreakdown(u.Namespaces, emptyNS)
		f := Finding{
			Teams:          teamsFor(nsNames, inv.Namespaces),
			Namespaces:     nsNames,
			Citations:      []string{deprecationGuideURL},
			Objects:        sortedObjects(u.Objects),
			ObjectsOmitted: u.ObjectsOmitted,
		}
		if r, ok := idx.ResolveReplacement(e, target); ok {
			f.Remediation = fmt.Sprintf("migrate to %s %s", gvString(r.Group, r.Version), r.Kind)
		}
		gv := gvString(u.Group, u.Version)
		switch {
		case e.Removed != nil && e.Removed.Compare(target) <= 0:
			f.Category = CatRemovedAPI
			f.Severity = SevBlocker
			f.Title = fmt.Sprintf("%s %s removed in %s (%s)", gv, u.Kind, e.Removed, pluralObjects(u.Count))
		case e.Removed != nil && e.Removed.Compare(target.Next()) == 0:
			f.Category = CatRemovedAPI
			f.Severity = SevWarning
			f.Title = fmt.Sprintf("%s %s removed in %s (%s)", gv, u.Kind, e.Removed, pluralObjects(u.Count))
		case e.Deprecated != nil:
			f.Category = CatDeprecatedAPI
			f.Severity = SevInfo
			f.Title = fmt.Sprintf("%s %s deprecated since %s (%s)", gv, u.Kind, e.Deprecated, pluralObjects(u.Count))
		default:
			continue // KB entry exists but is neither deprecated nor removed
		}
		f.Key = fmt.Sprintf("%s/%s/%s/%s", f.Category, keyGroup(u.Group), u.Version, u.Kind)
		detail := "%d object(s) still stored/served at this version"
		if manifests {
			detail = "%d manifest object(s) use this API"
		}
		if nsDetail == "" {
			f.Detail = fmt.Sprintf(detail+".", u.Count)
		} else {
			f.Detail = fmt.Sprintf(detail+": %s.", u.Count, nsDetail)
		}
		out = append(out, f)
	}
	return out
}

// sortedObjects returns a sorted copy of refs (file, line, namespace, name)
// so findings are deterministic whatever order a collector produced; nil
// stays nil.
func sortedObjects(refs []inventory.ObjectRef) []inventory.ObjectRef {
	if len(refs) == 0 {
		return nil
	}
	out := slices.Clone(refs)
	slices.SortStableFunc(out, func(a, b inventory.ObjectRef) int {
		return cmp.Or(
			cmp.Compare(a.File, b.File),
			cmp.Compare(a.Line, b.Line),
			cmp.Compare(a.Namespace, b.Namespace),
			cmp.Compare(a.Name, b.Name),
		)
	})
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
		f := Finding{
			Category:  CatDeprecatedAPIInUse,
			Key:       fmt.Sprintf("%s/%s/%s/%s", CatDeprecatedAPIInUse, keyGroup(c.Group), c.Version, res),
			Citations: []string{deprecationGuideURL},
		}
		removed, perr := inventory.ParseVersion(c.RemovedRelease)
		if c.RemovedRelease == "" || perr != nil {
			f.Severity = SevInfo
			f.Title = fmt.Sprintf("clients still requesting %s %s (deprecated)", gv, res)
			if c.RemovedRelease == "" {
				f.Detail = "apiserver_requested_deprecated_apis reports active clients for this API since the last apiserver restart; no removal release is recorded."
			} else {
				f.Detail = fmt.Sprintf("apiserver_requested_deprecated_apis reports active clients for this API since the last apiserver restart; removal release %q could not be parsed.", c.RemovedRelease)
			}
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

// evalAddOns looks every detected add-on up in the registry by ID and
// judges it with evalAddOn; an ID absent from the registry produces nothing.
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
		ver := inst.Version
		if ver == "" {
			ver = "(unknown)"
		}
		via := inst.Source
		if inst.ChartVersion != "" {
			via += " " + inst.ChartVersion // chart version: evidence only
		}
		out = append(out, evalAddOn(a, addOnSubject{
			version: inst.Version,
			located: fmt.Sprintf("Detected %s version %s via %s in namespace(s): %s.",
				a.DisplayName, ver, via, strings.Join(ns, ", ")),
			namespaces: ns,
			teams:      teamsFor(ns, inv.Namespaces),
		}, target, now)...)
	}
	return append(out, evalNodeRuntimes(inv, k.AddOns, target, now)...)
}

// evalNodeRuntimes judges node container runtimes
// (status.nodeInfo.containerRuntimeVersion, "containerd://1.7.27") against
// registry entries with a runtimes matcher, with evalAddOn. Nodes are
// grouped by release line so each finding names exactly the nodes on that
// line; a group is judged at its oldest version, and nodes whose version
// maps to no cycle (or is unknown) form one group that names each node's
// version.
func evalNodeRuntimes(inv inventory.Inventory, addons []registry.AddOn, target inventory.Version, now time.Time) []Finding {
	type group struct {
		version string
		nodes   []string
	}
	var out []Finding
	for _, a := range addons {
		if len(a.Matchers.Runtimes) == 0 {
			continue
		}
		groups := map[string]*group{} // by cycle; "" = no cycle
		for _, n := range inv.Nodes {
			runtime, ver, ok := strings.Cut(n.ContainerRuntime, "://")
			if !ok || !slices.Contains(a.Matchers.Runtimes, runtime) {
				continue
			}
			ver = strings.TrimPrefix(ver, "v")
			c, _ := cycleFor(ver, a.Cycles)
			g := groups[c.Cycle]
			if g == nil {
				g = &group{}
				groups[c.Cycle] = g
			}
			name := n.Name
			if c.Cycle == "" { // versions differ within this group: name each
				name += " (" + cmp.Or(ver, "version unknown") + ")"
			}
			g.nodes = append(g.nodes, name)
			if ver != "" && (g.version == "" || versionBefore(ver, g.version)) {
				g.version = ver
			}
		}
		for _, key := range slices.Sorted(maps.Keys(groups)) {
			g := groups[key]
			sort.Strings(g.nodes)
			located := fmt.Sprintf("Detected %s on node(s): %s.", a.DisplayName, strings.Join(g.nodes, ", "))
			if key != "" {
				located = fmt.Sprintf("Detected %s version %s on node(s): %s.", a.DisplayName, g.version, strings.Join(g.nodes, ", "))
			}
			out = append(out, evalAddOn(a, addOnSubject{
				version: g.version,
				located: located,
				node:    true,
			}, target, now)...)
		}
	}
	return out
}

// versionBefore orders versions numerically ("1.7.9" < "1.7.20"), falling
// back to string order when either does not parse.
func versionBefore(a, b string) bool {
	pa, okA := versionParts(a)
	pb, okB := versionParts(b)
	if okA && okB {
		return slices.Compare(pa, pb) < 0
	}
	return a < b
}

// addOnSubject is one detected installation of a registry add-on.
type addOnSubject struct {
	version    string   // normalised app version; "" when unknown
	located    string   // evidence sentence that opens every finding's detail
	namespaces []string // sorted
	teams      []string
	// node marks a node container runtime. It ships with the node image or
	// OS, which a node upgrade or node-pool image bump replaces, so an
	// ended release line is a warning; only a compat row (the kubelet
	// dropping support) blocks.
	node bool
}

// evalAddOn judges one detected add-on:
//
//   - product level (support), for whole-product retirements such as
//     ingress-nginx: status "eol" or eol_date ≤ now → blocker, eol-addon;
//     eol_date in (now, now+90d] → warning, eol-approaching.
//   - release line (cycles), unless the product carries a date or EOL
//     status: the installed version's cycle has ended → blocker, eol-addon
//     (warning for a node runtime); it ends in (now, now+90d] → warning,
//     eol-approaching.
//   - target outside the cycle's [k8s_min, k8s_max], or else outside the
//     bounds of the first compat row whose range matches the version
//     → blocker, chart-incompat.
//   - no product date and no cycle for the version, or no version at all
//     → info, addon-no-data: missing data must neither block nor read as
//     "checked, fine".
//
// Findings about a release line are keyed category/id/cycle, others
// category/id. Without a detected version no compat row is matched.
func evalAddOn(a registry.AddOn, s addOnSubject, target inventory.Version, now time.Time) []Finding {
	finding := func(cat Category, sev Severity, key, title, detail string, citations []string) Finding {
		return Finding{
			Category: cat, Severity: sev, Key: key, Title: title, Detail: detail,
			Teams: s.teams, Namespaces: s.namespaces, Remediation: a.Recommendation,
			Citations: append([]string(nil), citations...),
		}
	}
	window := now.Add(90 * 24 * time.Hour)
	var out []Finding

	var eolDate time.Time
	hasDate := false
	if a.Support.EOLDate != "" {
		if d, err := time.Parse("2006-01-02", a.Support.EOLDate); err == nil {
			eolDate, hasDate = d, true
		}
	}
	switch {
	case a.Support.Status == "eol" || (hasDate && !eolDate.After(now)):
		// Tense follows the date: status "eol" can carry a future
		// effective date (upstream already declared EOL).
		title := fmt.Sprintf("%s is end-of-life", a.DisplayName)
		detail := s.located + " Upstream support has ended."
		switch {
		case hasDate && eolDate.After(now):
			title = fmt.Sprintf("%s is end-of-life on %s", a.DisplayName, a.Support.EOLDate)
			detail = s.located + fmt.Sprintf(" Upstream support ends on %s.", a.Support.EOLDate)
		case a.Support.EOLDate != "":
			title = fmt.Sprintf("%s is end-of-life since %s", a.DisplayName, a.Support.EOLDate)
		}
		out = append(out, finding(CatEOLAddon, SevBlocker, string(CatEOLAddon)+"/"+a.ID, title, detail, a.Support.Citations))
	case hasDate && !eolDate.After(window):
		out = append(out, finding(CatEOLApproaching, SevWarning, string(CatEOLApproaching)+"/"+a.ID,
			fmt.Sprintf("%s reaches end-of-life on %s", a.DisplayName, a.Support.EOLDate),
			s.located+fmt.Sprintf(" Upstream support ends on %s.", a.Support.EOLDate),
			a.Support.Citations))
	}
	productDated := a.Support.Status == "eol" || hasDate

	cycle, inCycle := cycleFor(s.version, a.Cycles)
	key := func(cat Category) string {
		if inCycle {
			return string(cat) + "/" + a.ID + "/" + cycle.Cycle
		}
		return string(cat) + "/" + a.ID
	}
	if inCycle && !productDated {
		if f, ok := cycleEOL(a, cycle, s, now, window); ok {
			f.Key = key(f.Category)
			f.Teams, f.Namespaces = s.teams, s.namespaces
			if s.node && f.Severity == SevBlocker {
				f.Severity = SevWarning
				f.Detail += " The runtime comes with the node image or OS, not with the Kubernetes version, so this does not block the upgrade by itself."
			}
			out = append(out, f)
		}
	}

	if s.version != "" {
		compat := false
		if inCycle {
			if title, bad := k8sOutOfRange(a.DisplayName, s.version, cycle.K8sMin, cycle.K8sMax, target); bad {
				out = append(out, finding(CatChartIncompat, SevBlocker, key(CatChartIncompat), title,
					fmt.Sprintf("Installed version %s is in the %s release line, which supports Kubernetes %s.",
						s.version, cycle.Cycle, k8sRangeText(cycle.K8sMin, cycle.K8sMax)),
					cycle.Citations))
				compat = true
			}
		}
		for _, c := range a.Compat {
			if compat || !matchesRange(s.version, c.Range) {
				continue
			}
			if title, bad := k8sOutOfRange(a.DisplayName, s.version, c.K8sMin, c.K8sMax, target); bad {
				out = append(out, finding(CatChartIncompat, SevBlocker, key(CatChartIncompat), title,
					fmt.Sprintf("Installed version %s matches compatibility range %q, which supports Kubernetes %s.",
						s.version, c.Range, k8sRangeText(c.K8sMin, c.K8sMax)),
					c.Citations))
			}
			break // first matching range wins
		}
	}

	if !productDated && !inCycle {
		ver, reason := s.version, " The registry has no release-line data for this version, so its end of life was not assessed."
		if ver == "" {
			ver, reason = "(version unknown)", " No version could be read from the image tag or chart, so its end of life and Kubernetes compatibility were not assessed."
		}
		f := finding(CatAddOnNoData, SevInfo, string(CatAddOnNoData)+"/"+a.ID,
			fmt.Sprintf("no lifecycle data for %s %s", a.DisplayName, ver), s.located+reason, a.Support.Citations)
		f.Remediation = ""
		out = append(out, f)
	}
	return out
}

// cycleEOL judges the end of life of the release line a version is in:
// ended → blocker, ending by window → warning; ok is false otherwise. Key,
// Teams and Namespaces are left to the caller.
func cycleEOL(a registry.AddOn, c registry.Cycle, s addOnSubject, now, window time.Time) (Finding, bool) {
	if c.EOL == nil {
		return Finding{}, false // registry validation requires eol; defensive
	}
	f := Finding{Remediation: a.Recommendation}
	for _, u := range append(slices.Clone(c.Citations), a.Support.Citations...) {
		if !slices.Contains(f.Citations, u) {
			f.Citations = append(f.Citations, u)
		}
	}
	d, err := time.Parse("2006-01-02", c.EOL.Date)
	switch {
	case c.EOL.Date == "" && c.EOL.Ended:
		f.Category, f.Severity = CatEOLAddon, SevBlocker
		f.Title = fmt.Sprintf("%s %s is end-of-life", a.DisplayName, c.Cycle)
		f.Detail = s.located + fmt.Sprintf(" Upstream support for the %s release line has ended.", c.Cycle)
	case err != nil:
		return Finding{}, false // no end announced (eol: false)
	case !d.After(now):
		f.Category, f.Severity = CatEOLAddon, SevBlocker
		f.Title = fmt.Sprintf("%s %s is end-of-life since %s", a.DisplayName, c.Cycle, c.EOL.Date)
		f.Detail = s.located + fmt.Sprintf(" Upstream support for the %s release line ended on %s.", c.Cycle, c.EOL.Date)
	case !d.After(window):
		f.Category, f.Severity = CatEOLApproaching, SevWarning
		f.Title = fmt.Sprintf("%s %s reaches end-of-life on %s", a.DisplayName, c.Cycle, c.EOL.Date)
		f.Detail = s.located + fmt.Sprintf(" Upstream support for the %s release line ends on %s.", c.Cycle, c.EOL.Date)
	default:
		return Finding{}, false
	}
	if newest := newestSupportedCycle(a.Cycles, now); f.Remediation == "" && newest != "" {
		f.Remediation = fmt.Sprintf("Upgrade %s to a supported release line (newest: %s).", a.DisplayName, newest)
	}
	return f, true
}

// cycleFor maps a version to the most specific cycle whose dotted
// components are the version's leading components: "1.31.1" → "1.31" (or
// "1" in a major-only scheme). Pre-release and build suffixes are ignored;
// an empty or unparseable version has no cycle.
func cycleFor(version string, cycles []registry.Cycle) (registry.Cycle, bool) {
	v, ok := versionParts(version)
	if !ok {
		return registry.Cycle{}, false
	}
	best, bestLen := -1, 0
	for i, c := range cycles {
		p, ok := versionParts(c.Cycle)
		if !ok || len(p) > len(v) || len(p) <= bestLen || !slices.Equal(v[:len(p)], p) {
			continue
		}
		best, bestLen = i, len(p)
	}
	if best < 0 {
		return registry.Cycle{}, false
	}
	return cycles[best], true
}

// versionParts splits "1.31.0-rc.0" into [1 31 0].
func versionParts(s string) ([]int, bool) {
	if i := strings.IndexAny(s, "-+"); i >= 0 {
		s = s[:i]
	}
	if s == "" {
		return nil, false
	}
	var parts []int
	for _, f := range strings.Split(s, ".") {
		n, err := strconv.Atoi(f)
		if err != nil {
			return nil, false
		}
		parts = append(parts, n)
	}
	return parts, true
}

// newestSupportedCycle is the highest cycle not ended by now, "" if none.
func newestSupportedCycle(cycles []registry.Cycle, now time.Time) string {
	var best []int
	name := ""
	for _, c := range cycles {
		if c.EOL == nil || c.EOL.Ended {
			continue
		}
		if d, err := time.Parse("2006-01-02", c.EOL.Date); c.EOL.Date != "" && (err != nil || !d.After(now)) {
			continue
		}
		if p, ok := versionParts(c.Cycle); ok && slices.Compare(p, best) > 0 {
			best, name = p, c.Cycle
		}
	}
	return name
}

// k8sOutOfRange reports whether target is outside [k8sMin, k8sMax] (an
// empty bound is open) and titles the finding. Registry validation
// guarantees the bounds parse; an unparseable one is treated as open.
func k8sOutOfRange(name, version, k8sMin, k8sMax string, target inventory.Version) (string, bool) {
	if kmax, err := inventory.ParseVersion(k8sMax); k8sMax != "" && err == nil && kmax.Compare(target) < 0 {
		return fmt.Sprintf("%s %s supports Kubernetes up to %s (target %s)", name, version, k8sMax, target), true
	}
	if kmin, err := inventory.ParseVersion(k8sMin); k8sMin != "" && err == nil && target.Compare(kmin) < 0 {
		return fmt.Sprintf("%s %s requires Kubernetes %s or newer (target %s)", name, version, k8sMin, target), true
	}
	return "", false
}

// k8sRangeText renders a supported Kubernetes range with open bounds.
func k8sRangeText(k8sMin, k8sMax string) string {
	switch {
	case k8sMin == "":
		return "up to " + k8sMax
	case k8sMax == "":
		return k8sMin + " and newer"
	}
	return k8sMin + " through " + k8sMax
}

// matchesRange reports whether the detected add-on version satisfies a
// registry Compat.Range. It uses github.com/Masterminds/semver/v3 — the SAME
// grammar registry.Validate accepts (">=", "^", "~", ".x" wildcards, "||"
// unions, operator whitespace) — so every range that passes registry
// validation is matchable here; no silent false negatives.
//
// Pre-release/build suffixes on the candidate version are stripped before
// checking: a detected pre-release build of an add-on must still be evaluated
// against compat ranges (Masterminds would otherwise exclude pre-releases
// from non-pre-release constraints). An empty or unparseable constraint, or
// an unparseable version, never matches.
func matchesRange(version, constraint string) bool {
	c, err := semver.NewConstraint(constraint)
	if err != nil {
		return false
	}
	v, err := semver.NewVersion(strings.TrimSpace(version))
	if err != nil {
		return false
	}
	if v.Prerelease() != "" || v.Metadata() != "" {
		stripped, err := v.SetPrerelease("")
		if err != nil {
			return false
		}
		stripped, err = stripped.SetMetadata("")
		if err != nil {
			return false
		}
		v = &stripped
	}
	return c.Check(v)
}

// evalSkew evaluates the kubelet rules of the upstream version-skew policy
// against every observed kube-apiserver version (apiserverVersions):
//
//   - a kubelet more than KubeletMaxBehind minors behind the NEWEST
//     apiserver → warning (skew is about today);
//   - a kubelet that WOULD exceed that limit once the control plane reaches
//     target → blocker;
//   - a kubelet newer than the OLDEST apiserver → warning ("kubelet must not
//     be newer than kube-apiserver"; with HA skew the oldest replica narrows
//     the allowed versions).
//
// Kubelets older than 1.25 may only be 2 minors behind (legacyMaxBehind).
// Control-plane component rules live in evalControlPlaneSkew.
// KubectlMaxSkew is NOT evaluated by design: client kubectl versions are only
// visible in apiserver audit logs (User-Agent), which no collector reads —
// there is no cluster-state signal for them.
func evalSkew(inv inventory.Inventory, k kb.KB, target inventory.Version) []Finding {
	apis := apiserverVersions(inv)
	if len(apis) == 0 {
		return nil // no reference version: a required versions gap, recorded by assessmentGaps
	}
	oldest, newest := apis[0], apis[len(apis)-1]
	maxBehind := k.Skew.KubeletMaxBehind
	var nowBad, postBad, newer, unparseable []string
	var postLegacy, nowLegacy bool
	for _, n := range inv.Nodes {
		kv, err := inventory.ParseVersion(n.KubeletVersion)
		if err != nil {
			unparseable = append(unparseable, fmt.Sprintf("%s (%q)", n.Name, n.KubeletVersion))
			continue
		}
		entry := fmt.Sprintf("%s (%s)", n.Name, n.KubeletVersion)
		limit := legacyMaxBehind(maxBehind, kv)
		if minorsBehind(newest, kv) > limit {
			nowBad = append(nowBad, entry)
			nowLegacy = nowLegacy || limit < maxBehind
		}
		if minorsBehind(target, kv) > limit {
			postBad = append(postBad, entry)
			postLegacy = postLegacy || limit < maxBehind
		}
		if kv.Compare(oldest) > 0 {
			newer = append(newer, entry)
		}
	}
	sort.Strings(nowBad)
	sort.Strings(postBad)
	sort.Strings(newer)
	var out []Finding
	if len(postBad) > 0 {
		out = append(out, Finding{
			Category: CatVersionSkew, Severity: SevBlocker,
			Key:       string(CatVersionSkew) + "/kubelet-post-upgrade",
			Title:     fmt.Sprintf("%d node(s) would exceed kubelet version skew after upgrading to %s", len(postBad), target),
			Detail:    fmt.Sprintf("After upgrading the control plane to %s these nodes would be more than %d minor versions behind: %s.", target, maxBehind, strings.Join(postBad, ", ")) + legacyNote("Kubelets", postLegacy),
			Citations: []string{skewPolicyURL},
		})
	}
	if len(nowBad) > 0 {
		out = append(out, Finding{
			Category: CatVersionSkew, Severity: SevWarning,
			Key:       string(CatVersionSkew) + "/kubelet-current",
			Title:     fmt.Sprintf("%d node(s) exceed kubelet version skew vs control plane %s", len(nowBad), newest),
			Detail:    fmt.Sprintf("Nodes more than %d minor versions behind: %s.", maxBehind, strings.Join(nowBad, ", ")) + legacyNote("Kubelets", nowLegacy),
			Citations: []string{skewPolicyURL},
		})
	}
	if len(newer) > 0 {
		out = append(out, Finding{
			Category: CatVersionSkew, Severity: SevWarning,
			Key:       string(CatVersionSkew) + "/kubelet-newer-than-apiserver",
			Title:     fmt.Sprintf("%d node(s) run a kubelet newer than kube-apiserver %s", len(newer), oldest),
			Detail:    fmt.Sprintf("kubelet must not be newer than kube-apiserver; when kube-apiserver versions differ, the oldest one (%s) bounds the allowed kubelet versions. Newer nodes: %s.", oldest, strings.Join(newer, ", ")),
			Citations: []string{skewPolicyURL},
		})
	}
	if len(unparseable) > 0 {
		sort.Strings(unparseable)
		out = append(out, Finding{
			Category: CatVersionSkew, Severity: SevInfo,
			Key:       string(CatVersionSkew) + "/kubelet-unparseable",
			Title:     fmt.Sprintf("%d node(s) have unparseable kubelet versions", len(unparseable)),
			Detail:    fmt.Sprintf("These nodes could not be evaluated against the kubelet skew policy: %s.", strings.Join(unparseable, ", ")),
			Citations: []string{skewPolicyURL},
		})
	}
	return out
}

// apiserverVersions returns the distinct kube-apiserver minors observed,
// ascending: the server version the collector was answered with, plus any
// kube-apiserver pods (self-hosted HA control planes). Empty when neither
// is available or parseable.
func apiserverVersions(inv inventory.Inventory) []inventory.Version {
	var vs []inventory.Version
	add := func(raw string) {
		if v, err := inventory.ParseVersion(raw); err == nil && !slices.Contains(vs, v) {
			vs = append(vs, v)
		}
	}
	add(inv.ServerVersion)
	for _, cv := range inv.ControlPlane {
		if cv.Component == "kube-apiserver" {
			add(cv.Version)
		}
	}
	slices.SortFunc(vs, inventory.Version.Compare)
	return vs
}

// legacyMaxBehind narrows a kubelet / kube-proxy skew limit for components
// older than 1.25, which the policy allows only 2 minors behind.
func legacyMaxBehind(limit int, v inventory.Version) int {
	if v.Major == 1 && v.Minor < 25 {
		return min(limit, 2)
	}
	return limit
}

// legacyNote is appended to a skew detail when a pre-1.25 component's
// narrower limit applied.
func legacyNote(components string, applied bool) string {
	if !applied {
		return ""
	}
	return fmt.Sprintf(" %s older than 1.25 may be at most 2 minor versions behind.", components)
}

// minorsBehind flattens (major, minor) so cross-major comparisons stay sane.
func minorsBehind(ctrl, kubelet inventory.Version) int {
	return (ctrl.Major*1000 + ctrl.Minor) - (kubelet.Major*1000 + kubelet.Minor)
}

// evalControlPlaneSkew evaluates the control-plane rules of the upstream
// version-skew policy against inv.ControlPlane (component versions observed
// from kube-system pod image tags):
//
//   - HA apiserver spread: >1 distinct kube-apiserver minor with a spread
//     beyond policy.APIServerHASpread → warning.
//   - kube-controller-manager / kube-scheduler: newer than the OLDEST
//     apiserver → blocker (must never be newer than an apiserver they talk
//     to); more than policy.CtrlMgrMaxBehind behind the NEWEST → warning.
//   - kube-proxy: newer than the oldest apiserver, or more than
//     policy.KubeProxyMaxBehind behind the newest → warning; more than
//     KubeProxyMaxBehind behind target → blocker, since the control-plane
//     upgrade would put it out of policy (mirrors the kubelet rule). A
//     kube-proxy older than 1.25 may only be 2 minors behind.
//
// An empty ControlPlane (managed control planes — EKS/GKE/AKS run these
// components outside the cluster) yields no findings. When apiserver pods
// are not observed but other components are, inv.ServerVersion stands in
// as the apiserver version.
func evalControlPlaneSkew(inv inventory.Inventory, k kb.KB, target inventory.Version) []Finding {
	if len(inv.ControlPlane) == 0 {
		return nil
	}
	byComp := map[string][]inventory.Version{} // distinct minors per component, ascending
	for _, cv := range inv.ControlPlane {
		v, err := inventory.ParseVersion(cv.Version)
		if err != nil {
			continue // collector only emits parseable versions; defensive
		}
		if !slices.Contains(byComp[cv.Component], v) {
			byComp[cv.Component] = append(byComp[cv.Component], v)
		}
	}
	for _, vs := range byComp {
		slices.SortFunc(vs, inventory.Version.Compare)
	}

	var out []Finding
	// Post-upgrade kube-proxy needs only the target, not today's apiservers.
	var proxyPost []string
	proxyLegacy := false
	for _, v := range byComp["kube-proxy"] {
		limit := legacyMaxBehind(k.Skew.KubeProxyMaxBehind, v)
		if minorsBehind(target, v) > limit {
			proxyPost = append(proxyPost, v.String())
			proxyLegacy = proxyLegacy || limit < k.Skew.KubeProxyMaxBehind
		}
	}
	if len(proxyPost) > 0 {
		out = append(out, Finding{
			Category: CatVersionSkew, Severity: SevBlocker,
			Key:       string(CatVersionSkew) + "/kube-proxy-post-upgrade",
			Title:     fmt.Sprintf("kube-proxy would exceed version skew after upgrading to %s", target),
			Detail:    fmt.Sprintf("After upgrading the control plane to %s, kube-proxy %s would be more than %d minor versions behind kube-apiserver; upgrade kube-proxy first.", target, strings.Join(proxyPost, ", "), k.Skew.KubeProxyMaxBehind) + legacyNote("kube-proxy versions", proxyLegacy),
			Citations: []string{skewPolicyURL},
		})
	}

	api := byComp["kube-apiserver"]
	if len(api) > 1 {
		oldest, newest := api[0], api[len(api)-1]
		if spread := minorsBehind(newest, oldest); spread > k.Skew.APIServerHASpread {
			vers := make([]string, len(api))
			for i, v := range api {
				vers[i] = v.String()
			}
			out = append(out, Finding{
				Category: CatVersionSkew, Severity: SevWarning,
				Key:       string(CatVersionSkew) + "/apiserver-ha-spread",
				Title:     fmt.Sprintf("HA kube-apiserver replicas span %d minor versions", spread),
				Detail:    fmt.Sprintf("Observed kube-apiserver versions: %s. The version-skew policy requires HA kube-apiserver instances to be within %d minor version of each other.", strings.Join(vers, ", "), k.Skew.APIServerHASpread),
				Citations: []string{skewPolicyURL},
			})
		}
	}
	if len(api) == 0 { // apiserver pods not observed: fall back to the reported server version
		sv, err := inventory.ParseVersion(inv.ServerVersion)
		if err != nil {
			return out // nothing to compare components against
		}
		api = []inventory.Version{sv}
	}
	oldest, newest := api[0], api[len(api)-1]

	for _, rule := range []struct {
		component   string
		maxBehind   int
		legacy      bool // pre-1.25 versions may only be 2 minors behind
		newerSev    Severity
		newerDetail string
	}{
		{"kube-controller-manager", k.Skew.CtrlMgrMaxBehind, false, SevBlocker, "control-plane components must not be newer than the apiservers they talk to"},
		{"kube-scheduler", k.Skew.CtrlMgrMaxBehind, false, SevBlocker, "control-plane components must not be newer than the apiservers they talk to"},
		{"kube-proxy", k.Skew.KubeProxyMaxBehind, true, SevWarning, "kube-proxy must not be newer than kube-apiserver"},
	} {
		var newer, behind []string
		behindLegacy := false
		for _, v := range byComp[rule.component] {
			limit := rule.maxBehind
			if rule.legacy {
				limit = legacyMaxBehind(limit, v)
			}
			switch {
			case v.Compare(oldest) > 0:
				newer = append(newer, v.String())
			case minorsBehind(newest, v) > limit:
				behind = append(behind, v.String())
				behindLegacy = behindLegacy || limit < rule.maxBehind
			}
		}
		if len(newer) > 0 {
			out = append(out, Finding{
				Category: CatVersionSkew, Severity: rule.newerSev,
				Key:       string(CatVersionSkew) + "/" + rule.component,
				Title:     fmt.Sprintf("%s is newer than kube-apiserver", rule.component),
				Detail:    fmt.Sprintf("%s %s is newer than the oldest kube-apiserver (%s); %s.", rule.component, strings.Join(newer, ", "), oldest, rule.newerDetail),
				Citations: []string{skewPolicyURL},
			})
		}
		if len(behind) > 0 {
			out = append(out, Finding{
				Category: CatVersionSkew, Severity: SevWarning,
				Key:       string(CatVersionSkew) + "/" + rule.component,
				Title:     fmt.Sprintf("%s exceeds version skew vs kube-apiserver", rule.component),
				Detail:    fmt.Sprintf("%s %s is more than %d minor version(s) behind the newest kube-apiserver (%s).", rule.component, strings.Join(behind, ", "), rule.maxBehind, newest) + legacyNote("kube-proxy versions", behindLegacy),
				Citations: []string{skewPolicyURL},
			})
		}
	}
	return out
}

// evalKBStale: the cluster (current control plane) or the upgrade target is
// newer than anything the embedded KB knows about → the scan itself may be
// missing removals → warning. Evaluated against max(serverVersion, target);
// an unparseable/missing server version still leaves the target-driven check
// active.
func evalKBStale(inv inventory.Inventory, k kb.KB, target inventory.Version) []Finding {
	newest := target
	if server, err := inventory.ParseVersion(inv.ServerVersion); err == nil && server.Compare(newest) > 0 {
		newest = server
	}
	if newest.Compare(k.MaxKnownK8s) <= 0 {
		return nil
	}
	return []Finding{{
		Category: CatKBStale, Severity: SevWarning,
		Key:    string(CatKBStale),
		Title:  fmt.Sprintf("knowledge base does not cover Kubernetes %s (newest known: %s)", newest, k.MaxKnownK8s),
		Detail: fmt.Sprintf("Findings may be incomplete; regenerate the knowledge base (kb version %s).", k.Version),
	}}
}

// assessmentGaps lists what the evaluation could not assess, sorted by
// capability: every unavailable inventory capability, a versions gap when
// the server version is missing or unparseable (unless versions is already
// unavailable), and a kb-coverage gap when the target is beyond the KB
// horizon. Required is set per the verdict rules on CapabilityGap. A
// capability absent from inv.Capabilities is not a gap: collectors always
// report all of theirs, so absence only occurs in hand-built inventories.
func assessmentGaps(inv inventory.Inventory, k kb.KB, target inventory.Version) []CapabilityGap {
	required := map[inventory.Capability]bool{inventory.CapAPIUsage: true, GapKBCoverage: true}
	if inv.Source != inventory.SourceFiles { // "" = cluster (v0.1 agents)
		required[inventory.CapVersions] = true
	}
	var gaps []CapabilityGap
	for c, st := range inv.Capabilities {
		if !st.Available {
			gaps = append(gaps, CapabilityGap{Capability: c, Reason: st.Reason})
		}
	}
	if st, ok := inv.Capabilities[inventory.CapVersions]; !ok || st.Available {
		const notEvaluated = "kubelet and control-plane skew were not evaluated"
		if inv.ServerVersion == "" {
			gaps = append(gaps, CapabilityGap{Capability: inventory.CapVersions, Reason: "server version not reported; " + notEvaluated})
		} else if _, err := inventory.ParseVersion(inv.ServerVersion); err != nil {
			gaps = append(gaps, CapabilityGap{Capability: inventory.CapVersions,
				Reason: fmt.Sprintf("server version %q could not be parsed; %s", inv.ServerVersion, notEvaluated)})
		}
	}
	if target.Compare(k.MaxKnownK8s) > 0 {
		gaps = append(gaps, CapabilityGap{Capability: GapKBCoverage,
			Reason: fmt.Sprintf("knowledge base covers Kubernetes up to %s; target %s cannot be assessed", k.MaxKnownK8s, target)})
	}
	for i := range gaps {
		gaps[i].Required = required[gaps[i].Capability]
	}
	sort.Slice(gaps, func(i, j int) bool { return gaps[i].Capability < gaps[j].Capability })
	return gaps
}

// verdictFor: blocked on any blocker; otherwise unknown on any required gap
// (a blocker may have gone unseen); otherwise ready.
func verdictFor(findings []Finding, gaps []CapabilityGap) Verdict {
	for _, f := range findings {
		if f.Severity == SevBlocker {
			return VerdictBlocked
		}
	}
	for _, g := range gaps {
		if g.Required {
			return VerdictUnknown
		}
	}
	return VerdictReady
}

// Evaluate is the pure evaluation entrypoint: no I/O, no clock reads — now is
// injected for EOL-window math. Output is fully deterministic for a given
// (inventory, kb, target, now).
func Evaluate(inv inventory.Inventory, k kb.KB, target inventory.Version, now time.Time) Report {
	findings := []Finding{} // non-nil so JSON renders "findings": []
	findings = append(findings, evalAPIUsage(inv, k, target)...)
	findings = append(findings, evalDeprecatedCalls(inv, target)...)
	findings = append(findings, evalAddOns(inv, k, target, now)...)
	findings = append(findings, evalSkew(inv, k, target)...)
	findings = append(findings, evalControlPlaneSkew(inv, k, target)...)
	findings = append(findings, evalKBStale(inv, k, target)...)
	sortFindings(findings)
	score, _ := Score(findings)
	gaps := assessmentGaps(inv, k, target)
	verdict := verdictFor(findings, gaps)

	return Report{
		ClusterID:   inv.ClusterID,
		Target:      target,
		KBVersion:   k.Version,
		Score:       score,
		Ready:       verdict == VerdictReady,
		Verdict:     verdict,
		Findings:    findings,
		NotAssessed: gaps,
	}
}
