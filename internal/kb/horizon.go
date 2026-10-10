package kb

import (
	"encoding/json"
	"fmt"

	"github.com/abd-ulbasit/upgradescope/internal/inventory"
)

// Horizon returns the newest Kubernetes minor the embedded API lifecycle
// dataset covers, the same value as Load().MaxKnownK8s. It reads that one
// field, so a command that only names the horizon in its help text does not
// pay for loading the whole knowledge base.
func Horizon() (inventory.Version, error) {
	var f struct {
		MaxKnownK8s string `json:"maxKnownK8s"`
	}
	if err := json.Unmarshal(apilifecycleJSON, &f); err != nil {
		return inventory.Version{}, fmt.Errorf("kb: corrupt apilifecycle.json: %w", err)
	}
	v, err := inventory.ParseVersion(f.MaxKnownK8s)
	if err != nil {
		return inventory.Version{}, fmt.Errorf("kb: bad maxKnownK8s %q: %w", f.MaxKnownK8s, err)
	}
	return v, nil
}
