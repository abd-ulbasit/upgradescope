package engine

import (
	"testing"

	"github.com/abd-ulbasit/upgradescope/internal/inventory"
	"github.com/abd-ulbasit/upgradescope/internal/kb"
)

// TestRemovalOfCallShippedKB pins the removals that docs/operations.md and
// docs/claims.md (NT-01) use as their examples of a control-plane upgrade
// that removes an API, against the knowledge base that ships rather than a
// fixture: a flowschemas v1beta3 caller is gone after v1.31 to v1.32, but a
// servicecidrs v1beta1 caller is still served until v1.37.
func TestRemovalOfCallShippedKB(t *testing.T) {
	k, err := kb.Load()
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		key  string
		want inventory.Version
	}{
		{"deprecated-api-in-use/flowcontrol.apiserver.k8s.io/v1beta3/flowschemas", inventory.Version{Major: 1, Minor: 32}},
		{"deprecated-api-in-use/networking.k8s.io/v1beta1/servicecidrs", inventory.Version{Major: 1, Minor: 37}},
	} {
		got, ok := RemovalOfCall(k, tc.key)
		if !ok || got != tc.want {
			t.Errorf("RemovalOfCall(%q) = %v, %v; want %v, true", tc.key, got, ok, tc.want)
		}
	}
}

func TestRemovalOfCall(t *testing.T) {
	k := testKB()
	none := inventory.Version{}
	for _, tc := range []struct {
		key  string
		want inventory.Version
		ok   bool
	}{
		{"deprecated-api-in-use/extensions/v1beta1/ingresses", inventory.Version{Major: 1, Minor: 22}, true},
		{"deprecated-api-in-use/extensions/v1beta1/ingresses/status", inventory.Version{Major: 1, Minor: 22}, true},
		{"deprecated-api-in-use/batch/v1beta1/cronjobs", inventory.Version{Major: 1, Minor: 25}, true},
		{"deprecated-api-in-use/core/v1/componentstatuses", none, false}, // known, never removed
		{"deprecated-api-in-use/batch/v1beta2/cronjobs", none, false},    // unknown version
		{"deprecated-api-in-use/example.com/v1/widgets", none, false},    // unknown group
		{"removed-api/extensions/v1beta1/Ingress", none, false},          // another category
		{"deprecated-api-in-use/extensions", none, false},                // malformed
		{"", none, false},
	} {
		got, ok := RemovalOfCall(k, tc.key)
		if got != tc.want || ok != tc.ok {
			t.Errorf("RemovalOfCall(%q) = %v, %v; want %v, %v", tc.key, got, ok, tc.want, tc.ok)
		}
	}
}
