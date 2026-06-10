package kb

import (
	_ "embed"
	"fmt"

	"github.com/abd-ulbasit/upgradescope/internal/inventory"
	"github.com/abd-ulbasit/upgradescope/registry"
)

//go:embed data/apilifecycle.json
var apilifecycleJSON []byte

// registryDate versions the embedded registry data; bump it whenever
// registry/data changes. Part of KB.Version.
const registryDate = "2026-06-10"

type KB struct {
	Version      string // dataset version, e.g. "k8s-1.36+registry-2026-06-10"
	APILifecycle []APILifecycleEntry
	AddOns       []registry.AddOn
	Skew         SkewPolicy
	MaxKnownK8s  inventory.Version // newest minor the lifecycle data covers
}

// Load builds the KB from the embedded API lifecycle dataset, the embedded
// add-on registry, and the default skew policy. It fails loudly on a
// corrupt or empty dataset — a silent empty KB would mean silent green scans.
func Load() (KB, error) {
	f, err := parseLifecycle(apilifecycleJSON)
	if err != nil {
		return KB{}, err
	}
	maxKnown, err := inventory.ParseVersion(f.MaxKnownK8s)
	if err != nil {
		return KB{}, fmt.Errorf("kb: bad maxKnownK8s %q: %w", f.MaxKnownK8s, err)
	}
	addons, err := registry.Load()
	if err != nil {
		return KB{}, fmt.Errorf("kb: loading add-on registry: %w", err)
	}
	return KB{
		Version:      fmt.Sprintf("k8s-%s+registry-%s", maxKnown.String(), registryDate),
		APILifecycle: f.Entries,
		AddOns:       addons,
		Skew:         DefaultSkewPolicy(),
		MaxKnownK8s:  maxKnown,
	}, nil
}
