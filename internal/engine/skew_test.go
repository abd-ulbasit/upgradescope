package engine

import (
	"testing"

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

func TestEvalSkewNoServerVersionNoFindings(t *testing.T) {
	inv := inventory.Inventory{Nodes: []inventory.NodeInfo{{Name: "n", KubeletVersion: "v1.20.0"}}}
	if fs := evalSkew(inv, testKB(), inventory.Version{Major: 1, Minor: 35}); len(fs) != 0 {
		t.Fatalf("unparseable server version must yield nothing, got %+v", fs)
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
