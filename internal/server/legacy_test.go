package server

import (
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/abd-ulbasit/upgradescope/internal/inventory"
	"github.com/abd-ulbasit/upgradescope/internal/kb"
	"github.com/abd-ulbasit/upgradescope/registry"
)

// legacyKB is testKB plus Argo CD release lines (2.10 ended, 3.1
// supported) and the flowcontrol v1beta2 APIs removed in 1.29.
func legacyKB() kb.KB {
	k := testKB()
	cite := []string{"https://endoflife.date/argo-cd"}
	k.AddOns = []registry.AddOn{{
		SchemaVersion: 2, ID: "argo-cd", DisplayName: "Argo CD",
		Support: registry.Support{Status: "supported", Citations: cite},
		Cycles: []registry.Cycle{
			{Cycle: "3.1", EOL: &registry.CycleEOL{Date: "2027-05-01"}, Citations: cite},
			{Cycle: "2.10", EOL: &registry.CycleEOL{Date: "2025-05-06"}, Citations: cite},
		},
	}}
	deprecated := inventory.Version{Major: 1, Minor: 26}
	removed := inventory.Version{Major: 1, Minor: 29}
	for _, kind := range []string{"FlowSchema", "PriorityLevelConfiguration"} {
		k.APILifecycle = append(k.APILifecycle, kb.APILifecycleEntry{
			Group: "flowcontrol.apiserver.k8s.io", Version: "v1beta2", Kind: kind,
			Introduced: inventory.Version{Major: 1, Minor: 23}, Deprecated: &deprecated, Removed: &removed,
		})
	}
	return k
}

// pushAs pushes inv for cluster as an agent of agentVersion.
func (h *harness) pushAs(cluster, agentVersion string, inv inventory.Inventory) (int, map[string]any) {
	h.t.Helper()
	body, err := json.Marshal(map[string]any{
		"schemaVersion": 1, "clusterName": cluster, "agentVersion": agentVersion, "kbVersion": "agent-kb", "inventory": inv,
	})
	if err != nil {
		h.t.Fatal(err)
	}
	resp, out := postSnapshot(h.t, h.ts, "ingest-tok", body, false)
	return resp.StatusCode, out
}

type legacyReportView struct {
	Verdict  string `json:"verdict"`
	Score    int    `json:"score"`
	Findings []struct {
		Category string `json:"category"`
		Severity string `json:"severity"`
		Title    string `json:"title"`
	} `json:"findings"`
	NotAssessed []struct {
		Capability string   `json:"capability"`
		Reason     string   `json:"reason"`
		Required   bool     `json:"required"`
		Partial    bool     `json:"partial"`
		Skipped    []string `json:"skipped"`
	} `json:"notAssessed"`
}

func (h *harness) report(cluster, target string) legacyReportView {
	h.t.Helper()
	var rep legacyReportView
	if code := h.get("/api/v1/clusters/"+itoa(h.clusterID(cluster))+"/report?target="+target, &rep); code != http.StatusOK {
		h.t.Fatalf("GET %s report = %d", cluster, code)
	}
	return rep
}

func (r legacyReportView) gap(capability string) (reason string, required bool) {
	for _, g := range r.NotAssessed {
		if g.Capability == capability {
			return g.Reason, g.Required
		}
	}
	return "", false
}

// argoChartInventory is a 1.34 cluster running Argo CD found through its
// Helm chart.
func argoChartInventory(version string) inventory.Inventory {
	inv := testInventory()
	inv.Capabilities[inventory.CapAddOns] = inventory.CapabilityStatus{Available: true}
	inv.AddOns = []inventory.AddOnInstance{{ID: "argo-cd", Version: version, Namespaces: []string{"argocd"}, Source: "chart"}}
	return inv
}

// unmarked is inv as a collector before collectorSchema wrote it: v0.1.x,
// or a v0.2.0 release candidate.
func unmarked(inv inventory.Inventory) inventory.Inventory {
	inv.CollectorSchema = 0
	return inv
}

// TestLegacyChartSourcedAddOnNotReady: a v0.1.x agent reported a
// chart-found add-on's CHART version as its version (#110 changed that to
// the app version without a schema bump). Argo CD chart 7.1.0 is app 2.10,
// past its end of life, yet it was judged as "7.1.0": no release line,
// ready/100. A legacy agent's chart version is now evidence only, and its
// data can never make a cluster ready; a current agent is judged as before.
// "dev" is what nearly every v0.1.x agent reports (its Dockerfile, chart
// image tag and go install all default to it), so an unmarked inventory
// is legacy unless its agentVersion is a release at or after 0.2.0-0
// (#194 SV-12).
func TestLegacyChartSourcedAddOnNotReady(t *testing.T) {
	h := newHarness(t, Config{KB: legacyKB()}, aug1)
	for i, agent := range []string{"0.1.1", "v0.1.0", "dev", "", "unknown", "(devel)", "0.1.2-0.20261001120000-4bca610f1e2d"} {
		cluster := "legacy-" + itoa(int64(i))
		if code, out := h.pushAs(cluster, agent, unmarked(argoChartInventory("7.1.0"))); code != http.StatusAccepted {
			t.Fatalf("push as %q = %d %v", agent, code, out)
		}
		c := h.fleetCell(cluster, "1.35")
		if c == nil || c.Ready || c.Verdict == "ready" {
			t.Errorf("agent %q: cell = %+v, want not ready", agent, c)
		}
		rep := h.report(cluster, "1.35")
		if reason, required := rep.gap("api-usage"); !required || !strings.Contains(reason, `"`+agent+`"`) {
			t.Errorf("agent %q: api-usage gap = %q (required %v), want a required gap naming the agent", agent, reason, required)
		}
		for _, f := range rep.Findings {
			if strings.Contains(f.Title, "Argo CD") && strings.Contains(f.Title, "7.1.0") {
				t.Errorf("agent %q: finding %q judges the chart version as the app version", agent, f.Title)
			}
		}
	}

	// The same data from a current agent: the version is its app version.
	// A release candidate's inventory is unmarked; its version says it is
	// current.
	for cluster, push := range map[string]struct {
		agent   string
		inv     inventory.Inventory
		verdict string
	}{
		"current":  {"v0.2.0", argoChartInventory("2.10.0"), "blocked"}, // end of life
		"rc2":      {"0.2.0-rc.2", unmarked(argoChartInventory("2.10.0")), "blocked"},
		"dev":      {"dev", argoChartInventory("3.1.0"), "ready"}, // marked: a build from current source
		"marked-0": {"0.1.1", argoChartInventory("3.1.0"), "ready"},
	} {
		if code, _ := h.pushAs(cluster, push.agent, push.inv); code != http.StatusAccepted {
			t.Fatalf("%s push failed", cluster)
		}
		if c := h.fleetCell(cluster, "1.35"); c == nil || c.Verdict != push.verdict {
			t.Errorf("%s (agent %q): cell = %+v, want %s", cluster, push.agent, c, push.verdict)
		}
	}
}

// flowSchemaInventory is a v0.1.1-shaped 1.28 inventory: the APF objects
// the apiserver serves at v1beta2 counted as v1beta2 usage, no objects.
func flowSchemaInventory() inventory.Inventory {
	inv := testInventory()
	inv.ServerVersion = "v1.28.9"
	inv.Capabilities[inventory.CapAPIUsage] = inventory.CapabilityStatus{Available: true}
	inv.Capabilities[inventory.CapDeprecatedCalls] = inventory.CapabilityStatus{Available: true}
	inv.APIUsage = []inventory.APIUsage{
		{Group: "flowcontrol.apiserver.k8s.io", Version: "v1beta2", Kind: "FlowSchema", Count: 13, Namespaces: map[string]int{"": 13}},
		{Group: "flowcontrol.apiserver.k8s.io", Version: "v1beta2", Kind: "PriorityLevelConfiguration", Count: 8, Namespaces: map[string]int{"": 8}},
	}
	inv.DeprecatedCalls = []inventory.DeprecatedCall{{Group: "flowcontrol.apiserver.k8s.io", Version: "v1beta2", Resource: "flowschemas", RemovedRelease: "1.29"}}
	return inv
}

// TestLegacyResidencyFlagged: a v0.1.x agent listed every served
// deprecated version, so it counted objects the apiserver merely serves
// there (and its own LISTs landed in the deprecated-request metric). Its
// 1.28 inventory got two removed-API blockers nobody can fix, with no sign
// the data came from an older collector. Those signals are now not
// assessed, with the reason, instead of judged.
func TestLegacyResidencyFlagged(t *testing.T) {
	h := newHarness(t, Config{KB: legacyKB()}, aug1)
	// "dev" and "" are what a v0.1.1 agent built without a version sends
	// (#194 KB-14).
	for i, agent := range []string{"0.1.1", "dev", ""} {
		cluster := "old-" + itoa(int64(i))
		if code, out := h.pushAs(cluster, agent, unmarked(flowSchemaInventory())); code != http.StatusAccepted {
			t.Fatalf("legacy push as %q = %d %v", agent, code, out)
		}
		rep := h.report(cluster, "1.29")
		for _, f := range rep.Findings {
			if f.Severity == "blocker" {
				t.Errorf("agent %q: legacy blocker %q (%s), want residency not judged", agent, f.Title, f.Category)
			}
		}
		if rep.Verdict != "unknown" {
			t.Errorf("agent %q: verdict = %s, want unknown", agent, rep.Verdict)
		}
		for _, capability := range []string{"api-usage", "deprecated-calls"} {
			if reason, _ := rep.gap(capability); !strings.Contains(reason, `"`+agent+`"`) {
				t.Errorf("agent %q: %s gap = %q, want one naming the agent", agent, capability, reason)
			}
		}
	}
	var detail struct {
		Capabilities map[string]inventory.CapabilityStatus `json:"capabilities"`
	}
	h.get("/api/v1/clusters/"+itoa(h.clusterID("old-0")), &detail)
	if st, ok := detail.Capabilities["api-usage"]; !ok || st.Available {
		t.Errorf("cluster detail api-usage = %+v, want unavailable (legacy agent)", st)
	}

	// A current agent's data is judged.
	if code, _ := h.pushAs("new", "0.2.0", flowSchemaInventory()); code != http.StatusAccepted {
		t.Fatal("current push failed")
	}
	if rep := h.report("new", "1.29"); rep.Verdict != "blocked" {
		t.Errorf("current agent verdict = %s, want blocked", rep.Verdict)
	}
}

// TestUnmarkedResidencyWithoutObjectsNotJudged (#194 KB-14): an unmarked
// inventory whose agentVersion reads as v0.2.0 or later is judged as
// current, yet a usage row that counts objects and names none is shaped
// like v0.1.x residency data: every collector since v0.2.0-rc.1 names
// each object it counts. Who writes through that version is unknown, so
// the row is no blocker; it is a partial api-usage gap naming the API,
// required when the target removes it. A row that names its objects is
// judged as before.
func TestUnmarkedResidencyWithoutObjectsNotJudged(t *testing.T) {
	h := newHarness(t, Config{KB: legacyKB()}, aug1)
	residency := unmarked(flowSchemaInventory())
	residency.DeprecatedCalls = nil // only the residency counts are in question
	if code, out := h.pushAs("residency", "v0.2.0", residency); code != http.StatusAccepted {
		t.Fatalf("push = %d %v", code, out)
	}
	rep := h.report("residency", "1.29")
	for _, f := range rep.Findings {
		if f.Severity == "blocker" {
			t.Errorf("blocker %q (%s) from a count that names no object, want it not judged", f.Title, f.Category)
		}
	}
	if rep.Verdict != "unknown" {
		t.Errorf("verdict = %s, want unknown", rep.Verdict)
	}
	want := []string{"flowcontrol.apiserver.k8s.io/v1beta2 FlowSchema", "flowcontrol.apiserver.k8s.io/v1beta2 PriorityLevelConfiguration"}
	found := false
	for _, g := range rep.NotAssessed {
		if g.Capability != "api-usage" {
			continue
		}
		found = true
		if !g.Partial || !g.Required || !slices.Equal(g.Skipped, want) || !strings.Contains(g.Reason, "name no object") {
			t.Errorf("api-usage gap = %+v, want a required partial gap skipping %v", g, want)
		}
	}
	if !found {
		t.Errorf("no api-usage gap in %+v", rep.NotAssessed)
	}

	named := residency
	named.APIUsage = slices.Clone(residency.APIUsage)
	for i := range named.APIUsage {
		named.APIUsage[i].Objects = []inventory.ObjectRef{{Name: "probe", Manager: "kubectl"}}
	}
	if code, _ := h.pushAs("named", "0.2.0-rc.2", named); code != http.StatusAccepted {
		t.Fatal("push failed")
	}
	if rep := h.report("named", "1.29"); rep.Verdict != "blocked" {
		t.Errorf("rows naming their objects: verdict %s, want blocked", rep.Verdict)
	}
}

func TestLegacyInventory(t *testing.T) {
	for v, want := range map[string]bool{
		"0.1.0": true, "0.1.1": true, "v0.1.1": true, "0.0.9": true, "0.1.1-rc.1": true, "0.1.10": true,
		"0.1.1-0.20260720120000-4bca610f1e2d": true, // go install of a commit before v0.1.1
		// Unmarked builds that are not a release at or after 0.2.0-0: what a
		// v0.1.x agent reports when built without a version (its default),
		// a pseudo-version or snapshot of pre-0.2 source, and strings no
		// release ever sends.
		"dev": true, "": true, "unknown": true, "(devel)": true, "test": true,
		"0.1.2-0.20261001120000-4bca610f1e2d": true, "0.1.2-SNAPSHOT-abc123": true,
		"V0.1.1": true, "0.1.1 ": true, "0.01.1": true,
		// v0.2.0's release candidates and later stamp a semantic version.
		"0.2.0-0": false, "0.2.0-rc.2": false, "v0.2.0-rc.1": false, "0.2.0": false, "v0.2.0-test": false,
		"0.2.0-rc.2.0.20261001120000-4bca610f1e2d": false, "0.10.0": false, "1.0.0": false,
	} {
		if got := legacyInventory(unmarked(testInventory()), v); got != want {
			t.Errorf("unmarked inventory, agent %q: legacy = %v, want %v", v, got, want)
		}
		// A collector that stamps collectorSchema is current, whatever the
		// envelope says.
		if legacyInventory(testInventory(), v) {
			t.Errorf("marked inventory, agent %q: legacy, want current", v)
		}
	}
}

// TestLegacyViewJudgesSnapshotsAsCluster: ingest refuses a source other
// than a cluster (#194), and a snapshot stored before it did is judged as
// the cluster inventory every snapshot is, so a re-evaluation does not
// drop the versions and add-ons a files inventory is judged without.
func TestLegacyViewJudgesSnapshotsAsCluster(t *testing.T) {
	for _, src := range []inventory.Source{inventory.SourceFiles, "gate"} {
		for _, agent := range []string{"0.2.0", "dev"} {
			inv := testInventory()
			inv.Source = src
			if got := legacyView(inv, agent).Source; got != inventory.SourceCluster {
				t.Errorf("stored source %q, agent %q: judged as %q, want %q", src, agent, got, inventory.SourceCluster)
			}
		}
	}
	for _, src := range []inventory.Source{"", inventory.SourceCluster} {
		inv := testInventory()
		inv.Source = src
		if got := legacyView(inv, "0.2.0").Source; got != src {
			t.Errorf("stored source %q: judged as %q, want it kept", src, got)
		}
	}
}

// TestClusterDetailJudgesMarkerLikeReport (#194 SV-12): the cluster
// detail decodes only the snapshot's head, and must judge it as the
// report judges the whole inventory. A current agent built the default
// way reports agentVersion "dev" (hack/e2e.sh: "e2e"); its inventory
// carries the collector marker, so it is not legacy, and its api-usage
// and deprecated-calls must not read as an old agent's.
func TestClusterDetailJudgesMarkerLikeReport(t *testing.T) {
	h := newHarness(t, Config{KB: legacyKB()}, aug1)
	for i, agent := range []string{"dev", "e2e", ""} {
		cluster := "marked-" + itoa(int64(i))
		if code, out := h.pushAs(cluster, agent, testInventory()); code != http.StatusAccepted {
			t.Fatalf("push as %q = %d %v", agent, code, out)
		}
		var detail struct {
			Capabilities map[string]inventory.CapabilityStatus `json:"capabilities"`
		}
		if code := h.get("/api/v1/clusters/"+itoa(h.clusterID(cluster)), &detail); code != http.StatusOK {
			t.Fatalf("GET %s = %d", cluster, code)
		}
		for _, capability := range []string{"api-usage", "deprecated-calls"} {
			if st := detail.Capabilities[capability]; !st.Available || st.Reason != "" {
				t.Errorf("agent %q: cluster detail %s = %+v, want available with no legacy reason", agent, capability, st)
			}
		}
		if rep := h.report(cluster, "1.35"); rep.Verdict != "ready" || len(rep.NotAssessed) != 0 {
			t.Errorf("agent %q: report = %s, notAssessed %+v, want ready with none", agent, rep.Verdict, rep.NotAssessed)
		}
	}
}
