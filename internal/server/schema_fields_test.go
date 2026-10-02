package server

import (
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"testing"
)

// The published schemas (api/openapi.yaml, api/webhook.schema.json) allow
// fields they do not list, so a client that validates against them, or is
// generated from them, keeps working when a release adds a field (see
// docs/compatibility-policy.md). The strictness lives here instead:
// unlistedFields is what makes a field added without documentation fail
// the tests.

// unlistedFields returns the paths of object fields in v that schema does
// not list under properties (or cover with an object-valued
// additionalProperties). It follows local "#/..." $refs against root and
// merges allOf parts; under anyOf or oneOf, v is held to the branch that
// lists the most of it.
func unlistedFields(root, schema, v any, path string) []string {
	props, items, extra, alts := schemaParts(root, schema)
	if len(alts) > 0 {
		base := map[string]any{"properties": props}
		if items != nil {
			base["items"] = items
		}
		if extra != nil {
			base["additionalProperties"] = extra
		}
		var best []string
		for i, alt := range alts {
			got := unlistedFields(root, map[string]any{"allOf": []any{base, alt}}, v, path)
			if i == 0 || len(got) < len(best) {
				best = got
			}
		}
		return best
	}
	var out []string
	switch v := v.(type) {
	case map[string]any:
		for k, child := range v {
			if p, ok := props[k]; ok {
				out = append(out, unlistedFields(root, p, child, path+"/"+k)...)
			} else if extra != nil {
				out = append(out, unlistedFields(root, extra, child, path+"/"+k)...)
			} else {
				out = append(out, path+"/"+k)
			}
		}
	case []any:
		for i, child := range v {
			out = append(out, unlistedFields(root, items, child, fmt.Sprintf("%s/%d", path, i))...)
		}
	}
	sort.Strings(out)
	return out
}

// schemaParts resolves a schema's properties, items, object-valued
// additionalProperties and anyOf/oneOf branches, merging allOf parts and
// following local $refs.
func schemaParts(root, schema any) (props map[string]any, items any, extra map[string]any, alts []any) {
	s, _ := schema.(map[string]any)
	props = map[string]any{}
	merge := func(p map[string]any, i any, e map[string]any, a []any) {
		for k, v := range p {
			props[k] = v
		}
		if i != nil {
			items = i
		}
		if e != nil {
			extra = e
		}
		alts = append(alts, a...)
	}
	if ref, ok := s["$ref"].(string); ok {
		merge(schemaParts(root, resolveRef(root, ref)))
	}
	if all, ok := s["allOf"].([]any); ok {
		for _, part := range all {
			merge(schemaParts(root, part))
		}
	}
	for _, kw := range []string{"anyOf", "oneOf"} {
		if a, ok := s[kw].([]any); ok {
			alts = append(alts, a...)
		}
	}
	if p, ok := s["properties"].(map[string]any); ok {
		merge(p, nil, nil, nil)
	}
	if i, ok := s["items"]; ok && i != nil {
		items = i
	}
	if e, ok := s["additionalProperties"].(map[string]any); ok {
		extra = e
	}
	return props, items, extra, alts
}

// resolveRef resolves a local JSON pointer ("#/components/schemas/Finding",
// "#/$defs/finding") against root; any other ref resolves to nil.
func resolveRef(root any, ref string) any {
	ptr, ok := strings.CutPrefix(ref, "#")
	if !ok {
		return nil
	}
	cur := root
	for _, tok := range strings.Split(strings.TrimPrefix(ptr, "/"), "/") {
		if tok == "" {
			continue
		}
		tok = strings.ReplaceAll(strings.ReplaceAll(tok, "~1", "/"), "~0", "~")
		m, ok := cur.(map[string]any)
		if !ok {
			return nil
		}
		cur = m[tok]
	}
	return cur
}

// TestUnlistedFields: the walker sees through $ref, allOf, anyOf/oneOf,
// items and map-valued additionalProperties, so an undocumented field
// nested behind any of them is still reported.
func TestUnlistedFields(t *testing.T) {
	var root any
	if err := json.Unmarshal([]byte(`{
	  "components": {"schemas": {
	    "Doc": {
	      "allOf": [{"$ref": "#/components/schemas/Base"}],
	      "properties": {
	        "items": {"type": "array", "items": {"$ref": "#/components/schemas/Item"}},
	        "byName": {"type": "object", "additionalProperties": {"$ref": "#/components/schemas/Item"}},
	        "cell": {"anyOf": [{"$ref": "#/components/schemas/Item"}, {"type": "null"}]},
	        "either": {"oneOf": [{"$ref": "#/components/schemas/Item"}, {"$ref": "#/components/schemas/Err"}]}
	      }
	    },
	    "Base": {"type": "object", "properties": {"id": {"type": "integer"}}},
	    "Item": {"type": "object", "properties": {"name": {"type": "string"}}},
	    "Err": {"type": "object", "properties": {"error": {"type": "string"}}}
	  }}
	}`), &root); err != nil {
		t.Fatal(err)
	}
	schema := map[string]any{"$ref": "#/components/schemas/Doc"}
	for _, tc := range []struct {
		doc  string
		want []string
	}{
		{`{"id": 1, "items": [{"name": "a"}], "byName": {"x": {"name": "b"}}, "cell": null, "either": {"error": "e"}}`, nil},
		{`{"id": 1, "extra": true}`, []string{"/extra"}},
		{`{"items": [{"name": "a"}, {"name": "b", "new": 1}]}`, []string{"/items/1/new"}},
		{`{"byName": {"x": {"new": 1}}}`, []string{"/byName/x/new"}},
		{`{"cell": {"name": "a", "new": 1}}`, []string{"/cell/new"}},
		{`{"either": {"error": "e", "new": 1}}`, []string{"/either/new"}},
	} {
		var v any
		if err := json.Unmarshal([]byte(tc.doc), &v); err != nil {
			t.Fatal(err)
		}
		if got := unlistedFields(root, schema, v, ""); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: unlisted %v, want %v", tc.doc, got, tc.want)
		}
	}
}
