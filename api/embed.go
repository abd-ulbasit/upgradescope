// Package api embeds the published machine-readable contracts so the binary
// can hand the report schema to MCP clients as a tool output schema.
package api

import _ "embed"

// ReportSchema is report.schema.json, the contract of the JSON report that
// `scan --output json`, the REST API and the MCP tools share.
//
//go:embed report.schema.json
var ReportSchema []byte
