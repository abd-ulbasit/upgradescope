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
	// helmChartYAMLURL documents Chart.yaml's kubeVersion constraint.
	helmChartYAMLURL = "https://helm.sh/docs/topics/charts/#the-chartyaml-file"
	// helmKubernetesAPIsURL documents why helm upgrade fails on a stored
	// manifest with removed APIs, and the mapkubeapis fix.
	helmKubernetesAPIsURL = "https://helm.sh/docs/topics/kubernetes_apis/"
	// apiVersioningURL documents the alpha, beta and stable API levels and
	// which a cluster serves.
	apiVersioningURL = "https://kubernetes.io/docs/reference/using-api/#api-versioning"
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

// MaxFindingNamespaces caps the namespaces a finding lists, in Namespaces
// (NamespacesOmitted counts the rest) and in an API usage finding's
// evidence sentence, as inventory.MaxObjectRefs caps its objects: a
// finding's size does not grow with how many namespaces it affects. Its
// Teams still come from every affected namespace.
const MaxFindingNamespaces = 100

// capNamespaces lists at most MaxFindingNamespaces of f's sorted
// namespaces, counting the rest in NamespacesOmitted.
func capNamespaces(f *Finding) {
	if n := len(f.Namespaces) - MaxFindingNamespaces; n > 0 {
		f.Namespaces = f.Namespaces[:MaxFindingNamespaces:MaxFindingNamespaces]
		f.NamespacesOmitted += n
	}
}

// namespaceBreakdown renders "ns (count)" parts sorted by namespace name,
// at most MaxFindingNamespaces named ones and then how many more, and
// returns every sorted namespace name. The empty key "" renders as
// emptyLabel in the detail and is excluded from the returned names.
func namespaceBreakdown(counts map[string]int, emptyLabel string) (detail string, names []string) {
	keys := make([]string, 0, len(counts))
	for ns := range counts {
		keys = append(keys, ns)
	}
	sort.Strings(keys)
	parts := make([]string, 0, min(len(keys), MaxFindingNamespaces+1))
	for _, ns := range keys {
		label := ns
		if ns == "" {
			label = emptyLabel
		} else {
			names = append(names, ns)
			if len(names) > MaxFindingNamespaces {
				continue
			}
		}
		parts = append(parts, fmt.Sprintf("%s (%d)", label, counts[ns]))
	}
	detail = strings.Join(parts, ", ")
	if n := len(names) - MaxFindingNamespaces; n > 0 {
		detail += fmt.Sprintf(", and %d more namespace(s)", n)
	}
	return detail, names
}

// teamLookup maps namespace names to the team their label names, built
// once per evaluation pass from the inventory's namespaces: a lookup that
// rebuilt it per finding, Helm release or CRD cost their product with the
// namespaces (#333).
type teamLookup map[string]string

func newTeamLookup(nsInfo []inventory.NamespaceInfo) teamLookup {
	byName := make(teamLookup, len(nsInfo))
	for _, n := range nsInfo {
		byName[n.Name] = n.Team
	}
	return byName
}

// teamsFor maps namespace names to teams via the inventory's namespace team
// labels; result is deduped and sorted.
func (l teamLookup) teamsFor(namespaces []string) []string {
	seen := map[string]bool{}
	var teams []string
	for _, ns := range namespaces {
		if t := l[ns]; t != "" && !seen[t] {
			seen[t] = true
			teams = append(teams, t)
		}
	}
	sort.Strings(teams)
	return teams
}

// evalAPIUsage: for each deprecated/removed GVK in use (live objects
// written through it, or any stored object of a kind that goes away;
// manifest objects in files mode),
//   - removed at ≤ target          → blocker, removed-api
//   - removed exactly at target+1  → warning, removed-api
//     (a removal past the KB horizon is titled "(projected)": it is a
//     k8s.io/api lifecycle default, not a shipped release)
//   - proposed state (manifests: files mode, the gate) at an API version
//     the target does not serve yet (introduced after it) → blocker,
//     removed-api, "not served until X": applying it fails like a removed
//     API. Live stored objects are never judged so (the cluster serves
//     what it stores). Its key ends in /unserved (UnservedKey), so a rule
//     or baseline for it never accepts the removal that follows; the
//     removal blocker and the next-minor warning share the bare key.
//     A removed or deprecated API with no replacement chain the target
//     serves is remediated to the newest later version of its kind the
//     target serves (kb.Index.ServedSuccessor), with its stability.
//   - deprecated, removal beyond the window or unset → info, deprecated-api;
//     a deprecation after the target is titled as such, and "projected"
//     past the KB horizon
//   - not in the KB, in a built-in group (core, or any group k8s.io/api
//     registers: the KB's BuiltinGroups and the groups of its entries) →
//     info, unknown-api: the KB cannot say whether the target serves it
//     (upstream may have deleted it), and dropping it would read as
//     ready. Other groups (CRDs, aggregated APIs) are never in the KB and
//     produce nothing.
func evalAPIUsage(inv inventory.Inventory, k kb.KB, target inventory.Version, b *budget) []Finding {
	teams := newTeamLookup(inv.Namespaces)
	idx := kb.NewIndex(k.APILifecycle)
	builtin := map[string]bool{}
	for _, e := range k.APILifecycle {
		builtin[e.Group] = true
	}
	for _, g := range k.BuiltinGroups {
		builtin[g.Group] = true
	}
	var out []Finding
	for _, u := range inv.APIUsage {
		e, known := idx.Lookup(u.Group, u.Version, u.Kind)
		if !known && !builtin[u.Group] {
			continue
		}
		listed := listedObjects(inv.Source, u)
		nsDetail, nsNames := namespaceBreakdown(u.Namespaces, listed.emptyNamespace())
		f := Finding{
			Teams:          teams.teamsFor(nsNames),
			Namespaces:     nsNames,
			Citations:      []string{deprecationGuideURL},
			Objects:        sortedObjects(u.Objects),
			ObjectsOmitted: u.ObjectsOmitted,
		}
		// Proposed state: a manifest is applied to the cluster at the target.
		// Live objects are stored where they are served, so only manifests
		// can name an API the target does not serve yet.
		unserved := known && e.Introduced.Compare(target) > 0 && (inv.Source == inventory.SourceFiles || listed != storedObjects)
		if unserved {
			if alt, ok := idx.ServedAlternative(e, target); ok {
				enable := ""
				if kb.PreGA(alt.Version) {
					enable = " (alpha and beta APIs may need enabling)" // off by default in kube-apiserver
				}
				f.Remediation = fmt.Sprintf("write it as %s %s, the newest version Kubernetes %s can serve%s, or upgrade the cluster to Kubernetes %s before applying it", gvString(alt.Group, alt.Version), alt.Kind, target, enable, e.Introduced)
			} else {
				f.Remediation = fmt.Sprintf("Kubernetes %s serves no version of %s the knowledge base knows: upgrade the cluster to Kubernetes %s before applying it", target, u.Kind, e.Introduced)
			}
			f.Citations = append(f.Citations, apiVersioningURL)
		} else if r, ok := idx.ResolveReplacement(e, target); ok {
			f.Remediation = fmt.Sprintf("migrate to %s %s", gvString(r.Group, r.Version), r.Kind)
			// A replacement upstream tags nowhere is gen-kb's default, which
			// the migration guide (it ends at v1.32) cannot back: cite the
			// successor's changelog.
			if re, ok := idx.Lookup(r.Group, r.Version, r.Kind); ok && e.ReplacementDefaulted {
				f.Citations = append(f.Citations, kb.ChangelogURL(re.Introduced))
			}
		} else if later, from, ok := idx.LaterReplacement(e, target); ok {
			f.Remediation = fmt.Sprintf("no replacement Kubernetes %s serves is known; %s %s is served from %s", target, gvString(later.Group, later.Version), later.Kind, from)
			if e.Serves(target) {
				// Nothing as mature as the version in use is served yet (#332):
				// it is the right one for this target, not an older API.
				f.Remediation += fmt.Sprintf("; %s is the right version for Kubernetes %s until then", gvString(u.Group, u.Version), target)
			}
		} else if succ, ok := idx.ServedSuccessor(e, target); ok {
			f.Remediation = "migrate to " + successorRemedy(succ, k.MaxKnownK8s, false)
		} else if e.Replacement != nil {
			f.Remediation = fmt.Sprintf("no replacement Kubernetes %s serves is known", target)
		}
		if e.Migration != nil && !unserved {
			f.Remediation = withMigration(f.Remediation, *e.Migration)
			f.Citations = appendNew(f.Citations, e.Migration.Citations...)
		}
		gv := gvString(u.Group, u.Version)
		projectedRemoval := ""
		if e.Removed != nil && e.Removed.Compare(k.MaxKnownK8s) > 0 {
			projectedRemoval = " (projected)" // k8s.io/api's lifecycle markers, not a release
		}
		switch {
		case !known:
			f.Category = CatUnknownAPI
			f.Severity = SevInfo
			f.Title = fmt.Sprintf("%s %s is not in the knowledge base (%s)", gv, u.Kind, pluralObjects(u.Count))
		case unserved:
			f.Category = CatRemovedAPI
			f.Severity = SevBlocker
			f.Title = fmt.Sprintf("%s %s is not served until %s, after target %s (%s)", gv, u.Kind, e.Introduced, target, pluralObjects(u.Count))
		case e.Removed != nil && e.Removed.Compare(target) <= 0:
			f.Category = CatRemovedAPI
			f.Severity = SevBlocker
			f.Title = fmt.Sprintf("%s %s removed in %s%s (%s)", gv, u.Kind, e.Removed, projectedRemoval, pluralObjects(u.Count))
		case e.Removed != nil && e.Removed.Compare(target.Next()) == 0:
			f.Category = CatRemovedAPI
			f.Severity = SevWarning
			f.Title = fmt.Sprintf("%s %s removed in %s%s (%s)", gv, u.Kind, e.Removed, projectedRemoval, pluralObjects(u.Count))
		case e.Deprecated != nil:
			f.Category = CatDeprecatedAPI
			f.Severity = SevInfo
			f.Title = fmt.Sprintf("%s %s deprecated since %s (%s)", gv, u.Kind, e.Deprecated, pluralObjects(u.Count))
			if e.Deprecated.Compare(target) > 0 {
				when := e.Deprecated.String()
				if e.Deprecated.Compare(k.MaxKnownK8s) > 0 {
					when += " (projected)" // k8s.io/api's lifecycle markers, not a release
				}
				f.Title = fmt.Sprintf("%s %s deprecated in %s, after target %s (%s)", gv, u.Kind, when, target, pluralObjects(u.Count))
			}
		default:
			continue // KB entry exists but is neither deprecated nor removed
		}
		f.Key = string(f.Category) + "/" + apiKey(u.Group, u.Version, u.Kind)
		if unserved {
			f.Key = UnservedKey(f.Key) // not the key of the removal that may follow (#300)
		}
		// Live objects carry a Manager when they were flagged for being
		// written through this version; without one, the kind itself goes
		// away and every stored object counts.
		managers := objectManagers(u.Objects)
		detail := "%d object(s) still stored/served at this version"
		switch {
		case listed == manifestObjects:
			detail = "%d manifest object(s) use this API"
		case listed == mixedObjects:
			detail = "%d object(s) use this API"
		case len(managers) > 0:
			detail = "%d object(s) written through this API version"
		}
		if nsDetail == "" {
			f.Detail = fmt.Sprintf(detail+".", u.Count)
		} else {
			f.Detail = fmt.Sprintf(detail+": %s.", u.Count, nsDetail)
		}
		f.Detail += writtenBy(u)
		if !known {
			f.Detail += fmt.Sprintf(" The knowledge base has no lifecycle data for this built-in API: it may have been removed, so check that Kubernetes %s serves it.", target)
		}
		if unserved {
			f.Detail += fmt.Sprintf(" Kubernetes %s does not serve it yet (introduced in %s), so applying these objects to it fails with \"no matches for kind\".", target, e.Introduced)
		}
		if f.Category == CatRemovedAPI && projectedRemoval != "" && !unserved {
			f.Detail += fmt.Sprintf(" The removal in %s is projected: it is a default of k8s.io/api's lifecycle markers, not a shipped release (the newest the knowledge base covers is %s), and may change before it ships.", e.Removed, k.MaxKnownK8s)
		}
		if !b.add(&out, f) {
			return out
		}
	}
	return out
}

// objectsListed is what an API usage row's listed objects say its
// objects are (listedObjects).
type objectsListed int

const (
	// storedObjects: a cluster's objects (or none listed).
	storedObjects objectsListed = iota
	// manifestObjects: manifest objects (refs read from text carry a
	// line), proposed state rather than stored objects; an empty
	// namespace there means metadata.namespace is unset (helm template
	// output usually omits it), not that the object is cluster-scoped.
	manifestObjects
	// mixedObjects: manifest objects listed in a cluster's row that also
	// counts objects it does not list, which may be the cluster's: the
	// gate's proposed state, where the cluster's refs were past the
	// collector's cap or not recorded, or a team-scoped share's were past
	// the cap (readScope.clusterShare).
	mixedObjects
)

// listedObjects says what u's objects are, from its first listed ref (the
// gate lists the cluster's refs before the manifests', upsertUsage):
// manifests' when every counted object is listed, or in a files-mode
// inventory, where every object is a manifest's; mixed in a cluster's
// inventory whose row counts objects it does not list.
func listedObjects(src inventory.Source, u inventory.APIUsage) objectsListed {
	switch {
	case len(u.Objects) == 0 || u.Objects[0].Line == 0:
		return storedObjects
	case src == inventory.SourceFiles || len(u.Objects) >= u.Count:
		return manifestObjects
	}
	return mixedObjects
}

// emptyNamespace names the "" key of the row's namespace counts.
func (l objectsListed) emptyNamespace() string {
	switch l {
	case manifestObjects:
		return "no namespace set"
	case mixedObjects:
		return "cluster-scoped or no namespace set"
	}
	return "cluster-scoped"
}

// authorshipUnknownKey is the Key suffix of an authorship-unknown finding,
// which shares its category with the API's deprecated-api finding (when
// the objects the collector could attribute are not blockers) and needs a
// key of its own.
const authorshipUnknownKey = "authorship-unknown"

// evalAuthorshipUnknown reports the objects of a flagged kind that nothing
// can be attributed to (inv.APIAuthorshipUnknown) as one info finding per
// API, category deprecated-api, never more: stored the same whether
// created through the flagged version or its replacement, they are
// neither evidence of use nor of its absence, and so change neither
// verdict nor score. An API the KB does not flag is skipped, whatever an
// inventory says.
func evalAuthorshipUnknown(inv inventory.Inventory, k kb.KB, b *budget) []Finding {
	teams := newTeamLookup(inv.Namespaces)
	idx := kb.NewIndex(k.APILifecycle)
	var out []Finding
	for _, u := range inv.APIAuthorshipUnknown {
		if e, known := idx.Lookup(u.Group, u.Version, u.Kind); !known || (e.Deprecated == nil && e.Removed == nil) {
			continue
		}
		nsDetail, nsNames := namespaceBreakdown(u.Namespaces, "cluster-scoped")
		gv := gvString(u.Group, u.Version)
		f := Finding{
			Category:       CatDeprecatedAPI,
			Severity:       SevInfo,
			Key:            string(CatDeprecatedAPI) + "/" + apiKey(u.Group, u.Version, u.Kind) + "/" + authorshipUnknownKey,
			Title:          fmt.Sprintf("%s %s: authorship unknown (%s)", gv, u.Kind, pluralObjects(u.Count)),
			Teams:          teams.teamsFor(nsNames),
			Namespaces:     nsNames,
			Citations:      []string{deprecationGuideURL},
			Objects:        sortedObjects(u.Objects),
			ObjectsOmitted: u.ObjectsOmitted,
			Detail: fmt.Sprintf("%d object(s) have no managedFields entry to attribute them by (none outside the status subresource and the control plane) and no usable last-applied annotation, "+
				"which is what creating one with an empty spec leaves, so who writes them, and through which API version, cannot be told. "+
				"They are stored the same through every served version of %s: not counted as use of %s, no effect on the verdict or score.", u.Count, u.Kind, gv),
		}
		if nsDetail != "" {
			f.Detail += " Namespaces: " + nsDetail + "."
		}
		if !b.add(&out, f) {
			return out
		}
	}
	return out
}

// writtenBy renders the managers of u's objects as a detail sentence with
// a leading space, or "" when no object names one. Refs are capped
// (inventory.MaxObjectRefs), and a /gate's engine sees more than the answer
// lists (the PR's refs and the cluster's, before capObjects cuts the
// listing): when refs were dropped it says only that not all of the M
// objects are identified, never how many it names, which no listing is
// guaranteed to show.
func writtenBy(u inventory.APIUsage) string {
	managers := objectManagers(u.Objects)
	if len(managers) == 0 {
		return ""
	}
	by := " Written by: "
	if u.ObjectsOmitted > 0 {
		by = fmt.Sprintf(" Written by (of %d objects, not all identified): ", len(u.Objects)+u.ObjectsOmitted)
	}
	return by + strings.Join(managers, ", ") + "."
}

// apiKey renders the group/version/kind tail of an API finding's Key.
func apiKey(group, version, kind string) string {
	return keyGroup(group) + "/" + version + "/" + kind
}

// unservedKeyPhase is the last element of the key of a "not served until X"
// finding (UnservedKey).
const unservedKeyPhase = "unserved"

// UnservedKey is the Finding.Key of a removed-api finding for an API the
// target does not serve yet, from the key the removal of that API has,
// removed-api/<group>/<version>/<kind>: the phase is part of the key
// (removed-api/<group>/<version>/<kind>/unserved), as a support-lifecycle
// key's is (SupportKey), so a rule or a baseline for one phase never
// accepts the other (#300). The removal blocker and the next-minor removal
// warning share the bare key: an ignore rule accepts a key whatever the
// severity.
func UnservedKey(removalKey string) string { return removalKey + "/" + unservedKeyPhase }

// BaseOfUnservedKey is the key of the removal of the API an unserved
// finding's key names, and whether key is an unserved key at all.
//
// A Helm release's key is removed-api/helm-release/<namespace>/<name>, so
// a release named "unserved" is not one: only an API key (group, version
// and kind after the category) is.
func BaseOfUnservedKey(key string) (string, bool) {
	base, ok := strings.CutSuffix(key, "/"+unservedKeyPhase)
	if !ok || !strings.HasPrefix(base, string(CatRemovedAPI)+"/") || strings.HasPrefix(base, string(CatRemovedAPI)+"/helm-release/") {
		return "", false
	}
	return base, true
}

// keyAPI is the group/version/name of an API finding's key, after its
// category, without the phase an unserved key adds.
func keyAPI(key string) string {
	if base, ok := BaseOfUnservedKey(key); ok {
		key = base
	}
	_, api, _ := strings.Cut(key, "/")
	return api
}

// withMigration appends a KB migration note to a generated hint, or is the
// note alone when there is no hint (#330): what a manifest needs besides a
// new apiVersion, or where to go when the kind has no successor.
func withMigration(hint string, m kb.Migration) string {
	if hint == "" {
		return m.Note
	}
	return hint + "; " + m.Note
}

// appendNew appends the values of add that dst does not hold, in order.
func appendNew(dst []string, add ...string) []string {
	for _, a := range add {
		if !slices.Contains(dst, a) {
			dst = append(dst, a)
		}
	}
	return dst
}

// successorRemedy words the version ServedSuccessor found, with the
// stability the user needs to know about: alpha and beta APIs are off by
// default in kube-apiserver, and an alpha successor that is itself removed
// later says when ("projected" when that is past maxKnown, as a finding
// for that removal is titled). It has no nested parentheses: inList is
// whether it is one element of a parenthesised list (a Helm release's
// remediation), where the note follows a dash instead.
func successorRemedy(succ kb.APILifecycleEntry, maxKnown inventory.Version, inList bool) string {
	out := gvString(succ.Group, succ.Version) + " " + succ.Kind
	if !kb.PreGA(succ.Version) {
		return out
	}
	note, later := "beta; may need enabling", ""
	if strings.Contains(succ.Version, "alpha") {
		note = "alpha; must be enabled"
		if succ.Removed != nil {
			later = "itself removed in " + succ.Removed.String()
			switch projected := succ.Removed.Compare(maxKnown) > 0; {
			case projected && inList:
				later = "itself removed in the projected " + succ.Removed.String()
			case projected:
				later += " (projected)"
			}
		}
	}
	if inList {
		if later != "" {
			note += "; " + later
		}
		return out + " - " + note
	}
	out += " (" + note + ")"
	if later != "" {
		out += ", " + later
	}
	return out
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
// == target+1 → warning; otherwise (incl. no removal release known) →
// info. A row's removal is the knowledge base's when it has a removal for
// that group/version/resource (an inferred one included: the release
// kube-apiserver stopped serving the type in, which can be earlier than
// the upstream lifecycle tag the apiserver labels the row with), else the
// metric's removed_release label. The metric is the runtime-caller
// signal: it says a deprecated API was requested, not by whom.
func evalDeprecatedCalls(inv inventory.Inventory, k kb.KB, target inventory.Version, b *budget) []Finding {
	removals := kbRemovals(k)
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
		label, perr := inventory.ParseVersion(c.RemovedRelease)
		labelOK := c.RemovedRelease != "" && perr == nil
		removed, fromKB := removals[resourceKey(c.Group, c.Version, c.Resource)]
		var when string
		switch {
		case fromKB && labelOK && label.Compare(removed) == 0:
			when = fmt.Sprintf("it is removed in %s.", removed)
		case fromKB && labelOK:
			when = fmt.Sprintf("it is removed in %s per the knowledge base; the apiserver reports %s.", removed, label)
		case fromKB && c.RemovedRelease == "":
			when = fmt.Sprintf("it is removed in %s per the knowledge base; the apiserver records no removal release.", removed)
		case fromKB:
			when = fmt.Sprintf("it is removed in %s per the knowledge base; the apiserver's removal release %q could not be parsed.", removed, c.RemovedRelease)
		case labelOK:
			removed = label
			when = fmt.Sprintf("it is removed in %s.", removed)
		default:
			f.Severity = SevInfo
			f.Title = fmt.Sprintf("clients still requesting %s %s (deprecated)", gv, res)
			if c.RemovedRelease == "" {
				f.Detail = metric + "no removal release is recorded." + anonymous
			} else {
				f.Detail = metric + fmt.Sprintf("removal release %q could not be parsed.", c.RemovedRelease) + anonymous
			}
			if !b.add(&out, f) {
				return out
			}
			continue
		}
		f.Title = fmt.Sprintf("clients still requesting %s %s (removed in %s)", gv, res, removed)
		f.Detail = metric + when + anonymous
		switch {
		case removed.Compare(target) <= 0:
			f.Severity = SevBlocker
		case removed.Compare(target.Next()) == 0:
			f.Severity = SevWarning
		default:
			f.Severity = SevInfo
		}
		if !b.add(&out, f) {
			return out
		}
	}
	return out
}

// kbRemovals maps the REST resources of k's lifecycle entries that have a
// removal (resourceKey) to it; the first entry wins, as in RemovalOfCall.
func kbRemovals(k kb.KB) map[string]inventory.Version {
	m := make(map[string]inventory.Version)
	for _, e := range k.APILifecycle {
		if e.Removed == nil || e.Kind == "" {
			continue
		}
		lower, plural := resourceNames(e.Kind)
		for _, r := range []string{lower, plural} {
			if key := resourceKey(e.Group, e.Version, r); !hasKey(m, key) {
				m[key] = *e.Removed
			}
		}
	}
	return m
}

func hasKey[V any](m map[string]V, k string) bool {
	_, ok := m[k]
	return ok
}

// resourceKey is the key of a group/version/resource in the fold's and
// kbRemovals' indexes.
func resourceKey(group, version, resource string) string {
	return group + "\x00" + version + "\x00" + resource
}

// otherCallers returns inv.DeprecatedCalls without the scanner's own
// requests: rows for a resource the deprecated-calls capability names in
// Skipped ("group/version resource"), which the collector listed itself
// at a deprecated version. The metric cannot tell another client of such
// a resource from the scanner, so neither its presence nor its absence is
// evidence; the capability's partial gap says so instead, the same way on
// every scan. Rows for a subresource are someone else's: the scanner only
// lists.
func otherCallers(inv inventory.Inventory) []inventory.DeprecatedCall {
	self := inv.Capabilities[inventory.CapDeprecatedCalls].Skipped
	if len(self) == 0 {
		return inv.DeprecatedCalls
	}
	var out []inventory.DeprecatedCall
	for _, c := range inv.DeprecatedCalls {
		if c.Subresource == "" && slices.Contains(self, gvString(c.Group, c.Version)+" "+c.Resource) {
			continue
		}
		out = append(out, c)
	}
	return out
}

// foldDeprecatedCalls scores each API once. usage is evalAPIUsage's
// output and calls is evalDeprecatedCalls' (calls[i] judges
// inv.DeprecatedCalls[i]). A caller row for the group/version/kind of a
// usage finding becomes evidence on that finding instead of a second
// finding, so the finding keeps its key: a sentence of its Detail and,
// as data, one of its Callers. Rows for APIs without flagged objects stay
// standalone, as do rows more severe than the matching usage finding (the
// apiserver records a removal the KB does not know). The first usage
// entry whose kind has the row's resource decides, through an index built
// once, so the fold is linear in rows and usage entries.
func foldDeprecatedCalls(inv inventory.Inventory, usage, calls []Finding, b *budget) []Finding {
	byAPI := make(map[string]int, len(usage)) // apiKey → index in usage
	for i, f := range usage {
		byAPI[keyAPI(f.Key)] = i
	}
	byResource := make(map[string]int, 2*len(inv.APIUsage)) // resourceKey → first index in inv.APIUsage
	for i, u := range inv.APIUsage {
		if u.Kind == "" {
			continue
		}
		lower, plural := resourceNames(u.Kind)
		for _, r := range []string{lower, plural} {
			if key := resourceKey(u.Group, u.Version, r); !hasKey(byResource, key) {
				byResource[key] = i
			}
		}
	}
	evidence := map[int][]string{} // usage index → requested resources, in row order
	var order []int                // usage indexes with evidence, in first-row order
	var standalone []Finding
	for i, c := range inv.DeprecatedCalls {
		j, ok := -1, false
		if ui, found := byResource[resourceKey(c.Group, c.Version, c.Resource)]; found {
			u := inv.APIUsage[ui]
			j, ok = byAPI[apiKey(u.Group, u.Version, u.Kind)]
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
			order = append(order, j)
		}
		evidence[j] = append(evidence[j], res)
		usage[j].Callers = append(usage[j].Callers, Caller{
			Group: c.Group, Version: c.Version, Resource: c.Resource, Subresource: c.Subresource,
			Key: calls[i].Key, Severity: calls[i].Severity, Title: calls[i].Title, Detail: calls[i].Detail,
		})
		// The row was charged as a finding of its own, whose key, title
		// and detail its Caller repeats; the Caller also repeats the
		// requested API, which that charge did not count.
		if !b.charge(len(c.Group) + len(c.Version) + len(c.Resource) + len(c.Subresource)) {
			return nil
		}
	}
	for _, j := range order {
		more := fmt.Sprintf(" apiserver_requested_deprecated_apis also records requests to %s since the last apiserver restart; the metric does not identify the client.", strings.Join(evidence[j], ", "))
		usage[j].Detail += more
		// The sentence is charged as what it adds.
		if !b.charge(len(more)) {
			return nil
		}
	}
	return append(usage, standalone...)
}

// resourceNames returns kind's REST resource names under the apiserver's
// default pluralization: kind lowercased (for kinds that are already
// plural, Endpoints) and its plural ("s" → "ses", consonant+"y" → "ies",
// otherwise +"s").
func resourceNames(kind string) (lower, plural string) {
	k := strings.ToLower(kind)
	switch {
	case strings.HasSuffix(k, "s"):
		return k, k + "es"
	case strings.HasSuffix(k, "y") && len(k) > 1 && !strings.ContainsRune("aeiou", rune(k[len(k)-2])):
		return k, k[:len(k)-1] + "ies"
	}
	return k, k + "s"
}

// kindMatchesResource reports whether resource is one of kind's REST
// resource names (resourceNames).
func kindMatchesResource(kind, resource string) bool {
	if kind == "" {
		return false
	}
	lower, plural := resourceNames(kind)
	return resource == lower || resource == plural
}

// addOnInstall is one detected install of a registry add-on: an inventory
// instance (one namespace; several in inventories from agents that merged
// installs) or one node's container runtime.
type addOnInstall struct {
	version string   // normalised app version; "" when unknown
	via     string   // "image", "labels", "ingressclass", "chart 1.14.5", "gitops, chart 4.11.3"; "" for a node runtime
	where   []string // sorted namespaces (none for an IngressClass), or the node's name
	teams   []string // of the namespaces
}

// evalAddOns looks every detected add-on up in the registry by ID and
// judges all of its installs with evalAddOn, grouped by release line
// (groupInstalls), so each finding names only the namespaces and teams of
// the installs it is about; an ID absent from the registry produces
// nothing.
func evalAddOns(inv inventory.Inventory, k kb.KB, target inventory.Version, now time.Time) []Finding {
	teams := newTeamLookup(inv.Namespaces)
	byID := make(map[string]registry.AddOn, len(k.AddOns))
	for _, a := range k.AddOns {
		byID[a.ID] = a
	}
	installs := map[string][]addOnInstall{}
	var ids []string // in inventory order
	for _, inst := range inv.AddOns {
		if _, ok := byID[inst.ID]; !ok {
			continue
		}
		if _, seen := installs[inst.ID]; !seen {
			ids = append(ids, inst.ID)
		}
		via := inst.Source
		if inst.ChartVersion != "" { // chart version: evidence only
			if inst.Source == "chart" {
				via += " " + inst.ChartVersion
			} else { // a GitOps chart reference beside stronger or weaker evidence
				via += ", chart " + inst.ChartVersion
			}
		}
		ns := slices.Sorted(slices.Values(inst.Namespaces))
		installs[inst.ID] = append(installs[inst.ID], addOnInstall{
			version: inst.Version, via: via, where: ns, teams: teams.teamsFor(ns),
		})
	}
	// An inventory from a collector that predates the stamp never took the
	// version label of a pod for an image without a version (#301), so what
	// an addon-no-data detail says is missing must not say it was read.
	labelsConsulted := inv.CollectorSchema >= inventory.LabelVersionCollectorSchema
	var out []Finding
	for _, id := range ids {
		a := byID[id]
		all, groups := groupInstalls(a, installs[id], false, now)
		out = append(out, evalAddOn(a, all, groups, target, now, labelsConsulted)...)
	}
	return append(out, evalNodeRuntimes(inv, k.AddOns, target, now)...)
}

// evalNodeRuntimes judges node container runtimes
// (status.nodeInfo.containerRuntimeVersion, "containerd://1.7.27") against
// registry entries with a runtimes matcher, with evalAddOn: each node is an
// install, grouped by release line like any add-on's.
func evalNodeRuntimes(inv inventory.Inventory, addons []registry.AddOn, target inventory.Version, now time.Time) []Finding {
	var out []Finding
	for _, a := range addons {
		if len(a.Matchers.Runtimes) == 0 {
			continue
		}
		var ins []addOnInstall
		for _, n := range inv.Nodes {
			runtime, ver, ok := strings.Cut(n.ContainerRuntime, "://")
			if !ok || !slices.Contains(a.Matchers.Runtimes, runtime) {
				continue
			}
			ins = append(ins, addOnInstall{version: strings.TrimPrefix(ver, "v"), where: []string{n.Name}})
		}
		if len(ins) == 0 {
			continue
		}
		all, groups := groupInstalls(a, ins, true, now)
		out = append(out, evalAddOn(a, all, groups, target, now, true)...)
	}
	return out
}

// groupInstalls groups an add-on's installs by release line (cycleFor).
// Installs older than every tracked line whose oldest line has ended by
// now (predatesCycles) form one group, keyed "below-<oldest line>"; the
// other installs whose version maps to no cycle, or is unknown, form one
// group, sorted first. all covers every install, for product-level
// findings.
func groupInstalls(a registry.AddOn, ins []addOnInstall, node bool, now time.Time) (all addOnSubject, groups []addOnSubject) {
	byCycle := map[string][]addOnInstall{} // "" = no cycle
	for _, in := range ins {
		key := ""
		if c, ok := cycleFor(in.version, a.Cycles); ok {
			key = c.Cycle
		} else if oldest, ok := predatesCycles(in.version, a.Cycles, now); ok {
			key = belowPrefix + oldest.Cycle
		}
		byCycle[key] = append(byCycle[key], in)
	}
	for _, key := range slices.Sorted(maps.Keys(byCycle)) {
		line := key != "" && !strings.HasPrefix(key, belowPrefix) // one release line
		groups = append(groups, newAddOnSubject(a.DisplayName, byCycle[key], line, node))
	}
	if len(groups) == 1 {
		return groups[0], groups
	}
	return newAddOnSubject(a.DisplayName, ins, false, node), groups
}

// newAddOnSubject describes a set of installs: judged at the oldest known
// version, located by an evidence sentence that names each install's
// version where they differ. Node runtimes on one release line (line) are
// named by the line's oldest version alone. The sentence lists at most
// addOnLocatedLimit installs, so a mesh with sidecars in hundreds of
// namespaces stays one bounded finding; Teams name them all, Namespaces up to
// MaxFindingNamespaces (Evaluate caps it).
func newAddOnSubject(name string, ins []addOnInstall, line bool, node bool) addOnSubject {
	s := addOnSubject{installs: ins, node: node}
	if !node {
		s.unnamespaced = slices.ContainsFunc(ins, unnamespaced)
	}
	for _, in := range ins {
		if in.version != "" && (s.version == "" || versionBefore(in.version, s.version)) {
			s.version = in.version
		}
	}
	if node {
		var names []string
		for _, in := range ins {
			n := in.where[0]
			if !line { // versions differ within this group: name each
				n += " (" + cmp.Or(in.version, "version unknown") + ")"
			}
			names = append(names, n)
		}
		sort.Strings(names)
		s.located = fmt.Sprintf("Detected %s on node(s): %s.", name, located(names))
		if line {
			s.located = fmt.Sprintf("Detected %s version %s on node(s): %s.", name, s.version, located(names))
		}
		return s
	}
	same := true
	var parts []string
	for _, in := range ins {
		s.namespaces = append(s.namespaces, in.where...)
		s.teams = append(s.teams, in.teams...)
		same = same && in.version == ins[0].version && in.via == ins[0].via
		where := in.where
		if len(where) == 0 { // an IngressClass: cluster-scoped
			where = []string{"cluster-scoped"}
		}
		for _, ns := range where {
			parts = append(parts, fmt.Sprintf("%s (%s via %s)", nsLabel(ns), cmp.Or(in.version, "version unknown"), in.via))
		}
	}
	s.namespaces, s.teams = sortedSet(s.namespaces), sortedSet(s.teams)
	labels := make([]string, len(s.namespaces))
	for i, ns := range s.namespaces {
		labels[i] = nsLabel(ns)
	}
	if same && len(s.namespaces) == 0 {
		s.located = fmt.Sprintf("Detected %s version %s via %s (cluster-scoped).", name, cmp.Or(ins[0].version, "(unknown)"), ins[0].via)
	} else if same {
		s.located = fmt.Sprintf("Detected %s version %s via %s in namespace(s): %s.",
			name, cmp.Or(ins[0].version, "(unknown)"), ins[0].via, located(labels))
	} else {
		sort.Strings(parts)
		s.located = fmt.Sprintf("Detected %s in namespace(s): %s.", name, located(parts))
	}
	s.namespaces = namedNamespaces(s.namespaces)
	return s
}

// unnamespaced reports whether an install is in no named namespace: an
// IngressClass (cluster-scoped) or a manifest object without
// metadata.namespace (shown as ""). A namespace-scoped ignore rule cannot be
// shown to cover it (Finding.Unnamespaced).
func unnamespaced(in addOnInstall) bool {
	return len(in.where) == 0 || slices.Contains(in.where, "")
}

// nsLabel names a namespace in an evidence sentence; "" is a manifest
// object's unset metadata.namespace (files mode).
func nsLabel(ns string) string {
	return cmp.Or(ns, "no namespace set")
}

// namedNamespaces drops the unset namespace "" from a finding's sorted
// namespaces: it names no namespace (and no team) to filter by.
func namedNamespaces(ns []string) []string {
	if len(ns) > 0 && ns[0] == "" {
		ns = ns[1:]
	}
	if len(ns) == 0 {
		return nil
	}
	return ns
}

// addOnLocatedLimit caps the installs an add-on finding's evidence
// sentence lists.
const addOnLocatedLimit = 10

// located joins sorted install names, listing the first
// addOnLocatedLimit and counting the rest.
func located(names []string) string {
	if len(names) <= addOnLocatedLimit {
		return strings.Join(names, ", ")
	}
	return fmt.Sprintf("%s, and %d more", strings.Join(names[:addOnLocatedLimit], ", "), len(names)-addOnLocatedLimit)
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

// addOnSubject is a set of installs of one registry add-on, judged
// together (see newAddOnSubject).
type addOnSubject struct {
	version    string   // oldest normalised app version; "" when none is known
	located    string   // evidence sentence that opens every finding's detail
	namespaces []string // sorted
	teams      []string
	// unnamespaced: some install is in no named namespace (unnamespaced).
	unnamespaced bool
	installs     []addOnInstall
	// node marks node container runtimes. They ship with the node image or
	// OS, which a node upgrade or node-pool image bump replaces, so an
	// ended release line is a warning; only a compat row (the kubelet
	// dropping support) blocks.
	node bool
}

// noVersionReason is the sentence that ends an addon-no-data detail for
// installs whose version is unknown: what the evidence each was found by
// lacked, as the detector read it (#301), never a source it did not
// consult. labelsConsulted is whether the collector took a pod's
// app.kubernetes.io/version label for an image or chart without a version
// (inventory.LabelVersionCollectorSchema); an inventory from an older
// agent has the same gaps, but that label was not read, and the sentence
// says so instead of saying none gave a version.
//
// An install found from an image has no version read from it. The
// inventory cannot tell why, so the sentence covers both: the image has no
// version in its tag (a digest, ":latest") and, when labels were
// consulted, no pod running it whose labels name this add-on gives a
// usable version, or it is a component image whose release line the
// registry does not map yet (a label is never taken for those). One found
// from labels alone has no version label it can trust; one found from a
// Helm release records no appVersion, and no pod image tag (or, when
// consulted, label) gave one. For an inventory from an agent that predates
// the label rule the inventory cannot say that much: a v0.1.x agent
// reported the chart version where the app version belongs (legacyView
// blanks it), so the release may well record an appVersion and a pod tag
// may give one, and the release candidates did read it. The sentence then
// says only that none was read. node marks the subject of node container
// runtimes; a source the engine does not know gets a neutral sentence.
func noVersionReason(ins []addOnInstall, node, labelsConsulted bool) string {
	var parts []string
	add := func(s string) {
		if !slices.Contains(parts, s) {
			parts = append(parts, s)
		}
	}
	const unread = " The collecting agent predates reading a pod's app.kubernetes.io/version label for an image without a tag, so such a label may carry the version; upgrade the agent."
	for _, in := range ins {
		via, _, _ := strings.Cut(in.via, ",")
		via, _, _ = strings.Cut(via, " ")
		switch {
		case node:
			add("The node reports no container runtime version.")
		case via == "image" && labelsConsulted:
			add("No version was read from the image: either its tag names no version (a digest, :latest) and no pod running it whose labels name this add-on gives a usable app.kubernetes.io/version, or it is a component image whose release line the registry does not map yet.")
		case via == "image":
			add("No version was read from the image: either its tag names no version (a digest, :latest) or it is a component image whose release line the registry does not map yet." + unread)
		case via == "labels":
			add("The pod labels name it, but no app.kubernetes.io/version label gives a version that applies to it.")
		case via == "chart" && labelsConsulted:
			add("The Helm release records no appVersion, and no pod image tag or app.kubernetes.io/version label gives a version.")
		case via == "chart":
			add("No app version was read from the Helm release or from a pod image tag: either the release records none, or the collecting agent did not collect it (a v0.1.x agent reported only the chart version)." + unread)
		case via == "gitops" && labelsConsulted:
			add("The GitOps chart reference gives no app version, and no running pod's image tag or app.kubernetes.io/version label does.")
		case via == "gitops":
			add("The GitOps chart reference gives no app version, and no running pod's image tag does." + unread)
		case via == "ingressclass":
			add("An IngressClass names it but carries no version.")
		default:
			add("No version was recorded for this add-on.")
		}
	}
	return " " + strings.Join(parts, " ") + " Its end of life and Kubernetes compatibility were not assessed."
}

// evalAddOn judges the installs of one detected add-on, all of them for
// the product and each release-line group (see groupInstalls) on its own:
//
//   - product level (support), for whole-product retirements such as
//     ingress-nginx: status "eol" or eol_date ≤ now → blocker, eol-addon;
//     eol_date in (now, now+90d] → warning, eol-approaching. One finding
//     naming every install. With split support (extended_eol_date, see
//     splitSupportFinding) the blocker waits for the extended date, and
//     the time between the dates is a warning stating its condition.
//   - release line (cycles), unless the product carries a date or EOL
//     status: the group's cycle has ended → blocker, eol-addon (warning
//     for a node runtime); it ends in (now, now+90d] → warning,
//     eol-approaching. Versions older than every tracked line, when the
//     oldest has ended (predatesCycles), are past end of life too →
//     blocker, eol-addon (warning for a node runtime).
//   - an install whose version's cycle range [k8s_min, k8s_max], or else
//     the first compat row whose range matches the version, excludes the
//     target → blocker, chart-incompat, naming only such installs of the
//     group.
//   - no product date and no cycle for the version (between tracked lines,
//     newer than all of them, a product without lines), or no version at
//     all → info, addon-no-data: missing data must neither block nor read
//     as "checked, fine".
//
// Findings about a release line are keyed category/id/cycle, about
// versions older than every line category/id/below-<oldest line>, others
// category/id, so each key is one finding. A group is judged at its oldest
// version; an install without a detected version matches no compat row.
func evalAddOn(a registry.AddOn, all addOnSubject, groups []addOnSubject, target inventory.Version, now time.Time, labelsConsulted bool) []Finding {
	finding := func(s addOnSubject, cat Category, sev Severity, key, title, detail string, citations []string) Finding {
		return Finding{
			Category: cat, Severity: sev, Key: key, Title: title, Detail: detail,
			Teams: s.teams, Namespaces: s.namespaces, Unnamespaced: s.unnamespaced, Remediation: a.Recommendation,
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
	extDate, hasExt := time.Time{}, false
	if a.Support.ExtendedEOLDate != "" && a.Support.Status != "eol" {
		if d, err := time.Parse("2006-01-02", a.Support.ExtendedEOLDate); err == nil && hasDate {
			extDate, hasExt = d, true
		}
	}
	switch {
	case hasExt:
		if f, ok := splitSupportFinding(a, all, eolDate, extDate, now, window); ok {
			out = append(out, finding(all, f.Category, f.Severity, string(f.Category)+"/"+a.ID, f.Title, f.Detail, a.Support.Citations))
		}
	case a.Support.Status == "eol" || (hasDate && !eolDate.After(now)):
		// Tense follows the date: status "eol" can carry a future
		// effective date (upstream already declared EOL).
		title := fmt.Sprintf("%s is end-of-life", a.DisplayName)
		detail := all.located + " Upstream support has ended."
		switch {
		case hasDate && eolDate.After(now):
			title = fmt.Sprintf("%s is end-of-life on %s", a.DisplayName, a.Support.EOLDate)
			detail = all.located + fmt.Sprintf(" Upstream support ends on %s.", a.Support.EOLDate)
		case a.Support.EOLDate != "":
			title = fmt.Sprintf("%s is end-of-life since %s", a.DisplayName, a.Support.EOLDate)
		}
		out = append(out, finding(all, CatEOLAddon, SevBlocker, string(CatEOLAddon)+"/"+a.ID, title, detail, a.Support.Citations))
	case hasDate && !eolDate.After(window):
		out = append(out, finding(all, CatEOLApproaching, SevWarning, string(CatEOLApproaching)+"/"+a.ID,
			fmt.Sprintf("%s reaches end-of-life on %s", a.DisplayName, a.Support.EOLDate),
			all.located+fmt.Sprintf(" Upstream support ends on %s.", a.Support.EOLDate),
			a.Support.Citations))
	}
	productDated := a.Support.Status == "eol" || hasDate

	for _, s := range groups {
		cycle, inCycle := cycleFor(s.version, a.Cycles)
		oldest, below := predatesCycles(s.version, a.Cycles, now)
		key := func(cat Category) string {
			switch {
			case inCycle:
				return string(cat) + "/" + a.ID + "/" + cycle.Cycle
			case below:
				return string(cat) + "/" + a.ID + "/" + belowPrefix + oldest.Cycle
			}
			return string(cat) + "/" + a.ID
		}
		if (inCycle || below) && !productDated {
			f, ok := Finding{}, false
			if inCycle {
				f, ok = cycleEOL(a, cycle, s, now, window)
			} else {
				f, ok = predatesEOL(a, oldest, s, now), true
			}
			if ok {
				f.Key = key(f.Category)
				f.Teams, f.Namespaces, f.Unnamespaced = s.teams, s.namespaces, s.unnamespaced
				if s.node && f.Severity == SevBlocker {
					f.Severity = SevWarning
					f.Detail += " The runtime comes with the node image or OS, not with the Kubernetes version, so this does not block the upgrade by itself."
				}
				out = append(out, f)
			}
		}

		if f, ok := evalAddOnCompat(a, s, target); ok {
			f.Key = key(CatChartIncompat)
			out = append(out, f)
		}

		if !productDated && !inCycle && !below {
			ver, reason := s.version, " The registry has no release-line data for this version, so its end of life was not assessed."
			if ver == "" {
				ver, reason = "(version unknown)", noVersionReason(s.installs, s.node, labelsConsulted)
			}
			f := finding(s, CatAddOnNoData, SevInfo, string(CatAddOnNoData)+"/"+a.ID,
				fmt.Sprintf("no lifecycle data for %s %s", a.DisplayName, ver), s.located+reason, a.Support.Citations)
			f.Remediation = ""
			out = append(out, f)
		}
	}
	return out
}

// splitSupportFinding judges a product with split support (#265): support
// ends for everyone on eol_date (end) and lasts until extended_eol_date
// (ext) only if support.extended_support_condition holds, which the
// collector cannot see. end within 90 days, or the window between the two
// dates → warning, eol-approaching, stating the condition; ext passed →
// blocker, eol-addon; ok is false before end's 90-day window opens. Only
// Category, Severity, Title and Detail are set.
func splitSupportFinding(a registry.AddOn, all addOnSubject, end, ext, now, window time.Time) (Finding, bool) {
	s, cond := a.Support, a.Support.ExtendedSupportCondition
	switch {
	case !ext.After(now):
		return Finding{Category: CatEOLAddon, Severity: SevBlocker,
			Title:  fmt.Sprintf("%s is end-of-life since %s", a.DisplayName, s.ExtendedEOLDate),
			Detail: all.located + fmt.Sprintf(" Support ended on %s, and support that applied only if %s ended on %s.", s.EOLDate, cond, s.ExtendedEOLDate)}, true
	case !ext.After(window):
		return Finding{Category: CatEOLApproaching, Severity: SevWarning,
			Title:  fmt.Sprintf("%s reaches end-of-life on %s", a.DisplayName, s.ExtendedEOLDate),
			Detail: all.located + fmt.Sprintf(" Support until then applies only if %s; without it, support ended on %s.", cond, s.EOLDate)}, true
	case !end.After(now):
		return Finding{Category: CatEOLApproaching, Severity: SevWarning,
			Title:  fmt.Sprintf("%s is supported until %s only if %s", a.DisplayName, s.ExtendedEOLDate, cond),
			Detail: all.located + fmt.Sprintf(" Support without that condition ended on %s; upgradescope cannot see whether this cluster meets it.", s.EOLDate)}, true
	case !end.After(window):
		return Finding{Category: CatEOLApproaching, Severity: SevWarning,
			Title:  fmt.Sprintf("%s reaches end-of-life on %s", a.DisplayName, s.EOLDate),
			Detail: all.located + fmt.Sprintf(" Support ends on %s; after that it continues until %s only if %s, which upgradescope cannot see.", s.EOLDate, s.ExtendedEOLDate, cond)}, true
	}
	return Finding{}, false
}

// evalAddOnCompat judges each install of a group against target (see
// compatFor) and returns one chart-incompat blocker naming only the
// installs that cannot run it, titled for the oldest of them; ok is false
// when every install can. The detail lists them, at most addOnLocatedLimit,
// when their versions differ, and for node runtimes always: nodes have no
// Namespaces to name them by. Teams name them all, Namespaces up to
// MaxFindingNamespaces (Evaluate caps it). Key is left to the caller.
func evalAddOnCompat(a registry.AddOn, s addOnSubject, target inventory.Version) (Finding, bool) {
	f := Finding{Category: CatChartIncompat, Severity: SevBlocker, Remediation: a.Recommendation}
	var named []string // "where (version)" of each install that cannot run target
	versions := map[string]bool{}
	oldest := ""
	for _, in := range s.installs {
		if in.version == "" {
			continue
		}
		title, detail, citations, bad := compatFor(a, in.version, target)
		if !bad {
			continue
		}
		for _, w := range in.where {
			named = append(named, fmt.Sprintf("%s (%s)", nsLabel(w), in.version))
		}
		versions[in.version] = true
		if !s.node {
			f.Namespaces = append(f.Namespaces, in.where...)
			f.Teams = append(f.Teams, in.teams...)
			f.Unnamespaced = f.Unnamespaced || unnamespaced(in)
		}
		if oldest == "" || versionBefore(in.version, oldest) {
			oldest = in.version
			f.Title, f.Detail, f.Citations = title, detail, slices.Clone(citations)
		}
	}
	if oldest == "" {
		return Finding{}, false
	}
	f.Namespaces, f.Teams = namedNamespaces(sortedSet(f.Namespaces)), sortedSet(f.Teams)
	switch {
	case s.node: // no namespaces to name them by: always list the nodes (#169)
		sort.Strings(named)
		f.Detail += " Incompatible nodes: " + located(named) + "."
	case len(versions) > 1:
		sort.Strings(named)
		f.Detail += " Incompatible installs: " + located(named) + "."
	}
	return f, true
}

// sortedSet sorts s and drops duplicates, in place; empty stays nil.
func sortedSet(s []string) []string {
	if len(s) == 0 {
		return nil
	}
	slices.Sort(s)
	return slices.Compact(s)
}

// compatFor judges one installed version against target: the range of its
// release line when it has one, else the first compat row whose range
// matches it. ok reports that target is outside the range.
func compatFor(a registry.AddOn, version string, target inventory.Version) (title, detail string, citations []string, ok bool) {
	if cycle, inCycle := cycleFor(version, a.Cycles); inCycle {
		if title, bad := k8sOutOfRange(a.DisplayName, version, cycle.K8sMin, cycle.K8sMax, target); bad {
			return title, fmt.Sprintf("Installed version %s is in the %s release line, which supports Kubernetes %s.",
				version, cycle.Cycle, k8sRangeText(cycle.K8sMin, cycle.K8sMax)), cycle.Citations, true
		}
	}
	for _, c := range a.Compat {
		if !matchesRange(version, c.Range) {
			continue
		}
		if title, bad := k8sOutOfRange(a.DisplayName, version, c.K8sMin, c.K8sMax, target); bad {
			return title, fmt.Sprintf("Installed version %s matches compatibility range %q, which supports Kubernetes %s.",
				version, c.Range, k8sRangeText(c.K8sMin, c.K8sMax)), c.Citations, true
		}
		break // first matching range wins
	}
	return "", "", nil, false
}

// cycleEOL judges the end of life of the release line a version is in:
// ended → blocker, ending by window → warning; ok is false otherwise. Key,
// Teams and Namespaces are left to the caller.
func cycleEOL(a registry.AddOn, c registry.Cycle, s addOnSubject, now, window time.Time) (Finding, bool) {
	if c.EOL == nil {
		return Finding{}, false // registry validation requires eol; defensive
	}
	f := lineFinding(a, c, now)
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
	return f, true
}

// lineFinding starts a finding about release line c: its citations and
// the product's, and the registry's recommendation, else an upgrade to the
// newest supported line.
func lineFinding(a registry.AddOn, c registry.Cycle, now time.Time) Finding {
	f := Finding{Remediation: a.Recommendation}
	for _, u := range append(slices.Clone(c.Citations), a.Support.Citations...) {
		if !slices.Contains(f.Citations, u) {
			f.Citations = append(f.Citations, u)
		}
	}
	if newest := newestSupportedCycle(a.Cycles, now); f.Remediation == "" && newest != "" {
		f.Remediation = fmt.Sprintf("Upgrade %s to a supported release line (newest: %s).", a.DisplayName, newest)
	}
	return f
}

// belowPrefix marks the group, and the key, of installs older than every
// tracked release line: "eol-addon/cert-manager/below-1.10".
const belowPrefix = "below-"

// predatesCycles reports whether version is older than every release line
// in cycles, comparing the version's leading components with each line's,
// and the oldest line has ended by now; it returns that line. Upstream
// lifecycle data (endoflife.date) stops at some old line, and a line older
// than an ended one has ended too: cert-manager 1.5 is past end of life
// though the registry's oldest line is 1.10. While the oldest line is
// supported, an older version may be a line the data never had, so it is
// not judged. Neither is a version with fewer components than the oldest
// line ("1" against "1.10"), nor an unparseable one.
func predatesCycles(version string, cycles []registry.Cycle, now time.Time) (registry.Cycle, bool) {
	v, ok := versionParts(version)
	if !ok {
		return registry.Cycle{}, false
	}
	oldest, oldestParts := -1, []int(nil)
	for i, c := range cycles {
		if p, ok := versionParts(c.Cycle); ok && (oldest < 0 || slices.Compare(p, oldestParts) < 0) {
			oldest, oldestParts = i, p
		}
	}
	if oldest < 0 || len(v) < len(oldestParts) || slices.Compare(v[:len(oldestParts)], oldestParts) >= 0 {
		return registry.Cycle{}, false
	}
	c := cycles[oldest]
	if !cycleEnded(c, now) {
		return registry.Cycle{}, false
	}
	return c, true
}

// cycleEnded reports whether release line c has ended by now: eol true, or
// a date not after now.
func cycleEnded(c registry.Cycle, now time.Time) bool {
	if c.EOL == nil {
		return false
	}
	if c.EOL.Date == "" {
		return c.EOL.Ended
	}
	d, err := time.Parse("2006-01-02", c.EOL.Date)
	return err == nil && !d.After(now)
}

// predatesEOL is the end-of-life blocker for installs older than every
// tracked release line (predatesCycles), citing the oldest line, which
// has ended. Key, Teams and Namespaces are left to the caller.
func predatesEOL(a registry.AddOn, oldest registry.Cycle, s addOnSubject, now time.Time) Finding {
	f := lineFinding(a, oldest, now)
	f.Category, f.Severity = CatEOLAddon, SevBlocker
	f.Title = fmt.Sprintf("%s %s is end-of-life (older than the %s release line)", a.DisplayName, s.version, oldest.Cycle)
	ended := fmt.Sprintf("support for %s has ended", oldest.Cycle)
	if oldest.EOL.Date != "" {
		ended = fmt.Sprintf("support for %s ended on %s", oldest.Cycle, oldest.EOL.Date)
	}
	f.Detail = s.located + fmt.Sprintf(" Versions older than the %s release line, the oldest one upstream still documents, are past end of life: %s.", oldest.Cycle, ended)
	return f
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
			Detail:    fmt.Sprintf("After upgrading the control plane to %s these nodes would be more than %d minor versions behind: %s.", target, maxBehind, listedNodes(postBad)) + legacyNote("Kubelets", postLegacy),
			Citations: []string{skewPolicyURL},
		})
	}
	if len(nowBad) > 0 {
		out = append(out, Finding{
			Category: CatVersionSkew, Severity: SevWarning,
			Key:       string(CatVersionSkew) + "/kubelet-current",
			Title:     fmt.Sprintf("%d node(s) exceed kubelet version skew vs control plane %s", len(nowBad), newest),
			Detail:    fmt.Sprintf("Nodes more than %d minor versions behind: %s.", maxBehind, listedNodes(nowBad)) + legacyNote("Kubelets", nowLegacy),
			Citations: []string{skewPolicyURL},
		})
	}
	if len(newer) > 0 {
		out = append(out, Finding{
			Category: CatVersionSkew, Severity: SevWarning,
			Key:       string(CatVersionSkew) + "/kubelet-newer-than-apiserver",
			Title:     fmt.Sprintf("%d node(s) run a kubelet newer than kube-apiserver %s", len(newer), oldest),
			Detail:    fmt.Sprintf("kubelet must not be newer than kube-apiserver; when kube-apiserver versions differ, the oldest one (%s) bounds the allowed kubelet versions. Newer nodes: %s.", oldest, listedNodes(newer)),
			Citations: []string{skewPolicyURL},
		})
	}
	if len(unparseable) > 0 {
		sort.Strings(unparseable)
		out = append(out, Finding{
			Category: CatVersionSkew, Severity: SevInfo,
			Key:       string(CatVersionSkew) + "/kubelet-unparseable",
			Title:     fmt.Sprintf("%d node(s) have unparseable kubelet versions", len(unparseable)),
			Detail:    fmt.Sprintf("These nodes could not be evaluated against the kubelet skew policy: %s.", listedNodes(unparseable)),
			Citations: []string{skewPolicyURL},
		})
	}
	return out
}

// maxListedNodes caps the nodes a kubelet skew finding's detail names;
// its title counts them all.
const maxListedNodes = 100

// listedNodes joins sorted node entries, the first maxListedNodes of
// them, and counts the rest.
func listedNodes(entries []string) string {
	if len(entries) <= maxListedNodes {
		return strings.Join(entries, ", ")
	}
	return fmt.Sprintf("%s, and %d more", strings.Join(entries[:maxListedNodes], ", "), len(entries)-maxListedNodes)
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
// The newer and behind findings are keyed <component>-newer and
// <component>-behind: one component can be both at once (HA replicas
// mid-upgrade), at different severities.
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
				Key:       string(CatVersionSkew) + "/" + rule.component + "-newer",
				Title:     fmt.Sprintf("%s is newer than kube-apiserver", rule.component),
				Detail:    fmt.Sprintf("%s %s is newer than the oldest kube-apiserver (%s); %s.", rule.component, strings.Join(newer, ", "), oldest, rule.newerDetail),
				Citations: []string{skewPolicyURL},
			})
		}
		if len(behind) > 0 {
			out = append(out, Finding{
				Category: CatVersionSkew, Severity: SevWarning,
				Key:       string(CatVersionSkew) + "/" + rule.component + "-behind",
				Title:     fmt.Sprintf("%s exceeds version skew vs kube-apiserver", rule.component),
				Detail:    fmt.Sprintf("%s %s is more than %d minor version(s) behind the newest kube-apiserver (%s).", rule.component, strings.Join(behind, ", "), rule.maxBehind, newest) + legacyNote("kube-proxy versions", behindLegacy),
				Citations: []string{skewPolicyURL},
			})
		}
	}
	return out
}

// evalKubeProxyKubeletSkew evaluates the one skew rule that pairs two
// components on the same node: kube-proxy may be up to
// k.Skew.KubeProxyMaxBehind minors older or newer than the kubelet it runs
// alongside (2 for a kube-proxy older than 1.25). Each kube-proxy that
// names its node (ComponentVersion.Node) is paired with that node's
// kubelet; one that names none, or whose node or kubelet version is not
// known, is not paired and gives nothing. The result is one warning per
// node, keyed version-skew/kube-proxy-kubelet/<node> (a node name is
// unique, and a rolling DaemonSet update can leave two kube-proxy versions
// on one), listing the kube-proxy versions out of policy.
//
// Neither component moves when the control plane does, so unlike the
// kubelet and kube-proxy rules against the apiserver there is no
// post-upgrade form of this rule: the target does not enter into it.
func evalKubeProxyKubeletSkew(inv inventory.Inventory, k kb.KB) []Finding {
	kubelets := map[string]inventory.Version{}
	rawKubelets := map[string]string{}
	for _, n := range inv.Nodes {
		if kv, err := inventory.ParseVersion(n.KubeletVersion); err == nil {
			kubelets[n.Name], rawKubelets[n.Name] = kv, n.KubeletVersion
		}
	}
	type bad struct {
		kubelet string
		proxies []string
		legacy  bool
	}
	byNode := map[string]*bad{}
	for _, cv := range inv.ControlPlane {
		if cv.Component != "kube-proxy" || cv.Node == "" {
			continue
		}
		kv, ok := kubelets[cv.Node]
		pv, err := inventory.ParseVersion(cv.Version)
		if !ok || err != nil {
			continue
		}
		limit := legacyMaxBehind(k.Skew.KubeProxyMaxBehind, pv)
		if minorsBehind(kv, pv) <= limit && minorsBehind(pv, kv) <= limit {
			continue
		}
		b := byNode[cv.Node]
		if b == nil {
			b = &bad{kubelet: rawKubelets[cv.Node]}
			byNode[cv.Node] = b
		}
		b.proxies = append(b.proxies, cv.Version)
		b.legacy = b.legacy || limit < k.Skew.KubeProxyMaxBehind
	}
	var out []Finding
	for _, node := range slices.Sorted(maps.Keys(byNode)) {
		b := byNode[node]
		sort.Strings(b.proxies)
		out = append(out, Finding{
			Category: CatVersionSkew, Severity: SevWarning,
			Key:       string(CatVersionSkew) + "/kube-proxy-kubelet/" + node,
			Title:     fmt.Sprintf("kube-proxy on node %s exceeds version skew vs its kubelet", node),
			Detail:    fmt.Sprintf("kube-proxy %s on node %s is more than %d minor version(s) older or newer than the kubelet on that node (%s).", strings.Join(b.proxies, ", "), node, k.Skew.KubeProxyMaxBehind, b.kubelet) + legacyNote("kube-proxy versions", b.legacy),
			Citations: []string{skewPolicyURL + "#kube-proxy"},
		})
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

// upgradeFrom is the minor a live cluster upgrades from: its oldest
// observed kube-apiserver (HA replicas mid-upgrade still need the newer
// minor). ok is false in files mode or without a parseable version.
func upgradeFrom(inv inventory.Inventory) (inventory.Version, bool) {
	if inv.Source == inventory.SourceFiles {
		return inventory.Version{}, false
	}
	apis := apiserverVersions(inv)
	if len(apis) == 0 {
		return inventory.Version{}, false
	}
	return apis[0], true
}

// maxSpelledHops is the longest upgrade path whose title lists every step.
const maxSpelledHops = 3

// evalUpgradePath: a target more than one minor ahead of the cluster is
// several upgrades, since the control plane moves one minor at a time →
// info naming each step, up to maxSpelledHops. The title is copied into the
// ClusterReadiness status, so past that it shows only the ends of the path:
// its size must not grow with the distance to the target. It is not part of
// the finding's identity, which is the Key.
func evalUpgradePath(inv inventory.Inventory, target inventory.Version) []Finding {
	from, ok := upgradeFrom(inv)
	if !ok || target.Major != from.Major || target.Minor-from.Minor < 2 {
		return nil
	}
	hops := target.Minor - from.Minor
	var path string
	if hops <= maxSpelledHops {
		steps := make([]string, 0, hops)
		for v := from.Next(); v.Compare(target) <= 0; v = v.Next() {
			steps = append(steps, v.String())
		}
		path = ": " + strings.Join(steps, ", ")
	} else {
		path = fmt.Sprintf(" (%s → %s → … → %s)", from, from.Next(), target)
	}
	return []Finding{{
		Category: CatVersionSkew, Severity: SevInfo,
		Key:       string(CatVersionSkew) + "/upgrade-path",
		Title:     fmt.Sprintf("upgrading from %s to %s takes %d minor-version upgrades%s", from, target, hops, path),
		Detail:    fmt.Sprintf("The control plane is upgraded one minor version at a time. This report judges the cluster as it is against %s; add-ons, charts and nodes may need upgrading at each step in between.", target),
		Citations: []string{skewPolicyURL},
	}}
}

// assessmentGaps lists what the evaluation could not assess, sorted by
// capability: every unavailable or partial inventory capability, a
// versions gap when the server version is missing or unparseable (unless
// versions is already unavailable), a kb-coverage gap when the target is
// beyond the KB horizon, and a target gap when the target is not an
// upgrade of the cluster (see upgradeFrom). Required is set per the
// verdict rules on CapabilityGap. Collectors report every capability they
// have, so one absent from inv.Capabilities was not collected at all.
// A required one (api-usage; in a cluster inventory versions and addons
// too) is a required gap: every collector since v0.1.0 reports them, and
// an inventory without them — hand-built, a third-party or a regressed
// collector's, or one with no capabilities map — must not read ready on
// evidence it does not carry (#194). The source is the inventory's own
// claim, so ingest accepts only cluster ones. An absent optional one is
// no gap, except crds, which
// collectors older than it do not report: an inventory without it (an
// older agent's, or a files inventory an older CLI saved) is a crds gap,
// so its CRD versions read as not assessed rather than clean.
func assessmentGaps(inv inventory.Inventory, k kb.KB, target inventory.Version) []CapabilityGap {
	required := map[inventory.Capability]bool{inventory.CapAPIUsage: true, GapKBCoverage: true}
	cluster := inv.Source != inventory.SourceFiles // "" = cluster (v0.1 agents)
	if cluster {
		required[inventory.CapVersions] = true
		if len(k.AddOns) > 0 {
			required[inventory.CapAddOns] = true
		}
	}
	idx := kb.NewIndex(k.APILifecycle)
	var gaps []CapabilityGap
	for _, c := range []inventory.Capability{inventory.CapAPIUsage, inventory.CapVersions, inventory.CapAddOns} {
		if _, ok := inv.Capabilities[c]; !ok && required[c] {
			gaps = append(gaps, CapabilityGap{Capability: c, Required: true,
				Reason: "not reported in the inventory, so nothing it covers was assessed"})
		}
	}
	for c, st := range inv.Capabilities {
		switch {
		case !st.Available:
			gaps = append(gaps, CapabilityGap{Capability: c, Reason: st.Reason, Required: required[c]})
		case st.Partial:
			g := CapabilityGap{Capability: c, Reason: st.Reason, Partial: true, Skipped: st.Skipped}
			switch c {
			case inventory.CapAPIUsage:
				// SkippedNewerKB: the agent's knowledge base is not the
				// server's, so any API the target removes may have gone
				// unlisted.
				g.Required = slices.Contains(st.Skipped, inventory.SkippedNewerKB) ||
					slices.ContainsFunc(st.Skipped, func(api string) bool { return removedBy(idx, api, target) })
			case inventory.CapVersions:
				// A component whose version upstream would have told but
				// could not be read (#169), which collect names in Skipped,
				// may be the one past the skew policy: never READY on that.
				// Partial naming none (a vendor kube-proxy image, OKE's) is
				// disclosed, optional. "nodes" (no Node listed, #174) is
				// required like a forbidden Node list: no kubelet was
				// judged, and one past the policy would block.
				g.Required = required[c] && len(st.Skipped) > 0
			case inventory.CapAddOns:
				// Without pods only Helm releases and IngressClasses speak:
				// an add-on installed any other way goes undetected (#199).
				// IngressClasses alone are supplementary evidence, optional.
				// The same for add-ons the server's registry knows and the
				// agent's did not (inventory.SkippedNewerKB).
				g.Required = required[c] && (slices.Contains(st.Skipped, inventory.SkippedPods) || slices.Contains(st.Skipped, inventory.SkippedNewerKB))
			}
			gaps = append(gaps, g)
		}
	}
	if _, ok := inv.Capabilities[inventory.CapCRDs]; !ok {
		gaps = append(gaps, CapabilityGap{Capability: inventory.CapCRDs,
			Reason: "not reported by the collector, which predates CRD checks; upgrade it to assess CRD versions"})
	}
	if st, ok := inv.Capabilities[inventory.CapVersions]; (!ok && !cluster) || (ok && st.Available) {
		const notEvaluated = "kubelet and control-plane skew were not evaluated"
		if inv.ServerVersion == "" {
			gaps = append(gaps, CapabilityGap{Capability: inventory.CapVersions, Reason: "server version not reported; " + notEvaluated,
				Required: required[inventory.CapVersions]})
		} else if _, err := inventory.ParseVersion(inv.ServerVersion); err != nil {
			gaps = append(gaps, CapabilityGap{Capability: inventory.CapVersions, Required: required[inventory.CapVersions],
				Reason: fmt.Sprintf("server version %q could not be parsed; %s", inv.ServerVersion, notEvaluated)})
		}
	}
	if target.Compare(k.MaxKnownK8s) > 0 {
		gaps = append(gaps, CapabilityGap{Capability: GapKBCoverage, Required: true,
			Reason: fmt.Sprintf("knowledge base covers Kubernetes up to %s; target %s cannot be assessed (API removals after %s are projected from k8s.io/api lifecycle markers, not shipped releases)",
				k.MaxKnownK8s, target, k.MaxKnownK8s)})
	}
	if from, ok := upgradeFrom(inv); ok && target.Compare(from) <= 0 {
		gaps = append(gaps, CapabilityGap{Capability: GapTarget, Required: true,
			Reason: fmt.Sprintf("target %s is not an upgrade: kube-apiserver already runs %s, and every check judges a newer minor", target, from)})
	}
	sort.Slice(gaps, func(i, j int) bool { return gaps[i].Capability < gaps[j].Capability })
	return gaps
}

// removedBy reports whether api, a flagged API as a partial api-usage
// capability names it in Skipped ("group/version Kind", core "v1 Kind"),
// is one the KB removes at or before target. An API the KB does not know
// could not have produced a finding either way.
func removedBy(idx kb.Index, api string, target inventory.Version) bool {
	group, version, kind, ok := splitAPI(api)
	if !ok {
		return false
	}
	e, ok := idx.Lookup(group, version, kind)
	return ok && e.Removed != nil && e.Removed.Compare(target) <= 0
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

// helmInstalled reports whether a release has anything running. Collectors
// leave out releases with nothing installed; inventories from agents that
// predate that may still list ones removed with helm uninstall
// --keep-history.
func helmInstalled(rel inventory.HelmRelease) bool {
	return rel.Status != "uninstalled" && rel.Status != "uninstalling"
}

// helmReleaseRef names a release, with the revision the collector read
// when it recorded one: "revision 3 of Helm release shop/web".
func helmReleaseRef(rel inventory.HelmRelease) string {
	if rel.Revision > 0 {
		return fmt.Sprintf("revision %d of Helm release %s/%s", rel.Revision, rel.Namespace, rel.Name)
	}
	return fmt.Sprintf("Helm release %s/%s", rel.Namespace, rel.Name)
}

// evalHelmReleases judges each installed Helm release's chart and stored
// manifest against the target, with no registry data:
//
//   - chart kubeVersion: Helm refuses to install or upgrade a chart whose
//     Chart.yaml kubeVersion constraint the cluster version does not
//     satisfy (Masterminds semver, as Helm checks it: "-0" admits
//     pre-release builds). target.0 failing it → blocker, chart-incompat;
//     a constraint that does not parse → info, chart-incompat.
//   - manifest APIs: objects in the release's stored manifest at an API
//     removed at or before target → blocker, removed-api (helm upgrade
//     fails on a stored manifest the cluster cannot map); at an API that is
//     deprecated or removed later → warning, deprecated-api. One finding
//     per release and severity, keyed "<category>/helm-release/<ns>/<name>"
//     — "helm-release" is no API group, so these keys never collide with
//     the per-API keys of live findings. Objects the live scan already
//     flags (same API, name and namespace, an unset manifest namespace
//     standing for the release's) are left to the live finding when it is
//     at least as severe (a removed-api blocker, or a removed-api warning
//     at a removal in the next minor), so no object is counted twice at
//     one severity; a release whose objects are all so flagged gets no
//     manifest finding. A live info (deprecated-api) does not take them:
//     the manifest's warning stays, so more evidence never lowers the
//     severity.
func evalHelmReleases(inv inventory.Inventory, k kb.KB, target inventory.Version, b *budget) []Finding {
	teams := newTeamLookup(inv.Namespaces)
	idx := kb.NewIndex(k.APILifecycle)
	live := map[string][]inventory.ObjectRef{} // apiKey → live objects
	for _, u := range inv.APIUsage {
		key := apiKey(u.Group, u.Version, u.Kind)
		live[key] = append(live[key], u.Objects...)
	}
	var out []Finding
	for _, rel := range inv.HelmReleases {
		if !helmInstalled(rel) {
			continue
		}
		ns := []string{rel.Namespace}
		relTeams := teams.teamsFor(ns)
		if f, ok := evalChartKubeVersion(rel, target); ok {
			f.Namespaces, f.Teams = ns, relTeams
			if !b.add(&out, f) {
				return out
			}
		}
		for _, f := range evalHelmManifest(rel, idx, live, target, k.MaxKnownK8s) {
			f.Namespaces, f.Teams = ns, relTeams
			if !b.add(&out, f) {
				return out
			}
		}
	}
	return out
}

// evalChartKubeVersion judges a release's chart kubeVersion constraint
// against target (see evalHelmReleases); ok is false when it has none or
// target satisfies it.
func evalChartKubeVersion(rel inventory.HelmRelease, target inventory.Version) (Finding, bool) {
	if strings.TrimSpace(rel.KubeVersion) == "" {
		return Finding{}, false
	}
	f := Finding{
		Category:  CatChartIncompat,
		Key:       fmt.Sprintf("%s/helm-release/%s/%s", CatChartIncompat, rel.Namespace, rel.Name),
		Citations: []string{helmChartYAMLURL},
	}
	declares := fmt.Sprintf("Chart %s %s (%s) declares kubeVersion %q in Chart.yaml", rel.ChartName, rel.ChartVersion, helmReleaseRef(rel), rel.KubeVersion)
	c, err := semver.NewConstraint(rel.KubeVersion)
	if err != nil {
		f.Severity = SevInfo
		f.Title = fmt.Sprintf("Helm release %s/%s: chart kubeVersion %q could not be parsed", rel.Namespace, rel.Name, rel.KubeVersion)
		f.Detail = declares + ", which is not a valid semver constraint, so the chart's Kubernetes compatibility was not assessed."
		return f, true
	}
	// Only <target>.0 is checked: a bound with a patch level inside the
	// target minor ("<1.33.5", ">=1.33.2") is judged at its first patch,
	// so it may pass for a later patch it excludes, or block although later
	// patches satisfy it. Chart bounds are usually whole minors ("<1.33.0-0").
	if c.Check(semver.New(uint64(target.Major), uint64(target.Minor), 0, "", "")) {
		return Finding{}, false
	}
	f.Severity = SevBlocker
	f.Title = fmt.Sprintf("Helm release %s/%s: chart %s %s requires Kubernetes %q (target %s)", rel.Namespace, rel.Name, rel.ChartName, rel.ChartVersion, rel.KubeVersion, target)
	f.Detail = declares + fmt.Sprintf(", which Kubernetes %s does not satisfy. Helm refuses to install or upgrade a chart whose kubeVersion excludes the cluster version, so once the cluster runs %s every helm upgrade of this release fails until it moves to a chart version that supports %s.", target, target, target)
	f.Remediation = fmt.Sprintf("upgrade the release to a chart version whose kubeVersion includes %s before upgrading the cluster", target)
	return f, true
}

// evalHelmManifest judges a release's flagged manifest objects (see
// evalHelmReleases): at most a removed-api blocker and a deprecated-api
// warning, in that order.
func evalHelmManifest(rel inventory.HelmRelease, idx kb.Index, live map[string][]inventory.ObjectRef, target, maxKnown inventory.Version) []Finding {
	type bucket struct {
		apis, entries, replacements []string
		unserved                    []string // APIs no served replacement is known for
		projected                   []string // removals past the KB horizon (see evalAPIUsage)
		objects                     []inventory.ObjectRef
		count, omitted              int
	}
	var removed, deprecated bucket
	rows := slices.Clone(rel.ManifestAPIs)
	slices.SortFunc(rows, func(a, b inventory.APIUsage) int {
		return cmp.Or(cmp.Compare(a.Group, b.Group), cmp.Compare(a.Version, b.Version), cmp.Compare(a.Kind, b.Kind))
	})
	for _, u := range rows {
		e, ok := idx.Lookup(u.Group, u.Version, u.Kind)
		if !ok || e.Deprecated == nil && e.Removed == nil {
			continue
		}
		b, when := &deprecated, []string{}
		if e.Removed != nil && e.Removed.Compare(target) <= 0 {
			b = &removed
		} else if e.Deprecated != nil {
			when = append(when, "deprecated in "+e.Deprecated.String())
		}
		// The live finding for this API (evalAPIUsage) is a blocker when
		// the manifest's is, a removed-api warning at a removal in the
		// next minor, else an info: only one at least as severe as the
		// manifest's takes its objects, so more evidence never lowers the
		// severity.
		liveAsSevere := b == &removed || e.Removed != nil && e.Removed.Compare(target.Next()) == 0
		objs := slices.Clone(u.Objects)
		if flagged := live[apiKey(u.Group, u.Version, u.Kind)]; liveAsSevere {
			objs = slices.DeleteFunc(objs, func(o inventory.ObjectRef) bool {
				return slices.ContainsFunc(flagged, func(l inventory.ObjectRef) bool {
					return l.Name == o.Name && (l.Namespace == o.Namespace || o.Namespace == "" && l.Namespace == rel.Namespace)
				})
			})
		}
		count := u.Count - (len(u.Objects) - len(objs))
		if count <= 0 {
			continue
		}
		if e.Removed != nil {
			removedIn := "removed in " + e.Removed.String()
			if e.Removed.Compare(maxKnown) > 0 {
				// As evalAPIUsage: k8s.io/api's lifecycle markers, not a release.
				removedIn += " (projected)"
				if !slices.Contains(b.projected, e.Removed.String()) {
					b.projected = append(b.projected, e.Removed.String())
				}
			}
			when = append(when, removedIn)
		}
		api := gvString(u.Group, u.Version) + " " + u.Kind
		b.apis = append(b.apis, api)
		b.entries = append(b.entries, fmt.Sprintf("%s (%s; %s)", api, strings.Join(when, ", "), pluralObjects(count)))
		// As evalAPIUsage words it: only a replacement target serves is
		// recommended, else the one a later release serves is named.
		if r, ok := idx.ResolveReplacement(e, target); ok {
			b.replacements = append(b.replacements, gvString(r.Group, r.Version)+" "+r.Kind)
		} else if later, from, ok := idx.LaterReplacement(e, target); ok {
			b.unserved = append(b.unserved, fmt.Sprintf("%s (%s %s is served from %s)", api, gvString(later.Group, later.Version), later.Kind, from))
		} else if succ, ok := idx.ServedSuccessor(e, target); ok {
			b.replacements = append(b.replacements, successorRemedy(succ, maxKnown, true))
		} else if e.Replacement != nil {
			b.unserved = append(b.unserved, api)
		}
		b.objects = append(b.objects, objs...)
		b.count += count
		b.omitted += u.ObjectsOmitted
	}

	ref := helmReleaseRef(rel)
	stores := fmt.Sprintf("%s%s (chart %s %s) stores a manifest with", strings.ToUpper(ref[:1]), ref[1:], rel.ChartName, rel.ChartVersion)
	fix := "upgrade the release to a chart version that renders supported APIs"
	var out []Finding
	for _, b := range []struct {
		bucket
		cat Category
		sev Severity
	}{{removed, CatRemovedAPI, SevBlocker}, {deprecated, CatDeprecatedAPI, SevWarning}} {
		if len(b.apis) == 0 {
			continue
		}
		f := Finding{
			Category: b.cat, Severity: b.sev,
			Key:            fmt.Sprintf("%s/helm-release/%s/%s", b.cat, rel.Namespace, rel.Name),
			Citations:      []string{helmKubernetesAPIsURL, deprecationGuideURL},
			Objects:        sortedObjects(b.objects),
			ObjectsOmitted: b.omitted,
			Remediation:    fix,
		}
		projection := ""
		if len(b.projected) > 0 {
			slices.Sort(b.projected)
			projection = fmt.Sprintf(" The removal in %s is projected: it is a default of k8s.io/api's lifecycle markers, not a shipped release (the newest the knowledge base covers is %s), and may change before it ships.", strings.Join(b.projected, ", "), maxKnown)
		}
		if len(b.replacements) > 0 {
			f.Remediation += " (" + strings.Join(b.replacements, ", ") + ")"
		}
		apis, entries := strings.Join(b.apis, ", "), strings.Join(b.entries, ", ")
		if b.sev == SevBlocker {
			f.Title = fmt.Sprintf("helm upgrade of release %s/%s will fail: its manifest uses %s", rel.Namespace, rel.Name, apis)
			serves := "does not serve"
			if len(b.projected) > 0 {
				serves = "is projected not to serve"
			}
			f.Detail = fmt.Sprintf("%s %s at APIs Kubernetes %s %s: %s. Helm refuses to upgrade a release whose stored manifest uses APIs the cluster no longer serves.%s", stores, pluralObjects(b.count), target, serves, entries, projection)
			f.Remediation += " before upgrading the cluster; if the cluster already stopped serving them, rewrite the stored manifest with the helm-mapkubeapis plugin first"
		} else {
			f.Title = fmt.Sprintf("Helm release %s/%s manifest uses deprecated %s", rel.Namespace, rel.Name, apis)
			f.Detail = fmt.Sprintf("%s %s at deprecated APIs: %s.%s", stores, pluralObjects(b.count), entries, projection)
		}
		if len(b.unserved) > 0 {
			f.Remediation += fmt.Sprintf("; no replacement Kubernetes %s serves is known for %s", target, strings.Join(b.unserved, ", "))
		}
		out = append(out, f)
	}
	return out
}

// Evaluate is the pure evaluation entrypoint: no I/O, no clock reads — now is
// injected for EOL-window math. Output is fully deterministic for a given
// (inventory, kb, target, now), whatever the order of the inventory's slices.
// EvaluateWithin bounds its size.
func Evaluate(inv inventory.Inventory, k kb.KB, target inventory.Version, now time.Time) Report {
	r, _ := evaluate(inv, k, target, now, nil) // no budget: no error
	return r
}

// evaluate is Evaluate within b (nil: no limit), or ErrReportTooLarge.
func evaluate(inv inventory.Inventory, k kb.KB, target inventory.Version, now time.Time, b *budget) (Report, error) {
	// Caller rows are judged, and folded into usage findings as evidence,
	// in this order, so the same rows in another order (a pusher that
	// sorts differently) give the same report. A clone: otherCallers may
	// return the caller's own slice.
	inv.DeprecatedCalls = slices.Clone(otherCallers(inv))
	slices.SortFunc(inv.DeprecatedCalls, func(x, y inventory.DeprecatedCall) int {
		return cmp.Or(cmp.Compare(x.Group, y.Group), cmp.Compare(x.Version, y.Version),
			cmp.Compare(x.Resource, y.Resource), cmp.Compare(x.Subresource, y.Subresource),
			cmp.Compare(x.RemovedRelease, y.RemovedRelease))
	})
	findings := []Finding{} // non-nil so JSON renders "findings": []
	support, supportFindings := evalSupport(inv, k, now)
	b.charge(reportBaseSize(inv, k))
	usage := evalAPIUsage(inv, k, target, b)
	var calls []Finding
	if !b.exceeded() {
		calls = evalDeprecatedCalls(inv, k, target, b)
	}
	if !b.exceeded() {
		findings = append(findings, foldDeprecatedCalls(inv, usage, calls, b)...)
	}
	// The checks whose output does not grow with the inventory's size are
	// charged as a whole; the others charge each finding as they build it
	// and stop once the budget is spent.
	steps := []func(){
		func() { findings = append(findings, evalAuthorshipUnknown(inv, k, b)...) },
		func() { b.addAll(&findings, evalAddOns(inv, k, target, now)) },
		func() { findings = append(findings, evalUncoveredRuntimes(inv, k.AddOns, b)...) },
		func() { findings = append(findings, evalHelmReleases(inv, k, target, b)...) },
		func() { b.addAll(&findings, evalSkew(inv, k, target)) },
		func() { b.addAll(&findings, evalControlPlaneSkew(inv, k, target)) },
		func() { b.addAll(&findings, evalKubeProxyKubeletSkew(inv, k)) },
		func() { b.addAll(&findings, evalKBStale(inv, k, target)) },
		func() { b.addAll(&findings, evalUpgradePath(inv, target)) },
		func() { b.addAll(&findings, supportFindings) },
		func() { findings = append(findings, evalCRDVersions(inv, target, b)...) },
	}
	for _, step := range steps {
		if b.exceeded() {
			return Report{}, ErrReportTooLarge
		}
		step()
	}
	sortFindings(findings)
	score, _ := Score(findings)
	gaps := assessmentGaps(inv, k, target)
	verdict := verdictFor(findings, gaps)
	// Inventories from other collectors may arrive unsorted or over the cap.
	unrecognized := sortedSet(slices.Clone(inv.UnrecognizedImages))
	omitted := inv.UnrecognizedImagesOmitted
	if n := len(unrecognized) - inventory.MaxUnrecognizedImages; n > 0 {
		unrecognized, omitted = unrecognized[:inventory.MaxUnrecognizedImages], omitted+n
	}
	for i := range gaps {
		b.charge(gapSize(&gaps[i]))
	}
	for _, img := range unrecognized {
		b.charge(len(img) + 3)
	}
	if b.exceeded() {
		return Report{}, ErrReportTooLarge
	}

	return Report{
		ClusterID:     inv.ClusterID,
		Target:        target,
		ServerVersion: inv.ServerVersion,
		KBVersion:     k.Version,
		Score:         score,
		Ready:         verdict == VerdictReady,
		Verdict:       verdict,
		Findings:      findings,
		NotAssessed:   gaps,
		Support:       support,

		UnrecognizedImages:        unrecognized,
		UnrecognizedImagesOmitted: omitted,
		AddOnEvidenceAgeSeconds:   max(0, inv.AddOnEvidenceAgeSeconds),
	}, nil
}
