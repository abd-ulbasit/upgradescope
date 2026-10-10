package engine

import (
	"reflect"
	"slices"
	"testing"

	"github.com/abd-ulbasit/upgradescope/internal/inventory"
	"github.com/abd-ulbasit/upgradescope/internal/kb"
)

// #351: in-tree volume plugins, judged with the real dataset.

func volumeKB(t *testing.T) kb.KB {
	t.Helper()
	k := testKB()
	k.VolumePlugins = kb.VolumePlugins()
	return k
}

// A manifest without in-tree volume plugins is unchanged: no finding, no
// gap, 100.
func TestEvaluateNoVolumePluginsIsClean(t *testing.T) {
	inv := filesInv()
	inv.APIUsage = []inventory.APIUsage{{Group: "apps", Version: "v1", Kind: "Deployment", Count: 1}}
	r := Evaluate(inv, volumeKB(t), inventory.Version{Major: 1, Minor: 36}, testNow)
	if r.Score != 100 || slices.ContainsFunc(r.Findings, func(f Finding) bool { return f.Category == CatVolumePlugin }) {
		t.Errorf("score %d, findings %+v: want 100 and no volume-plugin finding", r.Score, r.Findings)
	}
	if slices.ContainsFunc(r.NotAssessed, func(g CapabilityGap) bool { return g.Capability == inventory.CapVolumes }) {
		t.Errorf("NotAssessed = %+v, want no volumes gap", r.NotAssessed)
	}
}

// An inventory that does not report the capability (an older agent's) is
// not assessed for volume plugins, never clean, and the gap is optional:
// the verdict is unchanged.
func TestEvaluateVolumesNotReported(t *testing.T) {
	inv := clusterInv()
	delete(inv.Capabilities, inventory.CapVolumes)
	r := Evaluate(inv, volumeKB(t), inventory.Version{Major: 1, Minor: 35}, testNow)
	want := []CapabilityGap{{Capability: inventory.CapVolumes, Reason: volumesNotReported}}
	if !reflect.DeepEqual(r.NotAssessed, want) || r.Verdict != VerdictReady {
		t.Errorf("verdict %s, NotAssessed %+v: want ready with %+v", r.Verdict, r.NotAssessed, want)
	}
	if hidden := HiddenBy(r.NotAssessed, CatVolumePlugin, "volume-plugin/glusterfs"); !reflect.DeepEqual(hidden, []inventory.Capability{inventory.CapVolumes}) {
		t.Errorf("HiddenBy = %v, want the volumes gap to hide a volume-plugin finding", hidden)
	}
}

// Every classification at the minors around its removal, with the real
// dataset: removed blocks at and after, warns the minor before, is info
// earlier; csi-migration warns at and after, is info before, never blocks;
// deprecated is info.
func TestEvaluateVolumePluginWindows(t *testing.T) {
	inv := clusterInv()
	inv.VolumePlugins = []inventory.VolumePluginUse{
		{Plugin: "awsElasticBlockStore", Count: 1, Namespaces: map[string]int{"a": 1}},
		{Plugin: "flexVolume", Count: 1, Namespaces: map[string]int{"a": 1}},
		{Plugin: "glusterfs", Count: 1, Namespaces: map[string]int{"a": 1}},
	}
	inv.ServerVersion, inv.Nodes = "v1.24.0", []inventory.NodeInfo{{Name: "n", KubeletVersion: "v1.24.0"}}
	for _, tc := range []struct {
		target             int
		ebs, flex, gluster Severity
	}{
		{25, SevInfo, SevInfo, SevWarning},
		{26, SevInfo, SevInfo, SevBlocker},
		{27, SevWarning, SevInfo, SevBlocker},
		{36, SevWarning, SevInfo, SevBlocker},
	} {
		r := Evaluate(inv, volumeKB(t), inventory.Version{Major: 1, Minor: tc.target}, testNow)
		got := map[string]Severity{}
		for _, f := range r.Findings {
			if f.Category == CatVolumePlugin {
				got[f.Key] = f.Severity
				if len(f.Citations) == 0 {
					t.Errorf("1.%d %s: no citation", tc.target, f.Key)
				}
			}
		}
		want := map[string]Severity{"volume-plugin/awsElasticBlockStore": tc.ebs, "volume-plugin/flexVolume": tc.flex, "volume-plugin/glusterfs": tc.gluster}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("target 1.%d: %v, want %v", tc.target, got, want)
		}
	}
}
