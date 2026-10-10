package server

import (
	"cmp"
	"maps"
	"slices"

	"github.com/abd-ulbasit/upgradescope/internal/inventory"
)

// mergeVolumePlugins is the proposed state's in-tree volume plugins
// (#351): the cluster's (pods, by namespace) and the posted manifests'
// (objects, located), summed per plugin, the manifests' objects listed up
// to inventory.MaxObjectRefs. A manifest that updates a workload the
// cluster already runs is counted twice; the count is evidence, and the
// severity of a plugin's finding does not depend on it. Neither input is
// modified. Every sum saturates at math.MaxInt (satAdd), so a cluster's
// count there is never wrapped negative and its finding kept (#361).
func mergeVolumePlugins(cluster, manifests []inventory.VolumePluginUse) []inventory.VolumePluginUse {
	if len(manifests) == 0 {
		return cluster
	}
	byPlugin := map[string]inventory.VolumePluginUse{}
	for _, u := range slices.Concat(cluster, manifests) {
		m, ok := byPlugin[u.Plugin]
		if !ok {
			m = inventory.VolumePluginUse{Plugin: u.Plugin, Namespaces: map[string]int{}}
		}
		m.Count = satAdd(m.Count, u.Count)
		for ns, n := range u.Namespaces {
			m.Namespaces[ns] = satAdd(m.Namespaces[ns], n)
		}
		room := max(0, inventory.MaxObjectRefs-len(m.Objects))
		m.Objects = append(slices.Clip(m.Objects), u.Objects[:min(room, len(u.Objects))]...)
		m.ObjectsOmitted = satAdd(m.ObjectsOmitted, satAdd(u.ObjectsOmitted, max(0, len(u.Objects)-room)))
		byPlugin[u.Plugin] = m
	}
	return slices.SortedFunc(maps.Values(byPlugin), func(a, b inventory.VolumePluginUse) int { return cmp.Compare(a.Plugin, b.Plugin) })
}
