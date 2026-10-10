package inventory

import (
	"errors"
	"strings"
	"testing"
)

func volumeInventory(vs ...VolumePluginUse) Inventory {
	return Inventory{SchemaVersion: SupportedSchemaVersion, ClusterID: "c", VolumePlugins: vs,
		Capabilities: map[Capability]CapabilityStatus{CapVolumes: {Available: true}}}
}

// #351: the volume plugin entries are checked at ingest as the API usage
// entries they resemble are: identifiers, one entry per plugin, the object
// cap, and the ignore annotations cut rather than refused.
func TestAdmitVolumePlugins(t *testing.T) {
	ok := volumeInventory(VolumePluginUse{Plugin: "glusterfs", Count: 2, Namespaces: map[string]int{"shop": 1, "": 1},
		Objects: []ObjectRef{{Namespace: "shop", Name: "web", File: "f.yaml", Line: 3}}})
	if err := ok.Admit(); err != nil {
		t.Fatalf("valid entry refused: %v", err)
	}

	var ie *IdentifierError
	bad := volumeInventory(VolumePluginUse{Plugin: "glusterfs", Count: 1, Namespaces: map[string]int{"Not_A_Namespace": 1}})
	if err := bad.Admit(); !errors.As(err, &ie) || ie.Field != "volumePlugins[0].namespaces" {
		t.Errorf("invalid namespace key: %v, want an IdentifierError at volumePlugins[0].namespaces", err)
	}
	bad = volumeInventory(VolumePluginUse{Plugin: "glusterfs", Count: 1, Objects: []ObjectRef{{Name: "a/b"}}})
	if err := bad.Admit(); !errors.As(err, &ie) || ie.Field != "volumePlugins[0].objects[0].name" {
		t.Errorf("invalid object name: %v, want an IdentifierError at volumePlugins[0].objects[0].name", err)
	}

	var le *LimitError
	dup := volumeInventory(VolumePluginUse{Plugin: "rbd", Count: 1}, VolumePluginUse{Plugin: "rbd", Count: 1})
	if err := dup.Admit(); !errors.As(err, &le) || le.Field != "volumePlugins[1].plugin" || strings.Contains(le.Unquoted(), "rbd") {
		t.Errorf("duplicate plugin: %v, want a LimitError at volumePlugins[1].plugin that Unquoted does not quote", err)
	}
	over := volumeInventory(VolumePluginUse{Plugin: "rbd", Count: MaxObjectRefs + 1, Objects: make([]ObjectRef, MaxObjectRefs+1)})
	if err := over.Admit(); !errors.As(err, &le) || le.Field != "volumePlugins[0].objects" {
		t.Errorf("over the object cap: %v, want a LimitError at volumePlugins[0].objects", err)
	}
	many := volumeInventory()
	for i := range MaxVolumePlugins + 1 {
		many.VolumePlugins = append(many.VolumePlugins, VolumePluginUse{Plugin: strings.Repeat("p", i+1), Count: 1})
	}
	if err := many.Admit(); !errors.As(err, &le) || le.Field != "volumePlugins" {
		t.Errorf("over the plugin cap: %v, want a LimitError at volumePlugins", err)
	}

	long := volumeInventory(VolumePluginUse{Plugin: "gitRepo", Count: 1,
		Objects: []ObjectRef{{Name: "w", IgnoreReason: strings.Repeat("x", MaxStringBytes+1)}}})
	if err := long.Admit(); err != nil {
		t.Fatalf("a long ignore-reason is cut, not refused: %v", err)
	}
	if got := long.VolumePlugins[0].Objects[0].IgnoreReason; len(got) > MaxStringBytes || !strings.HasSuffix(got, cutMark) {
		t.Errorf("ignore-reason not cut: %d bytes", len(got))
	}
}
