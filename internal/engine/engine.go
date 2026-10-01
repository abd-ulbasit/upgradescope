package engine

import (
	"cmp"
	"fmt"
	"slices"
	"sort"
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
		f.Key = string(f.Category) + "/" + apiKey(u.Group, u.Version, u.Kind)
		// Live objects carry a Manager when they were flagged for being
		// written through this version; without one, the kind itself goes
		// away and every stored object counts.
		managers := objectManagers(u.Objects)
		detail := "%d object(s) still stored/served at this version"
		switch {
		case manifests:
			detail = "%d manifest object(s) use this API"
		case len(managers) > 0:
			detail = "%d object(s) written through this API version"
		}
		if nsDetail == "" {
			f.Detail = fmt.Sprintf(detail+".", u.Count)
		} else {
			f.Detail = fmt.Sprintf(detail+": %s.", u.Count, nsDetail)
		}
		if len(managers) > 0 {
			f.Detail += " Written by: " + strings.Join(managers, ", ") + "."
		}
		out = append(out, f)
	}
	return out
}

// apiKey renders the group/version/kind tail of an API finding's Key.
func apiKey(group, version, kind string) string {
	return keyGroup(group) + "/" + version + "/" + kind
}

// objectManagers returns the distinct ObjectRef managers, sorted.
func objectManagers(refs []inventory.ObjectRef) []string {
	var ms []string
	for _, r := range refs {
		if r.Manager != "" && !slices.Contains(ms, r.Manager) {
			ms = append(ms, r.Manager)
		}
	}
	sort.Strings(ms)
	return ms
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
// findings, one per row in row order: removal ≤ target → blocker; removal
// == target+1 → warning; otherwise (incl. missing/unparseable
// removedRelease) → info. The metric is the runtime-caller signal: it
// says a deprecated API was requested, not by whom.
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
		const metric = "apiserver_requested_deprecated_apis records requests to this API since the last apiserver restart; "
		const anonymous = " The metric does not identify the client; apiserver audit logs do."
		removed, perr := inventory.ParseVersion(c.RemovedRelease)
		if c.RemovedRelease == "" || perr != nil {
			f.Severity = SevInfo
			f.Title = fmt.Sprintf("clients still requesting %s %s (deprecated)", gv, res)
			if c.RemovedRelease == "" {
				f.Detail = metric + "no removal release is recorded." + anonymous
			} else {
				f.Detail = metric + fmt.Sprintf("removal release %q could not be parsed.", c.RemovedRelease) + anonymous
			}
			out = append(out, f)
			continue
		}
		f.Title = fmt.Sprintf("clients still requesting %s %s (removed in %s)", gv, res, removed)
		f.Detail = metric + fmt.Sprintf("it is removed in %s.", removed) + anonymous
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

// foldDeprecatedCalls scores each API once. usage is evalAPIUsage's
// output and calls is evalDeprecatedCalls' (calls[i] judges
// inv.DeprecatedCalls[i]). A caller row for the group/version/kind of a
// usage finding becomes evidence on that finding instead of a second
// finding, so the finding keeps its key. Rows for APIs without flagged
// objects stay standalone, as do rows more severe than the matching usage
// finding (the apiserver records a removal the KB does not know).
func foldDeprecatedCalls(inv inventory.Inventory, usage, calls []Finding) []Finding {
	byAPI := make(map[string]int, len(usage)) // apiKey → index in usage
	for i, f := range usage {
		_, api, _ := strings.Cut(f.Key, "/")
		byAPI[api] = i
	}
	evidence := map[int][]string{} // usage index → requested resources, in row order
	var standalone []Finding
	for i, c := range inv.DeprecatedCalls {
		j, ok := -1, false
		for _, u := range inv.APIUsage {
			if u.Group == c.Group && u.Version == c.Version && kindMatchesResource(u.Kind, c.Resource) {
				j, ok = byAPI[apiKey(u.Group, u.Version, u.Kind)]
				break
			}
		}
		if !ok || severityRank[calls[i].Severity] < severityRank[usage[j].Severity] {
			standalone = append(standalone, calls[i])
			continue
		}
		res := c.Resource
		if c.Subresource != "" {
			res += "/" + c.Subresource
		}
		if len(evidence[j]) == 0 {
			res = gvString(c.Group, c.Version) + " " + res
		}
		evidence[j] = append(evidence[j], res)
	}
	for j, rs := range evidence {
		usage[j].Detail += fmt.Sprintf(" apiserver_requested_deprecated_apis also records requests to %s since the last apiserver restart; the metric does not identify the client.", strings.Join(rs, ", "))
	}
	return append(usage, standalone...)
}

// kindMatchesResource reports whether resource is kind's REST resource
// name under the apiserver's default pluralization (lowercase; "s" →
// "ses", consonant+"y" → "ies", otherwise +"s"), or kind lowercased for
// kinds that are already plural (Endpoints).
func kindMatchesResource(kind, resource string) bool {
	k := strings.ToLower(kind)
	switch {
	case k == "" || resource == k:
		return k != ""
	case strings.HasSuffix(k, "s"):
		return resource == k+"es"
	case strings.HasSuffix(k, "y") && len(k) > 1 && !strings.ContainsRune("aeiou", rune(k[len(k)-2])):
		return resource == k[:len(k)-1]+"ies"
	}
	return resource == k+"s"
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
			// Tense follows the date: status "eol" can carry a future
			// effective date (upstream already declared EOL).
			title := fmt.Sprintf("%s is end-of-life", a.DisplayName)
			detail := located + " Upstream support has ended."
			switch {
			case hasDate && eolDate.After(now):
				title = fmt.Sprintf("%s is end-of-life on %s", a.DisplayName, a.Support.EOLDate)
				detail = located + fmt.Sprintf(" Upstream support ends on %s.", a.Support.EOLDate)
			case a.Support.EOLDate != "":
				title = fmt.Sprintf("%s is end-of-life since %s", a.DisplayName, a.Support.EOLDate)
			}
			out = append(out, Finding{
				Category: CatEOLAddon, Severity: SevBlocker,
				Key:         string(CatEOLAddon) + "/" + a.ID,
				Title:       title,
				Detail:      detail,
				Teams:       teams,
				Namespaces:  ns,
				Remediation: a.Recommendation,
				Citations:   append([]string(nil), a.Support.Citations...),
			})
		case hasDate && eolDate.After(now) && !eolDate.After(now.Add(90*24*time.Hour)):
			out = append(out, Finding{
				Category: CatEOLApproaching, Severity: SevWarning,
				Key:         string(CatEOLApproaching) + "/" + a.ID,
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
			// Registry validation (registry.Validate) guarantees K8sMax is
			// MAJOR.MINOR (^[0-9]+\.[0-9]+$), so this parse cannot fail for
			// data that passed validation; the err check is defensive only.
			kmax, err := inventory.ParseVersion(c.K8sMax)
			if err == nil && kmax.Compare(target) < 0 {
				out = append(out, Finding{
					Category: CatChartIncompat, Severity: SevBlocker,
					Key:         string(CatChartIncompat) + "/" + a.ID,
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
	findings = append(findings, foldDeprecatedCalls(inv, evalAPIUsage(inv, k, target), evalDeprecatedCalls(inv, target))...)
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
