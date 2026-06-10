package kb

import (
	"testing"

	"github.com/abd-ulbasit/upgradescope/internal/inventory"
)

func TestMergeEntries(t *testing.T) {
	generated := []APILifecycleEntry{
		{Group: "batch", Version: "v1beta1", Kind: "CronJob",
			Introduced: inventory.Version{Major: 1, Minor: 8},
			Removed:    &inventory.Version{Major: 1, Minor: 25}},
	}
	supplement := []APILifecycleEntry{
		// Duplicate GVK with conflicting data — the generated entry must win.
		{Group: "batch", Version: "v1beta1", Kind: "CronJob",
			Introduced: inventory.Version{Major: 1, Minor: 9},
			Removed:    &inventory.Version{Major: 1, Minor: 99}},
		// Supplement-only entry — must be appended.
		{Group: "policy", Version: "v1beta1", Kind: "PodSecurityPolicy",
			Introduced: inventory.Version{Major: 1, Minor: 10},
			Removed:    &inventory.Version{Major: 1, Minor: 25}},
	}

	merged := mergeEntries(generated, supplement)
	if len(merged) != 2 {
		t.Fatalf("len(merged) = %d, want 2", len(merged))
	}
	idx := NewIndex(merged)

	cj, ok := idx.Lookup("batch", "v1beta1", "CronJob")
	if !ok {
		t.Fatal("merged missing batch/v1beta1 CronJob")
	}
	if cj.Introduced != (inventory.Version{Major: 1, Minor: 8}) || *cj.Removed != (inventory.Version{Major: 1, Minor: 25}) {
		t.Errorf("CronJob = %+v — supplement overrode generated entry, want generated to win", cj)
	}
	if _, ok := idx.Lookup("policy", "v1beta1", "PodSecurityPolicy"); !ok {
		t.Error("merged missing supplement-only policy/v1beta1 PodSecurityPolicy")
	}
}

// TestSupplement pins the hand-curated entries for types the deprecation
// guide documents but current k8s.io/api has deleted, end-to-end via Load().
// Facts verified against
// https://kubernetes.io/docs/reference/using-api/deprecation-guide/ and the
// historical zz_generated.prerelease-lifecycle.go files in
// k8s.io/api@kubernetes-1.28.0.
func TestSupplement(t *testing.T) {
	k, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	idx := NewIndex(k.APILifecycle)

	psp, ok := idx.Lookup("policy", "v1beta1", "PodSecurityPolicy")
	if !ok {
		t.Fatal("Load() missing policy/v1beta1 PodSecurityPolicy (supplement not merged?)")
	}
	if psp.Removed == nil || *psp.Removed != (inventory.Version{Major: 1, Minor: 25}) {
		t.Errorf("PSP Removed = %v, want 1.25", psp.Removed)
	}
	if psp.Replacement != nil {
		t.Errorf("PSP Replacement = %v, want nil (Pod Security Admission is not an API)", psp.Replacement)
	}

	cases := []struct {
		version string
		removed inventory.Version
	}{
		{"v2beta1", inventory.Version{Major: 1, Minor: 25}},
		{"v2beta2", inventory.Version{Major: 1, Minor: 26}},
	}
	for _, c := range cases {
		e, ok := idx.Lookup("autoscaling", c.version, "HorizontalPodAutoscaler")
		if !ok {
			t.Errorf("Load() missing autoscaling/%s HorizontalPodAutoscaler", c.version)
			continue
		}
		if e.Removed == nil || *e.Removed != c.removed {
			t.Errorf("HPA %s: Removed = %v, want %v", c.version, e.Removed, c.removed)
		}
		if e.Replacement == nil || e.Replacement.Group != "autoscaling" || e.Replacement.Version != "v2" {
			t.Errorf("HPA %s: Replacement = %v, want autoscaling/v2", c.version, e.Replacement)
		}
	}
}
