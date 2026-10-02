package server

import (
	"encoding/json"
	"net/http"
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
		Capability string `json:"capability"`
		Reason     string `json:"reason"`
		Required   bool   `json:"required"`
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

// TestLegacyChartSourcedAddOnNotReady: a v0.1.x agent reported a
// chart-found add-on's CHART version as its version (#110 changed that to
// the app version without a schema bump). Argo CD chart 7.1.0 is app 2.10,
// past its end of life, yet it was judged as "7.1.0": no release line,
// ready/100. A legacy agent's chart version is now evidence only, and its
// data can never make a cluster ready; a current agent is judged as before.
func TestLegacyChartSourcedAddOnNotReady(t *testing.T) {
	h := newHarness(t, Config{KB: legacyKB()}, aug1)
	for _, agent := range []string{"0.1.1", "v0.1.0"} {
		if code, out := h.pushAs("legacy-"+agent, agent, argoChartInventory("7.1.0")); code != http.StatusAccepted {
			t.Fatalf("push as %s = %d %v", agent, code, out)
		}
		c := h.fleetCell("legacy-"+agent, "1.35")
		if c == nil || c.Ready || c.Verdict == "ready" {
			t.Errorf("agent %s: cell = %+v, want not ready", agent, c)
		}
		rep := h.report("legacy-"+agent, "1.35")
		if reason, required := rep.gap("api-usage"); !required || !strings.Contains(reason, agent) {
			t.Errorf("agent %s: api-usage gap = %q (required %v), want a required gap naming the agent", agent, reason, required)
		}
		for _, f := range rep.Findings {
			if strings.Contains(f.Title, "Argo CD") && strings.Contains(f.Title, "7.1.0") {
				t.Errorf("agent %s: finding %q judges the chart version as the app version", agent, f.Title)
			}
		}
	}

	// The same data from a current agent: 7.1.0 is its app version.
	if code, _ := h.pushAs("current", "v0.2.0", argoChartInventory("2.10.0")); code != http.StatusAccepted {
		t.Fatal("current push failed")
	}
	if c := h.fleetCell("current", "1.35"); c == nil || c.Verdict != "blocked" {
		t.Errorf("current agent, Argo CD 2.10: cell = %+v, want blocked (end of life)", c)
	}
	if code, _ := h.pushAs("dev", "dev", argoChartInventory("3.1.0")); code != http.StatusAccepted {
		t.Fatal("dev push failed")
	}
	if c := h.fleetCell("dev", "1.35"); c == nil || c.Verdict != "ready" {
		t.Errorf("dev agent, Argo CD 3.1: cell = %+v, want ready", c)
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
	if code, out := h.pushAs("old", "0.1.1", flowSchemaInventory()); code != http.StatusAccepted {
		t.Fatalf("legacy push = %d %v", code, out)
	}
	rep := h.report("old", "1.29")
	for _, f := range rep.Findings {
		if f.Severity == "blocker" {
			t.Errorf("legacy blocker %q (%s), want residency not judged", f.Title, f.Category)
		}
	}
	if rep.Verdict != "unknown" {
		t.Errorf("verdict = %s, want unknown", rep.Verdict)
	}
	for _, capability := range []string{"api-usage", "deprecated-calls"} {
		if reason, _ := rep.gap(capability); !strings.Contains(reason, "0.1.1") {
			t.Errorf("%s gap = %q, want one naming agent 0.1.1", capability, reason)
		}
	}
	var detail struct {
		Capabilities map[string]inventory.CapabilityStatus `json:"capabilities"`
	}
	h.get("/api/v1/clusters/"+itoa(h.clusterID("old")), &detail)
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

func TestLegacyAgent(t *testing.T) {
	for v, want := range map[string]bool{
		"0.1.0": true, "0.1.1": true, "v0.1.1": true, "0.0.9": true, "0.1.1-rc.1": true,
		"0.1.1-0.20260720120000-4bca610f1e2d": true, // go install of a commit before v0.1.1
		// Built from source after v0.1.1: a go install pseudo-version, a
		// GoReleaser snapshot.
		"0.1.2-0.20261001120000-4bca610f1e2d": false, "0.1.2-SNAPSHOT-abc123": false,
		"0.2.0": false, "v0.2.0-rc.1": false, "1.0.0": false, "dev": false, "": false, "test": false, "0.10.0": false, "0.1.10": false,
	} {
		if got := legacyAgent(v); got != want {
			t.Errorf("legacyAgent(%q) = %v, want %v", v, got, want)
		}
	}
}
