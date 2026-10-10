package kb

import (
	"maps"
	"slices"
	"strings"
	"testing"
)

// TestVolumePluginsData holds the hand-authored in-tree volume plugin
// dataset (#351) to its contract: it parses, every entry is cited and its
// minors parse, and the plugins the issue names are classified as the
// upstream release notes say.
func TestVolumePluginsData(t *testing.T) {
	ps, err := parseVolumePlugins(volumePluginsJSON)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.IsSortedFunc(ps, func(a, b VolumePlugin) int { return strings.Compare(a.Plugin, b.Plugin) }) {
		t.Error("plugins are not sorted")
	}
	byName := map[string]VolumePlugin{}
	for _, p := range ps {
		byName[p.Plugin] = p
		if len(p.Citations) == 0 {
			t.Errorf("%s: no citation", p.Plugin)
		}
	}
	want := map[string]struct {
		class   VolumePluginClass
		removed string
		driver  string
	}{
		"glusterfs":            {VolumeRemoved, "1.26", ""},
		"cephfs":               {VolumeRemoved, "1.31", ""},
		"rbd":                  {VolumeRemoved, "1.31", ""},
		"gitRepo":              {VolumeRemoved, "1.33", ""},
		"awsElasticBlockStore": {VolumeCSIMigration, "1.27", "ebs.csi.aws.com"},
		"gcePersistentDisk":    {VolumeCSIMigration, "1.28", "pd.csi.storage.gke.io"},
		"flexVolume":           {VolumeDeprecated, "", ""},
	}
	for name, w := range want {
		p, ok := byName[name]
		if !ok {
			t.Errorf("%s: not in the dataset", name)
			continue
		}
		removed := ""
		if p.Removed != nil {
			removed = p.Removed.String()
		}
		if p.Classification != w.class || removed != w.removed || p.CSIDriver != w.driver {
			t.Errorf("%s = %s removed %q driver %q, want %s removed %q driver %q", name, p.Classification, removed, p.CSIDriver, w.class, w.removed, w.driver)
		}
	}
	k, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if !slices.EqualFunc(k.VolumePlugins, ps, func(a, b VolumePlugin) bool { return a.Plugin == b.Plugin }) {
		t.Error("Load does not carry the volume plugin dataset")
	}
	if got := VolumePluginNames(); len(got) != len(ps) || got[0] != ps[0].Plugin {
		t.Errorf("VolumePluginNames() = %v", got)
	}
}

func TestParseVolumePluginsRefuses(t *testing.T) {
	for name, raw := range map[string]string{
		"empty":            `{"plugins": []}`,
		"unknown field":    `{"plugins": [{"plugin": "x", "classification": "deprecated", "deprecated": "1.2", "replacement": "r", "citations": ["https://a.b/c"], "extra": 1}]}`,
		"uncited":          `{"plugins": [{"plugin": "x", "classification": "deprecated", "deprecated": "1.2", "replacement": "r", "citations": []}]}`,
		"bad minor":        `{"plugins": [{"plugin": "x", "classification": "removed", "removed": "one", "replacement": "r", "citations": ["https://a.b/c"]}]}`,
		"removed, no when": `{"plugins": [{"plugin": "x", "classification": "removed", "replacement": "r", "citations": ["https://a.b/c"]}]}`,
		"csi, no driver":   `{"plugins": [{"plugin": "x", "classification": "csi-migration", "removed": "1.27", "replacement": "r", "citations": ["https://a.b/c"]}]}`,
		"driver, not csi":  `{"plugins": [{"plugin": "x", "classification": "removed", "removed": "1.27", "csiDriver": "d", "replacement": "r", "citations": ["https://a.b/c"]}]}`,
		"unknown class":    `{"plugins": [{"plugin": "x", "classification": "gone", "removed": "1.27", "replacement": "r", "citations": ["https://a.b/c"]}]}`,
		"http citation":    `{"plugins": [{"plugin": "x", "classification": "removed", "removed": "1.27", "replacement": "r", "citations": ["http://a.b/c"]}]}`,
		"twice":            `{"plugins": [{"plugin": "x", "classification": "deprecated", "deprecated": "1.2", "replacement": "r", "citations": ["https://a.b/c"]}, {"plugin": "x", "classification": "deprecated", "deprecated": "1.2", "replacement": "r", "citations": ["https://a.b/c"]}]}`,
		"deprecated late":  `{"plugins": [{"plugin": "x", "classification": "removed", "deprecated": "1.30", "removed": "1.27", "replacement": "r", "citations": ["https://a.b/c"]}]}`,
		// #362: a StorageClass provisioner is in-tree, cited, and maps to
		// one plugin.
		"provisioner, uncited":     `{"plugins": [{"plugin": "x", "classification": "removed", "removed": "1.27", "provisioner": "kubernetes.io/x", "replacement": "r", "citations": ["https://a.b/c"]}]}`,
		"provisioner, http":        `{"plugins": [{"plugin": "x", "classification": "removed", "removed": "1.27", "provisioner": "kubernetes.io/x", "provisionerCitation": "http://a.b/c", "replacement": "r", "citations": ["https://a.b/c"]}]}`,
		"provisioner, not in-tree": `{"plugins": [{"plugin": "x", "classification": "removed", "removed": "1.27", "provisioner": "x.csi.example.com", "provisionerCitation": "https://a.b/c", "replacement": "r", "citations": ["https://a.b/c"]}]}`,
		"citation, no provisioner": `{"plugins": [{"plugin": "x", "classification": "removed", "removed": "1.27", "provisionerCitation": "https://a.b/c", "replacement": "r", "citations": ["https://a.b/c"]}]}`,
		"provisioner twice":        `{"plugins": [{"plugin": "x", "classification": "removed", "removed": "1.27", "provisioner": "kubernetes.io/x", "provisionerCitation": "https://a.b/c", "replacement": "r", "citations": ["https://a.b/c"]}, {"plugin": "y", "classification": "removed", "removed": "1.27", "provisioner": "kubernetes.io/x", "provisionerCitation": "https://a.b/c", "replacement": "r", "citations": ["https://a.b/c"]}]}`,
	} {
		if _, err := parseVolumePlugins([]byte(raw)); err == nil {
			t.Errorf("%s: parsed, want an error", name)
		}
	}
}

// TestVolumePluginProvisioners (#362): the in-tree provisioners a
// StorageClass names map to their plugins as upstream names them, each
// cited to the upstream source that names it; cephfs, which never had an
// in-tree provisioner, has none.
func TestVolumePluginProvisioners(t *testing.T) {
	want := map[string]string{
		"kubernetes.io/rbd":             "rbd",
		"kubernetes.io/glusterfs":       "glusterfs",
		"kubernetes.io/scaleio":         "scaleIO",
		"kubernetes.io/storageos":       "storageos",
		"kubernetes.io/quobyte":         "quobyte",
		"kubernetes.io/flocker":         "flocker",
		"kubernetes.io/aws-ebs":         "awsElasticBlockStore",
		"kubernetes.io/gce-pd":          "gcePersistentDisk",
		"kubernetes.io/azure-disk":      "azureDisk",
		"kubernetes.io/azure-file":      "azureFile",
		"kubernetes.io/cinder":          "cinder",
		"kubernetes.io/vsphere-volume":  "vsphereVolume",
		"kubernetes.io/portworx-volume": "portworxVolume",
	}
	if got := VolumePluginProvisioners(); !maps.Equal(got, want) {
		t.Errorf("VolumePluginProvisioners() = %v, want %v", got, want)
	}
	for _, p := range VolumePlugins() {
		if p.Provisioner != "" && !strings.HasPrefix(p.ProvisionerCitation, "https://github.com/kubernetes/kubernetes/blob/") {
			t.Errorf("%s: provisioner %s cited %q, want the upstream source that names it", p.Plugin, p.Provisioner, p.ProvisionerCitation)
		}
	}
	// A copy, not the dataset: a caller cannot change what the collector maps.
	VolumePluginProvisioners()["kubernetes.io/rbd"] = "x"
	if VolumePluginProvisioners()["kubernetes.io/rbd"] != "rbd" {
		t.Error("VolumePluginProvisioners returns the shared map")
	}
}
