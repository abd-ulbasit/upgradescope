package inventory

import (
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
)

// Conform repairs an inventory a collector built so that Admit does not
// refuse it, and records every repair in the status of the capability it
// cost data: the capability is marked Partial, Skipped names what was not
// recorded, and Reason says how much and why. A capability the
// collector never reported stays absent (the server reads that as a gap of
// its own); the repair is in the notes only. A collector reads identifiers
// and strings from objects the apiserver validated, and so Admit holds the
// server to them; but a Helm release's name is a label value, the objects
// in its stored manifest are text inside a payload anyone who can create a
// Secret can plant, and an image string or a chart's kubeVersion has no
// limit of its own. One such object must not make the server refuse
// (422) every push of a cluster for ever, as the agent would offer the same
// inventory each tick: the entry that is not valid is left out, and said to
// be, and the rest is sent.
//
// It repairs, in order:
//
//   - the free text CutFreeText cuts;
//   - a Helm release whose name or namespace is not an identifier, or any of
//     whose strings is over MaxStringBytes (its chart's name, version,
//     appVersion or kubeVersion, the last of which a cut would turn into a
//     narrower constraint): the release is dropped, and named in helm's
//     Skipped by its namespace and name, cut where long;
//   - an object of an API usage entry (apiUsage, apiAuthorshipUnknown, a
//     release's manifestApis, a CRD's usage) whose namespace or name is not
//     an identifier, or whose strings are over the limit: the object is
//     dropped and counted in objectsOmitted, and a namespace key that is not
//     a namespace name is dropped from the entry's namespaces;
//   - an add-on install any of whose strings (id, version, chartVersion,
//     source) is over MaxStringBytes: dropped, in one pass, and addons names
//     SkippedPods, as an install that is gone may be what blocks an upgrade.
//     An image tag is a pod creator's, so a version can be 17 KiB of digits
//     and pods in any number of namespaces make as many such installs; the
//     one-at-a-time net below could not keep up;
//   - a volume plugin entry (volumePlugins): its objects and namespace keys
//     as an API usage entry's; an entry whose plugin is not a field name,
//     or whose counts are negative, is dropped (dropElement), and the
//     entries beyond MaxVolumePlugins are dropped, each named in volumes'
//     Skipped by its plugin, quoted and cut where it is not a field name;
//   - unrecognizedImages over MaxStringBytes, or beyond MaxUnrecognizedImages:
//     dropped and counted in unrecognizedImagesOmitted;
//   - whatever else Admit's identifier and limit checks still refuse, one
//     element of its list at a time (see dropElement): an element it cannot
//     drop is returned as the error, as Admit would.
//
// It returns what it did, one line per kind of repair and capability, for a
// log; err is the first thing Admit still refuses after the repairs, which
// it does not repair (the schema versions, the server version, the
// capabilities), or nil: an inventory it returns no error for passes Admit.
func (inv *Inventory) Conform() (notes []string, err error) {
	inv.CutFreeText()
	r := &repairs{}
	inv.conformHelm(r)
	conformUsages(inv.APIUsage, CapAPIUsage, r)
	conformUsages(inv.APIAuthorshipUnknown, CapAPIUsage, r)
	for i := range inv.CRDs {
		conformUsages(inv.CRDs[i].Usage, CapCRDs, r)
	}
	inv.conformAddOns(r)
	inv.conformImages(r)
	inv.conformVolumes(r)
	// What is left is not a shape a collector is known to produce: drop the
	// element Admit names, until nothing is, or one cannot be dropped. Each
	// drop validates the whole inventory again, so the cost is the number
	// of drops times its size: at most maxFallbackDrops, and an inventory
	// that still has more to drop is returned with Admit's error, as one
	// this repair does not know how to make admissible.
	for drops := 0; ; drops++ {
		err = inv.checkAdmissible()
		if err == nil || drops == maxFallbackDrops || !inv.dropElement(err, r) {
			break
		}
	}
	notes = r.apply(inv)
	inv.CutFreeText() // the notes joined the reasons
	if err == nil {
		err = inv.checkAdmissible()
	}
	return notes, err
}

// checkAdmissible is Admit without the checks Conform does not repair
// (the schema versions, the server version) and without CutFreeText.
func (inv Inventory) checkAdmissible() error {
	if err := inv.ValidateIdentifiers(); err != nil {
		return err
	}
	return inv.ValidateLimits()
}

// maxFallbackDrops is the most elements Conform drops one at a time (see
// dropElement), each after validating the whole inventory again. The
// targeted repairs handle every shape a collector is known to produce in
// bulk; this is the net under them. It is not free: BenchmarkConformFallback
// measures an inventory of 5000 nodes, 3000 releases and 20k named API
// objects (a large cluster) at ~0.1 s for one validation and ~2.4 s for the
// cap's worth of drops (Apple M1 Pro), paid once a tick and only for a shape
// the targeted repairs above do not know.
const maxFallbackDrops = 256

// maxNamedSkips is the most Skipped entries Conform adds to one capability:
// a cluster full of invalid releases must not grow its reports without
// bound. The rest are counted in the note.
const maxNamedSkips = 20

// repairs collects what Conform did, per capability.
type repairs struct {
	byCap   map[Capability]*capRepairs
	ordered []Capability
}

type capRepairs struct {
	skipped []string
	named   map[string]bool
	notes   []string // distinct, in the order first made
	counts  map[string]int
}

func (r *repairs) of(c Capability) *capRepairs {
	if r.byCap == nil {
		r.byCap = map[Capability]*capRepairs{}
	}
	cr := r.byCap[c]
	if cr == nil {
		cr = &capRepairs{named: map[string]bool{}, counts: map[string]int{}}
		r.byCap[c] = cr
		r.ordered = append(r.ordered, c)
	}
	return cr
}

// add records one repair of kind note (what was dropped, why: it names no
// identifier) in capability c, naming skipped in its Skipped when not "".
func (r *repairs) add(c Capability, note, skipped string) {
	cr := r.of(c)
	if cr.counts[note] == 0 {
		cr.notes = append(cr.notes, note)
	}
	cr.counts[note]++
	if skipped != "" {
		r.skip(c, skipped)
	}
}

// skip names what in capability c's Skipped.
func (r *repairs) skip(c Capability, what string) {
	if cr := r.of(c); !cr.named[what] {
		cr.named[what] = true
		cr.skipped = append(cr.skipped, what)
	}
}

// apply writes the repairs into inv's capability statuses and returns one
// line per kind and capability.
func (r *repairs) apply(inv *Inventory) (lines []string) {
	for _, c := range r.ordered {
		cr := r.byCap[c]
		var parts []string
		for _, n := range cr.notes {
			parts = append(parts, fmt.Sprintf("%d %s", cr.counts[n], n))
		}
		skipped := slices.Sorted(slices.Values(cr.skipped))
		if len(skipped) > maxNamedSkips {
			parts = append(parts, fmt.Sprintf("%d more not named", len(skipped)-maxNamedSkips))
			skipped = skipped[:maxNamedSkips]
		}
		note := "not recorded, so not assessed: " + strings.Join(parts, "; ")
		lines = append(lines, string(c)+": "+note)
		if inv.Capabilities == nil {
			inv.Capabilities = map[Capability]CapabilityStatus{}
		}
		st, ok := inv.Capabilities[c]
		if !ok || !st.Available {
			// Not reported, or already not assessed at all: a capability
			// the collector never set is not made available here, which
			// would claim a read it did not make. The server treats an
			// absent one as a required gap of its own.
			continue
		}
		st.Partial = true
		if st.Reason != "" {
			st.Reason += "; "
		}
		st.Reason += note
		st.Skipped = slices.Compact(slices.Sorted(slices.Values(append(slices.Clone(st.Skipped), skipped...))))
		inv.Capabilities[c] = st
	}
	return lines
}

// conformHelm drops the Helm releases Admit would refuse and repairs the
// manifest objects of the others.
func (inv *Inventory) conformHelm(r *repairs) {
	kept := inv.HelmReleases[:0]
	for _, rel := range inv.HelmReleases {
		if why := releaseProblem(rel); why != "" {
			r.add(CapHelm, "Helm release(s) dropped for "+why, releaseLabel(rel.Namespace, rel.Name))
			continue
		}
		before := r.total(CapHelm)
		conformUsages(rel.ManifestAPIs, CapHelm, r)
		if r.total(CapHelm) != before {
			// Objects of its manifest were left out: it is not assessed whole.
			r.skip(CapHelm, rel.Namespace+"/"+rel.Name)
		}
		kept = append(kept, rel)
	}
	clear(inv.HelmReleases[len(kept):]) // let the dropped ones go
	inv.HelmReleases = kept
	if len(inv.HelmReleases) == 0 {
		inv.HelmReleases = nil
	}
}

// total is the number of repairs made so far in capability c.
func (r *repairs) total(c Capability) int {
	n := 0
	if cr := r.byCap[c]; cr != nil {
		for _, k := range cr.counts {
			n += k
		}
	}
	return n
}

// releaseProblem says why a Helm release would be refused, or "".
func releaseProblem(rel HelmRelease) string {
	switch {
	case problemsWith(rel.Name, subdomainProblems) != nil:
		return "a name that is not an RFC 1123 subdomain"
	case problemsWith(rel.Namespace, namespaceProblems) != nil:
		return "a namespace that is not a namespace name"
	}
	for _, s := range []string{rel.ChartName, rel.ChartVersion, rel.AppVersion, rel.KubeVersion, rel.Status} {
		if len(s) > MaxStringBytes {
			return fmt.Sprintf("chart metadata over %d bytes", MaxStringBytes)
		}
	}
	return ""
}

// releaseLabel names a release in Skipped as "namespace/name". An
// identifier that is not valid, and so may be anything, is quoted and cut.
func releaseLabel(ns, name string) string {
	return shortIdentifier(ns, namespaceProblems) + "/" + shortIdentifier(name, subdomainProblems)
}

// shortIdentifier is s as it stands when it is a valid identifier, else
// quoted and cut to 40 bytes: what is shown of a value that may be megabytes
// of anything.
func shortIdentifier(s string, problems func(string) []string) string {
	if problemsWith(s, problems) == nil {
		return s
	}
	const show = 40
	q := strconv.Quote(utf8Prefix(s, show))
	if len(s) > show {
		q = q[:len(q)-1] + "…\""
	}
	return q
}

// conformUsages repairs the objects of each entry of us in place, recording
// what it drops in capability c.
func conformUsages(us []APIUsage, c Capability, r *repairs) {
	for i := range us {
		u := &us[i]
		u.Objects = conformRefs(u.Namespaces, u.Objects, &u.ObjectsOmitted, c, "API usage", r)
	}
}

// conformRefs drops the namespace keys of counts that are not namespace
// names, and the objects Admit would refuse or beyond MaxObjectRefs,
// counting those in *omitted, recording each kind of repair in capability c
// as what ("API usage", "volume plugin use"). It returns the
// objects kept, sharing objs's array.
func conformRefs(counts map[string]int, objs []ObjectRef, omitted *int, c Capability, what string, r *repairs) []ObjectRef {
	for ns := range counts {
		if problemsWith(ns, namespaceProblems) != nil || len(ns) > MaxStringBytes {
			delete(counts, ns)
			r.add(c, "namespace key(s) of "+article(what)+" "+what+" count dropped for not being a namespace name", "")
		}
	}
	kept := objs[:0]
	for _, o := range objs {
		if why := objectProblem(o); why != "" {
			*omitted++
			r.add(c, "object(s) of "+what+" dropped for "+why, "")
			continue
		}
		kept = append(kept, o)
	}
	clear(objs[len(kept):])
	if len(kept) == 0 {
		kept = nil
	}
	if n := len(kept) - MaxObjectRefs; n > 0 {
		clear(kept[MaxObjectRefs:])
		kept, *omitted = kept[:MaxObjectRefs], *omitted+n
		r.add(c, "object(s) of "+what+" beyond the cap counted, not listed", "")
	}
	return kept
}

// objectProblem says why an object ref would be refused, or "".
func objectProblem(o ObjectRef) string {
	switch {
	case problemsWith(o.Namespace, namespaceProblems) != nil:
		return "a namespace that is not a namespace name"
	case problemsWith(o.Name, objectNameProblems) != nil:
		return "a name that is not an object name"
	}
	if p, _ := managerProblem(o.Manager); p != "" {
		return "a field manager the apiserver would not accept"
	}
	for _, s := range []string{o.File, o.Ignore, o.IgnoreReason, o.RenderedFrom} {
		if len(s) > MaxStringBytes {
			return fmt.Sprintf("a string over %d bytes", MaxStringBytes)
		}
	}
	return ""
}

// conformAddOns drops, in one pass, the add-on installs with a string over
// the limit. An install that is gone may be what blocks an upgrade, so the
// addons capability names SkippedPods (the engine then reads its gap as
// required), as dropElement does for the one it drops.
func (inv *Inventory) conformAddOns(r *repairs) {
	kept := inv.AddOns[:0]
	for _, a := range inv.AddOns {
		if len(a.ID) > MaxStringBytes || len(a.Version) > MaxStringBytes ||
			len(a.ChartVersion) > MaxStringBytes || len(a.Source) > MaxStringBytes {
			r.add(CapAddOns, fmt.Sprintf("add-on install(s) dropped for a string over %d bytes", MaxStringBytes), SkippedPods)
			continue
		}
		kept = append(kept, a)
	}
	clear(inv.AddOns[len(kept):])
	inv.AddOns = kept
	if len(inv.AddOns) == 0 {
		inv.AddOns = nil
	}
}

// conformImages drops the unrecognized image repositories that are over the
// limit, and those beyond the cap.
func (inv *Inventory) conformImages(r *repairs) {
	kept := inv.UnrecognizedImages[:0]
	for _, img := range inv.UnrecognizedImages {
		if len(img) > MaxStringBytes {
			inv.UnrecognizedImagesOmitted++
			r.add(CapAddOns, fmt.Sprintf("image repositor(ies) over %d bytes counted in unrecognizedImagesOmitted, not listed", MaxStringBytes), "")
			continue
		}
		kept = append(kept, img)
	}
	clear(inv.UnrecognizedImages[len(kept):])
	inv.UnrecognizedImages = kept
	if n := len(inv.UnrecognizedImages) - MaxUnrecognizedImages; n > 0 {
		inv.UnrecognizedImages = inv.UnrecognizedImages[:MaxUnrecognizedImages]
		inv.UnrecognizedImagesOmitted += n
		r.add(CapAddOns, "image repositor(ies) beyond the cap counted in unrecognizedImagesOmitted, not listed", "")
	}
	if len(inv.UnrecognizedImages) == 0 {
		inv.UnrecognizedImages = nil
	}
}

// dropElement drops the element of a list that err, an *IdentifierError or
// *LimitError from checkAdmissible, names, and reports whether it could.
// Only the lists a collector fills from objects are dropped from, an
// element at a time; a problem anywhere else (the provider apart, which
// becomes undetermined) is not repaired.
func (inv *Inventory) dropElement(err error, r *repairs) bool {
	var field string
	var ie *IdentifierError
	var le *LimitError
	switch {
	case errors.As(err, &ie):
		field = ie.Field
	case errors.As(err, &le):
		field = le.Field
	default:
		return false
	}
	if field == "provider" {
		inv.Provider = ""
		r.add(CapVersions, "provider name dropped for not being a provider name", "")
		return true
	}
	list, i, ok := elementOf(field)
	if !ok {
		return false
	}
	switch list {
	case "helmReleases":
		if i >= len(inv.HelmReleases) {
			return false
		}
		rel := inv.HelmReleases[i]
		r.add(CapHelm, "Helm release(s) dropped for a value Admit refuses", releaseLabel(rel.Namespace, rel.Name))
		inv.HelmReleases = slices.Delete(inv.HelmReleases, i, i+1)
	case "gitopsCharts":
		if i >= len(inv.GitOpsCharts) {
			return false
		}
		tool := inv.GitOpsCharts[i].Tool
		if tool != GitOpsArgoCD && tool != GitOpsFlux {
			tool = ""
		}
		r.add(CapHelm, "GitOps chart source(s) dropped for a value Admit refuses", tool)
		inv.GitOpsCharts = slices.Delete(inv.GitOpsCharts, i, i+1)
	case "addOns":
		if i >= len(inv.AddOns) {
			return false
		}
		// An add-on install that is gone may be what blocks an upgrade:
		// name what the addons capability names when its evidence is
		// incomplete in that way (the engine then reads it as required).
		r.add(CapAddOns, "add-on install(s) dropped for a value Admit refuses", SkippedPods)
		inv.AddOns = slices.Delete(inv.AddOns, i, i+1)
	case "apiUsage":
		if i >= len(inv.APIUsage) {
			return false
		}
		u := inv.APIUsage[i]
		r.add(CapAPIUsage, "API usage entr(ies) dropped for a value Admit refuses", usageLabel(u))
		inv.APIUsage = slices.Delete(inv.APIUsage, i, i+1)
	case "apiAuthorshipUnknown":
		if i >= len(inv.APIAuthorshipUnknown) {
			return false
		}
		u := inv.APIAuthorshipUnknown[i]
		r.add(CapAPIUsage, "API usage entr(ies) dropped for a value Admit refuses", usageLabel(u))
		inv.APIAuthorshipUnknown = slices.Delete(inv.APIAuthorshipUnknown, i, i+1)
	case "crds":
		if i >= len(inv.CRDs) {
			return false
		}
		r.add(CapCRDs, "CRD(s) dropped for a value Admit refuses", "")
		inv.CRDs = slices.Delete(inv.CRDs, i, i+1)
	case "deprecatedCalls":
		if i >= len(inv.DeprecatedCalls) {
			return false
		}
		r.add(CapDeprecatedCalls, "deprecated call row(s) dropped for a value Admit refuses", "")
		inv.DeprecatedCalls = slices.Delete(inv.DeprecatedCalls, i, i+1)
	case "nodes":
		if i >= len(inv.Nodes) {
			return false
		}
		r.add(CapVersions, "node(s) dropped for a value Admit refuses", "nodes")
		inv.Nodes = slices.Delete(inv.Nodes, i, i+1)
	case "controlPlane":
		if i >= len(inv.ControlPlane) {
			return false
		}
		r.add(CapVersions, "control-plane component(s) dropped for a value Admit refuses", "")
		inv.ControlPlane = slices.Delete(inv.ControlPlane, i, i+1)
	case "namespaces":
		if i >= len(inv.Namespaces) {
			return false
		}
		r.add(CapVersions, "namespace(s) dropped for a value Admit refuses", "")
		inv.Namespaces = slices.Delete(inv.Namespaces, i, i+1)
	case "volumePlugins":
		if i >= len(inv.VolumePlugins) {
			return false
		}
		r.add(CapVolumes, "volume plugin entr(ies) dropped for a value Admit refuses", volumeLabel(inv.VolumePlugins[i]))
		inv.VolumePlugins = slices.Delete(inv.VolumePlugins, i, i+1)
	case "unrecognizedImages":
		if i >= len(inv.UnrecognizedImages) {
			return false
		}
		r.add(CapAddOns, "image repositor(ies) dropped for a value Admit refuses", "")
		inv.UnrecognizedImages = slices.Delete(inv.UnrecognizedImages, i, i+1)
		inv.UnrecognizedImagesOmitted++
	default:
		return false
	}
	return true
}

// usageLabel names an API usage entry in api-usage's Skipped:
// "group/version Kind", core "v1 Kind", cut where long.
func usageLabel(u APIUsage) string {
	return utf8Prefix(gvString(u.Group, u.Version), 96) + " " + utf8Prefix(u.Kind, 64)
}

// elementOf splits the JSON path an error names, "helmReleases[3].name",
// into its top-level list and index.
func elementOf(field string) (list string, i int, ok bool) {
	list, rest, found := strings.Cut(field, "[")
	if !found {
		return "", 0, false
	}
	idx, _, found := strings.Cut(rest, "]")
	if !found {
		return "", 0, false
	}
	n, err := strconv.Atoi(idx)
	if err != nil || n < 0 {
		return "", 0, false
	}
	return list, n, true
}
