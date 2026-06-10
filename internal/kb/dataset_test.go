package kb

import (
	"testing"

	"github.com/abd-ulbasit/upgradescope/internal/inventory"
)

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
