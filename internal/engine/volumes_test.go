package engine

import (
	"reflect"
	"slices"
	"strings"
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

// #362: live, a PersistentVolume bound to a claim in data, an unbound one
// and a StorageClass with the in-tree provisioner, all rbd, beside a pod:
// a warning the minor before rbd's removal, a blocker at it, the objects
// named, the unbound PersistentVolume and the StorageClass counted as
// cluster-scoped, and the claims and the class's new claims explained.
func TestEvaluateVolumePluginPersistentVolumesAndStorageClasses(t *testing.T) {
	inv := clusterInv()
	inv.ServerVersion, inv.Nodes = "v1.30.4", []inventory.NodeInfo{{Name: "n", KubeletVersion: "v1.30.4"}}
	inv.VolumePlugins = []inventory.VolumePluginUse{{Plugin: "rbd", Count: 4, Namespaces: map[string]int{"data": 1, "shop": 1, "": 2},
		Objects: []inventory.ObjectRef{{Name: "sc-rbd"}, {Name: "pv-rbd"}, {Name: "pv-unbound"}}}}
	for target, want := range map[int]Severity{30: SevWarning, 31: SevBlocker, 36: SevBlocker} {
		r := Evaluate(inv, volumeKB(t), inventory.Version{Major: 1, Minor: target}, testNow)
		i := slices.IndexFunc(r.Findings, func(f Finding) bool { return f.Key == "volume-plugin/rbd" })
		if i < 0 {
			t.Fatalf("1.%d: no volume-plugin/rbd finding in %+v", target, r.Findings)
		}
		f := r.Findings[i]
		if f.Severity != want {
			t.Errorf("1.%d: severity %s, want %s", target, f.Severity, want)
		}
		if names := []inventory.ObjectRef{{Name: "pv-rbd"}, {Name: "pv-unbound"}, {Name: "sc-rbd"}}; !reflect.DeepEqual(f.Objects, names) {
			t.Errorf("1.%d: objects %+v, want %+v", target, f.Objects, names)
		}
		for _, s := range []string{"4 pods, PersistentVolumes or StorageClasses name it", "cluster-scoped (2), data (1), shop (1)",
			"a claim bound to a PersistentVolume", "StorageClass with provisioner kubernetes.io/rbd"} {
			if !strings.Contains(f.Detail, s) {
				t.Errorf("1.%d: detail %q, want %q in it", target, f.Detail, s)
			}
		}
		if f.Title != "In-tree volume plugin rbd removed in 1.31 (4 objects)" {
			t.Errorf("1.%d: title %q", target, f.Title)
		}
	}
}

// The gate's proposed state lists the cluster's PersistentVolumes and
// StorageClasses (no line) before the manifests' objects: an empty
// namespace there is either.
func TestEvaluateVolumePluginGateMixedObjects(t *testing.T) {
	inv := clusterInv()
	inv.VolumePlugins = []inventory.VolumePluginUse{{Plugin: "rbd", Count: 2, Namespaces: map[string]int{"": 2},
		Objects: []inventory.ObjectRef{{Name: "pv-rbd"}, {Name: "pv-new", File: "pv.yaml", Line: 1}}}}
	r := Evaluate(inv, volumeKB(t), inventory.Version{Major: 1, Minor: 31}, testNow)
	i := slices.IndexFunc(r.Findings, func(f Finding) bool { return f.Key == "volume-plugin/rbd" })
	if i < 0 || !strings.Contains(r.Findings[i].Detail, "in: cluster-scoped or no namespace set (2)") {
		t.Errorf("findings %+v, want the rbd one counting 2 cluster-scoped or with no namespace set", r.Findings)
	}
}
