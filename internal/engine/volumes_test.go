package engine

import (
	"fmt"
	"math"
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
		if f.ObjectsOmitted != 1 {
			t.Errorf("1.%d: ObjectsOmitted %d, want 1 (the pod, which is never listed)", target, f.ObjectsOmitted)
		}
		for _, s := range []string{"1 pod and 3 PersistentVolumes or StorageClasses name it", "cluster-scoped (2), data (1), shop (1)",
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

// A volume plugin finding accounts for every object its title counts:
// Count is len(Objects) + ObjectsOmitted, which suppression relies on to
// tell a finding whose every object was accepted from one with more. A
// cluster's row lists its PersistentVolumes and StorageClasses but never
// its pods, so the pods are counted omitted; and the gate's proposed state
// adds manifest objects to a cluster row that may list none. Without it,
// one ignore annotation on a PersistentVolume would take a blocker every
// pod naming the plugin inline still has (#362 review).
func TestEvaluateVolumePluginUnlistedPodsCountedOmitted(t *testing.T) {
	refs := func(n, line int) []inventory.ObjectRef {
		var out []inventory.ObjectRef
		for i := range n {
			out = append(out, inventory.ObjectRef{Name: fmt.Sprintf("o%d", i), Line: line})
		}
		return out
	}
	for _, tc := range []struct {
		name    string
		src     inventory.Source
		use     inventory.VolumePluginUse
		omitted int
		subject string
	}{
		{"live pods and a PV", inventory.SourceCluster, inventory.VolumePluginUse{Count: 6, Namespaces: map[string]int{"shop": 5, "": 1}, Objects: refs(1, 0)},
			5, "5 pods and 1 PersistentVolume or StorageClass name it"},
		{"live, one pod and one PV", inventory.SourceCluster, inventory.VolumePluginUse{Count: 2, Namespaces: map[string]int{"shop": 1, "": 1}, Objects: refs(1, 0)},
			1, "1 pod and 1 PersistentVolume or StorageClass name it"},
		{"live, refs capped", inventory.SourceCluster, inventory.VolumePluginUse{Count: 25, Namespaces: map[string]int{"shop": 20, "": 5}, Objects: refs(2, 0), ObjectsOmitted: 3},
			23, "20 pods and 5 PersistentVolumes or StorageClasses name it"},
		{"live, one PV alone", inventory.SourceCluster, inventory.VolumePluginUse{Count: 1, Namespaces: map[string]int{"": 1}, Objects: refs(1, 0)},
			0, "1 PersistentVolume or StorageClass names it"},
		{"live, PVs alone", inventory.SourceCluster, inventory.VolumePluginUse{Count: 2, Namespaces: map[string]int{"": 2}, Objects: refs(2, 0)},
			0, "2 PersistentVolumes or StorageClasses name it"},
		{"live, pods alone", inventory.SourceCluster, inventory.VolumePluginUse{Count: 3, Namespaces: map[string]int{"shop": 3}},
			0, "3 pods name it"},
		{"gate, cluster pods and a manifest object", inventory.SourceCluster, inventory.VolumePluginUse{Count: 3, Namespaces: map[string]int{"shop": 3}, Objects: refs(1, 1)},
			2, "3 objects name it"},
		{"gate, cluster pods, a PV and a manifest object", inventory.SourceCluster, inventory.VolumePluginUse{Count: 4, Namespaces: map[string]int{"shop": 3, "": 1},
			Objects: append(refs(1, 0), inventory.ObjectRef{Name: "web", Line: 1})}, 2, "4 objects name it"},
		{"files", inventory.SourceFiles, inventory.VolumePluginUse{Count: 2, Namespaces: map[string]int{"shop": 2}, Objects: refs(2, 1)},
			0, "2 objects name it"},
		{"count below the refs", inventory.SourceCluster, inventory.VolumePluginUse{Count: 1, Namespaces: map[string]int{"": 1}, Objects: refs(2, 0)},
			0, "2 PersistentVolumes or StorageClasses name it"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			inv := clusterInv()
			if tc.src == inventory.SourceFiles {
				inv = filesInv()
			}
			tc.use.Plugin = "rbd"
			inv.VolumePlugins = []inventory.VolumePluginUse{tc.use}
			r := Evaluate(inv, volumeKB(t), inventory.Version{Major: 1, Minor: 31}, testNow)
			i := slices.IndexFunc(r.Findings, func(f Finding) bool { return f.Key == "volume-plugin/rbd" })
			if i < 0 {
				t.Fatalf("no volume-plugin/rbd finding in %+v", r.Findings)
			}
			f := r.Findings[i]
			if f.ObjectsOmitted != tc.omitted || len(f.Objects) != len(tc.use.Objects) {
				t.Errorf("objects %d, ObjectsOmitted %d; want %d and %d", len(f.Objects), f.ObjectsOmitted, len(tc.use.Objects), tc.omitted)
			}
			// A row naming no object (pods alone, as an agent before #362
			// wrote every row) is an objectless finding, which suppression
			// takes only by a rule without object selectors or a namespace
			// rule matching all its namespaces.
			if len(tc.use.Objects) > 0 && tc.name != "count below the refs" && len(f.Objects)+f.ObjectsOmitted != tc.use.Count {
				t.Errorf("len(Objects)+ObjectsOmitted = %d, want the count %d", len(f.Objects)+f.ObjectsOmitted, tc.use.Count)
			}
			if !strings.HasPrefix(f.Detail, tc.subject+", in: ") {
				t.Errorf("detail %q, want it to start %q", f.Detail, tc.subject+", in: ")
			}
		})
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

// #361 with #362: the gate saturates a row's ObjectsOmitted at
// math.MaxInt, and a row naming objects sums them with its refs. That sum
// saturates too: a cluster row naming a PersistentVolume with every other
// object omitted at MaxInt still reads as one naming PersistentVolumes or
// StorageClasses, not as pods alone, and keeps its blocker.
func TestEvaluateVolumePluginRefsSaturate(t *testing.T) {
	inv := clusterInv()
	inv.VolumePlugins = []inventory.VolumePluginUse{{Plugin: "rbd", Count: math.MaxInt, Namespaces: map[string]int{"": math.MaxInt},
		Objects: []inventory.ObjectRef{{Name: "pv-rbd"}}, ObjectsOmitted: math.MaxInt}}
	r := Evaluate(inv, volumeKB(t), inventory.Version{Major: 1, Minor: 31}, testNow)
	i := slices.IndexFunc(r.Findings, func(f Finding) bool { return f.Key == "volume-plugin/rbd" })
	if i < 0 || r.Findings[i].Severity != SevBlocker {
		t.Fatalf("findings %+v, want the rbd blocker", r.Findings)
	}
	f := r.Findings[i]
	if want := fmt.Sprintf("%d PersistentVolumes or StorageClasses name it, in: ", math.MaxInt); !strings.HasPrefix(f.Detail, want) || f.ObjectsOmitted != math.MaxInt {
		t.Errorf("detail %q, ObjectsOmitted %d; want it to start %q, MaxInt omitted", f.Detail, f.ObjectsOmitted, want)
	}
}
