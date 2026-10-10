package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
	"time"

	"github.com/abd-ulbasit/upgradescope/internal/inventory"
)

// #362 on the gate path: a cluster whose rbd row counts five pods in shop
// that name rbd inline, which the agent never lists, beside the
// PersistentVolume and StorageClass it lists. Accepting every listed object
// (the PersistentVolume's own ignore annotation with the StorageClass taken
// by a name rule, or a name rule matching both) leaves the blocker standing
// for the pods and clusterVerdict blocked; a rule with no object selectors
// takes the whole finding, pods included.
func TestGateClusterVolumeSuppressionKeepsUnlistedPods(t *testing.T) {
	pv := inventory.ObjectRef{Name: "pv-rbd", Ignore: "volume-plugin/rbd", IgnoreReason: "migrated before the upgrade"}
	sc := inventory.ObjectRef{Name: "sc-rbd"}
	ts := httptest.NewServer(newTestServer(t, newFakeStore(), func(c *Config) { c.KB = volumesKB() }).Handler())
	defer ts.Close()
	seed := func(t *testing.T, i int, objs []inventory.ObjectRef) {
		t.Helper()
		inv := testInventory()
		inv.CollectedAt = inv.CollectedAt.Add(time.Duration(i) * time.Minute)
		inv.VolumePlugins = []inventory.VolumePluginUse{{Plugin: "rbd", Count: 5 + len(objs),
			Namespaces: map[string]int{"shop": 5, "": len(objs)}, Objects: objs}}
		// The same snapshot as the case before is a duplicate (200),
		// already stored.
		if resp, out := postSnapshot(t, ts, "ingest-tok", pushReqBody(t, inv), false); resp.StatusCode != http.StatusAccepted && resp.StatusCode != http.StatusOK {
			t.Fatalf("seed push = %d %v", resp.StatusCode, out)
		}
	}
	type finding struct {
		Key            string `json:"key"`
		Severity       string `json:"severity"`
		ObjectsOmitted int    `json:"objectsOmitted"`
		Objects        []struct {
			Name string `json:"name"`
		} `json:"objects"`
	}
	type answer struct {
		ClusterVerdict string    `json:"clusterVerdict"`
		Findings       []finding `json:"findings"`
		Suppressed     []struct {
			Key            string `json:"key"`
			ObjectsOmitted int    `json:"objectsOmitted"`
			Objects        []struct {
				Name string `json:"name"`
			} `json:"objects"`
		} `json:"suppressed"`
	}
	rule := func(selector string) string {
		return "ignore:\n  - key: volume-plugin/rbd\n" + selector + "    reason: x\n"
	}
	for i, tc := range []struct {
		name, config string
		objs         []inventory.ObjectRef // the cluster's listed objects
		whole        bool                  // the finding is suppressed whole
		listedLeft   int                   // listed objects the kept finding still names
		takenObjects int                   // listed objects suppressed
	}{
		{"an annotated PersistentVolume, the only listed object", "", []inventory.ObjectRef{pv}, false, 0, 1},
		{"the annotation, and a StorageClass left", "", []inventory.ObjectRef{pv, sc}, false, 1, 1},
		{"the annotation and a name rule for the StorageClass", rule("    name: sc-rbd\n"), []inventory.ObjectRef{pv, sc}, false, 0, 2},
		{"a name rule matching every listed object", rule("    name: \"*-rbd\"\n"), []inventory.ObjectRef{{Name: "pv-rbd"}, sc}, false, 0, 2},
		{"a rule with no object selectors", rule(""), []inventory.ObjectRef{pv, sc}, true, 0, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			seed(t, i, tc.objs)
			q := "?target=1.35&cluster=prod-eu-1"
			if tc.config != "" {
				q += withConfig(tc.config)
			}
			resp, raw := postGate(t, ts, q, "", deploymentManifest, "application/x-yaml")
			var b answer
			if err := json.Unmarshal(raw, &b); err != nil || resp.StatusCode != http.StatusOK {
				t.Fatalf("gate = %d, %v\n%s", resp.StatusCode, err, raw)
			}
			kept := slices.IndexFunc(b.Findings, func(f finding) bool { return f.Key == "volume-plugin/rbd" })
			if tc.whole {
				if kept >= 0 || b.ClusterVerdict == "blocked" || len(b.Suppressed) != 1 || b.Suppressed[0].ObjectsOmitted != 5 {
					t.Errorf("want the rbd finding suppressed whole with its 5 pods and clusterVerdict not blocked\n%s", raw)
				}
				return
			}
			if kept < 0 || b.Findings[kept].Severity != "blocker" || b.ClusterVerdict != "blocked" {
				t.Fatalf("want the rbd blocker kept for the pods and clusterVerdict blocked\n%s", raw)
			}
			if f := b.Findings[kept]; f.ObjectsOmitted != 5 || len(f.Objects) != tc.listedLeft {
				t.Errorf("kept finding names %d objects and counts %d omitted; want %d named and the 5 pods omitted\n%s", len(f.Objects), f.ObjectsOmitted, tc.listedLeft, raw)
			}
			taken := 0
			for _, s := range b.Suppressed {
				if s.Key == "volume-plugin/rbd" {
					taken += len(s.Objects)
				}
			}
			if taken != tc.takenObjects {
				t.Errorf("suppressed %d listed objects, want %d\n%s", taken, tc.takenObjects, raw)
			}
		})
	}
}
