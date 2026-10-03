package engine

import (
	"fmt"
	"reflect"
	"testing"

	"github.com/abd-ulbasit/upgradescope/internal/inventory"
	"github.com/abd-ulbasit/upgradescope/registry"
)

// The node-runtime compat blocker names the nodes that cannot run the
// target even when they all run one version: a node runtime has no
// namespaces, and gate, JUnit and code-quality outputs show only blockers,
// so the blocker must say where on its own (#169). Nodes that can run the
// target are not named, and the list is bounded like other located lists.
func TestEvalAddOnsNodeRuntimeCompatNamesNodes(t *testing.T) {
	v138 := inventory.Version{Major: 1, Minor: 38}
	const head = `Installed version 1.7.27 matches compatibility range "<2.0.0", which supports Kubernetes up to 1.37.`
	var many inventory.Inventory
	for i := range 12 {
		many.Nodes = append(many.Nodes, inventory.NodeInfo{Name: fmt.Sprintf("node-%02d", i+1), ContainerRuntime: "containerd://1.7.27"})
	}
	for name, tc := range map[string]struct {
		inv  inventory.Inventory
		want string
	}{
		"one version": {nodes("containerd://1.7.27", "containerd://2.0.5", "containerd://1.7.27"),
			head + " Incompatible nodes: worker-1 (1.7.27), worker-3 (1.7.27)."},
		"bounded": {many,
			head + " Incompatible nodes: node-01 (1.7.27), node-02 (1.7.27), node-03 (1.7.27), node-04 (1.7.27), node-05 (1.7.27), node-06 (1.7.27), node-07 (1.7.27), node-08 (1.7.27), node-09 (1.7.27), node-10 (1.7.27), and 2 more."},
	} {
		t.Run(name, func(t *testing.T) {
			var got []string
			for _, f := range evalAddOns(tc.inv, runtimeKB("1.37"), v138, day("2026-10-02")) {
				if f.Category == CatChartIncompat {
					got = append(got, f.Detail)
				}
			}
			if want := []string{tc.want}; !reflect.DeepEqual(got, want) {
				t.Errorf("chart-incompat details\n%q\nwant\n%q", got, want)
			}
		})
	}
}

// A node container runtime no registry entry covers (cri-o, docker, or
// containerd with a registry that lacks it) is disclosed as an info
// finding naming the nodes: its end of life and Kubernetes compatibility
// were not assessed, which must not read as "checked, fine" (#169). It
// changes neither score nor verdict. A runtime field without a scheme
// names no runtime and is skipped.
func TestEvalUncoveredNodeRuntimes(t *testing.T) {
	inv := nodes("cri-o://1.30.4", "docker://24.0.7", "", "containerd", "cri-o://1.30.4", "containerd://1.7.27", "cri-o://1.29.1")
	got := evalUncoveredRuntimes(inv, runtimeKB("1.37").AddOns, nil)
	want := []Finding{
		{Category: CatAddOnNoData, Severity: SevInfo, Key: "addon-no-data/cri-o", Title: "no lifecycle data for container runtime cri-o",
			Detail: "Detected cri-o on node(s): worker-1 (1.30.4), worker-5 (1.30.4), worker-7 (1.29.1). The registry has no entry for this container runtime, so its end of life and Kubernetes compatibility were not assessed."},
		{Category: CatAddOnNoData, Severity: SevInfo, Key: "addon-no-data/docker", Title: "no lifecycle data for container runtime docker",
			Detail: "Detected docker version 24.0.7 on node(s): worker-2. The registry has no entry for this container runtime, so its end of life and Kubernetes compatibility were not assessed."},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got\n%+v\nwant\n%+v", got, want)
	}

	if got := evalUncoveredRuntimes(nodes("containerd://1.7.27"), []registry.AddOn{{ID: "istio"}}, nil); len(got) != 1 || got[0].Key != "addon-no-data/containerd" {
		t.Errorf("containerd without a registry entry: got %+v, want one addon-no-data/containerd finding", got)
	}
	// A KB without add-ons (a custom --kb of API lifecycle only) assesses
	// no add-on at all: runtimes are not singled out among them.
	if got := evalUncoveredRuntimes(nodes("containerd://1.7.27", "cri-o://1.30.4"), nil, nil); len(got) != 0 {
		t.Errorf("KB without add-ons: got %+v, want no finding", got)
	}

	crio := nodes("cri-o://1.30.4")
	crio.ServerVersion = "v1.35.2"
	crio.Capabilities = map[inventory.Capability]inventory.CapabilityStatus{
		inventory.CapAPIUsage: {Available: true}, inventory.CapVersions: {Available: true}, inventory.CapAddOns: {Available: true},
	}
	r := Evaluate(crio, runtimeKB("1.37"), inventory.Version{Major: 1, Minor: 36}, day("2026-10-02"))
	if summary := summarize(r.Findings); !reflect.DeepEqual(summary, []string{"info addon-no-data addon-no-data/cri-o no lifecycle data for container runtime cri-o"}) || r.Score != 100 || r.Verdict != VerdictReady {
		t.Errorf("Evaluate: findings %q, score %d, verdict %s; want the cri-o info finding, 100, ready", summary, r.Score, r.Verdict)
	}
}
