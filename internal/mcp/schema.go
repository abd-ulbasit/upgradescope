package mcp

import (
	"encoding/json"
	"fmt"
	"maps"
	"sync"

	"github.com/abd-ulbasit/upgradescope/api"
)

// The tools' output schemas are built from api/report.schema.json, the
// published, versioned contract of the JSON report: a report a tool returns
// is the document `scan --output json` writes, and a finding is the one the
// report lists, so the CLI, the REST API and MCP share one contract.

// reportDoc is the parsed api/report.schema.json. Callers get copies of its
// pieces and never modify it.
var reportDoc = sync.OnceValue(func() map[string]any {
	var doc map[string]any
	if err := json.Unmarshal(api.ReportSchema, &doc); err != nil {
		panic(fmt.Sprintf("mcp: api/report.schema.json is not JSON: %v", err))
	}
	return doc
})

const schemaDialect = "https://json-schema.org/draft/2020-12/schema"

// ReportOutputSchema is api/report.schema.json itself, less its $id (the
// URL it is published at, which a client has no reason to fetch): the output
// schema of get_report.
func ReportOutputSchema() map[string]any {
	out := maps.Clone(reportDoc())
	delete(out, "$id")
	return out
}

// defsWithReport is the report schema's $defs plus the report itself as
// $defs.report, so a schema that holds reports or findings refers to the
// contract's definitions instead of restating them.
func defsWithReport() map[string]any {
	doc := reportDoc()
	defs := maps.Clone(doc["$defs"].(map[string]any))
	root := maps.Clone(doc)
	for _, k := range []string{"$id", "$schema", "$defs", "title", "description"} {
		delete(root, k)
	}
	defs["report"] = root
	return defs
}

// FindingsOutputSchema is the output schema of list_findings: the matching
// findings, each as api/report.schema.json defines it.
func FindingsOutputSchema() map[string]any {
	return map[string]any{
		"$schema":     schemaDialect,
		"description": "Findings of one report, filtered; each finding is a finding of api/report.schema.json.",
		"type":        "object",
		"required":    []any{"target", "findings", "total", "truncated"},
		"properties": map[string]any{
			"target":    map[string]any{"$ref": "#/$defs/minor"},
			"kbVersion": map[string]any{"description": "The knowledge base the report was judged with.", "type": "string"},
			"cluster":   map[string]any{"description": "Fleet mode: the cluster's name on the server.", "type": "string"},
			"findings":  map[string]any{"type": "array", "items": map[string]any{"$ref": "#/$defs/finding"}},
			"total":     map[string]any{"description": "Findings that match the filters, before limit.", "type": "integer", "minimum": 0},
			"truncated": map[string]any{"description": "More findings match than limit lets through.", "type": "boolean"},
		},
		"$defs": defsWithReport(),
	}
}

// ScanOutputSchema is the output schema of scan: one report per target.
func ScanOutputSchema() map[string]any {
	return map[string]any{
		"$schema":     schemaDialect,
		"description": "One report per requested target, in the order asked; each is api/report.schema.json.",
		"type":        "object",
		"required":    []any{"reports"},
		"properties": map[string]any{
			"reports": map[string]any{"type": "array", "items": map[string]any{"$ref": "#/$defs/report"}},
		},
		"$defs": defsWithReport(),
	}
}

// registryOutputSchema is the output schema of registry_lookup. Entries are
// the registry's own (registry/types.go, schema_version 2), which no
// published schema covers yet, so only their identity is required.
func registryOutputSchema() map[string]any {
	return map[string]any{
		"$schema":  schemaDialect,
		"type":     "object",
		"required": []any{"addons", "total", "truncated"},
		"properties": map[string]any{
			"addons": map[string]any{
				"type": "array",
				"items": map[string]any{
					"type":     "object",
					"required": []any{"id", "display_name", "support"},
				},
			},
			"total":     map[string]any{"type": "integer", "minimum": 0},
			"truncated": map[string]any{"type": "boolean"},
		},
	}
}

// fleetOutputSchema is the output schema of fleet_summary: the server's
// GET /api/v1/fleet document (api/openapi.yaml, FleetResponse).
func fleetOutputSchema() map[string]any {
	return map[string]any{
		"$schema":     schemaDialect,
		"description": "The server's GET /api/v1/fleet response: one row per cluster, one score cell per target (api/openapi.yaml, FleetResponse).",
		"type":        "object",
	}
}
