package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/abd-ulbasit/upgradescope/internal/inventory"
	"github.com/abd-ulbasit/upgradescope/internal/sarif/sariftest"
)

// rbdPV is a PersistentVolume a pull request adds, on the in-tree rbd
// plugin removed in 1.31.
const rbdPV = `apiVersion: v1
kind: PersistentVolume
metadata:
  name: rbdpv
spec:
  capacity: {storage: 1Gi}
  accessModes: [ReadWriteOnce]
  rbd:
    monitors: ["10.0.0.1:6789"]
    image: foo
`

// gitRepoPod is a pod template a pull request adds with an inline gitRepo
// volume, disabled in 1.33.
const gitRepoPod = `apiVersion: apps/v1
kind: Deployment
metadata: {name: web, namespace: shop}
spec:
  selector: {matchLabels: {app: web}}
  template:
    metadata: {labels: {app: web}}
    spec:
      containers: [{name: web, image: nginx}]
      volumes:
        - name: src
          gitRepo: {repository: "https://example.com/r.git"}
`

// seedLivePVCluster pushes the cluster of the volume-plugins-live-pv
// golden (rbd on a PersistentVolume, an unbound one and a StorageClass,
// three objects) (clusterName prod-eu-1) and returns the server.
func seedLivePVCluster(t *testing.T, mutate ...func(*inventory.Inventory)) *httptest.Server {
	t.Helper()
	raw, err := os.ReadFile("../engine/testdata/volume-plugins-live-pv/inventory.json")
	if err != nil {
		t.Fatal(err)
	}
	var inv inventory.Inventory
	if err := json.Unmarshal(raw, &inv); err != nil {
		t.Fatal(err)
	}
	for _, m := range mutate {
		m(&inv)
	}
	ts := httptest.NewServer(newTestServer(t, newFakeStore(), func(c *Config) { c.KB = volumesKB() }).Handler())
	t.Cleanup(ts.Close)
	if resp, out := postSnapshot(t, ts, "ingest-tok", pushReqBody(t, inv), false); resp.StatusCode != http.StatusAccepted {
		t.Fatalf("seed push = %d %v", resp.StatusCode, out)
	}
	return ts
}

type volumeBlameObject struct {
	Name string `json:"name"`
	File string `json:"file"`
}

type volumeBlameFinding struct {
	Key      string              `json:"key"`
	Source   string              `json:"source"`
	Severity string              `json:"severity"`
	Objects  []volumeBlameObject `json:"objects"`
}

type volumeBlameAnswer struct {
	Verdict        string               `json:"verdict"`
	ClusterVerdict string               `json:"clusterVerdict"`
	Findings       []volumeBlameFinding `json:"findings"`
}

// fullRBDRow is the live-PV cluster's rbd row grown to a listing that is
// already at inventory.MaxObjectRefs, with more pods and volumes behind it
// (a Ceph cluster with 100+ rbd PersistentVolumes).
func fullRBDRow(inv *inventory.Inventory) {
	objs := make([]inventory.ObjectRef, inventory.MaxObjectRefs)
	for i := range objs {
		objs[i] = inventory.ObjectRef{Name: fmt.Sprintf("pv-rbd-%03d", i)}
	}
	for i := range inv.VolumePlugins {
		if inv.VolumePlugins[i].Plugin == "rbd" {
			inv.VolumePlugins[i].Objects = objs
			inv.VolumePlugins[i].Count = 150
			inv.VolumePlugins[i].ObjectsOmitted = 50
		}
	}
}

// #365: with ?cluster=, an in-tree volume plugin a pull request adds is the
// manifests' finding, as a removed API is, however many objects of the
// cluster already use the plugin, listed or not. The finding stays whole (it
// lists the cluster's objects, up to the cap, and the PR's) and the PR is
// blamed; SARIF carries the PR's object only, located at ?path=.
func TestGateClusterVolumePluginBlamedOnManifests(t *testing.T) {
	for _, tc := range []struct {
		name, query, manifest, key, prName string
		mutate                             []func(*inventory.Inventory)
		gitRepo                            bool // the cluster has gitRepo pods too, so the key is the baseline's
		wantCluster                        int  // the cluster's listed objects the finding keeps
	}{
		{name: "an rbd PersistentVolume at 1.31", query: "?target=1.31&cluster=prod-eu-1&path=deploy/pv.yaml", manifest: rbdPV,
			key: "volume-plugin/rbd", prName: "rbdpv", wantCluster: 3},
		{name: "an rbd PersistentVolume at 1.31 beside a full listing", query: "?target=1.31&cluster=prod-eu-1&path=deploy/pv.yaml", manifest: rbdPV,
			key: "volume-plugin/rbd", prName: "rbdpv", mutate: []func(*inventory.Inventory){fullRBDRow}, wantCluster: inventory.MaxObjectRefs - 1},
		{name: "a gitRepo pod template at 1.35", query: "?target=1.35&cluster=prod-eu-1&path=deploy/web.yaml", manifest: gitRepoPod,
			key: "volume-plugin/gitRepo", prName: "web", gitRepo: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ts := seedLivePVCluster(t, tc.mutate...)
			if tc.gitRepo {
				seedGitRepo(t, ts)
			}
			resp, raw := postGate(t, ts, tc.query, "", tc.manifest, "application/x-yaml")
			var b volumeBlameAnswer
			if err := json.Unmarshal(raw, &b); err != nil {
				t.Fatalf("gate body: %v\n%s", err, raw)
			}
			i := slices.IndexFunc(b.Findings, func(f volumeBlameFinding) bool { return f.Key == tc.key })
			if resp.StatusCode != http.StatusUnprocessableEntity || resp.Header.Get("X-Upgradescope-Verdict") != "blocked" ||
				b.Verdict != "blocked" || b.ClusterVerdict != "blocked" || i < 0 || b.Findings[i].Source != sourceManifest {
				t.Fatalf("gate = %d verdict %q/%q: want 422 blocked, clusterVerdict blocked and %s from the manifests\n%s",
					resp.StatusCode, resp.Header.Get("X-Upgradescope-Verdict"), b.Verdict, tc.key, raw)
			}
			// Whole: the PR's object is listed, located at ?path= (the only
			// located one), and so are the cluster's, up to the cap.
			path := strings.SplitN(tc.query, "path=", 2)[1]
			var located []volumeBlameObject
			for _, o := range b.Findings[i].Objects {
				if o.File != "" {
					located = append(located, o)
				}
			}
			if len(located) != 1 || located[0].Name != tc.prName || located[0].File != path ||
				len(b.Findings[i].Objects) != 1+tc.wantCluster {
				t.Errorf("finding lists %d objects, located %+v; want the PR's %s at %s and the cluster's %d", len(b.Findings[i].Objects), located, tc.prName, path, tc.wantCluster)
			}

			resp, raw = postGate(t, ts, tc.query+"&format=sarif", "", tc.manifest, "application/x-yaml")
			sariftest.AssertGitHubAcceptable(t, raw)
			var log struct {
				Runs []struct {
					Results []struct {
						RuleID    string `json:"ruleId"`
						Locations []struct {
							PhysicalLocation struct {
								ArtifactLocation struct {
									URI string `json:"uri"`
								} `json:"artifactLocation"`
							} `json:"physicalLocation"`
						} `json:"locations"`
					} `json:"results"`
				} `json:"runs"`
			}
			if err := json.Unmarshal(raw, &log); err != nil || len(log.Runs) != 1 {
				t.Fatalf("SARIF: %v\n%s", err, raw)
			}
			res := log.Runs[0].Results
			if resp.StatusCode != http.StatusUnprocessableEntity || len(res) != 1 || res[0].RuleID != tc.key ||
				len(res[0].Locations) != 1 || res[0].Locations[0].PhysicalLocation.ArtifactLocation.URI != path {
				t.Errorf("sarif: %d, %d results; want 422 and exactly one, %s at %s\n%s", resp.StatusCode, len(res), tc.key, path, raw)
			}

			// gitlab-codequality places the PR's object at its file (the engine's
			// unlisted-objects summary sits at the synthetic path, and is the
			// cluster's).
			_, raw = postGate(t, ts, tc.query+"&format=gitlab-codequality", "", tc.manifest, "application/x-yaml")
			var issues []struct {
				CheckName string `json:"check_name"`
				Location  struct {
					Path string `json:"path"`
				} `json:"location"`
			}
			if err := json.Unmarshal(raw, &issues); err != nil {
				t.Fatalf("gitlab-codequality: %v\n%s", err, raw)
			}
			if at := slices.IndexFunc(issues, func(is struct {
				CheckName string `json:"check_name"`
				Location  struct {
					Path string `json:"path"`
				} `json:"location"`
			}) bool {
				return is.CheckName == tc.key && is.Location.Path == path
			}); at < 0 {
				t.Errorf("gitlab-codequality has no %s at %q\n%s", tc.key, path, raw)
			}
		})
	}
}

// seedGitRepo adds a gitRepo row to the cluster (a later snapshot).
func seedGitRepo(t *testing.T, ts *httptest.Server) {
	t.Helper()
	raw, err := os.ReadFile("../engine/testdata/volume-plugins-live-pv/inventory.json")
	if err != nil {
		t.Fatal(err)
	}
	var inv inventory.Inventory
	if err := json.Unmarshal(raw, &inv); err != nil {
		t.Fatal(err)
	}
	inv.CollectedAt = inv.CollectedAt.Add(time.Minute)
	inv.VolumePlugins = append(inv.VolumePlugins, inventory.VolumePluginUse{Plugin: "gitRepo", Count: 2, Namespaces: map[string]int{"shop": 2}})
	if resp, out := postSnapshot(t, ts, "ingest-tok", pushReqBody(t, inv), false); resp.StatusCode != http.StatusAccepted {
		t.Fatalf("seed push = %d %v", resp.StatusCode, out)
	}
}

// #365: a pull request that adds nothing on a plugin the cluster uses
// passes, and the cluster's blocker stays in clusterVerdict.
func TestGateClusterVolumePluginNotBlamedOnUnrelatedManifest(t *testing.T) {
	ts := seedLivePVCluster(t)
	cm := "apiVersion: v1\nkind: ConfigMap\nmetadata: {name: settings, namespace: shop}\n"
	resp, raw := postGate(t, ts, "?target=1.31&cluster=prod-eu-1&path=deploy/cm.yaml", "", cm, "application/x-yaml")
	var b volumeBlameAnswer
	if err := json.Unmarshal(raw, &b); err != nil {
		t.Fatalf("gate body: %v\n%s", err, raw)
	}
	src := ""
	for _, f := range b.Findings {
		if f.Key == "volume-plugin/rbd" {
			src = f.Source
		}
	}
	if resp.StatusCode != http.StatusOK || resp.Header.Get("X-Upgradescope-Verdict") != "ready" || b.ClusterVerdict != "blocked" || src != sourceCluster {
		t.Errorf("gate = %d verdict %q, clusterVerdict %q, rbd from %q: want 200 ready, cluster blocked, the rbd finding the cluster's\n%s",
			resp.StatusCode, resp.Header.Get("X-Upgradescope-Verdict"), b.ClusterVerdict, src, raw)
	}
}

// #365: the PR's volume plugin object carries ?path= without ?cluster= too,
// so SARIF holds it as a result at the PR's file (an object with no file is
// not a result).
func TestGateVolumePluginSARIFLocatedAtPath(t *testing.T) {
	ts := httptest.NewServer(newTestServer(t, newFakeStore(), func(c *Config) { c.KB = volumesKB() }).Handler())
	defer ts.Close()
	resp, raw := postGate(t, ts, "?target=1.31&path=deploy/pv.yaml&format=sarif", "", rbdPV, "application/x-yaml")
	sariftest.AssertGitHubAcceptable(t, raw)
	if got := sarifResults(t, raw)["volume-plugin/rbd"]; resp.StatusCode != http.StatusUnprocessableEntity || got != 1 {
		t.Errorf("sarif: %d, %d rbd results; want 422 and one at the PR's file\n%s", resp.StatusCode, got, raw)
	}
}
