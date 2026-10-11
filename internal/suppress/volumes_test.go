package suppress

import (
	"slices"
	"strings"
	"testing"

	"github.com/abd-ulbasit/upgradescope/internal/engine"
	"github.com/abd-ulbasit/upgradescope/internal/inventory"
	"github.com/abd-ulbasit/upgradescope/internal/kb"
)

// rbdLive is a live inventory (#362) whose rbd row counts five pods in shop
// that name rbd inline, which the collector never lists, and the
// PersistentVolumes and StorageClass it lists: pvs.
func rbdLive(t *testing.T, pvs ...inventory.ObjectRef) (inventory.Inventory, kb.KB) {
	t.Helper()
	k, err := kb.Load()
	if err != nil {
		t.Fatal(err)
	}
	inv := inventory.Inventory{
		SchemaVersion: 1, ClusterID: "live", Source: inventory.SourceCluster, ServerVersion: "v1.30.4",
		Capabilities: map[inventory.Capability]inventory.CapabilityStatus{inventory.CapVolumes: {Available: true}},
		VolumePlugins: []inventory.VolumePluginUse{{Plugin: "rbd", Count: 5 + len(pvs),
			Namespaces: map[string]int{"shop": 5, "": len(pvs)}, Objects: pvs}},
	}
	return inv, k
}

func rbdFinding(r engine.Report) (engine.Finding, bool) {
	i := slices.IndexFunc(r.Findings, func(f engine.Finding) bool { return f.Key == "volume-plugin/rbd" })
	if i < 0 {
		return engine.Finding{}, false
	}
	return r.Findings[i], true
}

// The #362 review's repro, end to end on the embedded knowledge base: one
// ignore annotation on the only listed rbd PersistentVolume, or a name
// rule matching every listed one, accepts those objects and leaves the
// blocker standing for the five pods that name rbd inline, with the
// verdict blocked. Before the engine counted the pods omitted, the
// finding's every listed object was taken and the whole blocker
// suppressed, so the cluster read as ready.
func TestApplyAcceptedPersistentVolumeLeavesThePodsBlocker(t *testing.T) {
	pv := inventory.ObjectRef{Name: "pv-rbd", Ignore: "volume-plugin/rbd", IgnoreReason: "migrated before the upgrade"}
	sc := inventory.ObjectRef{Name: "sc-rbd"}
	target := inventory.Version{Major: 1, Minor: 31}
	for _, tc := range []struct {
		name  string
		pvs   []inventory.ObjectRef
		rules []Rule
	}{
		{"annotation on the one PersistentVolume", []inventory.ObjectRef{pv}, nil},
		{"name rule on every listed object", []inventory.ObjectRef{{Name: "pv-rbd"}, sc}, []Rule{{Key: "volume-plugin/rbd", Name: "*-rbd", Reason: "x"}}},
		{"namespace rule", []inventory.ObjectRef{{Name: "pv-rbd"}}, []Rule{{Key: "volume-plugin/rbd", Namespace: "*", Reason: "x"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			inv, k := rbdLive(t, tc.pvs...)
			r := engine.Evaluate(inv, k, target, now)
			f, ok := rbdFinding(r)
			if !ok || f.Severity != engine.SevBlocker {
				t.Fatalf("before suppression: %+v, want an rbd blocker", r.Findings)
			}
			if len(f.Objects)+f.ObjectsOmitted != 5+len(tc.pvs) {
				t.Errorf("before suppression: %d listed, %d omitted; want all %d objects accounted for", len(f.Objects), f.ObjectsOmitted, 5+len(tc.pvs))
			}
			got, _ := Apply(r, tc.rules, Options{Now: now})
			f, ok = rbdFinding(got)
			if !ok || f.Severity != engine.SevBlocker || got.Verdict != engine.VerdictBlocked {
				t.Fatalf("findings %+v, verdict %s: want the rbd blocker kept for the pods and the verdict blocked", got.Findings, got.Verdict)
			}
			if f.ObjectsOmitted != 5 {
				t.Errorf("kept finding counts %d omitted, want the 5 pods", f.ObjectsOmitted)
			}
			taken := 0
			for _, s := range got.Suppressed {
				if s.Key == "volume-plugin/rbd" {
					taken += len(s.Objects)
				}
			}
			if want := len(tc.pvs) - len(f.Objects); taken != want || !strings.Contains(f.Detail, "suppressed (see suppressed)") != (want == 0) {
				t.Errorf("suppressed %d objects (detail %q), want %d", taken, f.Detail, want)
			}
		})
	}

	// A rule without object selectors still accepts the whole finding,
	// pods included.
	inv, k := rbdLive(t, pv)
	got, _ := Apply(engine.Evaluate(inv, k, target, now), []Rule{{Key: "volume-plugin/rbd", Reason: "x"}}, Options{Now: now})
	if _, ok := rbdFinding(got); ok || len(got.Suppressed) != 1 || got.Suppressed[0].ObjectsOmitted != 5 {
		t.Errorf("key rule: findings %+v, suppressed %+v; want the finding suppressed whole with its 5 pods", got.Findings, got.Suppressed)
	}

	// A files inventory lists every object it counts: annotating them all
	// still accepts the finding.
	files := inventory.Inventory{SchemaVersion: 1, ClusterID: "files", Source: inventory.SourceFiles,
		Capabilities: map[inventory.Capability]inventory.CapabilityStatus{inventory.CapVolumes: {Available: true}},
		VolumePlugins: []inventory.VolumePluginUse{{Plugin: "rbd", Count: 1, Namespaces: map[string]int{"shop": 1},
			Objects: []inventory.ObjectRef{{Namespace: "shop", Name: "web", File: "web.yaml", Line: 1, Ignore: "volume-plugin", IgnoreReason: "x"}}}},
	}
	got, _ = Apply(engine.Evaluate(files, k, target, now), nil, Options{Now: now})
	if _, ok := rbdFinding(got); ok || len(got.Suppressed) != 1 {
		t.Errorf("files: findings %+v, suppressed %+v; want the annotated object's finding suppressed", got.Findings, got.Suppressed)
	}
}
