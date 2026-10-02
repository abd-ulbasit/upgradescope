package kb

import (
	"testing"

	"github.com/abd-ulbasit/upgradescope/internal/inventory"
)

// TestKBCoversDeletedAlphaGVKs pins alpha APIs that k8s.io/api deleted
// before any generated dataset recorded them (#124 KB-01). Unknown to the
// KB, their objects were dropped without a finding, so a manifest still
// using one read as ready. tools/gen-kb now derives them from the k8s.io/api
// releases since v0.17.0. Removed is the first release whose kube-apiserver
// registers no storage for the type at that version (kubernetes/kubernetes
// pkg/registry/<group>/rest at v<removed>.0 and the release before), which
// matches the k8s.io/api release that deleted the type except for
// networking.k8s.io/v1alpha1, deleted in v0.34 but unserved from 1.31.
func TestKBCoversDeletedAlphaGVKs(t *testing.T) {
	k, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	idx := NewIndex(k.APILifecycle)
	cases := []struct {
		group, version string
		kinds          []string
		removed        int
	}{
		// pkg/registry/resource/rest/storage_resource.go
		{"resource.k8s.io", "v1alpha1", []string{"PodScheduling", "ResourceClaim", "ResourceClaimTemplate", "ResourceClass"}, 27},
		{"resource.k8s.io", "v1alpha2", []string{"PodSchedulingContext", "ResourceClaim", "ResourceClaimParameters",
			"ResourceClaimTemplate", "ResourceClass", "ResourceClassParameters", "ResourceSlice"}, 31},
		{"resource.k8s.io", "v1alpha3", []string{"PodSchedulingContext"}, 32},
		{"resource.k8s.io", "v1alpha3", []string{"DeviceClass", "ResourceClaim", "ResourceClaimTemplate", "ResourceSlice"}, 34},
		// pkg/registry/networking/rest/storage_settings.go
		{"networking.k8s.io", "v1alpha1", []string{"ClusterCIDR"}, 29},
		{"networking.k8s.io", "v1alpha1", []string{"IPAddress", "ServiceCIDR"}, 31},
		// pkg/registry/coordination/rest/storage_coordination.go
		{"coordination.k8s.io", "v1alpha1", []string{"LeaseCandidate"}, 32},
	}
	n := 0
	for _, c := range cases {
		for _, kind := range c.kinds {
			n++
			e, ok := idx.Lookup(c.group, c.version, kind)
			if !ok {
				t.Errorf("KB missing %s/%s %s", c.group, c.version, kind)
				continue
			}
			if want := (inventory.Version{Major: 1, Minor: c.removed}); e.Removed == nil || *e.Removed != want {
				t.Errorf("%s/%s %s: Removed = %v, want %v", c.group, c.version, kind, e.Removed, want)
			}
			if !e.RemovedInferred {
				t.Errorf("%s/%s %s: RemovedInferred = false, want true (no upstream tag records this removal)", c.group, c.version, kind)
			}
		}
	}
	if n != 20 {
		t.Fatalf("test covers %d GVKs, want the 20 #124 lists", n)
	}
}
