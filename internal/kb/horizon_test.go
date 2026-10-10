package kb

import "testing"

// Horizon is the horizon Load reports, without loading the knowledge base.
func TestHorizonMatchesLoad(t *testing.T) {
	k, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	h, err := Horizon()
	if err != nil {
		t.Fatal(err)
	}
	if h != k.MaxKnownK8s {
		t.Errorf("Horizon = %s, Load().MaxKnownK8s = %s", h, k.MaxKnownK8s)
	}
}
