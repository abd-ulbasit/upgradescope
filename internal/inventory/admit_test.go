package inventory

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

// TestAdmitChecksAsIngestDoes: Admit is the server's ingest check of a
// decoded inventory, in its order: the schema version, the collectorSchema,
// the serverVersion, the identifiers, then the limits once the free text is
// cut, so a capability reason over its limit is cut and admitted, not refused.
func TestAdmitChecksAsIngestDoes(t *testing.T) {
	long := strings.Repeat("x", MaxStringBytes+1)
	for _, tc := range []struct {
		name string
		edit func(*Inventory)
		want any // a pointer to the error type, or nil
	}{
		{"a valid inventory", func(*Inventory) {}, nil},
		{"no serverVersion (the versions collector failed)", func(inv *Inventory) { inv.ServerVersion = "" }, nil},
		{"a reason over its limit is cut", func(inv *Inventory) {
			inv.Capabilities = map[Capability]CapabilityStatus{CapHelm: {Available: true, Partial: true, Reason: strings.Repeat("r", MaxReasonBytes+6<<10)}}
		}, nil},
		{"another schemaVersion", func(inv *Inventory) { inv.SchemaVersion = 2 }, new(*SchemaVersionError)},
		{"no schemaVersion", func(inv *Inventory) { inv.SchemaVersion = 0 }, new(*SchemaVersionError)},
		{"the current collectorSchema", func(inv *Inventory) { inv.CollectorSchema = CurrentCollectorSchema }, nil},
		{"a later collectorSchema", func(inv *Inventory) { inv.CollectorSchema = CurrentCollectorSchema + 1 }, new(*CollectorSchemaError)},
		{"a negative collectorSchema", func(inv *Inventory) { inv.CollectorSchema = -1 }, new(*CollectorSchemaError)},
		{"a serverVersion that is not 1.x", func(inv *Inventory) { inv.ServerVersion = "v2.0.0" }, new(*ServerVersionError)},
		{"a serverVersion that is not a version", func(inv *Inventory) { inv.ServerVersion = "MARKER" }, new(*ServerVersionError)},
		{"an invalid identifier", func(inv *Inventory) { inv.Nodes[0].Name = "Not A Name" }, new(*IdentifierError)},
		{"an identifier is checked before the limits", func(inv *Inventory) {
			inv.Nodes[0].Name = "Not A Name"
			inv.Nodes[0].ContainerRuntime = long
		}, new(*IdentifierError)},
		{"a string over the limit", func(inv *Inventory) { inv.Nodes[0].ContainerRuntime = long }, new(*LimitError)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			inv := validInventory()
			inv.ServerVersion = "v1.34.2-gke.100"
			tc.edit(&inv)
			err := inv.Admit()
			switch want := tc.want.(type) {
			case nil:
				if err != nil {
					t.Fatalf("Admit() = %.300v, want nil", err)
				}
			case **SchemaVersionError:
				if !errors.As(err, want) {
					t.Fatalf("Admit() = %.300v, want a SchemaVersionError", err)
				}
			case **CollectorSchemaError:
				if !errors.As(err, want) {
					t.Fatalf("Admit() = %.300v, want a CollectorSchemaError", err)
				}
			case **ServerVersionError:
				if !errors.As(err, want) {
					t.Fatalf("Admit() = %.300v, want a ServerVersionError", err)
				}
			case **IdentifierError:
				if !errors.As(err, want) {
					t.Fatalf("Admit() = %.300v, want an IdentifierError", err)
				}
			case **LimitError:
				if !errors.As(err, want) {
					t.Fatalf("Admit() = %.300v, want a LimitError", err)
				}
			}
			for _, st := range inv.Capabilities {
				if len(st.Reason) > MaxReasonBytes {
					t.Errorf("a reason of %d bytes was admitted uncut", len(st.Reason))
				}
			}
		})
	}
}

// TestAdmitErrorsKeepIngestsMessages: the server answers a refused push
// with these messages (422), as it did before Admit held its checks.
func TestAdmitErrorsKeepIngestsMessages(t *testing.T) {
	inv := validInventory()
	inv.SchemaVersion = 2
	if err := inv.Admit(); err == nil || err.Error() != "unsupported inventory schemaVersion 2 (want 1)" {
		t.Errorf("schemaVersion: %v", err)
	}
	inv = validInventory()
	inv.CollectorSchema = CurrentCollectorSchema + 1
	if want := fmt.Sprintf("unsupported inventory collectorSchema %d (want %d, or none from collectors that predate it)", CurrentCollectorSchema+1, CurrentCollectorSchema); true {
		if err := inv.Admit(); err == nil || err.Error() != want {
			t.Errorf("collectorSchema: %v", err)
		}
	}
	inv = validInventory()
	inv.ServerVersion = "2.0"
	if err := inv.Admit(); err == nil || err.Error() != `invalid inventory serverVersion: invalid kubernetes version "2.0": major version must be 1` {
		t.Errorf("serverVersion: %v", err)
	}
}
