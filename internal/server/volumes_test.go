package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/abd-ulbasit/upgradescope/internal/inventory"
	"github.com/abd-ulbasit/upgradescope/internal/kb"
)

// #351: in-tree volume plugins at ingest.

func volumesKB() kb.KB {
	k := testKB()
	k.VolumePlugins = kb.VolumePlugins()
	return k
}

func glusterInventory() inventory.Inventory {
	inv := testInventory()
	inv.VolumePlugins = []inventory.VolumePluginUse{{Plugin: "glusterfs", Count: 2, Namespaces: map[string]int{"payments-prod": 2}}}
	return inv
}

// An inventory from an agent that predates the volumes capability (a
// v0.1.x agent's, a v0.2.0 release candidate's, or any that does not
// report it) reads as not assessed for volume plugins, never as clean, and
// the gap is optional: the verdict is the one its other data gives. The
// current agent's glusterfs pods block.
func TestOlderAgentVolumesNotAssessed(t *testing.T) {
	h := newHarness(t, Config{KB: volumesKB()}, aug1)
	for _, agent := range []string{"0.1.1", "dev", "0.2.0-rc.2", "v0.2.0"} {
		cluster := "old-" + strings.NewReplacer(".", "-", "v", "").Replace(agent)
		inv := unmarked(testInventory())
		if agent == "v0.2.0" {
			inv = testInventory() // stamped, as a build without the capability would be
		}
		delete(inv.Capabilities, inventory.CapVolumes)
		if code, out := h.pushAs(cluster, agent, inv); code != http.StatusAccepted {
			t.Fatalf("push as %q = %d %v", agent, code, out)
		}
		rep := h.report(cluster, "1.34")
		reason, required := rep.gap(string(inventory.CapVolumes))
		if !strings.Contains(reason, "predates in-tree volume plugin checks") || required {
			t.Errorf("agent %q: volumes gap = %q (required %v), want the optional predates gap", agent, reason, required)
		}
	}

	if code, out := h.pushAs("current", "v0.2.0", glusterInventory()); code != http.StatusAccepted {
		t.Fatalf("push = %d %v", code, out)
	}
	rep := h.report("current", "1.34")
	if rep.Verdict != "blocked" {
		t.Errorf("current agent with glusterfs pods: verdict %s (findings %+v), want blocked", rep.Verdict, rep.Findings)
	}
	if reason, _ := rep.gap(string(inventory.CapVolumes)); reason != "" {
		t.Errorf("current agent: volumes gap %q, want none", reason)
	}
}

// Volume plugin entries are validated like API usage entries: a namespace
// key that is no namespace name is refused (422) before anything is stored.
func TestIngestRefusesInvalidVolumePlugins(t *testing.T) {
	h := newHarness(t, Config{KB: volumesKB()}, aug1)
	inv := glusterInventory()
	inv.VolumePlugins[0].Namespaces = map[string]int{"Not_A_Namespace": 1}
	code, out := h.pushAs("bad", "v0.2.0", inv)
	if code != http.StatusUnprocessableEntity || !strings.Contains(out["error"].(string), "volumePlugins[0].namespaces") {
		t.Errorf("push = %d %v, want 422 naming volumePlugins[0].namespaces", code, out)
	}
}

// A hostile volume plugin entry (one a collector would not produce: a
// plugin with control characters or of 1000 bytes, a negative count,
// objects over the cap) is refused at ingest (422) naming the field, and
// nothing is stored for the cluster.
func TestIngestRefusesHostileVolumePlugins(t *testing.T) {
	h := newHarness(t, Config{KB: volumesKB()}, aug1)
	over := inventory.VolumePluginUse{Plugin: "glusterfs", Count: inventory.MaxObjectRefs + 1}
	for i := range inventory.MaxObjectRefs + 1 {
		over.Objects = append(over.Objects, inventory.ObjectRef{Namespace: "shop", Name: fmt.Sprintf("w%d", i)})
	}
	for _, tc := range []struct {
		name  string
		entry inventory.VolumePluginUse
		field string
	}{
		{"control characters", inventory.VolumePluginUse{Plugin: "glus\x1b[31mterfs\n", Count: 1}, "volumePlugins[0].plugin"},
		{"long plugin", inventory.VolumePluginUse{Plugin: strings.Repeat("g", 1000), Count: -1, Namespaces: map[string]int{"shop": -1}}, "volumePlugins[0].plugin"},
		{"negative count", inventory.VolumePluginUse{Plugin: "glusterfs", Count: -1}, "volumePlugins[0]"},
		{"objects over the cap", over, "volumePlugins[0].objects"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cluster := "hostile-" + strings.ReplaceAll(tc.name, " ", "-")
			inv := testInventory()
			inv.VolumePlugins = []inventory.VolumePluginUse{tc.entry}
			code, out := h.pushAs(cluster, "v0.2.0", inv)
			msg, _ := out["error"].(string)
			if code != http.StatusUnprocessableEntity || !strings.Contains(msg, tc.field) {
				t.Fatalf("push = %d %v, want 422 naming %s", code, out, tc.field)
			}
			if strings.ContainsAny(msg, "\x1b\n") || len(msg) > 1000 {
				t.Errorf("error quotes raw control characters or the whole value: %q", msg)
			}
			cs, err := h.st.ListClusters(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			for _, c := range cs {
				if c.Name == cluster {
					t.Errorf("cluster %s stored after a refused push", cluster)
				}
			}
		})
	}
}

// An agent newer than the server pushes fields and capabilities the server
// does not know: decoding ignores them, so the push is accepted, and an
// unknown available capability is no gap. That is why the volumes
// capability needed no collectorSchema bump, which would make an older
// server refuse every push of a newer agent.
func TestIngestAcceptsUnknownFieldsAndCapabilities(t *testing.T) {
	ts, done := fleetFixture(t)
	defer done()
	body := `{"schemaVersion":1,"clusterName":"newer","agentVersion":"v0.3.0","kbVersion":"k","inventory":{` +
		`"schemaVersion":1,"collectorSchema":1,"clusterId":"uid-newer","serverVersion":"v1.34.2",` +
		`"capabilities":{"api-usage":{"available":true},"versions":{"available":true},"addons":{"available":true},` +
		`"crds":{"available":true},"volumes":{"available":true},"future-capability":{"available":true}},` +
		`"futureField":[{"anything":1}],"nodes":[{"name":"n","kubeletVersion":"v1.34.2"}]}}`
	resp, out := postSnapshot(t, ts, "ingest-tok", []byte(body), false)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("push with unknown fields = %d %v, want 202", resp.StatusCode, out)
	}
}

// The gate with ?cluster= judges the manifests' volume plugins with the
// cluster's: the proposed state names both, and neither input changes.
func TestMergeManifestsVolumePlugins(t *testing.T) {
	cluster := glusterInventory()
	manifests := inventory.Inventory{
		Capabilities: map[inventory.Capability]inventory.CapabilityStatus{inventory.CapVolumes: {Available: true}},
		VolumePlugins: []inventory.VolumePluginUse{
			{Plugin: "gitRepo", Count: 1, Namespaces: map[string]int{"shop": 1}, Objects: []inventory.ObjectRef{{Namespace: "shop", Name: "web", Line: 1}}},
			{Plugin: "glusterfs", Count: 1, Namespaces: map[string]int{"shop": 1}, Objects: []inventory.ObjectRef{{Namespace: "shop", Name: "web", Line: 1}}},
		},
	}
	proposed := cluster
	mergeManifests(&proposed, manifests)
	if len(proposed.VolumePlugins) != 2 || proposed.VolumePlugins[0].Plugin != "gitRepo" {
		t.Fatalf("VolumePlugins = %+v, want gitRepo and glusterfs", proposed.VolumePlugins)
	}
	g := proposed.VolumePlugins[1]
	if g.Count != 3 || g.Namespaces["payments-prod"] != 2 || g.Namespaces["shop"] != 1 || len(g.Objects) != 1 {
		t.Errorf("glusterfs = %+v, want the cluster's 2 pods and the manifest's object", g)
	}
	if len(cluster.VolumePlugins) != 1 || cluster.VolumePlugins[0].Count != 2 || len(cluster.VolumePlugins[0].Namespaces) != 1 {
		t.Error("the cluster's inventory must not be modified")
	}
}

// #362: a cluster row that names its PersistentVolumes and StorageClasses
// keeps them, first, beside the posted manifests' located objects, so the
// engine reads the row as mixed (cluster-scoped or no namespace set).
func TestMergeManifestsVolumePluginsWithClusterObjects(t *testing.T) {
	cluster := testInventory()
	cluster.VolumePlugins = []inventory.VolumePluginUse{{Plugin: "rbd", Count: 2, Namespaces: map[string]int{"data": 1, "": 1},
		Objects: []inventory.ObjectRef{{Name: "pv-rbd"}, {Name: "sc-rbd"}}}}
	manifests := inventory.Inventory{
		Capabilities:  map[inventory.Capability]inventory.CapabilityStatus{inventory.CapVolumes: {Available: true}},
		VolumePlugins: []inventory.VolumePluginUse{{Plugin: "rbd", Count: 1, Namespaces: map[string]int{"": 1}, Objects: []inventory.ObjectRef{{Name: "pv-new", File: "pv.yaml", Line: 1}}}},
	}
	proposed := cluster
	mergeManifests(&proposed, manifests)
	want := []inventory.ObjectRef{{Name: "pv-rbd"}, {Name: "sc-rbd"}, {Name: "pv-new", File: "pv.yaml", Line: 1}}
	if len(proposed.VolumePlugins) != 1 || proposed.VolumePlugins[0].Count != 3 || proposed.VolumePlugins[0].Namespaces[""] != 2 ||
		fmt.Sprint(proposed.VolumePlugins[0].Objects) != fmt.Sprint(want) {
		t.Fatalf("VolumePlugins = %+v, want rbd counted 3, 2 with no namespace, objects %+v", proposed.VolumePlugins, want)
	}
	if len(cluster.VolumePlugins[0].Objects) != 2 {
		t.Error("the cluster's inventory must not be modified")
	}
}

// glusterPod is a posted pod naming glusterfs inline, accepted by its own
// ignore annotation.
const glusterPod = `apiVersion: v1
kind: Pod
metadata:
  name: web
  namespace: payments-prod
  annotations:
    upgradescope.basit.engineer/ignore: volume-plugin/glusterfs
    upgradescope.basit.engineer/ignore-reason: moved to CSI in this PR
spec:
  containers: [{name: web, image: nginx}]
  volumes:
    - name: data
      glusterfs: {endpoints: gluster, path: vol}
`

// #362 review: the gate's proposed state adds the PR's located objects to
// the cluster's volume plugin row, whose pods are never listed. The PR's
// object accepted by its annotation leaves the cluster's glusterfs pods
// blocking the proposed state (clusterVerdict), the finding standing with
// them counted omitted, and the PR passes, the remainder being the
// cluster's: one annotation in a PR does not hide the cluster's blocker.
func TestGateAcceptedManifestVolumeKeepsClusterPods(t *testing.T) {
	ts := httptest.NewServer(newTestServer(t, newFakeStore(), func(c *Config) { c.KB = volumesKB() }).Handler())
	defer ts.Close()
	if resp, out := postSnapshot(t, ts, "ingest-tok", pushReqBody(t, glusterInventory()), false); resp.StatusCode != http.StatusAccepted {
		t.Fatalf("seed push = %d %v", resp.StatusCode, out)
	}
	resp, raw := postGate(t, ts, "?target=1.35&cluster=prod-eu-1&path=deploy/pod.yaml", "", glusterPod, "application/x-yaml")
	var b struct {
		Verdict        string `json:"verdict"`
		ClusterVerdict string `json:"clusterVerdict"`
		Findings       []struct {
			Key            string `json:"key"`
			Severity       string `json:"severity"`
			ObjectsOmitted int    `json:"objectsOmitted"`
		} `json:"findings"`
		Suppressed []struct {
			Key string `json:"key"`
		} `json:"suppressed"`
	}
	if err := json.Unmarshal(raw, &b); err != nil {
		t.Fatalf("gate body: %v\n%s", err, raw)
	}
	kept := slices.IndexFunc(b.Findings, func(f struct {
		Key            string `json:"key"`
		Severity       string `json:"severity"`
		ObjectsOmitted int    `json:"objectsOmitted"`
	}) bool {
		return f.Key == "volume-plugin/glusterfs"
	})
	if resp.StatusCode != http.StatusOK || b.Verdict != "ready" || b.ClusterVerdict != "blocked" || kept < 0 ||
		b.Findings[kept].Severity != "blocker" || b.Findings[kept].ObjectsOmitted != 2 ||
		len(b.Suppressed) != 1 || b.Suppressed[0].Key != "volume-plugin/glusterfs" {
		t.Errorf("gate = %d, clusterVerdict %q: want 200 ready (the remainder is the cluster's), the glusterfs blocker kept with the cluster's 2 pods omitted, cluster blocked, and the PR's pod suppressed\n%s",
			resp.StatusCode, b.ClusterVerdict, raw)
	}
}
