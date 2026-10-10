package inventory

import (
	"errors"
	"fmt"
	"slices"
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
		many.VolumePlugins = append(many.VolumePlugins, VolumePluginUse{Plugin: fmt.Sprintf("p%d", i), Count: 1})
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

// A plugin name is a field name, and a count is what a collector saw: a
// plugin with control characters, one over 64 bytes, a negative count or
// namespace count, and a manager the apiserver would not accept are
// refused, naming the field and quoting the value cut.
func TestAdmitRefusesHostileVolumePlugins(t *testing.T) {
	for _, tc := range []struct {
		name  string
		entry VolumePluginUse
		field string
	}{
		{"control characters", VolumePluginUse{Plugin: "glus\x1b[31mterfs\n", Count: 1}, "volumePlugins[0].plugin"},
		{"1000 bytes", VolumePluginUse{Plugin: strings.Repeat("g", 1000), Count: 1}, "volumePlugins[0].plugin"},
		{"65 bytes", VolumePluginUse{Plugin: strings.Repeat("g", 65), Count: 1}, "volumePlugins[0].plugin"},
		{"starts with a digit", VolumePluginUse{Plugin: "9p", Count: 1}, "volumePlugins[0].plugin"},
		{"negative count", VolumePluginUse{Plugin: "glusterfs", Count: -1}, "volumePlugins[0]"},
		{"negative objectsOmitted", VolumePluginUse{Plugin: "glusterfs", Count: 1, ObjectsOmitted: -3}, "volumePlugins[0]"},
		{"negative namespace count", VolumePluginUse{Plugin: "glusterfs", Count: 1, Namespaces: map[string]int{"shop": 1, "b": -1, "a": -2}}, "volumePlugins[0].namespaces"},
		{"manager", VolumePluginUse{Plugin: "glusterfs", Count: 1, Objects: []ObjectRef{{Name: "w", Manager: "kubectl\x00"}}}, "volumePlugins[0].objects[0].manager"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			inv := volumeInventory(tc.entry)
			err := inv.Admit()
			var ie *IdentifierError
			var le *LimitError
			var field string
			switch {
			case errors.As(err, &ie):
				field = ie.Field
			case errors.As(err, &le):
				field = le.Field
			default:
				t.Fatalf("Admit = %v, want an IdentifierError or LimitError", err)
			}
			if field != tc.field {
				t.Errorf("Admit = %v, field %q, want %q", err, field, tc.field)
			}
			if len(err.Error()) > 600 || strings.ContainsAny(err.Error(), "\x1b\n\x00") {
				t.Errorf("error %d bytes, quoting raw control characters: %q", len(err.Error()), err.Error())
			}
		})
	}
	// Of several negative namespace counts, the least key is named whatever
	// the map order.
	inv := volumeInventory(VolumePluginUse{Plugin: "glusterfs", Count: 1, Namespaces: map[string]int{"b": -1, "a": -2, "c": -1}})
	for range 20 {
		if err := inv.Admit(); err == nil || !strings.Contains(err.Error(), `namespace "a"`) {
			t.Fatalf("Admit = %v, want namespace \"a\" named", err)
		}
	}
}

// What Admit refuses of a volume plugin entry, the agent's Conform drops
// with a named reason, marking volumes partial, and keeps the rest: one
// pod's hostile volume must not make every push of the cluster fail (#268).
func TestConformRepairsVolumePlugins(t *testing.T) {
	good := VolumePluginUse{Plugin: "rbd", Count: 1, Namespaces: map[string]int{"shop": 1}}
	for _, tc := range []struct {
		name    string
		entries []VolumePluginUse
		reason  string
		skipped []string
		check   func(*testing.T, Inventory)
	}{
		{"control-character plugin", []VolumePluginUse{{Plugin: "glus\x1b[31mterfs\n", Count: 1}, good},
			"volume plugin entr(ies) dropped for a value Admit refuses", []string{`"glus\x1b[31mterfs\n"`}, nil},
		{"long plugin", []VolumePluginUse{good, {Plugin: strings.Repeat("g", 1000), Count: -4, Namespaces: map[string]int{"shop": -1}}},
			"volume plugin entr(ies) dropped for a value Admit refuses", []string{`"` + strings.Repeat("g", 40) + `"`}, nil},
		{"negative namespace count", []VolumePluginUse{{Plugin: "glusterfs", Count: 1, Namespaces: map[string]int{"shop": -1}}, good},
			"volume plugin entr(ies) dropped for a value Admit refuses", []string{"glusterfs"}, nil},
		{"namespace key", []VolumePluginUse{{Plugin: "glusterfs", Count: 2, Namespaces: map[string]int{"shop": 1, "Not_A_Namespace": 1}}, good},
			"namespace key(s) of a volume plugin use count dropped", nil, func(t *testing.T, inv Inventory) {
				if got := inv.VolumePlugins[0].Namespaces; len(got) != 1 || got["shop"] != 1 {
					t.Errorf("namespaces = %v", got)
				}
			}},
		{"object name", []VolumePluginUse{{Plugin: "glusterfs", Count: 2, Objects: []ObjectRef{{Namespace: "shop", Name: "a/b"}, {Namespace: "shop", Name: "web"}}}, good},
			"object(s) of volume plugin use dropped for a name that is not an object name", nil, func(t *testing.T, inv Inventory) {
				if v := inv.VolumePlugins[0]; len(v.Objects) != 1 || v.Objects[0].Name != "web" || v.ObjectsOmitted != 1 {
					t.Errorf("entry = %+v, want the bad object counted in objectsOmitted", v)
				}
			}},
		{"objects over the cap", []VolumePluginUse{func() VolumePluginUse {
			v := VolumePluginUse{Plugin: "glusterfs", Count: MaxObjectRefs + 5}
			for i := range MaxObjectRefs + 5 {
				v.Objects = append(v.Objects, ObjectRef{Namespace: "shop", Name: fmt.Sprintf("w%d", i)})
			}
			return v
		}(), good}, "object(s) of volume plugin use beyond the cap counted, not listed", nil, func(t *testing.T, inv Inventory) {
			if v := inv.VolumePlugins[0]; len(v.Objects) != MaxObjectRefs || v.ObjectsOmitted != 5 {
				t.Errorf("entry = %d objects, %d omitted", len(v.Objects), v.ObjectsOmitted)
			}
		}},
		{"plugins over the cap", func() []VolumePluginUse {
			vs := []VolumePluginUse{good}
			for i := range MaxVolumePlugins + 1 {
				vs = append(vs, VolumePluginUse{Plugin: fmt.Sprintf("p%d", i), Count: 1})
			}
			return vs
		}(), "volume plugin entr(ies) beyond the cap dropped", []string{fmt.Sprintf("p%d", MaxVolumePlugins-1), fmt.Sprintf("p%d", MaxVolumePlugins)}, func(t *testing.T, inv Inventory) {
			if len(inv.VolumePlugins) != MaxVolumePlugins {
				t.Errorf("%d entries, want %d", len(inv.VolumePlugins), MaxVolumePlugins)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			inv := conformable()
			inv.Capabilities[CapVolumes] = CapabilityStatus{Available: true}
			inv.VolumePlugins = slices.Clone(tc.entries)
			if probe := inv; probe.Admit() == nil {
				t.Fatal("test setup: Admit accepts the inventory before Conform")
			}
			notes, err := inv.Conform()
			if err != nil {
				t.Fatalf("Conform = %v, notes %q", err, notes)
			}
			if err := inv.Admit(); err != nil {
				t.Fatalf("Admit after Conform = %v", err)
			}
			st := inv.Capabilities[CapVolumes]
			if !st.Available || !st.Partial || !strings.Contains(st.Reason, tc.reason) {
				t.Errorf("volumes = %+v, want partial, reason with %q", st, tc.reason)
			}
			for _, s := range tc.skipped {
				if !slices.Contains(st.Skipped, s) {
					t.Errorf("volumes skipped = %q, want %q in it", st.Skipped, s)
				}
			}
			if len(notes) == 0 {
				t.Error("no notes returned")
			}
			if !slices.ContainsFunc(inv.VolumePlugins, func(v VolumePluginUse) bool { return v.Plugin == "rbd" }) {
				t.Errorf("entries = %+v, want the valid rbd entry kept", inv.VolumePlugins)
			}
			if tc.check != nil {
				tc.check(t, inv)
			}
			if notes, err := inv.Conform(); err != nil || len(notes) != 0 {
				t.Errorf("second Conform = %q, %v, want nothing", notes, err)
			}
		})
	}
}
