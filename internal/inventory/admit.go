package inventory

import "fmt"

// SupportedSchemaVersion is the schemaVersion of the inventories this
// module writes and judges. Another version means fields it would misread.
const SupportedSchemaVersion = 1

// SchemaVersionError is an inventory of a schemaVersion this module does
// not judge ({} and a missing one included).
type SchemaVersionError struct {
	Got int
}

func (e *SchemaVersionError) Error() string {
	return fmt.Sprintf("unsupported inventory schemaVersion %d (want %d)", e.Got, SupportedSchemaVersion)
}

// CollectorSchemaError is an inventory from a later collector generation
// than this module knows, or a negative one: field meanings it would judge
// as its own.
type CollectorSchemaError struct {
	Got int
}

func (e *CollectorSchemaError) Error() string {
	return fmt.Sprintf("unsupported inventory collectorSchema %d (want %d, or none from collectors that predate it)", e.Got, CurrentCollectorSchema)
}

// ServerVersionError is an inventory whose serverVersion is not a
// Kubernetes 1.x version. Err, from parseMajorOne, quotes the value.
type ServerVersionError struct {
	Err error
}

func (e *ServerVersionError) Error() string {
	return "invalid inventory serverVersion: " + e.Err.Error()
}

func (e *ServerVersionError) Unwrap() error { return e.Err }

// Admit checks a decoded inventory before it is judged, as the server does
// every push: its schemaVersion is SupportedSchemaVersion, its
// collectorSchema none or at most CurrentCollectorSchema, its
// serverVersion, when it has one, a Kubernetes 1.x version, its
// identifiers valid (ValidateIdentifiers), and, once the free text
// collectors copy whole is cut to the limits (CutFreeText, which changes
// inv), its other values within them (ValidateLimits). It returns the
// first problem as a *SchemaVersionError, *CollectorSchemaError,
// *ServerVersionError,
// *IdentifierError or *LimitError, whose Error quotes the value at fault
// (in part, where it can be long); IdentifierError.Unquoted and
// LimitError.Unquoted say the same without it. An inventory with no
// serverVersion at all (its versions collector failed) is admitted.
func (inv *Inventory) Admit() error {
	if inv.SchemaVersion != SupportedSchemaVersion {
		return &SchemaVersionError{Got: inv.SchemaVersion}
	}
	// A later collector schema means field meanings this module would judge
	// as its own, the mistake the server's legacyView exists to avoid.
	if inv.CollectorSchema < 0 || inv.CollectorSchema > CurrentCollectorSchema {
		return &CollectorSchemaError{Got: inv.CollectorSchema}
	}
	if inv.ServerVersion != "" {
		if _, err := parseMajorOne(inv.ServerVersion); err != nil {
			return &ServerVersionError{Err: err}
		}
	}
	// Most identifiers are read from objects the apiserver validated, but
	// not all (a Helm release's name is a label value, and the objects of
	// its stored manifest are text in a Secret anyone can create), so a
	// genuine collector conforms its inventory first (Conform) and one
	// that is not valid even then is not from a genuine inventory. Reports
	// repeat identifiers, so they are refused before anything is stored or
	// evaluated.
	if err := inv.ValidateIdentifiers(); err != nil {
		return err
	}
	// So are values beyond what any collector records: the engine repeats
	// them in what it builds. The free text a collector copies whole,
	// which Kubernetes lets be longer (capability reasons, the ignore
	// annotations), is cut to the limits first, as collectors since
	// CutFreeText do themselves: an older agent's inventory is not refused
	// for it.
	inv.CutFreeText()
	return inv.ValidateLimits()
}
