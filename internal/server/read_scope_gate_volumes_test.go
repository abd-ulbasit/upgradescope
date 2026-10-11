package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/abd-ulbasit/upgradescope/internal/inventory"
	"github.com/abd-ulbasit/upgradescope/internal/kb"
)

// clusterShare keeps of the in-tree volume plugin rows (#351, #362) the
// scope's namespaces' counts only, summed with saturation; it drops the
// cluster-scoped counts (unbound PersistentVolumes, StorageClasses), the
// rows left with no count, the rows that cannot be divided, and every ref
// outside the scope's namespaces, live PersistentVolume and StorageClass
// refs included (they carry no namespace); and the rest of the scope's
// count is omitted. The fleet-wide share is the cluster.
func TestClusterShareVolumePlugins(t *testing.T) {
	inv := testInventory()
	inv.Namespaces = []inventory.NamespaceInfo{
		{Name: "a-prod", Team: "alpha"}, {Name: "a-dev", Team: "alpha"}, {Name: "b-prod", Team: "beta"}, {Name: "ops"},
	}
	inv.VolumePlugins = []inventory.VolumePluginUse{
		{Plugin: "rbd", Count: 8, Namespaces: map[string]int{"a-prod": 2, "a-dev": 0, "b-prod": 3, "ops": 1, "": 2},
			Objects: []inventory.ObjectRef{
				{Namespace: "a-prod", Name: "a-ref"}, {Namespace: "b-prod", Name: "b-ref"}, {Name: "pv-b-bound"}, {Name: "sc-rbd"},
			}, ObjectsOmitted: 4},
		{Plugin: "glusterfs", Count: 2, Namespaces: map[string]int{"b-prod": 2}},
		{Plugin: "cephfs", Count: 1, Namespaces: map[string]int{"": 1}, Objects: []inventory.ObjectRef{{Name: "sc-ceph"}}},
		{Plugin: "flocker", Count: 3}, // no per-namespace counts: undivided
		{Plugin: "quobyte", Count: 1, Namespaces: map[string]int{"a-prod": 0, "a-dev": -1}}, // nothing counted in scope
		{Plugin: "portworxVolume", Count: math.MaxInt, Namespaces: map[string]int{"a-prod": math.MaxInt - 1, "a-dev": 5}},
	}
	before, _ := json.Marshal(inv)

	if got, _ := json.Marshal(fleetScope.clusterShare(inv)); !bytes.Equal(got, before) {
		t.Errorf("the fleet-wide share is not the cluster")
	}
	share := scopeOfTeams([]string{"alpha"}).clusterShare(inv)
	if after, _ := json.Marshal(inv); !bytes.Equal(after, before) {
		t.Errorf("clusterShare modified its input")
	}
	got, _ := json.Marshal(share.VolumePlugins)
	for _, s := range []string{"b-ref", "pv-b-bound", "sc-rbd", "sc-ceph", "b-prod", `"ops"`, `""`, "glusterfs", "cephfs", "flocker", "quobyte", "a-dev\":0"} {
		if bytes.Contains(got, []byte(s)) {
			t.Errorf("alpha's volume plugins name %s: %s", s, got)
		}
	}
	want := []inventory.VolumePluginUse{
		{Plugin: "rbd", Count: 2, Namespaces: map[string]int{"a-prod": 2}, Objects: []inventory.ObjectRef{{Namespace: "a-prod", Name: "a-ref"}}, ObjectsOmitted: 1},
		{Plugin: "portworxVolume", Count: math.MaxInt, Namespaces: map[string]int{"a-prod": math.MaxInt - 1, "a-dev": 5}, ObjectsOmitted: math.MaxInt},
	}
	if w, _ := json.Marshal(want); !bytes.Equal(got, w) {
		t.Errorf("alpha's volume plugins =\n%s\nwant\n%s", got, w)
	}
	if share := scopeOfTeams([]string{"gamma"}).clusterShare(inv); share.VolumePlugins != nil {
		t.Errorf("a team with no namespace has volume plugins %+v", share.VolumePlugins)
	}
}

// volumeTeamsInventory is a cluster of alpha (a-prod) and beta (b-prod)
// whose rbd row counts beta's PersistentVolumes, bound to claims in
// b-prod, and an rbd StorageClass (cluster scoped), the live refs naming
// them carrying no namespace, as the agent reports them (#362); alphaPods
// adds that many of alpha's pods on rbd in a-prod, and betaPVs is how
// many PersistentVolumes beta has (named after variant). With neither,
// the cluster has no rbd row at all.
func volumeTeamsInventory(variant string, alphaPods, betaPVs int) inventory.Inventory {
	inv := testInventory()
	inv.Namespaces = []inventory.NamespaceInfo{{Name: "a-prod", Team: "alpha"}, {Name: "b-prod", Team: "beta"}}
	if alphaPods == 0 && betaPVs == 0 {
		return inv
	}
	row := inventory.VolumePluginUse{Plugin: "rbd", Namespaces: map[string]int{"": 1}}
	for i := range betaPVs {
		row.Objects = append(row.Objects, inventory.ObjectRef{Name: fmt.Sprintf("pv-beta-%s%d", variant, i)})
	}
	row.Objects = append(row.Objects, inventory.ObjectRef{Name: "sc-beta-rbd-" + variant})
	row.Namespaces["b-prod"] = betaPVs
	if alphaPods > 0 {
		row.Namespaces["a-prod"] = alphaPods
	}
	row.Count = alphaPods + betaPVs + 1
	inv.VolumePlugins = []inventory.VolumePluginUse{row}
	return inv
}

// rbdTeamsManifest is alpha's PR: a Deployment on rbd in alpha's
// namespace, one in a namespace the cluster does not have, and a
// PersistentVolume (cluster scoped) on rbd. All of it is the caller's own.
const rbdTeamsManifest = `apiVersion: apps/v1
kind: Deployment
metadata: {name: pr-web, namespace: a-prod}
spec:
  selector: {matchLabels: {app: web}}
  template:
    metadata: {labels: {app: web}}
    spec:
      containers: [{name: web, image: nginx}]
      volumes: [{name: data, rbd: {monitors: ["10.0.0.1:6789"], image: web}}]
---
apiVersion: apps/v1
kind: Deployment
metadata: {name: pr-new, namespace: pr-new-ns}
spec:
  selector: {matchLabels: {app: new}}
  template:
    metadata: {labels: {app: new}}
    spec:
      containers: [{name: new, image: nginx}]
      volumes: [{name: data, rbd: {monitors: ["10.0.0.1:6789"], image: new}}]
---
apiVersion: v1
kind: PersistentVolume
metadata: {name: pr-pv}
spec:
  capacity: {storage: 1Gi}
  accessModes: [ReadWriteOnce]
  rbd: {monitors: ["10.0.0.1:6789"], image: pv}
`

// What only beta's evidence says: none of it may reach an alpha-scoped
// answer.
var volumeTeamsSecrets = []string{"pv-beta", "sc-beta-rbd", "b-prod", `"beta"`}

// A team-scoped gate with ?cluster= counts, titles and details the
// in-tree volume plugin finding a PR introduces from the scope's share
// and the PR alone (#72, #365): beta's PersistentVolumes and the
// cluster's StorageClass never reach alpha's answer, whether alpha has rbd
// pods of its own or none, so clusters that differ only in beta's
// evidence answer alpha byte for byte the same, in every format, and a
// cluster where only beta uses rbd answers alpha as one where nobody
// does: its cluster verdict and score are alpha's. The fleet-wide token
// sees beta's.
func TestScopedGateVolumePluginsAreTheScopesShare(t *testing.T) {
	_, st, ts, _ := scopeServer(t, func(c *Config) { c.KB.VolumePlugins = kb.VolumePlugins() })
	mintReadToken(t, st, "alpha-tok", "alpha")
	for _, c := range []struct {
		name               string
		alphaPods, betaPVs int
	}{{"vol-a1", 1, 2}, {"vol-a1b", 1, 5}, {"vol-a0", 0, 2}, {"vol-a0b", 0, 5}, {"vol-none", 0, 0}} {
		pushScopeCluster(t, ts, c.name, volumeTeamsInventory(strings.TrimPrefix(c.name, "vol-"), c.alphaPods, c.betaPVs))
	}
	const configMap = "apiVersion: v1\nkind: ConfigMap\nmetadata: {name: settings, namespace: a-prod}\n"
	for _, manifest := range []string{rbdTeamsManifest, configMap} {
		for _, format := range gateFormats {
			q := "?target=1.35&path=deploy/app.yaml" + format + "&cluster="
			for _, same := range [][]string{{"vol-a1", "vol-a1b"}, {"vol-a0", "vol-a0b", "vol-none"}} {
				respA, a := postGate(t, ts, q+same[0], "alpha-tok", manifest, "application/x-yaml")
				for _, other := range same {
					resp, raw := postGate(t, ts, q+other, "alpha-tok", manifest, "application/x-yaml")
					if resp.StatusCode != respA.StatusCode || resp.Header.Get("X-Upgradescope-Verdict") != respA.Header.Get("X-Upgradescope-Verdict") ||
						!bytes.Equal(normUID(a), normUID(raw)) {
						t.Errorf("alpha-scoped gate %s differs between %s (%d) and %s (%d), which differ only in beta's evidence:\n%s\n%s",
							q, same[0], respA.StatusCode, other, resp.StatusCode, a, raw)
					}
					for _, s := range volumeTeamsSecrets {
						if bytes.Contains(raw, []byte(s)) {
							t.Errorf("alpha-scoped gate %s names %s:\n%s", q+other, s, raw)
						}
					}
				}
			}
		}
	}

	// What alpha reads of the PR's finding: its own pods and the PR's
	// objects, the PR blamed.
	type finding struct {
		Key, Source, Title, Detail string
		Teams, Namespaces          []string
		Objects                    []inventory.ObjectRef
		ObjectsOmitted             int
	}
	var prNames = []string{"pr-new", "pr-pv", "pr-web"}
	for _, tc := range []struct {
		cluster, title, where string
		omitted               int
	}{
		{"vol-a1", "(4 objects)", "no namespace set (1), a-prod (2), pr-new-ns (1)", 1},
		{"vol-a0", "(3 objects)", "no namespace set (1), a-prod (1), pr-new-ns (1)", 0},
	} {
		resp, raw := postGate(t, ts, "?target=1.35&path=deploy/app.yaml&cluster="+tc.cluster, "alpha-tok", rbdTeamsManifest, "application/x-yaml")
		var body struct {
			Verdict  string    `json:"verdict"`
			Findings []finding `json:"findings"`
		}
		if err := json.Unmarshal(raw, &body); err != nil {
			t.Fatal(err)
		}
		i := slices.IndexFunc(body.Findings, func(f finding) bool { return f.Key == "volume-plugin/rbd" })
		if resp.StatusCode != http.StatusUnprocessableEntity || body.Verdict != "blocked" || i < 0 || body.Findings[i].Source != sourceManifest {
			t.Fatalf("%s: gate = %d %s, want 422 blocked with volume-plugin/rbd from the manifests\n%s", tc.cluster, resp.StatusCode, body.Verdict, raw)
		}
		f := body.Findings[i]
		var names []string
		for _, o := range f.Objects {
			names = append(names, o.Name)
			if o.File != "deploy/app.yaml" {
				t.Errorf("%s: object %+v is not located at the PR's file", tc.cluster, o)
			}
		}
		slices.Sort(names)
		if !slices.Equal(names, prNames) || f.ObjectsOmitted != tc.omitted || !strings.HasSuffix(f.Title, tc.title) ||
			!slices.Equal(f.Teams, []string{"alpha"}) || !slices.Equal(f.Namespaces, []string{"a-prod", "pr-new-ns"}) {
			t.Errorf("%s: objects %v (+%d), title %q, teams %v, namespaces %v; want the PR's %v (+%d), %q, [alpha], [a-prod pr-new-ns]",
				tc.cluster, names, f.ObjectsOmitted, f.Title, f.Teams, f.Namespaces, prNames, tc.omitted, tc.title)
		}
		if !strings.Contains(f.Detail, tc.where) {
			t.Errorf("%s: detail %q does not count %s", tc.cluster, f.Detail, tc.where)
		}
		if strings.Contains(f.Title, "in scope") || strings.Contains(f.Detail, "Cut to this read's teams") {
			t.Errorf("%s: the finding was cut after the fact (%q, %q): it should be evaluated from the scope's share", tc.cluster, f.Title, f.Detail)
		}
	}

	// Where only beta uses rbd, alpha's cluster is ready with a PR that
	// adds no rbd, as one where nobody does; the whole cluster is not.
	resp, raw := postGate(t, ts, "?target=1.35&cluster=vol-a0", "alpha-tok", configMap, "application/x-yaml")
	share := summarize(t, resp, raw)
	resp, raw = postGate(t, ts, "?target=1.35&cluster=vol-none", "alpha-tok", configMap, "application/x-yaml")
	none := summarize(t, resp, raw)
	if share.ClusterVerdict != "ready" || share.Score != none.Score || share.Status != none.Status || len(share.Findings) != 0 {
		t.Errorf("alpha-scoped gate where only beta uses rbd: %+v; want ready, as where nobody does: %+v", share, none)
	}
	resp, raw = postGate(t, ts, "?target=1.35&cluster=vol-a0", "fleet-tok", configMap, "application/x-yaml")
	if whole := summarize(t, resp, raw); whole.ClusterVerdict != "blocked" || whole.Score >= none.Score {
		t.Errorf("fleet-wide gate where beta uses rbd: %+v; want the cluster blocked: the fixture does not test the scope", whole)
	}
	_, fleet := postGate(t, ts, "?target=1.35&path=deploy/app.yaml&cluster=vol-a1", "fleet-tok", rbdTeamsManifest, "application/x-yaml")
	for _, s := range []string{"pv-beta-a10", "sc-beta-rbd-a1", "b-prod (2)"} {
		if !bytes.Contains(fleet, []byte(s)) {
			t.Errorf("fleet-wide gate does not show beta's %s: the fixture does not test the scope:\n%s", s, fleet)
		}
	}
}

// What a gate's manifests hold includes their in-tree volume plugin
// rows' refs and namespaces, which a team-scoped answer keeps as the
// caller's own (#365), whatever API usage lists.
func TestManifestsOwnVolumePlugins(t *testing.T) {
	pv := inventory.ObjectRef{Name: "pr-pv", File: "deploy/pv.yaml", Line: 3}
	web := inventory.ObjectRef{Namespace: "pr-ns", Name: "web", File: "deploy/web.yaml", Line: 1}
	m := manifestsOwn(inventory.Inventory{VolumePlugins: []inventory.VolumePluginUse{
		{Plugin: "rbd", Count: 2, Namespaces: map[string]int{"": 1, "pr-ns": 1}, Objects: []inventory.ObjectRef{pv, web}},
	}})
	if !m.objects[pv] || !m.objects[web] || !m.namespaces["pr-ns"] || m.namespaces[""] {
		t.Errorf("manifestsOwn = objects %v, namespaces %v; want both refs and pr-ns", m.objects, m.namespaces)
	}
}
