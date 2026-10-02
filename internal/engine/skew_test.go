package engine

import (
	"reflect"
	"testing"
	"time"

	"github.com/abd-ulbasit/upgradescope/internal/inventory"
)

func TestEvalSkewCurrentViolationWarnsAndBlocksPostUpgrade(t *testing.T) {
	inv := inventory.Inventory{
		ServerVersion: "v1.34.2",
		Nodes: []inventory.NodeInfo{
			{Name: "node-ok", KubeletVersion: "v1.33.0"},  // 1 behind — fine
			{Name: "node-old", KubeletVersion: "v1.30.0"}, // 4 behind — violates max 3
		},
	}
	fs := evalSkew(inv, testKB(), inventory.Version{Major: 1, Minor: 35})
	if len(fs) != 2 {
		t.Fatalf("want post-upgrade blocker + current warning, got %+v", fs)
	}
	blocker, warning := fs[0], fs[1]
	if blocker.Severity != SevBlocker || blocker.Category != CatVersionSkew ||
		blocker.Title != "1 node(s) would exceed kubelet version skew after upgrading to 1.35" {
		t.Fatalf("blocker = %+v", blocker)
	}
	if blocker.Detail != "After upgrading the control plane to 1.35 these nodes would be more than 3 minor versions behind: node-old (v1.30.0)." {
		t.Fatalf("blocker detail = %q", blocker.Detail)
	}
	if blocker.Key != "version-skew/kubelet-post-upgrade" {
		t.Fatalf("blocker key = %q", blocker.Key)
	}
	if warning.Severity != SevWarning ||
		warning.Title != "1 node(s) exceed kubelet version skew vs control plane 1.34" {
		t.Fatalf("warning = %+v", warning)
	}
	if warning.Key != "version-skew/kubelet-current" {
		t.Fatalf("warning key = %q", warning.Key)
	}
}

func TestEvalSkewPostUpgradeOnlyBlocker(t *testing.T) {
	inv := inventory.Inventory{
		ServerVersion: "v1.34.2",
		Nodes:         []inventory.NodeInfo{{Name: "node-edge", KubeletVersion: "v1.31.0"}}, // 3 behind now (OK), 4 after 1.35
	}
	fs := evalSkew(inv, testKB(), inventory.Version{Major: 1, Minor: 35})
	if len(fs) != 1 || fs[0].Severity != SevBlocker || fs[0].Category != CatVersionSkew {
		t.Fatalf("want exactly one post-upgrade blocker, got %+v", fs)
	}
}

func TestEvalSkewWithinPolicyNoFindings(t *testing.T) {
	inv := inventory.Inventory{
		ServerVersion: "v1.34.2",
		Nodes:         []inventory.NodeInfo{{Name: "node-a", KubeletVersion: "v1.34.2"}},
	}
	if fs := evalSkew(inv, testKB(), inventory.Version{Major: 1, Minor: 35}); len(fs) != 0 {
		t.Fatalf("want no findings, got %+v", fs)
	}
}

func TestEvalSkewUnparseableKubeletVersionsReportedAsInfo(t *testing.T) {
	inv := inventory.Inventory{
		ServerVersion: "v1.34.2",
		Nodes: []inventory.NodeInfo{
			{Name: "node-a", KubeletVersion: "v1.34.2"},     // fine, ignored
			{Name: "node-weird", KubeletVersion: "garbage"}, // unparseable
			{Name: "node-blank", KubeletVersion: ""},        // unparseable
		},
	}
	fs := evalSkew(inv, testKB(), inventory.Version{Major: 1, Minor: 35})
	if len(fs) != 1 || fs[0].Severity != SevInfo || fs[0].Category != CatVersionSkew {
		t.Fatalf("want one version-skew info, got %+v", fs)
	}
	if fs[0].Title != "2 node(s) have unparseable kubelet versions" {
		t.Fatalf("title = %q", fs[0].Title)
	}
	if fs[0].Key != "version-skew/kubelet-unparseable" {
		t.Fatalf("key = %q", fs[0].Key)
	}
	if fs[0].Detail != `These nodes could not be evaluated against the kubelet skew policy: node-blank (""), node-weird ("garbage").` {
		t.Fatalf("detail = %q", fs[0].Detail)
	}
}

// Without a parseable server version (and no observed apiserver pods) the
// kubelet rules have no reference point. That must not read as "no skew":
// Evaluate records a required versions gap and the verdict is unknown.
func TestEvaluateUnparseableServerVersionIsVersionsGap(t *testing.T) {
	for _, sv := range []string{"", "garbage"} {
		inv := inventory.Inventory{
			ServerVersion: sv,
			Capabilities:  map[inventory.Capability]inventory.CapabilityStatus{inventory.CapAPIUsage: {Available: true}, inventory.CapVersions: {Available: true}, inventory.CapCRDs: {Available: true}},
			Nodes:         []inventory.NodeInfo{{Name: "n", KubeletVersion: "v1.20.0"}},
		}
		r := Evaluate(inv, testKB(), inventory.Version{Major: 1, Minor: 35}, time.Now())
		if r.Verdict != VerdictUnknown || r.Ready {
			t.Errorf("server %q: verdict = %q ready = %v, want unknown/false", sv, r.Verdict, r.Ready)
		}
		if len(r.NotAssessed) != 1 || r.NotAssessed[0].Capability != inventory.CapVersions || !r.NotAssessed[0].Required {
			t.Errorf("server %q: NotAssessed = %+v, want one required versions gap", sv, r.NotAssessed)
		}
	}
}

func TestEvalControlPlaneSkewEmptyControlPlaneNoFindings(t *testing.T) {
	// Managed control planes (EKS/GKE) expose no control-plane pods: the
	// collector leaves ControlPlane empty and no rule may fire — even with
	// server version and nodes present.
	inv := inventory.Inventory{
		ServerVersion: "v1.34.2",
		Nodes:         []inventory.NodeInfo{{Name: "n", KubeletVersion: "v1.30.0"}},
	}
	if fs := evalControlPlaneSkew(inv, testKB(), v134); len(fs) != 0 {
		t.Fatalf("empty ControlPlane must yield no control-plane findings, got %+v", fs)
	}
}

func TestEvalControlPlaneSkewHASpread(t *testing.T) {
	cases := []struct {
		name     string
		versions []string
		want     bool
	}{
		{"spread 2 violates", []string{"v1.34.0", "v1.36.0"}, true},
		{"spread 1 ok", []string{"v1.34.0", "v1.35.0"}, false},
		{"same minor different patch ok", []string{"v1.34.1", "v1.34.2"}, false},
		{"single replica ok", []string{"v1.34.0"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var inv inventory.Inventory
			for _, v := range tc.versions {
				inv.ControlPlane = append(inv.ControlPlane, inventory.ComponentVersion{Component: "kube-apiserver", Version: v})
			}
			fs := evalControlPlaneSkew(inv, testKB(), v134)
			if !tc.want {
				if len(fs) != 0 {
					t.Fatalf("want no findings, got %+v", fs)
				}
				return
			}
			if len(fs) != 1 || fs[0].Severity != SevWarning || fs[0].Category != CatVersionSkew {
				t.Fatalf("want one ha-spread warning, got %+v", fs)
			}
			if fs[0].Key != "version-skew/apiserver-ha-spread" {
				t.Fatalf("key = %q", fs[0].Key)
			}
			if fs[0].Title != "HA kube-apiserver replicas span 2 minor versions" {
				t.Fatalf("title = %q", fs[0].Title)
			}
			if fs[0].Detail != "Observed kube-apiserver versions: 1.34, 1.36. The version-skew policy requires HA kube-apiserver instances to be within 1 minor version of each other." {
				t.Fatalf("detail = %q", fs[0].Detail)
			}
			if len(fs[0].Citations) != 1 || fs[0].Citations[0] != skewPolicyURL {
				t.Fatalf("citations = %v", fs[0].Citations)
			}
		})
	}
}

func TestEvalControlPlaneSkewCtrlMgrNewerIsBlocker(t *testing.T) {
	inv := inventory.Inventory{ControlPlane: []inventory.ComponentVersion{
		{Component: "kube-apiserver", Version: "v1.34.2"},
		{Component: "kube-controller-manager", Version: "v1.35.0"},
	}}
	fs := evalControlPlaneSkew(inv, testKB(), v134)
	if len(fs) != 1 || fs[0].Severity != SevBlocker || fs[0].Category != CatVersionSkew {
		t.Fatalf("want one blocker, got %+v", fs)
	}
	if fs[0].Key != "version-skew/kube-controller-manager-newer" {
		t.Fatalf("key = %q", fs[0].Key)
	}
	if fs[0].Title != "kube-controller-manager is newer than kube-apiserver" {
		t.Fatalf("title = %q", fs[0].Title)
	}
	if fs[0].Detail != "kube-controller-manager 1.35 is newer than the oldest kube-apiserver (1.34); control-plane components must not be newer than the apiservers they talk to." {
		t.Fatalf("detail = %q", fs[0].Detail)
	}
}

func TestEvalControlPlaneSkewSchedulerBehind(t *testing.T) {
	inv := inventory.Inventory{ControlPlane: []inventory.ComponentVersion{
		{Component: "kube-apiserver", Version: "v1.34.2"},
		{Component: "kube-scheduler", Version: "v1.32.0"}, // 2 behind, max 1
	}}
	fs := evalControlPlaneSkew(inv, testKB(), v134)
	if len(fs) != 1 || fs[0].Severity != SevWarning || fs[0].Category != CatVersionSkew {
		t.Fatalf("want one warning, got %+v", fs)
	}
	if fs[0].Key != "version-skew/kube-scheduler-behind" {
		t.Fatalf("key = %q", fs[0].Key)
	}
	if fs[0].Title != "kube-scheduler exceeds version skew vs kube-apiserver" {
		t.Fatalf("title = %q", fs[0].Title)
	}
	if fs[0].Detail != "kube-scheduler 1.32 is more than 1 minor version(s) behind the newest kube-apiserver (1.34)." {
		t.Fatalf("detail = %q", fs[0].Detail)
	}

	// exactly at the limit → fine
	inv.ControlPlane[1].Version = "v1.33.0"
	if fs := evalControlPlaneSkew(inv, testKB(), v134); len(fs) != 0 {
		t.Fatalf("1 behind is within policy, got %+v", fs)
	}
}

func TestEvalControlPlaneSkewKubeProxy(t *testing.T) {
	base := []inventory.ComponentVersion{{Component: "kube-apiserver", Version: "v1.34.2"}}
	cases := []struct {
		name      string
		proxy     string
		wantTitle string // "" means no finding
		wantKey   string
	}{
		{"4 behind violates", "v1.30.0", "kube-proxy exceeds version skew vs kube-apiserver", "version-skew/kube-proxy-behind"},
		{"3 behind ok", "v1.31.0", "", ""},
		{"newer violates", "v1.35.0", "kube-proxy is newer than kube-apiserver", "version-skew/kube-proxy-newer"},
		{"matching ok", "v1.34.2", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			inv := inventory.Inventory{ControlPlane: append(append([]inventory.ComponentVersion{}, base...),
				inventory.ComponentVersion{Component: "kube-proxy", Version: tc.proxy})}
			fs := withoutKey(evalControlPlaneSkew(inv, testKB(), v134), "version-skew/kube-proxy-post-upgrade")
			if tc.wantTitle == "" {
				if len(fs) != 0 {
					t.Fatalf("want no findings, got %+v", fs)
				}
				return
			}
			if len(fs) != 1 || fs[0].Severity != SevWarning || fs[0].Category != CatVersionSkew {
				t.Fatalf("want one kube-proxy warning, got %+v", fs)
			}
			if fs[0].Key != tc.wantKey {
				t.Fatalf("key = %q, want %q", fs[0].Key, tc.wantKey)
			}
			if fs[0].Title != tc.wantTitle {
				t.Fatalf("title = %q, want %q", fs[0].Title, tc.wantTitle)
			}
		})
	}
}

// A component can be both newer than the oldest apiserver and too far
// behind the newest one (HA replicas mid-upgrade). Those are two findings
// of different severity, so they need two keys: a shared key let a
// --baseline holding the warning mark a new blocker unchanged.
func TestControlPlaneSkewKeysUnique(t *testing.T) {
	inv := inventory.Inventory{ControlPlane: []inventory.ComponentVersion{
		{Component: "kube-apiserver", Version: "v1.33.1"},
		{Component: "kube-apiserver", Version: "v1.35.0"},
		{Component: "kube-controller-manager", Version: "v1.33.1"},
		{Component: "kube-controller-manager", Version: "v1.34.0"},
		{Component: "kube-scheduler", Version: "v1.33.1"},
		{Component: "kube-scheduler", Version: "v1.34.0"},
		{Component: "kube-proxy", Version: "v1.31.0"},
		{Component: "kube-proxy", Version: "v1.34.0"},
	}}
	fs := evalControlPlaneSkew(inv, testKB(), inventory.Version{Major: 1, Minor: 36})
	want := []string{
		"blocker version-skew/kube-proxy-post-upgrade",
		"warning version-skew/apiserver-ha-spread",
		"blocker version-skew/kube-controller-manager-newer",
		"warning version-skew/kube-controller-manager-behind",
		"blocker version-skew/kube-scheduler-newer",
		"warning version-skew/kube-scheduler-behind",
		"warning version-skew/kube-proxy-newer",
		"warning version-skew/kube-proxy-behind",
	}
	if got := keys(fs); !reflect.DeepEqual(got, want) {
		t.Fatalf("keys:\n got %q\nwant %q", got, want)
	}
}

func TestEvalControlPlaneSkewFallsBackToServerVersion(t *testing.T) {
	// Self-hosted clusters where only the kube-proxy DaemonSet is visible:
	// no apiserver pods collected, so the apiserver version comes from
	// inv.ServerVersion.
	inv := inventory.Inventory{
		ServerVersion: "v1.34.2",
		ControlPlane:  []inventory.ComponentVersion{{Component: "kube-proxy", Version: "v1.30.0"}},
	}
	fs := withoutKey(evalControlPlaneSkew(inv, testKB(), v134), "version-skew/kube-proxy-post-upgrade")
	if len(fs) != 1 || fs[0].Key != "version-skew/kube-proxy-behind" || fs[0].Severity != SevWarning {
		t.Fatalf("want one kube-proxy warning via ServerVersion fallback, got %+v", fs)
	}

	// No apiserver pods AND unparseable server version → no current-state
	// reference; only the post-upgrade rule, which needs just the target, fires.
	inv.ServerVersion = "garbage"
	fs = evalControlPlaneSkew(inv, testKB(), v134)
	if len(fs) != 1 || fs[0].Key != "version-skew/kube-proxy-post-upgrade" {
		t.Fatalf("want only the post-upgrade kube-proxy blocker, got %+v", fs)
	}
}

// v134 as the target equals the current apiserver minor in these tests, so
// only current-state rules (and post-upgrade rules for already-too-old
// components) fire.
var v134 = inventory.Version{Major: 1, Minor: 34}

func withoutKey(fs []Finding, key string) []Finding {
	var out []Finding
	for _, f := range fs {
		if f.Key != key {
			out = append(out, f)
		}
	}
	return out
}

func keys(fs []Finding) []string {
	out := make([]string, len(fs))
	for i, f := range fs {
		out[i] = string(f.Severity) + " " + f.Key
	}
	return out
}

// Issue #30 (a): EKS upgrades kube-proxy separately and it commonly lags.
// kube-proxy 1.30 is within policy of a 1.33 apiserver but would be 4 minors
// behind a 1.34 control plane — an upgrade blocker, like the kubelet rule.
func TestEvalControlPlaneSkewKubeProxyPostUpgrade(t *testing.T) {
	inv := inventory.Inventory{
		ServerVersion: "v1.33.4",
		Capabilities:  map[inventory.Capability]inventory.CapabilityStatus{inventory.CapAPIUsage: {Available: true}, inventory.CapVersions: {Available: true}, inventory.CapCRDs: {Available: true}},
		Nodes:         []inventory.NodeInfo{{Name: "n", KubeletVersion: "v1.33.4-eks-aeac579"}},
		ControlPlane:  []inventory.ComponentVersion{{Component: "kube-proxy", Version: "v1.30.0"}},
	}
	target := inventory.Version{Major: 1, Minor: 34}
	fs := evalControlPlaneSkew(inv, testKB(), target)
	if len(fs) != 1 || fs[0].Severity != SevBlocker || fs[0].Key != "version-skew/kube-proxy-post-upgrade" {
		t.Fatalf("want one kube-proxy post-upgrade blocker, got %+v", fs)
	}
	if fs[0].Title != "kube-proxy would exceed version skew after upgrading to 1.34" {
		t.Errorf("title = %q", fs[0].Title)
	}
	if fs[0].Detail != "After upgrading the control plane to 1.34, kube-proxy 1.30 would be more than 3 minor versions behind kube-apiserver; upgrade kube-proxy first." {
		t.Errorf("detail = %q", fs[0].Detail)
	}
	if len(fs[0].Citations) != 1 || fs[0].Citations[0] != skewPolicyURL {
		t.Errorf("citations = %v", fs[0].Citations)
	}
	r := Evaluate(inv, testKB(), target, time.Now())
	if r.Verdict != VerdictBlocked || r.Ready {
		t.Errorf("verdict = %q ready = %v, want blocked/false", r.Verdict, r.Ready)
	}

	// 3 behind the target is still within policy.
	inv.ControlPlane[0].Version = "v1.31.0"
	if fs := evalControlPlaneSkew(inv, testKB(), target); len(fs) != 0 {
		t.Fatalf("kube-proxy 3 behind the target is within policy, got %+v", fs)
	}
}

// Issue #30 (b) and (c): "kubelet must not be newer than kube-apiserver",
// and with HA skew the OLDEST apiserver bounds it.
func TestEvalSkewKubeletNewerThanAPIServer(t *testing.T) {
	cases := []struct {
		name   string
		server string
		cp     []inventory.ComponentVersion
	}{
		{"single apiserver older than kubelet", "v1.32.3", nil},
		{"HA narrowing: older replica bounds the kubelet", "v1.33.1", []inventory.ComponentVersion{
			{Component: "kube-apiserver", Version: "v1.32.3"},
			{Component: "kube-apiserver", Version: "v1.33.1"},
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			inv := inventory.Inventory{
				ServerVersion: tc.server,
				ControlPlane:  tc.cp,
				Nodes: []inventory.NodeInfo{
					{Name: "node-new", KubeletVersion: "v1.33.1"},
					{Name: "node-ok", KubeletVersion: "v1.32.0"},
				},
			}
			fs := evalSkew(inv, testKB(), inventory.Version{Major: 1, Minor: 34})
			if len(fs) != 1 || fs[0].Severity != SevWarning || fs[0].Key != "version-skew/kubelet-newer-than-apiserver" {
				t.Fatalf("want one kubelet-newer warning, got %+v", keys(fs))
			}
			if fs[0].Title != "1 node(s) run a kubelet newer than kube-apiserver 1.32" {
				t.Errorf("title = %q", fs[0].Title)
			}
			if fs[0].Detail != "kubelet must not be newer than kube-apiserver; when kube-apiserver versions differ, the oldest one (1.32) bounds the allowed kubelet versions. Newer nodes: node-new (v1.33.1)." {
				t.Errorf("detail = %q", fs[0].Detail)
			}
		})
	}
}

// HA narrowing, lower bound: a kubelet must stay within 3 minors of EVERY
// apiserver, so the newest replica sets the floor even when the reported
// server version came from an older replica.
func TestEvalSkewKubeletBehindNewestHAApiserver(t *testing.T) {
	inv := inventory.Inventory{
		ServerVersion: "v1.32.3", // answered by the older replica
		ControlPlane: []inventory.ComponentVersion{
			{Component: "kube-apiserver", Version: "v1.32.3"},
			{Component: "kube-apiserver", Version: "v1.33.1"},
		},
		Nodes: []inventory.NodeInfo{{Name: "node-old", KubeletVersion: "v1.29.9"}},
	}
	fs := evalSkew(inv, testKB(), inventory.Version{Major: 1, Minor: 33})
	if len(fs) != 2 || fs[0].Key != "version-skew/kubelet-post-upgrade" || fs[1].Key != "version-skew/kubelet-current" {
		t.Fatalf("want post-upgrade blocker + current warning, got %+v", keys(fs))
	}
	if fs[1].Title != "1 node(s) exceed kubelet version skew vs control plane 1.33" {
		t.Errorf("title = %q", fs[1].Title)
	}
}

// Kubelets and kube-proxies older than 1.25 may only be two minors behind.
func TestSkewLegacyComponentsAllowTwoMinors(t *testing.T) {
	inv := inventory.Inventory{
		ServerVersion: "v1.25.16",
		Nodes:         []inventory.NodeInfo{{Name: "node-old", KubeletVersion: "v1.22.17"}},
		ControlPlane:  []inventory.ComponentVersion{{Component: "kube-proxy", Version: "v1.22.17"}},
	}
	target := inventory.Version{Major: 1, Minor: 26}
	fs := evalSkew(inv, testKB(), target)
	if len(fs) != 2 || fs[0].Key != "version-skew/kubelet-post-upgrade" || fs[1].Key != "version-skew/kubelet-current" {
		t.Fatalf("kubelet 1.22 is 3 behind 1.25: want post-upgrade blocker + current warning, got %+v", keys(fs))
	}
	if fs[0].Detail != "After upgrading the control plane to 1.26 these nodes would be more than 3 minor versions behind: node-old (v1.22.17). Kubelets older than 1.25 may be at most 2 minor versions behind." {
		t.Errorf("detail = %q", fs[0].Detail)
	}
	cp := evalControlPlaneSkew(inv, testKB(), target)
	if got := keys(cp); len(got) != 2 || got[0] != "blocker version-skew/kube-proxy-post-upgrade" || got[1] != "warning version-skew/kube-proxy-behind" {
		t.Fatalf("kube-proxy 1.22 is 3 behind 1.25: want post-upgrade blocker + current warning, got %v", got)
	}
}

func TestEvalKBStale(t *testing.T) {
	k := testKB() // MaxKnownK8s = 1.36
	cases := []struct {
		name      string
		inv       inventory.Inventory
		target    inventory.Version
		wantTitle string // "" means no finding
	}{
		{
			"server beyond kb",
			inventory.Inventory{ServerVersion: "v1.37.0"},
			inventory.Version{Major: 1, Minor: 36},
			"knowledge base does not cover Kubernetes 1.37 (newest known: 1.36)",
		},
		{
			"target beyond kb",
			inventory.Inventory{ServerVersion: "v1.36.1"},
			inventory.Version{Major: 1, Minor: 38},
			"knowledge base does not cover Kubernetes 1.38 (newest known: 1.36)",
		},
		{
			"target beyond kb with missing server version",
			inventory.Inventory{},
			inventory.Version{Major: 1, Minor: 37},
			"knowledge base does not cover Kubernetes 1.37 (newest known: 1.36)",
		},
		{
			"server beyond kb and beyond target",
			inventory.Inventory{ServerVersion: "v1.38.0"},
			inventory.Version{Major: 1, Minor: 37},
			"knowledge base does not cover Kubernetes 1.38 (newest known: 1.36)",
		},
		{
			"both within kb",
			inventory.Inventory{ServerVersion: "v1.36.1"},
			inventory.Version{Major: 1, Minor: 36},
			"",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fs := evalKBStale(tc.inv, k, tc.target)
			if tc.wantTitle == "" {
				if len(fs) != 0 {
					t.Fatalf("want no findings, got %+v", fs)
				}
				return
			}
			if len(fs) != 1 || fs[0].Category != CatKBStale || fs[0].Severity != SevWarning {
				t.Fatalf("want one kb-stale warning, got %+v", fs)
			}
			if fs[0].Title != tc.wantTitle {
				t.Fatalf("title = %q, want %q", fs[0].Title, tc.wantTitle)
			}
			if fs[0].Key != "kb-stale" {
				t.Fatalf("key = %q, want kb-stale", fs[0].Key)
			}
		})
	}
}
