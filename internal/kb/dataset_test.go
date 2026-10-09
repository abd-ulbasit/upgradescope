package kb

import (
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
