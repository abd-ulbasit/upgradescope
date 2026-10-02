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

// supplementJSON holds hand-curated lifecycle entries for types the
// deprecation guide documents but current k8s.io/api has deleted (so
// tools/gen-kb cannot extract them), e.g. policy/v1beta1 PodSecurityPolicy.
// Same file shape as apilifecycle.json; its maxKnownK8s is informational
// only — Load() takes MaxKnownK8s from the generated dataset.
//
//go:embed data/supplement.json
var supplementJSON []byte

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
func Load() (KB, error) {
	f, err := parseLifecycle(apilifecycleJSON)
	if err != nil {
		return KB{}, err
	}
	sup, err := parseLifecycle(supplementJSON)
	if err != nil {
		return KB{}, fmt.Errorf("kb: parsing supplement: %w", err)
	}
	maxKnown, err := inventory.ParseVersion(f.MaxKnownK8s)
	if err != nil {
		return KB{}, fmt.Errorf("kb: bad maxKnownK8s %q: %w", f.MaxKnownK8s, err)
	}
	addons, err := registry.Load()
	if err != nil {
		return KB{}, fmt.Errorf("kb: loading add-on registry: %w", err)
	}
	entries := mergeEntries(f.Entries, sup.Entries)
	version, err := datasetVersion(f.GeneratedFrom, entries, addons)
	if err != nil {
		return KB{}, err
	}
	return KB{
		Version:      version,
		APILifecycle: entries,
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
// the merged lifecycle entries or the parsed add-on registry. Any change to
// either dataset (an eol-sync date flip, a regenerated or hand-curated
// entry) changes the label; YAML comments and formatting do not.
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

// mergeEntries appends supplement entries to the generated ones, skipping
// any GVK the generator already covers — generated data always wins so a
// stale supplement can never mask fresher upstream lifecycle data.
func mergeEntries(generated, supplement []APILifecycleEntry) []APILifecycleEntry {
	seen := make(map[GVK]struct{}, len(generated))
	for _, e := range generated {
		seen[GVK{Group: e.Group, Version: e.Version, Kind: e.Kind}] = struct{}{}
	}
	out := generated
	for _, e := range supplement {
		if _, dup := seen[GVK{Group: e.Group, Version: e.Version, Kind: e.Kind}]; !dup {
			out = append(out, e)
		}
	}
	return out
}
