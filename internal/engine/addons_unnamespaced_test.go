package engine

import (
	"testing"

	"github.com/abd-ulbasit/upgradescope/internal/inventory"
)

// An install in no named namespace (a manifest object without
// metadata.namespace in files mode, an IngressClass on a cluster) is not
// listed in a finding's Namespaces, so a namespace-scoped ignore rule must
// be able to tell the finding covers it: Finding.Unnamespaced (#237).
func TestAddOnFindingsMarkInstallsInNoNamedNamespace(t *testing.T) {
	cases := []struct {
		name   string
		addons []inventory.AddOnInstance
		want   bool
	}{
		{"unset namespace beside sandbox", []inventory.AddOnInstance{
			{ID: "ingress-nginx", Version: "4.7.1", Namespaces: []string{""}, Source: "image"},
			{ID: "ingress-nginx", Version: "4.7.1", Namespaces: []string{"sandbox"}, Source: "image"},
		}, true},
		{"an unset namespace among a install's namespaces", []inventory.AddOnInstance{
			{ID: "ingress-nginx", Version: "4.7.1", Namespaces: []string{"", "sandbox"}, Source: "image"},
		}, true},
		{"cluster-scoped IngressClass beside sandbox", []inventory.AddOnInstance{
			{ID: "ingress-nginx", Version: "4.7.1", Source: "ingressclass"},
			{ID: "ingress-nginx", Version: "4.7.1", Namespaces: []string{"sandbox"}, Source: "image"},
		}, true},
		{"unset namespace alone", []inventory.AddOnInstance{
			{ID: "ingress-nginx", Version: "4.7.1", Namespaces: []string{""}, Source: "image"},
		}, true},
		{"every install in a named namespace", []inventory.AddOnInstance{
			{ID: "ingress-nginx", Version: "4.7.1", Namespaces: []string{"edge"}, Source: "image"},
			{ID: "ingress-nginx", Version: "4.7.1", Namespaces: []string{"sandbox"}, Source: "image"},
		}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Target 1.38 is past the registry's 1.33: an EOL blocker and a
			// chart-incompat blocker, both about every install.
			fs := evalAddOns(inventory.Inventory{AddOns: tc.addons}, testRegistryKB(), inventory.Version{Major: 1, Minor: 38}, testNow)
			seen := map[Category]bool{}
			for _, f := range fs {
				seen[f.Category] = true
				if f.Unnamespaced != tc.want {
					t.Errorf("%s: Unnamespaced = %v, want %v (namespaces %v)", f.Key, f.Unnamespaced, tc.want, f.Namespaces)
				}
			}
			if !seen[CatEOLAddon] || !seen[CatChartIncompat] {
				t.Fatalf("findings = %+v, want an eol-addon and a chart-incompat finding", fs)
			}
		})
	}
}

// Only the installs that cannot run the target are the chart-incompat
// finding's: a fine install in no named namespace does not mark it.
func TestChartIncompatMarksOnlyTheIncompatibleInstalls(t *testing.T) {
	inv := inventory.Inventory{AddOns: []inventory.AddOnInstance{
		{ID: "ingress-nginx", Version: "4.7.1", Namespaces: []string{"sandbox"}, Source: "image"},
		{ID: "ingress-nginx", Version: "3.0.0", Namespaces: []string{""}, Source: "image"}, // matches no compat row
	}}
	for _, f := range evalAddOns(inv, testRegistryKB(), inventory.Version{Major: 1, Minor: 38}, testNow) {
		if f.Category == CatChartIncompat && f.Unnamespaced {
			t.Errorf("chart-incompat finding %v is marked unnamespaced, but only the sandbox install is incompatible", f.Namespaces)
		}
	}
}

// Node runtimes have no namespaces at all and are not marked.
func TestNodeRuntimeFindingsAreNotUnnamespaced(t *testing.T) {
	fs := evalAddOns(nodes("containerd://1.7.27"), runtimeKB("1.37"), inventory.Version{Major: 1, Minor: 38}, day("2026-10-02"))
	if len(fs) == 0 {
		t.Fatal("no node runtime findings")
	}
	for _, f := range fs {
		if f.Unnamespaced {
			t.Errorf("%s: a node runtime finding is marked unnamespaced", f.Key)
		}
	}
}
