package server

import (
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/abd-ulbasit/upgradescope/internal/collect"
	"github.com/abd-ulbasit/upgradescope/internal/engine"
	"github.com/abd-ulbasit/upgradescope/internal/inventory"
)

// mergeManifests applies what the manifests deploy besides API usage
// (handleGate upserts that, see upsertUsage) to proposed, the cluster's
// inventory, and returns the manifests' side: the manifests with their
// custom resources judged against the proposed CRDs, whose findings are
// the ones the manifests introduce (introducedKeys). proposed's slices
// and maps are replaced, never modified, so the cluster's inventory stays
// as it is. #150.
//
// Add-ons: each install the manifests carry is upserted by add-on and
// namespaces (upsertAddOns). CRDs: a CRD in the manifests replaces the
// cluster's of its group and kind, as kubectl apply would, keeping the
// cluster's status.storedVersions, which a manifest cannot change. Every
// manifest custom resource is judged against the result, as files mode
// judges a render against its own CRDs (collect.AssessCRDs), and upserted
// into its CRD's Usage; the cluster's live usage stays where the version
// is still deprecated or unserved. Custom resources for which neither the
// cluster nor the manifests have a CRD make the crds capability partial.
// What the cluster has at a version a posted CRD newly deprecates or stops
// serving is not known (the cluster lists only objects at versions its
// CRDs flag), so it is not judged.
func mergeManifests(proposed *inventory.Inventory, manifests inventory.Inventory) inventory.Inventory {
	proposed.AddOns = upsertAddOns(proposed.AddOns, manifests.AddOns)
	proposed.VolumePlugins = mergeVolumePlugins(proposed.VolumePlugins, manifests.VolumePlugins)
	proposed.Capabilities = maps.Clone(proposed.Capabilities)

	type groupKind struct{ group, kind string }
	cluster := map[groupKind]inventory.CRD{}
	defs := make([]inventory.CRD, 0, len(proposed.CRDs)+len(manifests.CRDs))
	for _, c := range proposed.CRDs {
		cluster[groupKind{c.Group, c.Kind}] = c
	}
	for _, c := range slices.Concat(proposed.CRDs, manifests.CRDs) {
		c.Usage, c.StoredVersions = nil, nil
		defs = append(defs, c)
	}
	side := manifests
	side.Capabilities = maps.Clone(manifests.Capabilities)
	if side.Capabilities == nil {
		side.Capabilities = map[inventory.Capability]inventory.CapabilityStatus{}
	}
	collect.AssessCRDs(&side, defs)

	crds := make([]inventory.CRD, 0, len(side.CRDs))
	for _, c := range side.CRDs {
		p := c
		p.Usage = nil
		if old, ok := cluster[groupKind{c.Group, c.Kind}]; ok {
			p.StoredVersions = old.StoredVersions
			for _, u := range old.Usage {
				if !currentVersion(c, u.Version) {
					p.Usage = append(p.Usage, u)
				}
			}
		}
		p.Usage = upsertUsage(p.Usage, c.Usage)
		slices.SortStableFunc(p.Usage, func(a, b inventory.APIUsage) int { return strings.Compare(a.Version, b.Version) })
		crds = append(crds, p)
	}
	proposed.CRDs = crds

	if st, ok := proposed.Capabilities[inventory.CapCRDs]; ok && st.Available {
		if orphans := side.Capabilities[inventory.CapCRDs].Skipped; len(orphans) > 0 {
			reason := orphansReason(orphans)
			if st.Reason != "" {
				reason = st.Reason + "; " + reason
			}
			skipped := slices.Concat(st.Skipped, orphans)
			slices.Sort(skipped)
			st.Partial, st.Reason, st.Skipped = true, reason, slices.Compact(skipped)
			proposed.Capabilities[inventory.CapCRDs] = st
		}
	}
	return side
}

// currentVersion reports whether c serves version v without deprecating
// it, so custom resources at v are not a finding.
func currentVersion(c inventory.CRD, v string) bool {
	i := slices.IndexFunc(c.Versions, func(cv inventory.CRDVersion) bool { return cv.Name == v })
	return i >= 0 && c.Versions[i].Served && !c.Versions[i].Deprecated
}

// orphansReason is the crds gap's reason for custom resource APIs (sorted,
// "group/version Kind") that neither the cluster nor the manifests define.
func orphansReason(orphans []string) string {
	if len(orphans) == 1 {
		return fmt.Sprintf("no CRD in the cluster or the posted manifests for %s, so whether its version is served was not assessed", orphans[0])
	}
	listed, more := orphans[:min(len(orphans), 5)], ""
	if n := len(orphans) - len(listed); n > 0 {
		more = fmt.Sprintf(" and %d more", n)
	}
	return fmt.Sprintf("no CRD in the cluster or the posted manifests for %d custom resource APIs (%s%s), so whether their versions are served was not assessed",
		len(orphans), strings.Join(listed, ", "), more)
}

// upsertAddOns is the proposed state's add-ons: the cluster's, with each
// install from the manifests upserted by add-on and namespaces — it
// replaces the cluster's installs of that add-on in exactly those
// namespaces (the manifests are what will run there), or is added. Like
// upsertUsage, identity is exact: an install whose manifests leave the
// namespace unset does not replace the cluster's namespaced install.
func upsertAddOns(cluster, manifests []inventory.AddOnInstance) []inventory.AddOnInstance {
	same := func(a, b inventory.AddOnInstance) bool {
		return a.ID == b.ID && slices.Equal(slices.Sorted(slices.Values(a.Namespaces)), slices.Sorted(slices.Values(b.Namespaces)))
	}
	// A namespace can hold several installs of an add-on (a release and
	// an image on another release line): the manifests replace them all,
	// at the first one's place.
	out := make([]inventory.AddOnInstance, 0, len(cluster)+len(manifests))
	placed := make([]bool, len(manifests))
	for _, c := range cluster {
		replaced := false
		for i, m := range manifests {
			if !same(c, m) {
				continue
			}
			if !placed[i] {
				out, placed[i] = append(out, m), true
			}
			replaced = true
		}
		if !replaced {
			out = append(out, c)
		}
	}
	for i, m := range manifests {
		if !placed[i] {
			out = append(out, m)
		}
	}
	return out
}

// introducedKeys returns the keys of the findings the manifests' side
// (mergeManifests) produces from the manifests' own content, which the
// gate blames on them whatever the cluster already has (see gateResult):
// removed or deprecated APIs their objects use (usageKeys), the add-ons
// they deploy, and their custom resources at a version the proposed CRDs
// deprecate or do not serve (crd-version findings listing objects; one
// without, such as an unused deprecated version, is the CRD's).
func introducedKeys(rep engine.Report) map[string]bool {
	keys := usageKeys(rep)
	for _, f := range rep.Findings {
		switch f.Category {
		case engine.CatEOLAddon, engine.CatEOLApproaching, engine.CatChartIncompat, engine.CatAddOnNoData:
			keys[findingKey(f)] = true
		case engine.CatCRDVersion:
			if len(f.Objects)+f.ObjectsOmitted > 0 {
				keys[findingKey(f)] = true
			}
		}
	}
	return keys
}
