package engine

import (
	"reflect"
	"testing"
	"time"

	"github.com/abd-ulbasit/upgradescope/internal/inventory"
)

// allCaps returns a capability map with every capability available.
func allCaps() map[inventory.Capability]inventory.CapabilityStatus {
	return map[inventory.Capability]inventory.CapabilityStatus{
		inventory.CapAPIUsage:        {Available: true},
		inventory.CapDeprecatedCalls: {Available: true},
		inventory.CapHelm:            {Available: true},
		inventory.CapAddOns:          {Available: true},
		inventory.CapVersions:        {Available: true},
	}
}

func clusterInv() inventory.Inventory {
	return inventory.Inventory{
		Source:        inventory.SourceCluster,
		ServerVersion: "v1.34.2",
		Capabilities:  allCaps(),
		Nodes:         []inventory.NodeInfo{{Name: "n", KubeletVersion: "v1.34.2"}},
	}
}

func filesInv() inventory.Inventory {
	const reason = "files mode"
	return inventory.Inventory{
		Source: inventory.SourceFiles,
		Capabilities: map[inventory.Capability]inventory.CapabilityStatus{
			inventory.CapAPIUsage:        {Available: true},
			inventory.CapDeprecatedCalls: {Available: false, Reason: reason},
			inventory.CapHelm:            {Available: false, Reason: reason},
			inventory.CapAddOns:          {Available: false, Reason: reason},
			inventory.CapVersions:        {Available: false, Reason: reason},
		},
	}
}

func degrade(inv inventory.Inventory, c inventory.Capability, reason string) inventory.Inventory {
	caps := make(map[inventory.Capability]inventory.CapabilityStatus, len(inv.Capabilities))
	for k, v := range inv.Capabilities {
		caps[k] = v
	}
	caps[c] = inventory.CapabilityStatus{Available: false, Reason: reason}
	inv.Capabilities = caps
	return inv
}

func TestEvaluateVerdict(t *testing.T) {
	now := time.Date(2026, 6, 10, 0, 0, 0, 0, time.UTC)
	t135 := inventory.Version{Major: 1, Minor: 35}
	t136 := inventory.Version{Major: 1, Minor: 36} // == testKB MaxKnownK8s
	t137 := inventory.Version{Major: 1, Minor: 37}

	withRemoved := clusterInv() // extensions/v1beta1 Ingress: removed 1.22 → blocker
	withRemoved.APIUsage = []inventory.APIUsage{{Group: "extensions", Version: "v1beta1", Kind: "Ingress", Count: 1}}

	noServer := clusterInv()
	noServer.ServerVersion = ""
	badServer := clusterInv()
	badServer.ServerVersion = "garbage"
	legacy := degrade(clusterInv(), inventory.CapVersions, "list nodes: forbidden")
	legacy.Source = "" // v0.1 agents push no source: treated as cluster

	cases := []struct {
		name    string
		inv     inventory.Inventory
		target  inventory.Version
		verdict Verdict
		gaps    []CapabilityGap
	}{
		{"complete cluster scan", clusterInv(), t135, VerdictReady, nil},
		{"target at kb horizon", clusterInv(), t136, VerdictReady, nil},
		{"optional capability missing stays ready",
			degrade(clusterInv(), inventory.CapHelm, "secrets list forbidden"), t135, VerdictReady,
			[]CapabilityGap{{Capability: inventory.CapHelm, Reason: "secrets list forbidden"}}},
		{"api-usage missing", degrade(clusterInv(), inventory.CapAPIUsage, "discovery: forbidden"), t135, VerdictUnknown,
			[]CapabilityGap{{Capability: inventory.CapAPIUsage, Reason: "discovery: forbidden", Required: true}}},
		{"versions missing in cluster mode", degrade(clusterInv(), inventory.CapVersions, "list nodes: forbidden"), t135, VerdictUnknown,
			[]CapabilityGap{{Capability: inventory.CapVersions, Reason: "list nodes: forbidden", Required: true}}},
		{"empty source is cluster mode", legacy, t135, VerdictUnknown,
			[]CapabilityGap{{Capability: inventory.CapVersions, Reason: "list nodes: forbidden", Required: true}}},
		{"server version missing", noServer, t135, VerdictUnknown,
			[]CapabilityGap{{Capability: inventory.CapVersions, Reason: "server version not reported; kubelet and control-plane skew were not evaluated", Required: true}}},
		{"server version unparseable", badServer, t135, VerdictUnknown,
			[]CapabilityGap{{Capability: inventory.CapVersions, Reason: `server version "garbage" could not be parsed; kubelet and control-plane skew were not evaluated`, Required: true}}},
		{"target above kb horizon", clusterInv(), t137, VerdictUnknown,
			[]CapabilityGap{{Capability: GapKBCoverage, Reason: "knowledge base covers Kubernetes up to 1.36; target 1.37 cannot be assessed", Required: true}}},
		{"files mode does not require versions", filesInv(), t135, VerdictReady,
			[]CapabilityGap{
				{Capability: inventory.CapAddOns, Reason: "files mode"},
				{Capability: inventory.CapDeprecatedCalls, Reason: "files mode"},
				{Capability: inventory.CapHelm, Reason: "files mode"},
				{Capability: inventory.CapVersions, Reason: "files mode"},
			}},
		{"files mode still requires api-usage", degrade(filesInv(), inventory.CapAPIUsage, "no manifests"), t135, VerdictUnknown,
			[]CapabilityGap{
				{Capability: inventory.CapAddOns, Reason: "files mode"},
				{Capability: inventory.CapAPIUsage, Reason: "no manifests", Required: true},
				{Capability: inventory.CapDeprecatedCalls, Reason: "files mode"},
				{Capability: inventory.CapHelm, Reason: "files mode"},
				{Capability: inventory.CapVersions, Reason: "files mode"},
			}},
		{"blocker wins over gaps", degrade(withRemoved, inventory.CapVersions, "list nodes: forbidden"), t135, VerdictBlocked,
			[]CapabilityGap{{Capability: inventory.CapVersions, Reason: "list nodes: forbidden", Required: true}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := Evaluate(tc.inv, testKB(), tc.target, now)
			if r.Verdict != tc.verdict {
				t.Errorf("Verdict = %q, want %q (gaps %+v)", r.Verdict, tc.verdict, r.NotAssessed)
			}
			if r.Ready != (tc.verdict == VerdictReady) {
				t.Errorf("Ready = %v, want %v (must equal Verdict == ready)", r.Ready, tc.verdict == VerdictReady)
			}
			if !reflect.DeepEqual(r.NotAssessed, tc.gaps) {
				t.Errorf("NotAssessed = %+v\nwant %+v", r.NotAssessed, tc.gaps)
			}
		})
	}
}

// The verdict never touches the score: an unknown verdict with no findings
// still scores 100, and the kb-stale warning is kept alongside the
// kb-coverage gap.
func TestEvaluateUnknownKeepsScoreAndKBStaleWarning(t *testing.T) {
	r := Evaluate(clusterInv(), testKB(), inventory.Version{Major: 1, Minor: 37}, time.Now())
	if r.Score != 95 {
		t.Errorf("Score = %d, want 95 (one kb-stale warning)", r.Score)
	}
	if len(r.Findings) != 1 || r.Findings[0].Category != CatKBStale {
		t.Errorf("findings = %+v, want exactly the kb-stale warning", r.Findings)
	}
}
