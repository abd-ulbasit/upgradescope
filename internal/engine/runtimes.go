package engine

import (
	"maps"
	"slices"
	"strings"

	"github.com/abd-ulbasit/upgradescope/internal/inventory"
	"github.com/abd-ulbasit/upgradescope/registry"
)

// evalUncoveredRuntimes discloses the node container runtimes no registry
// entry's runtimes matcher covers (cri-o and docker today, or containerd
// with a registry that lacks it): one info finding per runtime, keyed
// addon-no-data/<runtime> and naming its nodes like evalNodeRuntimes does,
// so a runtime whose end of life and Kubernetes compatibility were not
// assessed does not read as checked and fine (#169). A runtime field
// without "<runtime>://" names no runtime and is skipped. A KB without
// add-ons (a custom --kb of API lifecycle only) assesses no add-on at all,
// so runtimes are not singled out: nothing is disclosed.
func evalUncoveredRuntimes(inv inventory.Inventory, addons []registry.AddOn) []Finding {
	if len(addons) == 0 {
		return nil
	}
	covered := map[string]bool{}
	for _, a := range addons {
		for _, r := range a.Matchers.Runtimes {
			covered[r] = true
		}
	}
	byRuntime := map[string][]addOnInstall{}
	for _, n := range inv.Nodes {
		rt, ver, ok := strings.Cut(n.ContainerRuntime, "://")
		if !ok || rt == "" || covered[rt] {
			continue
		}
		byRuntime[rt] = append(byRuntime[rt], addOnInstall{version: strings.TrimPrefix(ver, "v"), where: []string{n.Name}})
	}
	var out []Finding
	for _, rt := range slices.Sorted(maps.Keys(byRuntime)) {
		ins := byRuntime[rt]
		// One known version names it once; otherwise each node's own.
		same := ins[0].version != "" && !slices.ContainsFunc(ins, func(in addOnInstall) bool { return in.version != ins[0].version })
		s := newAddOnSubject(rt, ins, same, true)
		out = append(out, Finding{
			Category: CatAddOnNoData, Severity: SevInfo, Key: string(CatAddOnNoData) + "/" + rt,
			Title: "no lifecycle data for container runtime " + rt,
			Detail: s.located + " The registry has no entry for this container runtime," +
				" so its end of life and Kubernetes compatibility were not assessed.",
		})
	}
	return out
}
