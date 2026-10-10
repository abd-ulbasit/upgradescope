package engine

import (
	"encoding/json"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/abd-ulbasit/upgradescope/internal/inventory"
)

// migrationInventory is a manifest set of the kinds whose fix is more than
// a new apiVersion (#330), each used once with a line, as files mode reads
// them.
func migrationInventory() inventory.Inventory {
	use := func(group, version, kind string) inventory.APIUsage {
		return inventory.APIUsage{Group: group, Version: version, Kind: kind, Count: 1, Namespaces: map[string]int{"web": 1},
			Objects: []inventory.ObjectRef{{Name: "x", Namespace: "web", Line: 3}}}
	}
	return inventory.Inventory{SchemaVersion: 1, ClusterID: "files", Source: inventory.SourceFiles, ServerVersion: "v1.21.0",
		APIUsage: []inventory.APIUsage{
			use("policy", "v1beta1", "PodSecurityPolicy"),
			use("extensions", "v1beta1", "Ingress"),
			use("networking.k8s.io", "v1beta1", "Ingress"),
			use("apiextensions.k8s.io", "v1beta1", "CustomResourceDefinition"),
			use("admissionregistration.k8s.io", "v1beta1", "ValidatingWebhookConfiguration"),
			use("admissionregistration.k8s.io", "v1beta1", "MutatingWebhookConfiguration"),
		}}
}

// #330: a finding for a kind whose replacement is no drop-in, or that has
// none, carries the hand-written migration note: the generated hint, "; ",
// then the note, or the note alone when there is no hint, with the note's
// citations after the deprecation guide. PSP at 1.25 names Pod Security
// Admission and cites the migration page; Ingress, CRD and both webhook
// configurations at 1.22 name the schema changes. Pinned by a golden.
func TestMigrationNotesInFindings(t *testing.T) {
	k := shippedKB(t)
	const pspPage = "https://kubernetes.io/docs/tasks/configure-pod-container/migrate-from-psp/"
	got := map[string]json.RawMessage{}
	for _, tgt := range []struct {
		name  string
		minor int
		kinds []string
		want  map[string][]string // kind -> phrases the remediation must hold
	}{
		{"1.25", 25, []string{"PodSecurityPolicy"}, map[string][]string{"PodSecurityPolicy": {"Pod Security Admission", "namespace"}}},
		{"1.22", 22, []string{"Ingress", "CustomResourceDefinition", "ValidatingWebhookConfiguration", "MutatingWebhookConfiguration"}, map[string][]string{
			"Ingress":                        {"migrate to networking.k8s.io/v1 Ingress; ", "spec.defaultBackend", "service.port.number", "pathType"},
			"CustomResourceDefinition":       {"migrate to apiextensions.k8s.io/v1 CustomResourceDefinition; ", "structural", "spec.versions[*]"},
			"ValidatingWebhookConfiguration": {"migrate to admissionregistration.k8s.io/v1 ValidatingWebhookConfiguration; ", "sideEffects", "admissionReviewVersions"},
			"MutatingWebhookConfiguration":   {"migrate to admissionregistration.k8s.io/v1 MutatingWebhookConfiguration; ", "sideEffects", "admissionReviewVersions"},
		}},
	} {
		r := Evaluate(migrationInventory(), k, inventory.Version{Major: 1, Minor: tgt.minor}, testNow)
		seen := map[string]bool{}
		for _, f := range r.Findings {
			if f.Category != CatRemovedAPI {
				continue
			}
			for kind, phrases := range tgt.want {
				if !strings.Contains(f.Title, " "+kind+" ") {
					continue
				}
				seen[kind] = true
				for _, p := range phrases {
					if !strings.Contains(f.Remediation, p) {
						t.Errorf("target %s, %s: remediation %q lacks %q", tgt.name, f.Title, f.Remediation, p)
					}
				}
				if kind == "PodSecurityPolicy" && !slices.Contains(f.Citations, pspPage) {
					t.Errorf("PSP citations %v lack the migration page", f.Citations)
				}
				if kind == "PodSecurityPolicy" && strings.HasPrefix(f.Remediation, "migrate to") {
					t.Errorf("PSP has no successor to migrate to, got %q", f.Remediation)
				}
				if f.Citations[0] != deprecationGuideURL {
					t.Errorf("%s: the deprecation guide must stay the first citation: %v", f.Title, f.Citations)
				}
			}
		}
		for kind := range tgt.want {
			if !seen[kind] {
				t.Errorf("target %s: no removed-api finding for %s", tgt.name, kind)
			}
		}
		raw, err := json.Marshal(r.Findings)
		if err != nil {
			t.Fatal(err)
		}
		got[tgt.name] = raw
	}
	gotRaw, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	const golden = "testdata/migration-notes.expected.json"
	if *update {
		if err := os.WriteFile(golden, []byte(canonical(t, gotRaw)), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	wantRaw, err := os.ReadFile(golden)
	if err != nil {
		t.Fatal(err)
	}
	if want := canonical(t, wantRaw); canonical(t, gotRaw) != want {
		t.Errorf("migration-notes golden mismatch\n got:\n%s\nwant:\n%s", canonical(t, gotRaw), want)
	}
}
