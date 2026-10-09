package engine

import (
	"slices"
	"strings"
	"testing"

	"github.com/abd-ulbasit/upgradescope/internal/inventory"
	"github.com/abd-ulbasit/upgradescope/internal/kb"
)

func realKB(t *testing.T) kb.KB {
	t.Helper()
	k, err := kb.Load()
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func manifestUsage(group, version, kind string) inventory.APIUsage {
	return inventory.APIUsage{Group: group, Version: version, Kind: kind, Count: 1, Namespaces: map[string]int{"": 1},
		Objects: []inventory.ObjectRef{{Name: "x", File: "d/m.yaml", Line: 1}}}
}

func manifestsInv(us ...inventory.APIUsage) inventory.Inventory {
	return inventory.Inventory{SchemaVersion: 1, ClusterID: "files", Source: inventory.SourceFiles, APIUsage: us}
}

// #266 on the shipped dataset: a manifest at an API version the target does
// not serve yet is a blocker naming the release that serves it, and the
// same manifest one release later is clean.
func TestFilesModeUnservedAPIIsABlocker(t *testing.T) {
	k := realKB(t)
	dc := manifestUsage("resource.k8s.io", "v1", "DeviceClass")
	mapol := manifestUsage("admissionregistration.k8s.io", "v1", "MutatingAdmissionPolicy")
	for _, c := range []struct {
		name      string
		us        inventory.APIUsage
		target    int
		wantTitle string // "" = no finding
		wantFix   string
	}{
		{"DeviceClass v1 at 1.33", dc, 33, "resource.k8s.io/v1 DeviceClass is not served until 1.34, after target 1.33 (1 object)", "write it as resource.k8s.io/v1beta2 DeviceClass"},
		{"DeviceClass v1 at its introduction", dc, 34, "", ""},
		{"DeviceClass v1 after it", dc, 35, "", ""},
		{"MutatingAdmissionPolicy v1 at 1.33", mapol, 33, "admissionregistration.k8s.io/v1 MutatingAdmissionPolicy is not served until 1.36, after target 1.33 (1 object)", "write it as admissionregistration.k8s.io/v1alpha1 MutatingAdmissionPolicy"},
		{"MutatingAdmissionPolicy v1 at 1.36", mapol, 36, "", ""},
		// Workload v1beta1 is introduced in 1.37 but deprecated from 1.40: the
		// Deprecated branch must not answer for it ("deprecated in 1.40,
		// after target 1.33" was the misleading info).
		{"Workload v1beta1 at 1.33", manifestUsage("scheduling.k8s.io", "v1beta1", "Workload"), 33,
			"scheduling.k8s.io/v1beta1 Workload is not served until 1.37, after target 1.33 (1 object)",
			"Kubernetes 1.33 serves no version of Workload the knowledge base knows: upgrade the cluster to Kubernetes 1.37"},
		{"Workload v1beta1 at 1.36 names v1alpha2", manifestUsage("scheduling.k8s.io", "v1beta1", "Workload"), 36,
			"scheduling.k8s.io/v1beta1 Workload is not served until 1.37, after target 1.36 (1 object)",
			"write it as scheduling.k8s.io/v1alpha2 Workload"},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := Evaluate(manifestsInv(c.us), k, inventory.Version{Major: 1, Minor: c.target}, testNow)
			var got []Finding
			for _, f := range r.Findings {
				if f.Category == CatRemovedAPI || f.Category == CatDeprecatedAPI {
					got = append(got, f)
				}
			}
			if c.wantTitle == "" {
				if len(got) != 0 {
					t.Fatalf("findings = %+v, want none", got)
				}
				return
			}
			if len(got) != 1 {
				t.Fatalf("findings = %+v, want exactly one", got)
			}
			f := got[0]
			if f.Severity != SevBlocker || f.Title != c.wantTitle || !strings.HasPrefix(f.Remediation, c.wantFix) {
				t.Errorf("finding = %s %q, remediation %q; want a blocker %q with remediation starting %q", f.Severity, f.Title, f.Remediation, c.wantTitle, c.wantFix)
			}
			if !strings.Contains(f.Detail, "fails with \"no matches for kind\"") || f.Key != "removed-api/"+apiKey(c.us.Group, c.us.Version, c.us.Kind) {
				t.Errorf("key %q detail %q", f.Key, f.Detail)
			}
			if r.Verdict != VerdictBlocked {
				t.Errorf("verdict = %s, want blocked", r.Verdict)
			}
		})
	}
}

// A live cluster's stored objects are never judged by Introduced: the
// cluster serves what it stores. Only objects the apply would create are.
func TestLiveInventoryIsSilentAboutIntroduced(t *testing.T) {
	k := realKB(t)
	stored := inventory.APIUsage{Group: "resource.k8s.io", Version: "v1", Kind: "DeviceClass", Count: 2, Namespaces: map[string]int{"": 2},
		Objects: []inventory.ObjectRef{{Name: "a"}, {Name: "b"}}}
	inv := inventory.Inventory{SchemaVersion: 1, ClusterID: "c", Source: inventory.SourceCluster, ServerVersion: "v1.32.4", APIUsage: []inventory.APIUsage{stored}}
	for _, f := range Evaluate(inv, k, inventory.Version{Major: 1, Minor: 33}, testNow).Findings {
		if f.Category == CatRemovedAPI {
			t.Errorf("live stored objects: %s", f.Title)
		}
	}
	// The gate's proposed state: manifest objects listed in a cluster's row.
	inv.APIUsage = []inventory.APIUsage{{Group: "resource.k8s.io", Version: "v1", Kind: "DeviceClass", Count: 1, Namespaces: map[string]int{"": 1},
		Objects: []inventory.ObjectRef{{Name: "gpu", File: "pr.yaml", Line: 1}}}}
	found := false
	for _, f := range Evaluate(inv, k, inventory.Version{Major: 1, Minor: 33}, testNow).Findings {
		found = found || f.Category == CatRemovedAPI && strings.Contains(f.Title, "is not served until 1.34")
	}
	if !found {
		t.Error("proposed manifest objects in a cluster inventory (the gate) are not judged by Introduced")
	}
}

// #266 removals past the horizon: "(projected)" in the title and detail,
// for the blocker (removed at or before the target) and the warning
// (removed at target+1) alike; a removal inside the horizon is unchanged.
func TestRemovalPastHorizonIsProjected(t *testing.T) {
	k := realKB(t)                                                                            // horizon 1.37
	u := manifestUsage("admissionregistration.k8s.io", "v1alpha1", "MutatingAdmissionPolicy") // removed 1.38
	for _, c := range []struct {
		target    int
		sev       Severity
		projected bool
	}{
		{37, SevWarning, true}, // removal at target+1, past the horizon
		{38, SevBlocker, true}, // removal at the target
		{39, SevBlocker, true}, // removal before the target
		{36, SevInfo, false},   // not yet within the window
	} {
		r := Evaluate(manifestsInv(u), k, inventory.Version{Major: 1, Minor: c.target}, testNow)
		var f *Finding
		for i := range r.Findings {
			if r.Findings[i].Key == "removed-api/admissionregistration.k8s.io/v1alpha1/MutatingAdmissionPolicy" {
				f = &r.Findings[i]
			}
		}
		if c.sev == SevInfo {
			if f != nil {
				t.Errorf("target 1.%d: finding %s, want none", c.target, f.Title)
			}
			continue
		}
		if f == nil || f.Severity != c.sev {
			t.Fatalf("target 1.%d: finding %+v, want a %s removed-api finding", c.target, f, c.sev)
		}
		if !strings.Contains(f.Title, "removed in 1.38 (projected)") || !strings.Contains(f.Detail, "projected") {
			t.Errorf("target 1.%d: title %q detail %q, want removed in 1.38 (projected) in the title and projected in the detail", c.target, f.Title, f.Detail)
		}
	}
	// A removal inside the horizon is a fact: no marker.
	r := Evaluate(manifestsInv(manifestUsage("extensions", "v1beta1", "Ingress")), k, inventory.Version{Major: 1, Minor: 30}, testNow)
	for _, f := range r.Findings {
		if strings.Contains(f.Title, "projected") || strings.Contains(f.Detail, "projected") {
			t.Errorf("a removal inside the horizon is marked projected: %q / %q", f.Title, f.Detail)
		}
	}
	// A target past the horizon says so in the kb-coverage gap.
	r = Evaluate(manifestsInv(u), k, inventory.Version{Major: 1, Minor: 38}, testNow)
	noted := false
	for _, g := range r.NotAssessed {
		noted = noted || g.Capability == GapKBCoverage && g.Required && strings.Contains(g.Reason, "API removals after 1.37 are projected")
	}
	if !noted {
		t.Errorf("notAssessed = %+v, want a required kb-coverage gap noting that removals after 1.37 are projected", r.NotAssessed)
	}
	r = Evaluate(manifestsInv(u), k, inventory.Version{Major: 1, Minor: 37}, testNow)
	for _, g := range r.NotAssessed {
		if g.Capability == GapKBCoverage {
			t.Errorf("target at the horizon has a kb-coverage gap: %+v", g)
		}
	}
}

// A replacement upstream tags nowhere is gen-kb's default (the kind's GA
// version): the finding that advises it also cites the successor's
// changelog, since the migration guide ends at v1.32 (#266).
func TestDefaultedReplacementCitesTheSuccessorsChangelog(t *testing.T) {
	k := realKB(t)
	idx := kb.NewIndex(k.APILifecycle)
	old, ok := idx.Lookup("admissionregistration.k8s.io", "v1beta1", "ValidatingAdmissionPolicy")
	if !ok || !old.ReplacementDefaulted || old.Replacement == nil {
		t.Fatalf("ValidatingAdmissionPolicy v1beta1 = %+v, want a defaulted replacement", old)
	}
	succ, _ := idx.Lookup(old.Replacement.Group, old.Replacement.Version, old.Replacement.Kind)
	r := Evaluate(manifestsInv(manifestUsage("admissionregistration.k8s.io", "v1beta1", "ValidatingAdmissionPolicy")), k, inventory.Version{Major: 1, Minor: 36}, testNow)
	for _, f := range r.Findings {
		if f.Key != "removed-api/admissionregistration.k8s.io/v1beta1/ValidatingAdmissionPolicy" {
			continue
		}
		if !strings.HasPrefix(f.Remediation, "migrate to admissionregistration.k8s.io/v1 ValidatingAdmissionPolicy") {
			t.Errorf("remediation = %q", f.Remediation)
		}
		if !slices.Contains(f.Citations, kb.ChangelogURL(succ.Introduced)) || !slices.Contains(f.Citations, deprecationGuideURL) {
			t.Errorf("citations = %v, want the migration guide and %s", f.Citations, kb.ChangelogURL(succ.Introduced))
		}
		return
	}
	t.Fatalf("no removed-api finding in %+v", r.Findings)
}
