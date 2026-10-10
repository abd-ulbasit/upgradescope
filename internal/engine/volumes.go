package engine

import (
	"fmt"
	"strings"

	"github.com/abd-ulbasit/upgradescope/internal/inventory"
	"github.com/abd-ulbasit/upgradescope/internal/kb"
)

// CatVolumePlugin: an in-tree volume plugin that pods, pod templates or
// PersistentVolumes name (#351), judged by the knowledge base's
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
// pods, manifest counts objects (workloads, pods, PersistentVolumes).
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
		listed := storedObjects
		if inv.Source == inventory.SourceFiles || len(u.Objects) > 0 {
			listed = manifestObjects
		}
		count := pluralObjects(u.Count)
		if listed == storedObjects {
			count = strings.Replace(count, "object", "pod", 1)
		}
		f := Finding{Category: CatVolumePlugin, Key: string(CatVolumePlugin) + "/" + u.Plugin,
			Remediation: p.Replacement, Citations: p.Citations,
			Objects: sortedObjects(u.Objects), ObjectsOmitted: u.ObjectsOmitted}
		var consequence string
		switch p.Classification {
		case kb.VolumeRemoved:
			f.Title = fmt.Sprintf("In-tree volume plugin %s removed in %s (%s)", u.Plugin, p.Removed, count)
			consequence = fmt.Sprintf("From %s a pod that names it does not start.", p.Removed)
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
		f.Detail = fmt.Sprintf("%s %s it, in: %s. %s", count, verb, where, consequence)
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

// deprecatedWhen says when a plugin was deprecated relative to target:
// "deprecated since 1.28" at or after it, "deprecated in 1.28" before.
func deprecatedWhen(deprecated, target inventory.Version) string {
	if deprecated.Compare(target) > 0 {
		return "deprecated in " + deprecated.String()
	}
	return "deprecated since " + deprecated.String()
}
