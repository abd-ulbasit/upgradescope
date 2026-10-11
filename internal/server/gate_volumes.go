package server

import (
	"cmp"
	"maps"
	"slices"

	"github.com/abd-ulbasit/upgradescope/internal/inventory"
)

// mergeVolumePlugins is the proposed state's in-tree volume plugins
// (#351): the cluster's (pods, by namespace) and the posted manifests'
// (objects, located), summed per plugin. A row lists the cluster's refs
// first and then every one of the manifests', which can be more than
// inventory.MaxObjectRefs, as upsertUsage does: the engine and suppression
// see them all, and capObjects cuts the answer after suppression, the
// manifests' refs kept, so a cluster whose row already lists the cap's
// worth of objects does not push the PR's out of the finding, of SARIF
// and of the code-quality report. A manifest that updates a workload the
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
		m.Objects = append(slices.Clip(m.Objects), u.Objects...)
		m.ObjectsOmitted = satAdd(m.ObjectsOmitted, u.ObjectsOmitted)
		byPlugin[u.Plugin] = m
	}
	return slices.SortedFunc(maps.Values(byPlugin), func(a, b inventory.VolumePluginUse) int { return cmp.Compare(a.Plugin, b.Plugin) })
}
