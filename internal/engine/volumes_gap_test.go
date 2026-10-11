package engine

import (
	"reflect"
	"slices"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"

	"github.com/abd-ulbasit/upgradescope/internal/inventory"
	"github.com/abd-ulbasit/upgradescope/internal/kb"
)

// Issue #374: a partial volumes gap whose Skipped names the
// PersistentVolume or StorageClass list is required when the KB has a
// removed-class plugin that list could reveal (a PersistentVolume source
// field, or an in-tree provisioner) removed at or before the target. The
// earliest such removal in the embedded KB is scaleIO's, at 1.22. A skipped
// pod list stays optional here (addons makes it required), CSI-migration
// plugins never make it required, and a files inventory is unaffected.
func TestAssessmentGapsVolumesPartialRequired(t *testing.T) {
	embedded, err := kb.Load()
	if err != nil {
		t.Fatalf("kb.Load: %v", err)
	}
	v := func(minor int) *inventory.Version { return &inventory.Version{Major: 1, Minor: minor} }
	only := func(ps ...kb.VolumePlugin) kb.KB {
		k := embedded
		k.VolumePlugins = ps
		return k
	}
	const cite = "https://kubernetes.io/"
	csiOnly := only(
		kb.VolumePlugin{Plugin: "awsElasticBlockStore", Classification: kb.VolumeCSIMigration, Removed: v(27), CSIDriver: "ebs.csi.aws.com", Provisioner: "kubernetes.io/aws-ebs"},
		kb.VolumePlugin{Plugin: "azureFile", Classification: kb.VolumeCSIMigration, Removed: v(30), CSIDriver: "file.csi.azure.com", Provisioner: "kubernetes.io/azure-file"},
		kb.VolumePlugin{Plugin: "flexVolume", Classification: kb.VolumeDeprecated, Deprecated: v(23)},
	)
	podOnly := only(kb.VolumePlugin{Plugin: "gitRepo", Classification: kb.VolumeRemoved, Removed: v(33), Citations: []string{cite}})
	noProvisioner := only(kb.VolumePlugin{Plugin: "cephfs", Classification: kb.VolumeRemoved, Removed: v(31), Citations: []string{cite}})
	pv, sc, pods := inventory.SkippedPersistentVolumes, inventory.SkippedStorageClasses, inventory.SkippedPods

	for _, tc := range []struct {
		name     string
		k        kb.KB
		source   inventory.Source
		skipped  []string
		target   int
		required bool
	}{
		{"PV list, target 1.31 (rbd)", embedded, inventory.SourceCluster, []string{pv}, 31, true},
		{"PV list, target 1.22 (scaleIO, the earliest removal)", embedded, inventory.SourceCluster, []string{pv}, 22, true},
		{"PV list, target 1.21 (before any removal)", embedded, inventory.SourceCluster, []string{pv}, 21, false},
		{"SC list, target 1.31", embedded, inventory.SourceCluster, []string{sc}, 31, true},
		{"SC list, target 1.22", embedded, inventory.SourceCluster, []string{sc}, 22, true},
		{"SC list, target 1.21", embedded, inventory.SourceCluster, []string{sc}, 21, false},
		{"both lists, target 1.31", embedded, inventory.SourceCluster, []string{sc, pv}, 31, true},
		{"both lists, target 1.21", embedded, inventory.SourceCluster, []string{sc, pv}, 21, false},
		{"pods only, target 1.31: addons carries it", embedded, inventory.SourceCluster, []string{pods}, 31, false},
		{"pods and PV list, target 1.31", embedded, inventory.SourceCluster, []string{pv, pods}, 31, true},
		{"v0.1 agent (no source), PV list, target 1.31", embedded, "", []string{pv}, 31, true},
		{"files inventory, PV list, target 1.31", embedded, inventory.SourceFiles, []string{pv}, 31, false},
		{"CSI migration only, both lists, target 1.36", csiOnly, inventory.SourceCluster, []string{sc, pv}, 36, false},
		{"pod-only plugin, both lists, past its removal", podOnly, inventory.SourceCluster, []string{sc, pv}, 34, false},
		{"no provisioner, SC list, past its removal", noProvisioner, inventory.SourceCluster, []string{sc}, 31, false},
		{"no provisioner, PV list, at its removal", noProvisioner, inventory.SourceCluster, []string{pv}, 31, true},
		{"no provisioner, PV list, before its removal", noProvisioner, inventory.SourceCluster, []string{pv}, 30, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			inv := clusterInv()
			inv.Source = tc.source
			inv = partially(inv, inventory.CapVolumes, "list persistentvolumes: forbidden", tc.skipped...)
			var got []CapabilityGap
			for _, g := range assessmentGaps(inv, tc.k, inventory.Version{Major: 1, Minor: tc.target}) {
				if g.Capability == inventory.CapVolumes {
					got = append(got, g)
				}
			}
			want := []CapabilityGap{{Capability: inventory.CapVolumes, Reason: "list persistentvolumes: forbidden",
				Partial: true, Skipped: tc.skipped, Required: tc.required}}
			if !reflect.DeepEqual(got, want) {
				t.Errorf("volumes gap = %+v\nwant          %+v", got, want)
			}
		})
	}
}

// pvSourcePlugins is the JSON name of every field of
// corev1.PersistentVolumeSource: what a PersistentVolume list can reveal.
// The engine keeps its own copy so it does not import k8s.io/api (DO-04).
func TestPVSourcePluginsMatchesTheAPI(t *testing.T) {
	var want []string
	typ := reflect.TypeFor[corev1.PersistentVolumeSource]()
	for i := range typ.NumField() {
		name, _, _ := strings.Cut(typ.Field(i).Tag.Get("json"), ",")
		want = append(want, name)
	}
	slices.Sort(want)
	got := slices.Sorted(func(yield func(string) bool) {
		for p := range pvSourcePlugins {
			if !yield(p) {
				return
			}
		}
	})
	if !slices.Equal(got, want) {
		t.Errorf("pvSourcePlugins = %q\nwant corev1.PersistentVolumeSource's fields %q", got, want)
	}
}
