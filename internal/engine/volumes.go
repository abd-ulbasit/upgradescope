package engine

import (
	"fmt"
	"slices"
	"strings"

	"github.com/abd-ulbasit/upgradescope/internal/inventory"
	"github.com/abd-ulbasit/upgradescope/internal/kb"
)

// CatVolumePlugin: an in-tree volume plugin that pods, pod templates or
// PersistentVolumes name (#351), or StorageClasses name the in-tree
// provisioner of (#362), judged by the knowledge base's
// kb.VolumePlugins: removed with no migration path (blocker at or after
// the removal, warning the minor before it, info earlier), served only
// through a named CSI driver (warning at or after the minor the in-tree
// code went, info earlier; never a blocker, since a cluster with the
// driver is fine), or deprecated only (info).
const CatVolumePlugin Category = "volume-plugin"

// volumesNotReported is the volumes gap of an inventory whose collector
// predates the capability: an older agent's, or a files inventory an older
// CLI saved. Its pods are not assessed, never read as clean.
const volumesNotReported = "not reported by the collector, which predates in-tree volume plugin checks; upgrade it to assess volume plugins"

// volumesGap is the gap of an inventory that does not report the volumes
// capability at all; the capability is optional, so it is never required.
func volumesGap(inv inventory.Inventory) []CapabilityGap {
	if _, ok := inv.Capabilities[inventory.CapVolumes]; ok {
		return nil
	}
	return []CapabilityGap{{Capability: inventory.CapVolumes, Reason: volumesNotReported}}
}

// evalVolumePlugins judges inv.VolumePlugins against k.VolumePlugins at
// target, one finding per plugin in use, keyed volume-plugin/<plugin>. A
// plugin the knowledge base does not list is not judged. Live counts are
// pods, and the PersistentVolumes and StorageClasses the row names;
// manifest counts objects (workloads, pods, PersistentVolumes,
// StorageClasses).
func evalVolumePlugins(inv inventory.Inventory, k kb.KB, target inventory.Version, b *budget) []Finding {
	byName := make(map[string]kb.VolumePlugin, len(k.VolumePlugins))
	for _, p := range k.VolumePlugins {
		byName[p.Plugin] = p
	}
	teams := newTeamLookup(inv.Namespaces)
	var out []Finding
	for _, u := range inv.VolumePlugins {
		p, ok := byName[u.Plugin]
		if !ok || u.Count <= 0 {
			continue
		}
		listed := volumeObjectsListed(inv.Source, u.Objects)
		named := len(u.Objects) > 0 || u.ObjectsOmitted > 0
		count := pluralObjects(u.Count)
		subject := count
		switch {
		case listed == storedObjects && !named:
			count = strings.Replace(count, "object", "pod", 1)
			subject = count
		case listed == storedObjects:
			// A cluster's row naming objects: its PersistentVolumes and
			// StorageClasses, beside the pods it counts.
			subject = fmt.Sprintf("%d pods, PersistentVolumes or StorageClasses", u.Count)
		}
		f := Finding{Category: CatVolumePlugin, Key: string(CatVolumePlugin) + "/" + u.Plugin,
			Remediation: p.Replacement, Citations: p.Citations,
			Objects: sortedObjects(u.Objects), ObjectsOmitted: u.ObjectsOmitted}
		var consequence string
		switch p.Classification {
		case kb.VolumeRemoved:
			f.Title = fmt.Sprintf("In-tree volume plugin %s removed in %s (%s)", u.Plugin, p.Removed, count)
			consequence = fmt.Sprintf("From %s a pod that names it, or that mounts a claim bound to a PersistentVolume that does, does not start.", p.Removed)
			if p.Provisioner != "" && named {
				consequence += fmt.Sprintf(" A StorageClass with provisioner %s has its new claims provisioned by the in-tree plugin, so from %s they are not provisioned.", p.Provisioner, p.Removed)
			}
			switch {
			case p.Removed.Compare(target) <= 0:
				f.Severity = SevBlocker
			case p.Removed.Compare(target.Next()) == 0:
				f.Severity = SevWarning
			default:
				f.Severity = SevInfo
				if p.Deprecated != nil {
					f.Title = fmt.Sprintf("In-tree volume plugin %s %s, removed in %s (%s)", u.Plugin, deprecatedWhen(*p.Deprecated, target), p.Removed, count)
				}
			}
		case kb.VolumeCSIMigration:
			f.Title = fmt.Sprintf("In-tree volume plugin %s needs CSI driver %s from %s (%s)", u.Plugin, p.CSIDriver, p.Removed, count)
			consequence = fmt.Sprintf("From %s the %s CSI driver serves every operation on these volumes: a cluster without it installed cannot mount them, one with it is fine.", p.Removed, p.CSIDriver)
			if p.Provisioner != "" && named {
				consequence += fmt.Sprintf(" A StorageClass with provisioner %s has its new claims provisioned by the in-tree plugin, so from %s by that CSI driver.", p.Provisioner, p.Removed)
			}
			f.Severity = SevInfo
			if p.Removed.Compare(target) <= 0 {
				f.Severity = SevWarning
			}
		default: // kb.VolumeDeprecated
			f.Title = fmt.Sprintf("In-tree volume plugin %s %s (%s)", u.Plugin, deprecatedWhen(*p.Deprecated, target), count)
			consequence = "It still works; upstream has not scheduled its removal."
			f.Severity = SevInfo
		}
		where, names := namespaceBreakdown(u.Namespaces, listed.emptyNamespace())
		verb := "name"
		if u.Count == 1 {
			verb = "names"
		}
		f.Detail = fmt.Sprintf("%s %s it, in: %s. %s", subject, verb, where, consequence)
		if p.Note != "" {
			f.Detail += " Upstream: " + p.Note + "."
		}
		f.Namespaces, f.Teams = names, teams.teamsFor(names)
		if !b.add(&out, f) {
			break
		}
	}
	return out
}

// volumeObjectsListed says what a volume plugin row's objects are: a
// files inventory's are manifests'; a cluster's are its PersistentVolumes
// and StorageClasses (no line), where an empty namespace is a
// cluster-scoped object; and the gate's proposed state lists the
// cluster's before the manifests' (mergeVolumePlugins), which are
// manifests' alone when the first has a line, and mixed otherwise.
func volumeObjectsListed(src inventory.Source, refs []inventory.ObjectRef) objectsListed {
	switch {
	case src == inventory.SourceFiles:
		return manifestObjects
	case len(refs) == 0:
		return storedObjects
	case refs[0].Line > 0:
		return manifestObjects
	case slices.ContainsFunc(refs, func(r inventory.ObjectRef) bool { return r.Line > 0 }):
		return mixedObjects
	}
	return storedObjects
}

// deprecatedWhen says when a plugin was deprecated relative to target:
// "deprecated since 1.28" at or after it, "deprecated in 1.28" before.
func deprecatedWhen(deprecated, target inventory.Version) string {
	if deprecated.Compare(target) > 0 {
		return "deprecated in " + deprecated.String()
	}
	return "deprecated since " + deprecated.String()
}
