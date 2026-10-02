package engine

// ReportSchemaVersion versions the JSON shape of a report: `scan --output
// json` (and --write-baseline) and the server's report-shaped /api/v1
// responses. Within one schemaVersion fields are only ever added, never
// renamed, removed, retyped or given a new meaning, so consumers must
// ignore fields they do not know; any breaking change bumps it (see
// docs/compatibility-policy.md and api/report.schema.json).
const ReportSchemaVersion = 1

// Envelope leads every report-shaped JSON document: embedded first in the
// presentation struct, its fields come before the report's. ToolVersion is
// informational (the build that wrote the document) and carries no
// compatibility meaning.
type Envelope struct {
	SchemaVersion int    `json:"schemaVersion"`
	ToolVersion   string `json:"toolVersion"`
}

// NewEnvelope is the envelope of a report written by toolVersion.
func NewEnvelope(toolVersion string) Envelope {
	return Envelope{SchemaVersion: ReportSchemaVersion, ToolVersion: toolVersion}
}
