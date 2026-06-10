package kb

import (
	"testing"

	"github.com/abd-ulbasit/upgradescope/internal/inventory"
)

func TestLoad(t *testing.T) {
	k, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if k.MaxKnownK8s != (inventory.Version{Major: 1, Minor: 36}) {
		t.Errorf("MaxKnownK8s = %v, want 1.36", k.MaxKnownK8s)
	}
	if want := "k8s-1.36+registry-" + registryDate; k.Version != want {
		t.Errorf("Version = %q, want %q", k.Version, want)
	}
	if len(k.APILifecycle) == 0 {
		t.Error("APILifecycle is empty")
	}
	if len(k.AddOns) == 0 {
		t.Error("AddOns is empty — registry.Load() returned nothing")
	}
	if k.Skew != DefaultSkewPolicy() {
		t.Errorf("Skew = %+v, want DefaultSkewPolicy()", k.Skew)
	}
}
