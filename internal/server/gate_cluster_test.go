package server

import (
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/abd-ulbasit/upgradescope/internal/engine"
	"github.com/abd-ulbasit/upgradescope/internal/inventory"
	"github.com/abd-ulbasit/upgradescope/internal/kb"
)

// callersKB is testKB plus batch/v1beta1 CronJob, removed in 1.25.
func callersKB() kb.KB {
	k := testKB()
	deprecated, removed := inventory.Version{Major: 1, Minor: 21}, inventory.Version{Major: 1, Minor: 25}
	k.APILifecycle = append(k.APILifecycle, kb.APILifecycleEntry{
		Group: "batch", Version: "v1beta1", Kind: "CronJob",
		Introduced: inventory.Version{Major: 1, Minor: 8}, Deprecated: &deprecated, Removed: &removed,
	})
	return k
}

// callersCluster serves a gate whose cluster prod-eu-1 (v1.24) is the
// engine's removed-api-with-callers golden: two batch/v1beta1 CronJobs
// written through the old API (default/nightly, shop/reports), with
// apiserver caller rows for cronjobs and cronjobs/status folded into
// their finding, and a standalone policy/v1beta1 poddisruptionbudgets
// caller. Target 1.25 removes both APIs.
func callersCluster(t *testing.T) *httptest.Server {
	t.Helper()
	raw, err := os.ReadFile("../engine/testdata/removed-api-with-callers/inventory.json")
	if err != nil {
		t.Fatal(err)
	}
	var inv inventory.Inventory
	if err := json.Unmarshal(raw, &inv); err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(newTestServer(t, newFakeStore(), func(c *Config) { c.KB = callersKB() }).Handler())
	t.Cleanup(ts.Close)
	if resp, out := postSnapshot(t, ts, "ingest-tok", pushReqBody(t, inv), false); resp.StatusCode != http.StatusAccepted {
		t.Fatalf("seed push = %d %v", resp.StatusCode, out)
	}
	return ts
}

// gateSources maps each finding key of a gate answer to its source.
func gateSources(b failOnGateResponse) map[string]string {
	m := map[string]string{}
	for _, f := range b.Findings {
		m[f.Key] = f.Source
	}
	return m
}

const newCronJob = `apiVersion: batch/v1beta1
kind: CronJob
metadata:
  name: new-job
  namespace: shop
`

// #120 SV-06: a PR that adds an object at a removed API is blocked even
// when the cluster already has objects at that API — attribution is per
// object, not per key.
func TestGateClusterNewObjectOfResidentGVK(t *testing.T) {
	ts := callersCluster(t)
	resp, raw := postGate(t, ts, "?target=1.25&cluster=prod-eu-1&fail-on=blocker", "", newCronJob, "application/x-yaml")
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422 (body %s)", resp.StatusCode, raw)
	}
	b := decodeGate(t, raw)
	if b.Verdict != "blocked" || gateSources(b)["removed-api/batch/v1beta1/CronJob"] != "manifest" {
		t.Errorf("verdict %q sources %v, want blocked with the CronJob blocker from the manifest", b.Verdict, gateSources(b))
	}
}

// #120 SV-06: a harmless PR is not blocked by the cluster's own deprecated
// API callers. The manifests are merged into the cluster's API usage, not
// substituted for it, so the caller rows fold into the same CronJob
// finding in the baseline and in the proposed state.
func TestGateClusterBenignManifestKeepsFold(t *testing.T) {
	ts := callersCluster(t)
	cm := "apiVersion: v1\nkind: ConfigMap\nmetadata: {name: settings, namespace: shop}\n"
	resp, raw := postGate(t, ts, "?target=1.25&cluster=prod-eu-1&fail-on=blocker", "", cm, "application/x-yaml")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", resp.StatusCode, raw)
	}
	b := decodeGate(t, raw)
	if b.Verdict != "ready" || b.ClusterVerdict != "blocked" {
		t.Errorf("verdict %q clusterVerdict %q, want ready / blocked", b.Verdict, b.ClusterVerdict)
	}
	for key, src := range gateSources(b) {
		if src != "cluster" {
			t.Errorf("finding %s is tagged %s, want every finding from the cluster", key, src)
		}
	}
	if _, ok := gateSources(b)["deprecated-api-in-use/batch/v1beta1/cronjobs"]; ok {
		t.Errorf("findings %v: the cronjobs caller rows must stay folded into the CronJob finding", gateSources(b))
	}
}

// A manifest object at a removed API is always introduced by the PR (the
// work package's rule; #120's criterion of the same name says source:
// cluster, and is to be updated to match), also when it re-renders an
// object the cluster already has. It replaces that object (no double
// count), and the gate blocks until the manifest is migrated: in a GitOps
// repository that renders everything on every PR, every PR fails until
// the legacy object is migrated.
func TestGateClusterRerenderedObject(t *testing.T) {
	ts := callersCluster(t)
	reports := "apiVersion: batch/v1beta1\nkind: CronJob\nmetadata: {name: reports, namespace: shop}\n"
	resp, raw := postGate(t, ts, "?target=1.25&cluster=prod-eu-1&fail-on=blocker", "", reports, "application/x-yaml")
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422 (body %s)", resp.StatusCode, raw)
	}
	var b struct {
		Verdict  string `json:"verdict"`
		Findings []struct {
			Key, Title, Source string
		} `json:"findings"`
	}
	if err := json.Unmarshal(raw, &b); err != nil {
		t.Fatal(err)
	}
	for _, f := range b.Findings {
		if f.Key == "removed-api/batch/v1beta1/CronJob" && (f.Source != "manifest" || !strings.Contains(f.Title, "(2 objects)")) {
			t.Errorf("CronJob finding = %+v, want source manifest over 2 objects (shop/reports upserted, not added)", f)
		}
	}

	// Migrated to batch/v1: nothing the PR introduces is removed.
	migrated := strings.Replace(reports, "batch/v1beta1", "batch/v1", 1)
	resp, raw = postGate(t, ts, "?target=1.25&cluster=prod-eu-1&fail-on=blocker", "", migrated, "application/x-yaml")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("migrated: status = %d, want 200 (body %s)", resp.StatusCode, raw)
	}
}

// The proposed state's row lists every cluster ref and every manifest
// ref, so the engine and suppression see them all; the answer's
// findings are cut to the MaxObjectRefs cap after suppression
// (capObjects), the cluster's refs making room for the manifests',
// which SARIF needs to place results. Counts stay exact.
func TestUpsertUsageKeepsManifestRefs(t *testing.T) {
	cluster := inventory.APIUsage{Group: "batch", Version: "v1beta1", Kind: "CronJob", Namespaces: map[string]int{"shop": inventory.MaxObjectRefs + 2}}
	for i := range inventory.MaxObjectRefs {
		cluster.Objects = append(cluster.Objects, inventory.ObjectRef{Namespace: "shop", Name: "job-" + strconv.Itoa(i)})
	}
	cluster.Count, cluster.ObjectsOmitted = inventory.MaxObjectRefs+2, 2
	manifest := inventory.APIUsage{Group: "batch", Version: "v1beta1", Kind: "CronJob", Count: 2, Namespaces: map[string]int{"shop": 2},
		Objects: []inventory.ObjectRef{{Namespace: "shop", Name: "job-0", Line: 1}, {Namespace: "shop", Name: "new", Line: 6}}}

	got := upsertUsage([]inventory.APIUsage{cluster}, []inventory.APIUsage{manifest})
	if len(got) != 1 {
		t.Fatalf("rows = %+v, want one", got)
	}
	u := got[0]
	// job-0 is replaced, new is added: 102 - 1 + 2, of which the 99
	// cluster refs left and both manifest refs are listed.
	if u.Count != inventory.MaxObjectRefs+3 || u.Namespaces["shop"] != u.Count || len(u.Objects) != inventory.MaxObjectRefs+1 ||
		len(u.Objects)+u.ObjectsOmitted != u.Count {
		t.Errorf("count %d namespaces %v objects %d omitted %d, want %d in all, %d listed", u.Count, u.Namespaces, len(u.Objects), u.ObjectsOmitted, inventory.MaxObjectRefs+3, inventory.MaxObjectRefs+1)
	}
	if last := u.Objects[len(u.Objects)-2:]; last[0].Line != 1 || last[1].Line != 6 {
		t.Errorf("listed objects end %+v, want both manifest refs", last)
	}
	if len(cluster.Objects) != inventory.MaxObjectRefs || cluster.Namespaces["shop"] != inventory.MaxObjectRefs+2 {
		t.Error("the cluster's inventory must not be modified")
	}

	mine := map[inventory.ObjectRef]bool{manifest.Objects[0]: true, manifest.Objects[1]: true}
	f := engine.Finding{Key: "k", Objects: u.Objects, ObjectsOmitted: u.ObjectsOmitted}
	rep := engine.Report{Findings: []engine.Finding{f}, Suppressed: []engine.SuppressedFinding{{Finding: f}}}
	capped := capObjects(rep, mine)
	for _, c := range []engine.Finding{capped.Findings[0], capped.Suppressed[0].Finding} {
		if len(c.Objects) != inventory.MaxObjectRefs || c.ObjectsOmitted != 3 {
			t.Errorf("capped to %d listed (+%d), want %d (+3)", len(c.Objects), c.ObjectsOmitted, inventory.MaxObjectRefs)
		}
		if last := c.Objects[len(c.Objects)-2:]; !mine[last[0]] || !mine[last[1]] || c.Objects[len(c.Objects)-3].Name != "job-98" {
			t.Errorf("capped listing ends %+v, want job-98 and both manifest refs", c.Objects[len(c.Objects)-3:])
		}
	}
	if len(rep.Findings[0].Objects) != inventory.MaxObjectRefs+1 || len(rep.Suppressed[0].Objects) != inventory.MaxObjectRefs+1 {
		t.Error("capObjects must not modify its report")
	}
}

// A cluster named "1" is found by its name, not mistaken for cluster id 1.
func TestGateClusterNumericName(t *testing.T) {
	st := newFakeStore()
	ts := httptest.NewServer(newTestServer(t, st).Handler())
	defer ts.Close()
	first := testInventory() // uid-123, pushed as prod-eu-1: id 1
	if resp, out := postSnapshot(t, ts, "ingest-tok", pushReqBody(t, first), false); resp.StatusCode != http.StatusAccepted {
		t.Fatalf("seed push = %d %v", resp.StatusCode, out)
	}
	second := testInventory()
	second.ClusterID = "uid-456"
	body := strings.Replace(string(pushReqBody(t, second)), `"clusterName":"prod-eu-1"`, `"clusterName":"1"`, 1)
	if resp, out := postSnapshot(t, ts, "ingest-tok", []byte(body), false); resp.StatusCode != http.StatusAccepted {
		t.Fatalf("seed push = %d %v", resp.StatusCode, out)
	}

	clusters, err := st.ListClusters(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	ids := map[string]string{}
	for _, c := range clusters {
		ids[c.Name] = strconv.FormatInt(c.ID, 10)
	}
	if ids["prod-eu-1"] != "1" {
		t.Fatalf("cluster ids = %v, want prod-eu-1 to be id 1", ids)
	}
	// An id still resolves when no cluster has it as its name.
	for ref, want := range map[string]string{"1": "uid-456", "prod-eu-1": "uid-123", ids["1"]: "uid-456"} {
		resp, raw := postGate(t, ts, "?target=1.35&cluster="+ref, "", deploymentManifest, "application/x-yaml")
		var b struct {
			ClusterID string `json:"clusterId"`
		}
		if err := json.Unmarshal(raw, &b); err != nil || resp.StatusCode != http.StatusOK || b.ClusterID != want {
			t.Errorf("cluster=%s: status %d clusterId %q (%v), want %s", ref, resp.StatusCode, b.ClusterID, err, want)
		}
	}
}

// #361: the gate adds the posted manifests' counts to the cluster's with
// a saturating add, so a cluster row at math.MaxInt never wraps negative
// (the engine skips rows with Count <= 0, which would drop the cluster's
// own finding from the proposed state). A table over API usage and volume
// plugins: the Count, a namespace's count and ObjectsOmitted at MaxInt
// stay MaxInt, and the engine at a target where the API or plugin is
// removed still reports the cluster's blocker.
func TestGateCountsSaturate(t *testing.T) {
	const maxInt = math.MaxInt
	base := func() inventory.Inventory {
		inv := testInventory()
		inv.Source = inventory.SourceCluster
		inv.ServerVersion = "v1.24.3"
		return inv
	}
	k := callersKB()
	k.VolumePlugins = kb.VolumePlugins()

	type usageRow struct{ count, ns, omitted int }
	for _, tc := range []struct {
		name            string
		cluster         usageRow
		manifest        usageRow
		wantNS, wantOmt int
	}{
		{"count", usageRow{maxInt, 1, 0}, usageRow{1, 1, 0}, 2, 0},
		{"namespace", usageRow{1, maxInt, 0}, usageRow{1, 1, 0}, maxInt, 0},
		{"objects omitted", usageRow{1, 1, maxInt}, usageRow{1, 1, 1}, 2, maxInt},
		{"all three", usageRow{maxInt, maxInt, maxInt}, usageRow{maxInt, maxInt, maxInt}, maxInt, maxInt},
	} {
		wantCount := maxInt
		if tc.cluster.count != maxInt {
			wantCount = tc.cluster.count + tc.manifest.count
		}
		check := func(t *testing.T, count int, ns map[string]int, omitted int, nsKey string) {
			t.Helper()
			if count != wantCount || count <= 0 {
				t.Errorf("count = %d, want %d (positive)", count, wantCount)
			}
			if ns[nsKey] != tc.wantNS || omitted != tc.wantOmt {
				t.Errorf("namespace count %d, omitted %d, want %d and %d", ns[nsKey], omitted, tc.wantNS, tc.wantOmt)
			}
		}

		t.Run("api usage/"+tc.name, func(t *testing.T) {
			cluster := inventory.APIUsage{Group: "batch", Version: "v1beta1", Kind: "CronJob", Count: tc.cluster.count,
				Namespaces: map[string]int{"shop": tc.cluster.ns}, ObjectsOmitted: tc.cluster.omitted}
			manifest := inventory.APIUsage{Group: "batch", Version: "v1beta1", Kind: "CronJob", Count: tc.manifest.count,
				Namespaces: map[string]int{"shop": tc.manifest.ns}, ObjectsOmitted: tc.manifest.omitted,
				Objects: []inventory.ObjectRef{{Namespace: "shop", Name: "new", Line: 1}}}
			got := upsertUsage([]inventory.APIUsage{cluster}, []inventory.APIUsage{manifest})
			if len(got) != 1 {
				t.Fatalf("rows = %+v, want one", got)
			}
			check(t, got[0].Count, got[0].Namespaces, got[0].ObjectsOmitted, "shop")
			inv := base()
			inv.APIUsage = got
			r := engine.Evaluate(inv, k, inventory.Version{Major: 1, Minor: 25}, time.Now())
			if r.Verdict != engine.VerdictBlocked || !slices.ContainsFunc(r.Findings, func(f engine.Finding) bool {
				return f.Category == engine.CatRemovedAPI && f.Severity == engine.SevBlocker
			}) {
				t.Errorf("verdict %s, findings %+v: want the cluster's removed-API blocker kept", r.Verdict, r.Findings)
			}
		})

		t.Run("volume plugin/"+tc.name, func(t *testing.T) {
			cluster := inventory.VolumePluginUse{Plugin: "rbd", Count: tc.cluster.count,
				Namespaces: map[string]int{"shop": tc.cluster.ns}, ObjectsOmitted: tc.cluster.omitted}
			manifest := inventory.VolumePluginUse{Plugin: "rbd", Count: tc.manifest.count,
				Namespaces: map[string]int{"shop": tc.manifest.ns}, ObjectsOmitted: tc.manifest.omitted,
				Objects: []inventory.ObjectRef{{Namespace: "shop", Name: "new", Line: 1}}}
			got := mergeVolumePlugins([]inventory.VolumePluginUse{cluster}, []inventory.VolumePluginUse{manifest})
			if len(got) != 1 {
				t.Fatalf("rows = %+v, want one", got)
			}
			check(t, got[0].Count, got[0].Namespaces, got[0].ObjectsOmitted, "shop")
			inv := base()
			inv.VolumePlugins = got
			r := engine.Evaluate(inv, k, inventory.Version{Major: 1, Minor: 31}, time.Now())
			if !slices.ContainsFunc(r.Findings, func(f engine.Finding) bool {
				return f.Category == engine.CatVolumePlugin && f.Key == "volume-plugin/rbd" && f.Severity == engine.SevBlocker
			}) {
				t.Errorf("verdict %s, findings %+v: want the cluster's rbd blocker kept", r.Verdict, r.Findings)
			}
		})
	}
}

// #361: the decrement of a replaced cluster object never takes a count
// below zero, even when the stored row's counts are lower than the refs
// it lists, and a namespace at zero is dropped as before.
func TestUpsertUsageDecrementFloorsAtZero(t *testing.T) {
	cluster := inventory.APIUsage{Group: "batch", Version: "v1beta1", Kind: "CronJob", Count: 0,
		Namespaces: map[string]int{"shop": 0}, Objects: []inventory.ObjectRef{{Namespace: "shop", Name: "a"}}}
	manifest := inventory.APIUsage{Group: "batch", Version: "v1beta1", Kind: "CronJob", Count: 0,
		Objects: []inventory.ObjectRef{{Namespace: "shop", Name: "a", Line: 1}}}
	got := upsertUsage([]inventory.APIUsage{cluster}, []inventory.APIUsage{manifest})
	if got[0].Count != 0 {
		t.Errorf("count = %d, want 0, never negative", got[0].Count)
	}
	if _, ok := got[0].Namespaces["shop"]; ok {
		t.Errorf("namespaces = %v, want the emptied namespace dropped", got[0].Namespaces)
	}
}
