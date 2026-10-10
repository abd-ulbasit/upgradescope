package engine

import (
	"reflect"
	"strings"
	"testing"

	"github.com/abd-ulbasit/upgradescope/internal/inventory"
	"github.com/abd-ulbasit/upgradescope/internal/kb"
	"github.com/abd-ulbasit/upgradescope/registry"
)

// runtimeKB carries endoflife.date's containerd cycles as synced on
// 2026-10-02 and the kubelet's announced end of containerd 1.x support.
func runtimeKB(removalAfter string) kb.KB {
	eol := func(d string) *registry.CycleEOL { return &registry.CycleEOL{Date: d} }
	cite := []string{"https://endoflife.date/containerd"}
	return kb.KB{
		AddOns: []registry.AddOn{{
			SchemaVersion: 2, ID: "containerd", DisplayName: "containerd",
			Matchers: registry.Matchers{Runtimes: []string{"containerd"}},
			Support:  registry.Support{Status: "supported", Citations: cite},
			Cycles: []registry.Cycle{
				{Cycle: "2.3", EOL: eol("2028-04-30"), Citations: cite},
				{Cycle: "2.0", EOL: eol("2027-03-01"), Citations: cite},
				{Cycle: "1.7", EOL: eol("2026-09-01"), Citations: cite},
			},
			Compat: []registry.Compat{{Range: "<2.0.0", K8sMax: removalAfter,
				Citations: []string{"https://github.com/kubernetes/kubernetes/pull/139121"}}},
			EndoflifeProduct: "containerd",
		}},
		Skew:        kb.DefaultSkewPolicy(),
		MaxKnownK8s: inventory.Version{Major: 1, Minor: 38},
	}
}

func nodes(runtimes ...string) inventory.Inventory {
	var inv inventory.Inventory
	for i, rt := range runtimes {
		inv.Nodes = append(inv.Nodes, inventory.NodeInfo{Name: "worker-" + string(rune('1'+i)), KubeletVersion: "v1.35.2", ContainerRuntime: rt})
	}
	return inv
}

// The runtime ships with the node image or OS, which a node upgrade or a
// node-pool image bump replaces, so an ended containerd release line is a
// warning that names the nodes. Only the kubelet's own end of 1.x support
// (the compat row, k8s_max 1.37) blocks, at target 1.38.
func TestEvalAddOnsNodeRuntimes(t *testing.T) {
	now := day("2026-10-02")
	v136, v137, v138 := inventory.Version{Major: 1, Minor: 36}, inventory.Version{Major: 1, Minor: 37}, inventory.Version{Major: 1, Minor: 38}
	ended := "warning eol-addon eol-addon/containerd/1.7 containerd 1.7 is end-of-life since 2026-09-01"
	cases := []struct {
		name   string
		inv    inventory.Inventory
		target inventory.Version
		want   []string
	}{
		{"containerd 1.7 ended on 2026-09-01, target 1.36", nodes("containerd://1.7.27"), v136, []string{ended}},
		{"containerd 1.7 ended on 2026-09-01, target 1.37", nodes("containerd://1.7.27"), v137, []string{ended}},
		{"containerd 1.x at the kubelet's removal release", nodes("containerd://1.7.27"), v138, []string{
			ended,
			"blocker chart-incompat chart-incompat/containerd/1.7 containerd 1.7.27 supports Kubernetes up to 1.37 (target 1.38)",
		}},
		{"containerd 2.0 is fine", nodes("containerd://2.0.5"), v138, nil},
		{"one finding per release line, oldest version, every node named", nodes("containerd://1.7.27", "containerd://2.3.1", "containerd://1.7.20"), v136,
			[]string{ended}},
		{"distro suffix and v prefix", nodes("containerd://v1.7.23-k3s2"), v136, []string{ended}},
		{"version inside the tracked lines without a cycle", nodes("containerd://1.9.0"), v136,
			[]string{"info addon-no-data addon-no-data/containerd no lifecycle data for containerd 1.9.0"}},
		// Older than the oldest tracked line, which has ended: past end of
		// life, a warning like any ended runtime line (#165).
		{"version older than the oldest tracked line", nodes("containerd://1.4.0", "containerd://1.6.20"), v136,
			[]string{"warning eol-addon eol-addon/containerd/below-1.7 containerd 1.4.0 is end-of-life (older than the 1.7 release line)"}},
		{"other runtimes and unset fields are not containerd", nodes("cri-o://1.30.4", "docker://24.0.7", "", "containerd"), v136, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := summarize(evalAddOns(tc.inv, runtimeKB("1.37"), tc.target, now)); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("got  %q\nwant %q", got, tc.want)
			}
		})
	}
}

// End to end: a cluster whose only problem is containerd 1.7 nodes is ready
// for 1.36 and 1.37 and blocked for 1.38.
func TestEvaluateNodeRuntimeVerdict(t *testing.T) {
	inv := nodes("containerd://1.7.27")
	inv.ServerVersion = "v1.35.2"
	inv.Capabilities = map[inventory.Capability]inventory.CapabilityStatus{
		inventory.CapAPIUsage: {Available: true}, inventory.CapVersions: {Available: true}, inventory.CapAddOns: {Available: true},
	}
	for target, want := range map[string]Verdict{"1.36": VerdictReady, "1.37": VerdictReady, "1.38": VerdictBlocked} {
		v, err := inventory.ParseVersion(target)
		if err != nil {
			t.Fatal(err)
		}
		if r := Evaluate(inv, runtimeKB("1.37"), v, day("2026-10-02")); r.Verdict != want {
			t.Errorf("target %s: verdict %s, want %s; findings %q", target, r.Verdict, want, summarize(r.Findings))
		}
	}
}

func TestEvalAddOnsNodeRuntimeDetail(t *testing.T) {
	inv := nodes("containerd://1.7.27", "containerd://2.3.1", "containerd://1.7.20")
	fs := evalAddOns(inv, runtimeKB("1.37"), inventory.Version{Major: 1, Minor: 36}, day("2026-10-02"))
	if len(fs) != 1 {
		t.Fatalf("want one finding, got %+v", fs)
	}
	want := "Detected containerd version 1.7.20 on node(s): worker-1, worker-3. Upstream support for the 1.7 release line ended on 2026-09-01." +
		" The runtime comes with the node image or OS, not with the Kubernetes version, so this does not block the upgrade by itself."
	if fs[0].Detail != want {
		t.Errorf("detail = %q, want %q", fs[0].Detail, want)
	}
	if fs[0].Remediation != "Upgrade containerd to a supported release line (newest: 2.3)." {
		t.Errorf("remediation = %q", fs[0].Remediation)
	}
}

// Nodes without release-line data share one finding, so it names each
// node's own version rather than claiming one for all of them.
func TestEvalAddOnsNodeRuntimeNoDataDetail(t *testing.T) {
	fs := evalAddOns(nodes("containerd://1.9.0", "containerd://"), runtimeKB("1.37"), inventory.Version{Major: 1, Minor: 36}, day("2026-10-02"))
	if len(fs) != 1 || fs[0].Category != CatAddOnNoData {
		t.Fatalf("want one addon-no-data finding, got %+v", fs)
	}
	if want := "Detected containerd on node(s): worker-1 (1.9.0), worker-2 (version unknown)."; !strings.HasPrefix(fs[0].Detail, want) {
		t.Errorf("detail = %q, want prefix %q", fs[0].Detail, want)
	}
	// A node whose runtime version is unread keeps the node sentence.
	fs = evalAddOns(nodes("containerd://"), runtimeKB("1.37"), inventory.Version{Major: 1, Minor: 36}, day("2026-10-02"))
	if want := " No version could be read from the node's container runtime version. Its end of life and Kubernetes compatibility were not assessed."; len(fs) != 1 || !strings.HasSuffix(fs[0].Detail, want) {
		t.Errorf("findings = %+v, want one with suffix %q", fs, want)
	}
}

// The kubelet's removal release is registry data: upstream already moved
// it twice, and moving it again must change the verdict with no code change.
func TestEvalAddOnsRuntimeRemovalIsData(t *testing.T) {
	inv := nodes("containerd://1.7.27")
	v138 := inventory.Version{Major: 1, Minor: 38}
	for removal, want := range map[string]bool{"1.37": true, "1.38": false} {
		got := false
		for _, f := range evalAddOns(inv, runtimeKB(removal), v138, day("2026-10-02")) {
			got = got || f.Category == CatChartIncompat
		}
		if got != want {
			t.Errorf("k8s_max %s at target 1.38: chart-incompat = %v, want %v", removal, got, want)
		}
	}
}
