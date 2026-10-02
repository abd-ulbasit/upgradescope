package engine

import (
	"reflect"
	"testing"
	"time"

	"github.com/abd-ulbasit/upgradescope/internal/inventory"
	"github.com/abd-ulbasit/upgradescope/internal/kb"
	"github.com/abd-ulbasit/upgradescope/registry"
)

// allCaps returns a capability map with every capability available.
func allCaps() map[inventory.Capability]inventory.CapabilityStatus {
	return map[inventory.Capability]inventory.CapabilityStatus{
		inventory.CapAPIUsage:        {Available: true},
		inventory.CapDeprecatedCalls: {Available: true},
		inventory.CapHelm:            {Available: true},
		inventory.CapAddOns:          {Available: true},
		inventory.CapVersions:        {Available: true},
		inventory.CapCRDs:            {Available: true},
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
			inventory.CapCRDs:            {Available: true},
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

// A target at or below the minor kube-apiserver already runs is not an
// upgrade: every check asks what breaks at a newer minor, so judging it
// says nothing (a typo "1.4" for "1.40" read READY 100/100). It is a
// required gap; files mode has no cluster version and no such gap. With HA
// replicas mid-upgrade the oldest one decides: it still needs the target.
func TestEvaluateTargetNotAnUpgrade(t *testing.T) {
	now := time.Date(2026, 6, 10, 0, 0, 0, 0, time.UTC)
	gap := func(target, server string) []CapabilityGap {
		return []CapabilityGap{{Capability: GapTarget, Required: true,
			Reason: "target " + target + " is not an upgrade: kube-apiserver already runs " + server + ", and every check judges a newer minor"}}
	}
	midUpgrade := clusterInv()
	midUpgrade.ControlPlane = []inventory.ComponentVersion{
		{Component: "kube-apiserver", Version: "v1.33.5"}, {Component: "kube-apiserver", Version: "v1.34.2"},
	}
	cases := []struct {
		name    string
		inv     inventory.Inventory
		target  inventory.Version
		verdict Verdict
		gaps    []CapabilityGap
	}{
		{"same minor", clusterInv(), inventory.Version{Major: 1, Minor: 34}, VerdictUnknown, gap("1.34", "1.34")},
		{"downgrade", clusterInv(), inventory.Version{Major: 1, Minor: 30}, VerdictUnknown, gap("1.30", "1.34")},
		{"typo 1.4", clusterInv(), inventory.Version{Major: 1, Minor: 4}, VerdictUnknown, gap("1.4", "1.34")},
		{"next minor", clusterInv(), inventory.Version{Major: 1, Minor: 35}, VerdictReady, nil},
		{"HA replicas mid-upgrade", midUpgrade, inventory.Version{Major: 1, Minor: 34}, VerdictReady, nil},
		{"files mode", filesInv(), inventory.Version{Major: 1, Minor: 4}, VerdictReady, []CapabilityGap{
			{Capability: inventory.CapAddOns, Reason: "files mode"},
			{Capability: inventory.CapDeprecatedCalls, Reason: "files mode"},
			{Capability: inventory.CapHelm, Reason: "files mode"},
			{Capability: inventory.CapVersions, Reason: "files mode"},
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := Evaluate(tc.inv, testKB(), tc.target, now)
			if r.Verdict != tc.verdict || !reflect.DeepEqual(r.NotAssessed, tc.gaps) {
				t.Errorf("verdict %q, gaps %+v\nwant %q, %+v", r.Verdict, r.NotAssessed, tc.verdict, tc.gaps)
			}
		})
	}
}

// A target more than one minor ahead is several upgrades, which the
// control plane takes one minor at a time: an info finding names them.
func TestEvaluateUpgradeHops(t *testing.T) {
	now := time.Date(2026, 6, 10, 0, 0, 0, 0, time.UTC)
	hops := func(r Report) []Finding {
		var out []Finding
		for _, f := range r.Findings {
			if f.Key == "version-skew/upgrade-path" {
				out = append(out, f)
			}
		}
		return out
	}
	if fs := hops(Evaluate(clusterInv(), testKB(), inventory.Version{Major: 1, Minor: 35}, now)); len(fs) != 0 {
		t.Errorf("one minor ahead: want no upgrade-path finding, got %+v", fs)
	}
	fs := hops(Evaluate(clusterInv(), testKB(), inventory.Version{Major: 1, Minor: 36}, now))
	want := []Finding{{
		Category: CatVersionSkew, Severity: SevInfo, Key: "version-skew/upgrade-path",
		Title:     "upgrading from 1.34 to 1.36 takes 2 minor-version upgrades: 1.35, 1.36",
		Detail:    "The control plane is upgraded one minor version at a time. This report judges the cluster as it is against 1.36; add-ons, charts and nodes may need upgrading at each step in between.",
		Citations: []string{skewPolicyURL},
	}}
	if !reflect.DeepEqual(fs, want) {
		t.Errorf("two minors ahead:\n got %+v\nwant %+v", fs, want)
	}
}

// The report names the server version the target was judged against, so a
// reader can see a target that is not an upgrade.
func TestReportIncludesServerVersion(t *testing.T) {
	r := Evaluate(clusterInv(), testKB(), inventory.Version{Major: 1, Minor: 35}, time.Now())
	if r.ServerVersion != "v1.34.2" {
		t.Errorf("ServerVersion = %q, want v1.34.2", r.ServerVersion)
	}
	if r := Evaluate(filesInv(), testKB(), inventory.Version{Major: 1, Minor: 35}, time.Now()); r.ServerVersion != "" {
		t.Errorf("files mode ServerVersion = %q, want empty", r.ServerVersion)
	}
}

// The verdict never touches the score: an unknown verdict with no findings
// still scores 100, and the kb-stale warning is kept alongside the
// kb-coverage gap.
func TestEvaluateUnknownKeepsScoreAndKBStaleWarning(t *testing.T) {
	inv := clusterInv() // one minor below the target: no upgrade-path info
	inv.ServerVersion = "v1.36.1"
	inv.Nodes = []inventory.NodeInfo{{Name: "n", KubeletVersion: "v1.36.1"}}
	r := Evaluate(inv, testKB(), inventory.Version{Major: 1, Minor: 37}, time.Now())
	if r.Score != 95 {
		t.Errorf("Score = %d, want 95 (one kb-stale warning)", r.Score)
	}
	if len(r.Findings) != 1 || r.Findings[0].Category != CatKBStale {
		t.Errorf("findings = %+v, want exactly the kb-stale warning", r.Findings)
	}
}

// partially marks c available but Partial, skipping skipped.
func partially(inv inventory.Inventory, c inventory.Capability, reason string, skipped ...string) inventory.Inventory {
	caps := make(map[inventory.Capability]inventory.CapabilityStatus, len(inv.Capabilities))
	for k, v := range inv.Capabilities {
		caps[k] = v
	}
	caps[c] = inventory.CapabilityStatus{Available: true, Partial: true, Reason: reason, Skipped: skipped}
	inv.Capabilities = caps
	return inv
}

// Issue #122: a partial capability is a gap in every report, and the
// verdict is unknown exactly when the gap can hide a blocker: a partial
// api-usage skipping an API the KB removes at or before the target, or a
// cluster scan with no add-on data while the registry knows add-ons.
func TestEvaluatePartialAndAddOnGaps(t *testing.T) {
	now := time.Date(2026, 6, 10, 0, 0, 0, 0, time.UTC)
	t125 := inventory.Version{Major: 1, Minor: 25}
	t124 := inventory.Version{Major: 1, Minor: 24}
	k := testKB()
	k.APILifecycle = append(k.APILifecycle, kb.APILifecycleEntry{
		Group: "policy", Version: "v1beta1", Kind: "PodSecurityPolicy",
		Introduced: inventory.Version{Major: 1, Minor: 10}, Deprecated: vp(1, 21), Removed: vp(1, 25),
	})
	withRegistry := k
	withRegistry.AddOns = []registry.AddOn{{ID: "ingress-nginx"}}

	on124 := clusterInv()
	on124.ServerVersion = "v1.24.17"
	on124.Nodes = []inventory.NodeInfo{{Name: "n", KubeletVersion: "v1.24.17"}}
	const pspForbidden = "list policy/v1beta1 podsecuritypolicies: forbidden"
	pspSkipped := partially(on124, inventory.CapAPIUsage, pspForbidden, "policy/v1beta1 PodSecurityPolicy")
	on123 := on124
	on123.ServerVersion = "v1.23.17"
	on123.Nodes = []inventory.NodeInfo{{Name: "n", KubeletVersion: "v1.23.17"}}
	const helmReason = "helm releases: 1 via secrets; 1 release(s) not decodable, first a/b: gunzip"
	const unreadProxy = "version not read from 1 control-plane pod(s) (kube-proxy), first kube-system/kube-proxy-x: tag latest; their skew was not evaluated"
	const vendorProxy = "version not read from 1 control-plane pod(s) (kube-proxy), first kube-system/kube-proxy-x: labelled kube-proxy but runs a vendor image whose version is not read (iad.ocir.io/ns/oke-public-kube-proxy@sha256:1755); their skew was not evaluated"

	cases := []struct {
		name    string
		inv     inventory.Inventory
		kb      kb.KB
		target  inventory.Version
		verdict Verdict
		gaps    []CapabilityGap
	}{
		{"partial api-usage skipping an API removed at the target", pspSkipped, k, t125, VerdictUnknown,
			[]CapabilityGap{{Capability: inventory.CapAPIUsage, Reason: pspForbidden, Partial: true,
				Skipped: []string{"policy/v1beta1 PodSecurityPolicy"}, Required: true}}},
		{"partial api-usage skipping an API removed after the target",
			partially(on123, inventory.CapAPIUsage, pspForbidden, "policy/v1beta1 PodSecurityPolicy"), k, t124, VerdictReady,
			[]CapabilityGap{{Capability: inventory.CapAPIUsage, Reason: pspForbidden, Partial: true,
				Skipped: []string{"policy/v1beta1 PodSecurityPolicy"}}}},
		{"partial api-usage skipping a never-removed API",
			partially(on124, inventory.CapAPIUsage, "list v1 componentstatuses: forbidden", "v1 ComponentStatus"), k, t125, VerdictReady,
			[]CapabilityGap{{Capability: inventory.CapAPIUsage, Reason: "list v1 componentstatuses: forbidden", Partial: true,
				Skipped: []string{"v1 ComponentStatus"}}}},
		{"partial api-usage skipping an API the KB does not know",
			partially(on124, inventory.CapAPIUsage, "list example.com/v1 widgets: forbidden", "example.com/v1 Widget"), k, t125, VerdictReady,
			[]CapabilityGap{{Capability: inventory.CapAPIUsage, Reason: "list example.com/v1 widgets: forbidden", Partial: true,
				Skipped: []string{"example.com/v1 Widget"}}}},
		{"partial api-usage skipping nothing flagged",
			partially(on124, inventory.CapAPIUsage, "discovery: groups metrics.k8s.io/v1beta1 skipped"), k, t125, VerdictReady,
			[]CapabilityGap{{Capability: inventory.CapAPIUsage, Reason: "discovery: groups metrics.k8s.io/v1beta1 skipped", Partial: true}}},
		{"partial versions is a required gap: an unread kube-proxy may hide a skew blocker", // #169
			partially(on124, inventory.CapVersions, unreadProxy, "kube-proxy"), k, t125, VerdictUnknown,
			[]CapabilityGap{{Capability: inventory.CapVersions, Reason: unreadProxy, Partial: true,
				Skipped: []string{"kube-proxy"}, Required: true}}},
		{"partial versions naming no component is optional: a vendor kube-proxy image (OKE) upstream would not have told", // #169
			partially(on124, inventory.CapVersions, vendorProxy), k, t125, VerdictReady,
			[]CapabilityGap{{Capability: inventory.CapVersions, Reason: vendorProxy, Partial: true}}},
		{"partial helm is an optional gap", partially(on124, inventory.CapHelm, helmReason, "a/b"), withRegistry, t125, VerdictReady,
			[]CapabilityGap{{Capability: inventory.CapHelm, Reason: helmReason, Partial: true, Skipped: []string{"a/b"}}}},
		{"an informational reason is no gap", func() inventory.Inventory {
			inv := on124
			inv.Capabilities = allCaps() // on124 shares its map with other cases
			inv.Capabilities[inventory.CapHelm] = inventory.CapabilityStatus{Available: true, Reason: "helm releases: 2 via secrets, 0 via configmaps"}
			return inv
		}(), withRegistry, t125, VerdictReady, nil},
		{"addons missing in cluster mode with a registry", degrade(on124, inventory.CapAddOns, "list pods: forbidden"), withRegistry, t125, VerdictUnknown,
			[]CapabilityGap{{Capability: inventory.CapAddOns, Reason: "list pods: forbidden", Required: true}}},
		{"addons missing with an empty registry", degrade(on124, inventory.CapAddOns, "list pods: forbidden"), k, t125, VerdictReady,
			[]CapabilityGap{{Capability: inventory.CapAddOns, Reason: "list pods: forbidden"}}},
		{"addons missing in files mode", filesInv(), withRegistry, t125, VerdictReady,
			[]CapabilityGap{
				{Capability: inventory.CapAddOns, Reason: "files mode"},
				{Capability: inventory.CapDeprecatedCalls, Reason: "files mode"},
				{Capability: inventory.CapHelm, Reason: "files mode"},
				{Capability: inventory.CapVersions, Reason: "files mode"},
			}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := Evaluate(tc.inv, tc.kb, tc.target, now)
			if r.Verdict != tc.verdict {
				t.Errorf("Verdict = %q, want %q (gaps %+v)", r.Verdict, tc.verdict, r.NotAssessed)
			}
			if !reflect.DeepEqual(r.NotAssessed, tc.gaps) {
				t.Errorf("NotAssessed = %+v\nwant %+v", r.NotAssessed, tc.gaps)
			}
		})
	}
}
