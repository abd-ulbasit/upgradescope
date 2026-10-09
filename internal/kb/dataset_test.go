package kb

import (
	"fmt"
	"os"
	"regexp"
	"testing"

	"github.com/abd-ulbasit/upgradescope/internal/inventory"
)

// TestNonResourceKindsAreNotInTheKB: wrappers and subresource bodies that
// kube-apiserver never stored (tools/gen-kb nonPersisted) have no lifecycle
// to judge. PodStatusResult used to be "removed 1.37" by inference, a
// removed-api blocker for a manifest no cluster could have (#166).
func TestNonResourceKindsAreNotInTheKB(t *testing.T) {
	f, err := parseLifecycle(apilifecycleJSON)
	if err != nil {
		t.Fatal(err)
	}
	idx := NewIndex(f.Entries)
	for _, c := range []struct{ group, version, kind string }{
		{"", "v1", "PodStatusResult"},
		{"", "v1", "EphemeralContainers"},
		{"extensions", "v1beta1", "ReplicationControllerDummy"},
		{"batch", "v1beta1", "JobTemplate"},
		{"batch", "v2alpha1", "JobTemplate"},
		{"apps", "v1beta1", "Scale"},
		{"apps", "v1beta2", "Scale"},
		{"extensions", "v1beta1", "Scale"},
		{"apps", "v1beta1", "DeploymentRollback"},
		{"extensions", "v1beta1", "DeploymentRollback"},
		{"admission.k8s.io", "v1beta1", "AdmissionReview"},
		{"apiextensions.k8s.io", "v1beta1", "ConversionReview"},
		{"policy", "v1beta1", "Eviction"},
		{"apidiscovery.k8s.io", "v2beta1", "APIGroupDiscovery"},
	} {
		if e, ok := idx.Lookup(c.group, c.version, c.kind); ok {
			t.Errorf("dataset has %s/%s %s (%+v), want none: it is not a persisted resource", c.group, c.version, c.kind, e)
		}
	}
}

// TestDatasetSanity cross-checks the generated dataset against well-known
// removal milestones (same facts pluto/kubent encode — used as a sanity
// check only, never copied).
func TestDatasetSanity(t *testing.T) {
	f, err := parseLifecycle(apilifecycleJSON)
	if err != nil {
		t.Fatalf("parseLifecycle(embedded) error = %v", err)
	}
	if len(f.Entries) < 150 {
		t.Fatalf("dataset has %d entries, want >= 150 — was gen-kb run?", len(f.Entries))
	}
	idx := NewIndex(f.Entries)

	cases := []struct {
		group, version, kind string
		removed              inventory.Version
	}{
		{"extensions", "v1beta1", "Ingress", inventory.Version{Major: 1, Minor: 22}},
		{"batch", "v1beta1", "CronJob", inventory.Version{Major: 1, Minor: 25}},
		// note: full group name as registered in the scheme
		{"flowcontrol.apiserver.k8s.io", "v1beta3", "FlowSchema", inventory.Version{Major: 1, Minor: 32}},
		// generated from k8s.io/apiextensions-apiserver and k8s.io/kube-aggregator
		{"apiextensions.k8s.io", "v1beta1", "CustomResourceDefinition", inventory.Version{Major: 1, Minor: 22}},
		{"apiregistration.k8s.io", "v1beta1", "APIService", inventory.Version{Major: 1, Minor: 22}},
	}
	for _, c := range cases {
		e, ok := idx.Lookup(c.group, c.version, c.kind)
		if !ok {
			t.Errorf("dataset missing %s/%s %s", c.group, c.version, c.kind)
			continue
		}
		if e.Removed == nil || *e.Removed != c.removed {
			t.Errorf("%s/%s %s: Removed = %v, want %v", c.group, c.version, c.kind, e.Removed, c.removed)
		}
	}
}

// TestActionFloorIsTheKnowledgeBaseFloor pins the floor action/run.sh
// refuses a target below ("local oldest=N") to inventory.OldestCovered, so
// the two cannot drift.
func TestActionFloorIsTheKnowledgeBaseFloor(t *testing.T) {
	b, err := os.ReadFile("../../action/run.sh")
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`(?m)^\s*local oldest=(\d+)$`).FindSubmatch(b)
	if m == nil {
		t.Fatal(`action/run.sh has no "local oldest=N" line`)
	}
	if want := fmt.Sprint(inventory.OldestCovered().Minor); string(m[1]) != want {
		t.Errorf("action/run.sh refuses targets below 1.%s, but inventory.OldestCovered() is %s", m[1], inventory.OldestCovered())
	}
}

// TestStaticFilesNameTheKnowledgeBaseFloor pins the floor the files that
// cannot compute it say (the Action's metadata and README, the OpenAPI
// document, the hand-written docs) to inventory.OldestCovered, as
// TestActionFloorIsTheKnowledgeBaseFloor does for run.sh, so a refresh that
// moves the floor fails here until they follow (#237). Each pattern has one
// capture group, the version it states; a pattern that matches nothing
// fails too, so rewording a sentence cannot silently leave a stale floor
// unchecked. scan --help and the reference pages generated from it and from
// the OpenAPI document follow the floor by construction
// (TestScanHelpNamesTheKnowledgeBaseFloor, the generators' drift checks).
func TestStaticFilesNameTheKnowledgeBaseFloor(t *testing.T) {
	floor := inventory.OldestCovered().String()
	for _, c := range []struct {
		file     string
		patterns []string
	}{
		{"action.yml", []string{`any\s+target\s+below\s+(1\.\d+),\s+the\s+oldest\s+minor`}},
		{"action/action.yml", []string{`any\s+target\s+below\s+(1\.\d+),\s+the\s+oldest\s+minor`}},
		{"action/README.md", []string{
			`refuses\s+any\s+target\s+below\s+(1\.\d+),\s+the\s+oldest\s+minor`,
			`or\s+below\s+(1\.\d+)\)`,
		}},
		{"api/openapi.yaml", []string{
			`oldest\s+the\s+knowledge\s+base\s+covers\s+\((1\.\d+)\)`,
			`from\s+(1\.\d+)\s+\(the\s+oldest`,
		}},
		{"docs/concepts/version-skew.md", []string{
			`covers,\s+(1\.\d+),\s+and`,
			`between\s+(1\.\d+)\s+and\s+the\s+cluster`,
		}},
		{"docs/concepts/verdict-and-score.md", []string{`below\s+(1\.\d+),\s+the\s+oldest\s+minor`}},
		{"docs/getting-started/ci-gate.md", []string{`below\s+(1\.\d+),\s+the\s+oldest\s+minor`}},
		{"docs/claims.md", []string{`a\s+minor\s+below\s+(1\.\d+),\s+the\s+oldest`}},
	} {
		b, err := os.ReadFile("../../" + c.file)
		if err != nil {
			t.Errorf("%s: %v", c.file, err)
			continue
		}
		for _, p := range c.patterns {
			ms := regexp.MustCompile(p).FindAllSubmatch(b, -1)
			if len(ms) == 0 {
				t.Errorf("%s: no match for /%s/: reword the pattern with the sentence that names the floor", c.file, p)
			}
			for _, m := range ms {
				if string(m[1]) != floor {
					t.Errorf("%s says the knowledge base covers from %s, but inventory.OldestCovered() is %s", c.file, m[1], floor)
				}
			}
		}
	}
}

// TestOldestCoveredMinorIsTheFirstRemoval pins the floor of every target
// (inventory.OldestCovered) to the dataset: the release of its earliest
// recorded API removal. A target below it judges nothing, so a truncated
// one (YAML makes target: 1.30 the number 1.3) must be refused rather
// than read ready (#237). A refresh that records an earlier removal fails
// here until the floor follows it.
func TestOldestCoveredMinorIsTheFirstRemoval(t *testing.T) {
	f, err := parseLifecycle(apilifecycleJSON)
	if err != nil {
		t.Fatal(err)
	}
	var first *inventory.Version
	for _, e := range f.Entries {
		if e.Removed != nil && (first == nil || e.Removed.Compare(*first) < 0) {
			first = e.Removed
		}
	}
	if first == nil {
		t.Fatal("dataset records no removal")
	}
	if got := inventory.OldestCovered(); got != *first {
		t.Errorf("inventory.OldestCovered() = %s, but the dataset's earliest removal is %s: update oldestCoveredMinor (and action/run.sh's floor)", got, first)
	}
}
