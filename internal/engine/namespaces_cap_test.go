package engine

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/abd-ulbasit/upgradescope/internal/inventory"
)

// A finding lists at most MaxFindingNamespaces namespaces, in Namespaces
// and in its evidence sentence, and counts the rest, as it does objects:
// a 21 MB push of usages with 1,000 namespaces each wrote every name twice
// per finding, three 42 MB reports. Teams still come from every affected
// namespace, so team scores count the finding for each team.
func TestFindingNamespacesAreCapped(t *testing.T) {
	const n = MaxFindingNamespaces + 50
	counts := map[string]int{"": 1}
	var nsInfo []inventory.NamespaceInfo
	for i := range n {
		ns := fmt.Sprintf("ns-%03d", i)
		counts[ns] = 1
		nsInfo = append(nsInfo, inventory.NamespaceInfo{Name: ns, Team: fmt.Sprintf("team-%03d", i)})
	}
	inv := inventory.Inventory{
		SchemaVersion: 1, ServerVersion: "v1.21.0",
		APIUsage:   []inventory.APIUsage{{Group: "extensions", Version: "v1beta1", Kind: "Ingress", Count: n + 1, Namespaces: counts}},
		Namespaces: nsInfo,
	}
	rep := Evaluate(inv, testKB(), inventory.Version{Major: 1, Minor: 22}, time.Date(2026, 6, 10, 0, 0, 0, 0, time.UTC))
	var f *Finding
	for i := range rep.Findings {
		if rep.Findings[i].Key == "removed-api/extensions/v1beta1/Ingress" {
			f = &rep.Findings[i]
		}
	}
	if f == nil {
		t.Fatalf("no removed-api finding in %+v", rep.Findings)
	}
	if len(f.Namespaces) != MaxFindingNamespaces || f.NamespacesOmitted != n-MaxFindingNamespaces ||
		f.Namespaces[0] != "ns-000" || f.Namespaces[MaxFindingNamespaces-1] != fmt.Sprintf("ns-%03d", MaxFindingNamespaces-1) {
		t.Fatalf("namespaces %d listed (%v…), %d omitted; want the first %d sorted, %d omitted",
			len(f.Namespaces), f.Namespaces[:3], f.NamespacesOmitted, MaxFindingNamespaces, n-MaxFindingNamespaces)
	}
	if len(f.Teams) != n {
		t.Fatalf("%d teams, want all %d affected namespaces' teams", len(f.Teams), n)
	}
	wantTail := fmt.Sprintf("ns-%03d (1), and %d more namespace(s).", MaxFindingNamespaces-1, n-MaxFindingNamespaces)
	if !strings.HasPrefix(f.Detail, fmt.Sprintf("%d object(s) still stored/served at this version: cluster-scoped (1), ns-000 (1), ", n+1)) ||
		!strings.HasSuffix(f.Detail, wantTail) || strings.Count(f.Detail, "(1)") != MaxFindingNamespaces+1 {
		t.Fatalf("detail = %q, want cluster-scoped, the first %d namespaces and %q", f.Detail, MaxFindingNamespaces, wantTail)
	}
	if got := TeamScores(rep); len(got) != n {
		t.Fatalf("team scores for %d teams, want %d", len(got), n)
	}

	// At the cap nothing is omitted or counted.
	at := map[string]int{}
	for i := range MaxFindingNamespaces {
		at[fmt.Sprintf("ns-%03d", i)] = 1
	}
	inv.APIUsage[0].Namespaces = at
	rep = Evaluate(inv, testKB(), inventory.Version{Major: 1, Minor: 22}, time.Date(2026, 6, 10, 0, 0, 0, 0, time.UTC))
	for _, f := range rep.Findings {
		if f.NamespacesOmitted != 0 || strings.Contains(f.Detail, "more namespace") {
			t.Fatalf("at the cap: %d omitted, detail %q", f.NamespacesOmitted, f.Detail)
		}
	}
}

// Add-on findings name their installs' namespaces, capped the same way.
func TestAddOnFindingNamespacesAreCapped(t *testing.T) {
	var ns []string
	for i := range MaxFindingNamespaces + 7 {
		ns = append(ns, fmt.Sprintf("mesh-%03d", i))
	}
	inv := inventory.Inventory{
		AddOns: []inventory.AddOnInstance{{ID: "ingress-nginx", Version: "4.7.1", Namespaces: ns, Source: "chart"}},
	}
	rep := Evaluate(inv, testRegistryKB(), inventory.Version{Major: 1, Minor: 30}, testNow)
	found := false
	for _, f := range rep.Findings {
		if f.Category != CatEOLAddon && f.Category != CatChartIncompat {
			continue
		}
		found = true
		if len(f.Namespaces) != MaxFindingNamespaces || f.NamespacesOmitted != 7 {
			t.Fatalf("%s: %d namespaces listed, %d omitted; want %d and 7", f.Key, len(f.Namespaces), f.NamespacesOmitted, MaxFindingNamespaces)
		}
	}
	if !found {
		t.Fatalf("no add-on finding in %+v", rep.Findings)
	}
}
