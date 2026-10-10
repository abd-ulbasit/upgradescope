package kb

import (
	"bytes"
	"cmp"
	_ "embed"
	"encoding/json"
	"fmt"
	"net/url"
	"slices"

	"github.com/abd-ulbasit/upgradescope/internal/inventory"
)

//go:embed data/volumeplugins.json
var volumePluginsJSON []byte

// VolumePluginClass says what an in-tree volume plugin's end does to the
// workloads that still name it (#351).
type VolumePluginClass string

const (
	// VolumeRemoved: the plugin was removed with no migration path (or,
	// gitRepo, disabled): from Removed on, a pod that names it does not
	// start, whatever is installed.
	VolumeRemoved VolumePluginClass = "removed"
	// VolumeCSIMigration: the in-tree code is gone (or its CSI migration
	// can no longer be turned off) from Removed on, but the API field still
	// works: every operation is redirected to CSIDriver, which must be
	// installed. A cluster with the driver is fine.
	VolumeCSIMigration VolumePluginClass = "csi-migration"
	// VolumeDeprecated: deprecated only; nothing stops working yet.
	VolumeDeprecated VolumePluginClass = "deprecated"
)

// VolumePlugin is one in-tree volume plugin of data/volumeplugins.json:
// Plugin is its field name in a pod's volumes[] (corev1.VolumeSource) and
// a PersistentVolume's spec (corev1.PersistentVolumeSource), as the JSON
// spells it ("awsElasticBlockStore").
type VolumePlugin struct {
	Plugin         string             `json:"plugin"`
	Classification VolumePluginClass  `json:"classification"`
	Deprecated     *inventory.Version `json:"deprecated,omitempty"`
	// Removed is the minor whose release stopped the plugin (removed),
	// or from which it works only through CSIDriver (csi-migration).
	Removed     *inventory.Version `json:"removed,omitempty"`
	CSIDriver   string             `json:"csiDriver,omitempty"`
	Note        string             `json:"note,omitempty"`
	Replacement string             `json:"replacement"`
	// Citations are the release notes and pull requests every minor
	// above is taken from; never empty.
	Citations []string `json:"citations"`
}

type volumePluginsFile struct {
	Comment string         `json:"_comment"`
	Plugins []VolumePlugin `json:"plugins"`
}

// volumePlugins is the embedded dataset, parsed once. A dataset that does
// not parse or validate is a build defect (TestVolumePluginsData), so it
// panics at init rather than load as an empty list, which would read every
// volume as fine.
var volumePlugins = mustParseVolumePlugins(volumePluginsJSON)

// VolumePlugins returns a copy of the embedded in-tree volume plugin
// dataset, sorted by plugin.
func VolumePlugins() []VolumePlugin { return slices.Clone(volumePlugins) }

// VolumePluginNames lists the plugins' field names, sorted: what the
// collectors look for in pod volumes and PersistentVolumes.
func VolumePluginNames() []string {
	names := make([]string, len(volumePlugins))
	for i, p := range volumePlugins {
		names[i] = p.Plugin
	}
	return names
}

func mustParseVolumePlugins(raw []byte) []VolumePlugin {
	ps, err := parseVolumePlugins(raw)
	if err != nil {
		panic(err)
	}
	return ps
}

// parseVolumePlugins decodes and validates the dataset: unknown fields
// refused, each plugin once, a known classification, a removal minor for
// removed and csi-migration (and none for deprecated, which needs a
// deprecation minor), a CSI driver exactly for csi-migration, a
// replacement, and at least one https citation.
func parseVolumePlugins(raw []byte) ([]VolumePlugin, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var f volumePluginsFile
	if err := dec.Decode(&f); err != nil {
		return nil, fmt.Errorf("kb: volumeplugins.json: %w", err)
	}
	if len(f.Plugins) == 0 {
		return nil, fmt.Errorf("kb: volumeplugins.json lists no plugins")
	}
	seen := map[string]bool{}
	for _, p := range f.Plugins {
		bad := func(why string) error { return fmt.Errorf("kb: volumeplugins.json: plugin %q: %s", p.Plugin, why) }
		switch {
		case p.Plugin == "":
			return nil, bad("no plugin name")
		case seen[p.Plugin]:
			return nil, bad("listed twice")
		case p.Replacement == "":
			return nil, bad("no replacement")
		case len(p.Citations) == 0:
			return nil, bad("no citation")
		}
		seen[p.Plugin] = true
		switch p.Classification {
		case VolumeRemoved, VolumeCSIMigration:
			if p.Removed == nil {
				return nil, bad("classification " + string(p.Classification) + " needs a removed minor")
			}
		case VolumeDeprecated:
			if p.Removed != nil || p.Deprecated == nil {
				return nil, bad("classification deprecated takes a deprecated minor and no removed one")
			}
		default:
			return nil, bad(fmt.Sprintf("unknown classification %q", p.Classification))
		}
		if (p.Classification == VolumeCSIMigration) != (p.CSIDriver != "") {
			return nil, bad("a CSI driver is named exactly for classification csi-migration")
		}
		if p.Deprecated != nil && p.Removed != nil && p.Deprecated.Compare(*p.Removed) > 0 {
			return nil, bad("deprecated after it was removed")
		}
		for _, c := range p.Citations {
			if u, err := url.Parse(c); err != nil || u.Scheme != "https" || u.Host == "" {
				return nil, bad(fmt.Sprintf("citation %q is not an https URL", c))
			}
		}
	}
	slices.SortFunc(f.Plugins, func(a, b VolumePlugin) int { return cmp.Compare(a.Plugin, b.Plugin) })
	return f.Plugins, nil
}
