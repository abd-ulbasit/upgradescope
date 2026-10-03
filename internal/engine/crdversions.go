package engine

import (
	"fmt"
	"slices"
	"strings"

	"github.com/abd-ulbasit/upgradescope/internal/inventory"
)

const (
	// crdVersioningURL documents CRD versions: deprecated, served, and
	// status.storedVersions.
	crdVersioningURL = "https://kubernetes.io/docs/tasks/extend-kubernetes/custom-resources/custom-resource-definition-versioning/"
	// storageMigrationURL documents moving stored objects to the storage
	// version and pruning status.storedVersions.
	storageMigrationURL = crdVersioningURL + "#upgrade-existing-objects-to-a-new-stored-version"
)

// evalCRDVersions judges each CRD's own versions, which the add-on that
// ships the CRD sets, whatever Kubernetes runs:
//   - custom resources at a version the CRD does not serve (served: false,
//     or missing from spec.versions) → blocker: the apiserver rejects
//     them;
//   - a served version marked deprecated: true → warning while custom
//     resources use it, info otherwise; the CRD's deprecationWarning is
//     quoted;
//   - a status.storedVersions entry, other than the storage version, that
//     the CRD does not serve → warning: the CRD update that drops it is
//     rejected until stored objects are migrated and the entry removed.
//
// "Use" is CRD.Usage: live objects some manager still writes through the
// version, manifest objects at it. target only appears in the text.
func evalCRDVersions(inv inventory.Inventory, target inventory.Version, b *budget) []Finding {
	unchecked := map[string]bool{}
	for _, s := range inv.Capabilities[inventory.CapCRDs].Skipped {
		unchecked[s] = true
	}
	independent := fmt.Sprintf(" CRD versions are set by the add-on that ships the CRD, independent of Kubernetes %s.", target)
	var out []Finding
	for _, c := range inv.CRDs {
		name := c.Plural + "." + c.Group
		listed := map[string]inventory.CRDVersion{}
		var served []string
		storage := ""
		for _, v := range c.Versions {
			listed[v.Name] = v
			if v.Served {
				served = append(served, v.Name)
			}
			if v.Storage {
				storage = v.Name
			}
		}
		servedText := "no version"
		if len(served) > 0 {
			servedText = strings.Join(served, ", ")
		}
		used := map[string]inventory.APIUsage{}
		for _, u := range c.Usage {
			if u.Count > 0 {
				used[u.Version] = u
			}
		}
		to := c.PreferredVersion()

		for _, v := range c.Versions {
			if !v.Served || !v.Deprecated {
				continue
			}
			gv := gvString(c.Group, v.Name)
			f := Finding{
				Category:  CatCRDVersion,
				Key:       string(CatCRDVersion) + "/deprecated/" + apiKey(c.Group, v.Name, c.Kind),
				Severity:  SevInfo,
				Title:     fmt.Sprintf("%s %s is a deprecated CRD version", gv, c.Kind),
				Citations: []string{crdVersioningURL + "#version-deprecation"},
			}
			u, inUse := used[v.Name]
			switch {
			case inUse:
				f.Severity = SevWarning
				f.Title += fmt.Sprintf(" (%s)", pluralObjects(u.Count))
				f.Detail = crdUsage(&f, u, inv.Namespaces)
			case unchecked[gv+" "+c.Kind]:
				f.Detail = "Custom resources written through this version were not checked (see the crds gap)."
			default:
				f.Detail = "No custom resource was found written through this version."
			}
			f.Detail += fmt.Sprintf(" CRD %s marks %s deprecated.", name, v.Name)
			if w := strings.TrimSpace(v.DeprecationWarning); w != "" {
				f.Detail += fmt.Sprintf(" The CRD says: %q.", strings.TrimSuffix(w, "."))
			}
			if v.Name != storage && slices.Contains(c.StoredVersions, v.Name) {
				f.Detail += fmt.Sprintf(" Objects may still be stored at %s (status.storedVersions).", v.Name)
			}
			f.Detail += independent
			switch {
			case to == "":
				f.Remediation = fmt.Sprintf("check the add-on's upgrade notes for the version that replaces %s: the CRD serves none that is not deprecated", v.Name)
			case inUse:
				f.Remediation = fmt.Sprintf("move these objects to %s before an add-on upgrade stops serving %s", gvString(c.Group, to), v.Name)
			default:
				f.Remediation = fmt.Sprintf("use %s; an add-on upgrade may stop serving %s", gvString(c.Group, to), v.Name)
			}
			if !b.add(&out, f) {
				return out
			}
		}

		for _, u := range c.Usage {
			v, ok := listed[u.Version]
			if u.Count == 0 || ok && v.Served {
				continue
			}
			gv := gvString(c.Group, u.Version)
			f := Finding{
				Category:  CatCRDVersion,
				Severity:  SevBlocker,
				Key:       string(CatCRDVersion) + "/unserved/" + apiKey(c.Group, u.Version, c.Kind),
				Title:     fmt.Sprintf("%s %s is not served by its CRD (%s)", gv, c.Kind, pluralObjects(u.Count)),
				Citations: []string{crdVersioningURL},
			}
			f.Detail = crdUsage(&f, u, inv.Namespaces)
			if ok {
				f.Detail += fmt.Sprintf(" CRD %s marks %s served: false (it serves %s), so the apiserver rejects these objects at %s.", name, u.Version, servedText, u.Version)
			} else {
				f.Detail += fmt.Sprintf(" CRD %s does not list %s (it serves %s), so the apiserver rejects these objects at %s.", name, u.Version, servedText, u.Version)
			}
			f.Detail += independent
			if to != "" {
				f.Remediation = fmt.Sprintf("move these objects to %s; the apiserver rejects writes at %s", gvString(c.Group, to), u.Version)
			} else {
				f.Remediation = fmt.Sprintf("move these objects to a version the CRD serves; the apiserver rejects writes at %s", u.Version)
			}
			if len(objectManagers(u.Objects)) > 0 {
				// Live objects count by managedFields authorship: an
				// entry a manager left before it stopped writing at the
				// version stays until something replaces it.
				f.Remediation += ". A named manager that no longer writes them keeps them listed through its managedFields entry until another manager takes over its fields or the entry is removed"
			}
			if !b.add(&out, f) {
				return out
			}
		}

		for _, s := range c.StoredVersions {
			if v, ok := listed[s]; s == storage || ok && v.Served {
				continue
			}
			f := Finding{
				Category: CatCRDVersion,
				Severity: SevWarning,
				Key:      string(CatCRDVersion) + "/stored-unserved/" + apiKey(c.Group, s, c.Kind),
				Title:    fmt.Sprintf("CRD %s lists unserved version %s in status.storedVersions", name, s),
				Detail: fmt.Sprintf("Objects may still be persisted at %s, which CRD %s no longer serves (storage version: %s). A CRD update that drops %s from spec.versions is rejected while status.storedVersions lists it, so the add-on upgrade that drops it fails.",
					s, name, storage, s) + independent,
				Remediation: fmt.Sprintf("migrate every stored %s to the storage version %s (kube-storage-version-migrator, the add-on's own tool such as cert-manager's cmctl upgrade migrate-api-version, or a no-op update of each object), then remove %s from status.storedVersions (kubectl patch crd %s --subresource=status)",
					c.Kind, storage, s, name),
				Citations: []string{storageMigrationURL},
			}
			if !b.add(&out, f) {
				return out
			}
		}
	}
	return out
}

// crdUsage fills f's teams, namespaces and objects from u and returns the
// detail sentences on who uses the version, worded as evalAPIUsage words
// them: manifest objects (refs with a line), or live objects and the
// managers writing them.
func crdUsage(f *Finding, u inventory.APIUsage, nsInfo []inventory.NamespaceInfo) string {
	manifests := len(u.Objects) > 0 && u.Objects[0].Line > 0
	emptyNS, detail := "cluster-scoped", "%d object(s) written through this version"
	if manifests {
		emptyNS, detail = "namespace unset", "%d manifest object(s) use this version"
	}
	nsDetail, nsNames := namespaceBreakdown(u.Namespaces, emptyNS)
	f.Teams, f.Namespaces = teamsFor(nsNames, nsInfo), nsNames
	f.Objects, f.ObjectsOmitted = sortedObjects(u.Objects), u.ObjectsOmitted
	if nsDetail == "" {
		detail = fmt.Sprintf(detail+".", u.Count)
	} else {
		detail = fmt.Sprintf(detail+": %s.", u.Count, nsDetail)
	}
	return detail + writtenBy(u)
}
