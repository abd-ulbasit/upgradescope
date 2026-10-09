package server

import (
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/abd-ulbasit/upgradescope/internal/inventory"
)

// gateSkewResponse is what the skew tests read of a ?cluster= gate answer.
type gateSkewResponse struct {
	Verdict        string `json:"verdict"`
	Ready          bool   `json:"ready"`
	ClusterVerdict string `json:"clusterVerdict"`
	NotAssessed    []struct {
		Capability string   `json:"capability"`
		Reason     string   `json:"reason"`
		Required   bool     `json:"required"`
		Skipped    []string `json:"skipped"`
	} `json:"notAssessed"`
}

// #268, the CI gate: POST /api/v1/gate?cluster= judges the cluster's latest
// snapshot like every other read, so a cluster whose agent collected with
// another knowledge base than the server's is never ready there either. An
// rc.2 agent's weave-net (retired: a blocker at any version) is sent as an
// unrecognized image; before, the report read unknown while the gate passed
// the PR (verdict ready, clusterVerdict ready).
func TestGateClusterSkewIsNeverReady(t *testing.T) {
	k := embeddedKB(t)
	lifecycle, registry, _ := kbDigests(k.Version)
	rc2 := func() inventory.Inventory {
		inv := testInventory()
		inv.CollectorSchema = 0 // v0.2.0-rc.2 predates the stamp
		inv.UnrecognizedImages = []string{"docker.io/weaveworks/weave-kube"}
		return inv
	}
	h := newHarness(t, Config{KB: k}, aug1)
	for _, tc := range []struct {
		cluster, kbVersion string
		ready              bool
		gap                string // the capability whose required gap names the skew
	}{
		{"same-data", k.Version, true, ""},
		{"registry-skew", withDigests(t, k.Version, lifecycle, "deadbeef"), false, "addons"},
		{"lifecycle-skew", withDigests(t, k.Version, "deadbeef", registry), false, "api-usage"},
		{"label-names-no-digests", "rc2-kb", false, "addons"},
	} {
		t.Run(tc.cluster, func(t *testing.T) {
			h.pushKB(tc.cluster, "0.2.0-rc.2", tc.kbVersion, rc2())
			// The report says what the gate must: the same snapshot, judged.
			if rep := h.report(tc.cluster, "1.35"); (rep.Verdict == "ready") != tc.ready {
				t.Fatalf("report verdict = %s, want ready = %v", rep.Verdict, tc.ready)
			}
			resp, raw := postGate(t, h.ts, "?target=1.35&fail-on=never&cluster="+tc.cluster, "", deploymentManifest, "application/x-yaml")
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("gate = %d %s", resp.StatusCode, raw)
			}
			var b gateSkewResponse
			if err := json.Unmarshal(raw, &b); err != nil {
				t.Fatal(err)
			}
			if b.Ready != tc.ready || (b.Verdict == "ready") != tc.ready || (b.ClusterVerdict == "ready") != tc.ready ||
				(resp.Header.Get("X-Upgradescope-Verdict") == "ready") != tc.ready {
				t.Errorf("gate = ready %v verdict %q clusterVerdict %q header %q, want ready = %v", b.Ready, b.Verdict, b.ClusterVerdict,
					resp.Header.Get("X-Upgradescope-Verdict"), tc.ready)
			}
			if tc.gap == "" {
				if len(b.NotAssessed) != 0 {
					t.Errorf("gaps %+v, want none: the data is the same", b.NotAssessed)
				}
				return
			}
			found := false
			for _, g := range b.NotAssessed {
				if g.Capability == tc.gap && g.Required && slices.Contains(g.Skipped, inventory.SkippedNewerKB) && strings.Contains(g.Reason, "upgrade the agent") {
					found = true
				}
			}
			if !found {
				t.Errorf("gaps %+v, want a required %s gap naming the knowledge base skew and telling to upgrade the agent", b.NotAssessed, tc.gap)
			}
			// fail-on=ready would stop the pipeline: the default 200 for
			// "unknown" is the contract, but a required gap is not a pass.
			if b.Ready {
				t.Error("the gate passed a PR against a cluster the server's data reads as unknown")
			}
		})
	}
}
