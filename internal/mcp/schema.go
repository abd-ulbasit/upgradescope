package mcp

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"regexp"
	"strings"
	"sync"

	"github.com/google/jsonschema-go/jsonschema"

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

// resolveSchema compiles a tool's output schema by the validator the MCP
// SDK applies to a tool's output, so a document that passes it is one the SDK
// sends.
func resolveSchema(schema map[string]any) *jsonschema.Resolved {
	raw, err := json.Marshal(schema)
	if err != nil {
		panic(fmt.Sprintf("mcp: output schema: %v", err))
	}
	var s jsonschema.Schema
	if err := json.Unmarshal(raw, &s); err != nil {
		panic(fmt.Sprintf("mcp: output schema: %v", err))
	}
	r, err := s.Resolve(nil)
	if err != nil {
		panic(fmt.Sprintf("mcp: output schema does not resolve: %v", err))
	}
	return r
}

// The output schemas of the tools, compiled once. markedResult checks the
// document it has cut against its tool's.
var (
	reportResolved   = sync.OnceValue(func() *jsonschema.Resolved { return resolveSchema(ReportOutputSchema()) })
	findingsResolved = sync.OnceValue(func() *jsonschema.Resolved { return resolveSchema(FindingsOutputSchema()) })
	scanResolved     = sync.OnceValue(func() *jsonschema.Resolved { return resolveSchema(ScanOutputSchema()) })
	registryResolved = sync.OnceValue(func() *jsonschema.Resolved { return resolveSchema(registryOutputSchema()) })
	fleetResolved    = sync.OnceValue(func() *jsonschema.Resolved { return resolveSchema(fleetOutputSchema()) })
)

// errNotJSON is the whole reason given for a document that is not JSON:
// encoding/json's error quotes the offending character, and the document
// may be any file an assistant named.
var errNotJSON = errors.New("not a JSON document")

// checkReport reports why doc, a report from a file or the fleet server, is
// not one api/report.schema.json accepts, or nil. Without it the SDK's own
// output check would refuse the result as a JSON-RPC protocol error, which
// an assistant cannot read as a reason. The reason names where in the
// schema the document fails and the rule it breaks, never a value of it.
func checkReport(doc json.RawMessage) error {
	var v any
	if err := json.Unmarshal(doc, &v); err != nil {
		return errNotJSON
	}
	if err := reportResolved().Validate(&v); err != nil {
		return fmt.Errorf("it does not follow api/report.schema.json (%s)", schemaReason(err))
	}
	// The schema leaves a report's objects open, and decoders differ in
	// what they make of a repeated or case-folded member name, so the
	// document the schema check judged must be the one every reader reads;
	// and marking the text may not cut a value the schema gives a form.
	if err := checkMembers(doc); err != nil {
		return err
	}
	return nil
}

var (
	schemaLocation = regexp.MustCompile(`^[A-Za-z0-9_/$.-]+$`)
	schemaKeyword  = regexp.MustCompile(`^[A-Za-z]+$`)
)

// schemaReason is the validator's error less the values it quotes: the
// innermost schema location it names ("validating /properties/target: ...")
// and the keyword that failed ("pattern"), which come from the schema, not
// from the document.
func schemaReason(err error) string {
	s := err.Error()
	at := "at the top level"
	for strings.HasPrefix(s, "validating ") {
		loc, rest, ok := strings.Cut(strings.TrimPrefix(s, "validating "), ": ")
		if !ok {
			break
		}
		if loc != "root" && schemaLocation.MatchString(loc) {
			at = "at " + loc
		}
		s = rest
	}
	if kw, _, ok := strings.Cut(s, ":"); ok && schemaKeyword.MatchString(kw) {
		return at + ", rule " + kw
	}
	return at
}
