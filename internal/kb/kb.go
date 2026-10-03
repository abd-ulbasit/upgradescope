package kb

import (
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"fmt"

	"github.com/abd-ulbasit/upgradescope/internal/inventory"
	"github.com/abd-ulbasit/upgradescope/registry"
)

//go:embed data/apilifecycle.json
var apilifecycleJSON []byte

type KB struct {
	// Version labels the dataset in reports, the CRD status and auditor
	// exports. It is derived from the embedded data (see datasetVersion),
	// e.g. "k8s.io/api v0.37.1; lifecycle 1a2b3c4d; registry 5e6f7a8b".
	Version      string
	APILifecycle []APILifecycleEntry
	AddOns       []registry.AddOn
	Skew         SkewPolicy
	MaxKnownK8s  inventory.Version // newest minor the lifecycle data covers
	// UpgradeSteps are control-plane upgrades allowed to skip minors;
	// upgrade plans take them in place of the one-minor hops they span
	// (engine.HopTargets). Upstream has none, since the control plane is
	// upgraded one minor at a time, and Load adds none.
	UpgradeSteps []UpgradeStep
}

// UpgradeStep is one control-plane upgrade from From straight to To,
// skipping the minors between, such as a provider's long-term-support
// path. Citation links the provider documentation that allows it: a step
// is never added without one.
type UpgradeStep struct {
	From, To inventory.Version
	Citation string
}

// Load builds the KB from the embedded API lifecycle dataset, the embedded
// add-on registry, and the default skew policy. It fails loudly on a
// corrupt or empty dataset — a silent empty KB would mean silent green scans.
func Load() (KB, error) { return load(apilifecycleJSON) }

// LoadWithRegistry is Load with operator-supplied registry entries applied
// to the embedded ones (registry.LoadExtra: a file or a directory of
// *.yaml, validated like the embedded entries; an entry with an embedded
// id replaces it). extra "" is Load. A bad extra entry fails the load,
// naming the path and the file. The extra entries are part of the version
// label, so a report says which registry judged it.
func LoadWithRegistry(extra string) (KB, error) { return loadWith(apilifecycleJSON, extra) }

// load builds the KB from the given lifecycle dataset; Load passes the
// embedded one. Beyond parsing, it refuses a dataset too small or with too
// few removals to be the generated one (checkLifecycleFloors).
func load(lifecycle []byte) (KB, error) { return loadWith(lifecycle, "") }

func loadWith(lifecycle []byte, extra string) (KB, error) {
	f, err := parseLifecycle(lifecycle)
	if err != nil {
		return KB{}, err
	}
	if err := checkLifecycleFloors(f); err != nil {
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
	if extra != "" {
		more, err := registry.LoadExtra(extra)
		if err != nil {
			return KB{}, fmt.Errorf("kb: loading extra registry %s: %w", extra, err)
		}
		addons = registry.Merge(addons, more)
	}
	version, err := datasetVersion(f.GeneratedFrom, f.Entries, addons)
	if err != nil {
		return KB{}, err
	}
	return KB{
		Version:      version,
		APILifecycle: f.Entries,
		AddOns:       addons,
		Skew:         DefaultSkewPolicy(),
		MaxKnownK8s:  maxKnown,
	}, nil
}

// datasetVersion derives KB.Version from the data itself, so no constant
// has to be bumped when the weekly refresh changes it:
//
//	"<generatedFrom>; lifecycle <digest>; registry <digest>"
//
// generatedFrom names the upstream release ("k8s.io/api v0.37.1"); each
// digest is the first 8 hex digits of the SHA-256 of the canonical JSON of
// the lifecycle entries or the parsed add-on registry. Any change to
// either dataset (an eol-sync date flip, a regenerated entry) changes the
// label; YAML comments and formatting do not.
func datasetVersion(generatedFrom string, entries []APILifecycleEntry, addons []registry.AddOn) (string, error) {
	lifecycle, err := digest(entries)
	if err != nil {
		return "", fmt.Errorf("kb: digest lifecycle data: %w", err)
	}
	reg, err := digest(addons)
	if err != nil {
		return "", fmt.Errorf("kb: digest registry: %w", err)
	}
	return fmt.Sprintf("%s; lifecycle %s; registry %s", generatedFrom, lifecycle, reg), nil
}

func digest(v any) (string, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:4]), nil
}
