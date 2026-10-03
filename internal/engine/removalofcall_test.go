package engine

import (
	"testing"

	"github.com/abd-ulbasit/upgradescope/internal/inventory"
)

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
