package main

// genAPIMarkdown renders api/openapi.yaml into the docs site's REST API
// reference: operations grouped by tag, then the schemas. It reads the
// document as YAML nodes so operations, fields and schemas keep the order
// the document gives them.

import (
	"bytes"
	"fmt"
	"slices"
	"strings"

	"go.yaml.in/yaml/v3"
)

const openapiSourceURL = "https://github.com/abd-ulbasit/upgradescope/blob/main/api/openapi.yaml"

var httpMethods = []string{"get", "put", "post", "delete", "options", "head", "patch", "trace"}

type apiDoc struct {
	root *yaml.Node // the document's top-level mapping
}

// get returns the value under key in mapping n, or nil.
func get(n *yaml.Node, key string) *yaml.Node {
	if n == nil || n.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(n.Content); i += 2 {
		if n.Content[i].Value == key {
			return n.Content[i+1]
		}
	}
	return nil
}

type pair struct {
	key string
	val *yaml.Node
}

// pairs lists mapping n's entries in document order.
func pairs(n *yaml.Node) []pair {
	if n == nil || n.Kind != yaml.MappingNode {
		return nil
	}
	out := make([]pair, 0, len(n.Content)/2)
	for i := 0; i+1 < len(n.Content); i += 2 {
		out = append(out, pair{n.Content[i].Value, n.Content[i+1]})
	}
	return out
}

func str(n *yaml.Node) string {
	if n == nil {
		return ""
	}
	return n.Value
}

func strs(n *yaml.Node) []string {
	if n == nil {
		return nil
	}
	var out []string
	for _, c := range n.Content {
		out = append(out, c.Value)
	}
	return out
}

// resolve follows a local $ref ("#/components/schemas/X") to its node.
func (d apiDoc) resolve(n *yaml.Node) *yaml.Node {
	for n != nil {
		ref := str(get(n, "$ref"))
		if !strings.HasPrefix(ref, "#/") {
			return n
		}
		target := d.root
		for _, tok := range strings.Split(ref[2:], "/") {
			target = get(target, strings.ReplaceAll(strings.ReplaceAll(tok, "~1", "/"), "~0", "~"))
		}
		if target == nil {
			return n
		}
		n = target
	}
	return n
}

// schemaLink is the Markdown link to a component schema's section.
func schemaLink(ref string) string {
	name := ref[strings.LastIndex(ref, "/")+1:]
	return fmt.Sprintf("[%s](#%s)", name, strings.ToLower(name))
}

// typeOf names a schema's type for a table cell.
func (d apiDoc) typeOf(s *yaml.Node) string {
	if s == nil {
		return "any"
	}
	if ref := str(get(s, "$ref")); strings.HasPrefix(ref, "#/components/schemas/") {
		return schemaLink(ref)
	}
	for _, k := range []string{"anyOf", "oneOf"} {
		if alts := get(s, k); alts != nil {
			var parts []string
			for _, a := range alts.Content {
				parts = append(parts, d.typeOf(a))
			}
			return strings.Join(parts, " or ")
		}
	}
	if all := get(s, "allOf"); all != nil && len(all.Content) == 1 {
		return d.typeOf(all.Content[0])
	}
	if c := get(s, "const"); c != nil {
		return "`" + c.Value + "`"
	}
	if e := get(s, "enum"); e != nil {
		var vals []string
		for _, v := range e.Content {
			vals = append(vals, "`"+v.Value+"`")
		}
		return strings.Join(vals, " | ")
	}
	t := get(s, "type")
	var types []string
	if t != nil && t.Kind == yaml.SequenceNode {
		types = strs(t)
	} else if t != nil {
		types = []string{t.Value}
	}
	if len(types) == 0 {
		if get(s, "properties") != nil || get(s, "allOf") != nil {
			return "object"
		}
		return "any"
	}
	var parts []string
	for _, ty := range types {
		switch ty {
		case "array":
			if items := get(s, "items"); items != nil {
				parts = append(parts, "array of "+d.typeOf(items))
			} else {
				parts = append(parts, "array")
			}
		case "object":
			if ap := get(s, "additionalProperties"); ap != nil && ap.Kind == yaml.MappingNode && get(s, "properties") == nil {
				parts = append(parts, "map of "+d.typeOf(ap))
			} else {
				parts = append(parts, "object")
			}
		default:
			if f := str(get(s, "format")); f != "" {
				ty += " (" + f + ")"
			}
			parts = append(parts, ty)
		}
	}
	return strings.Join(parts, " or ")
}

type field struct {
	name     string
	schema   *yaml.Node
	required bool
}

// fields flattens a schema's properties, allOf parts included, in order.
func (d apiDoc) fields(s *yaml.Node) []field {
	s = d.resolve(s)
	var out []field
	add := func(f field) {
		for i := range out {
			if out[i].name == f.name {
				out[i].schema = f.schema
				out[i].required = out[i].required || f.required
				return
			}
		}
		out = append(out, f)
	}
	if all := get(s, "allOf"); all != nil {
		for _, part := range all.Content {
			for _, f := range d.fields(part) {
				add(f)
			}
		}
	}
	required := strs(get(s, "required"))
	for _, p := range pairs(get(s, "properties")) {
		add(field{p.key, p.val, slices.Contains(required, p.key)})
	}
	return out
}

func yesNo(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}

// genAPIMarkdown renders an OpenAPI 3.1 document.
func genAPIMarkdown(spec []byte) ([]byte, error) {
	var file yaml.Node
	if err := yaml.Unmarshal(spec, &file); err != nil {
		return nil, err
	}
	if len(file.Content) == 0 {
		return nil, fmt.Errorf("empty OpenAPI document")
	}
	d := apiDoc{root: file.Content[0]}
	info := get(d.root, "info")
	var b bytes.Buffer
	fmt.Fprintf(&b, "# %s\n\n", str(get(info, "title")))
	b.WriteString("<!-- Generated by tools/gen-docs from api/openapi.yaml (make docs-gen). Do not edit. -->\n\n")
	fmt.Fprintf(&b, "Rendered from the OpenAPI %s document [`api/openapi.yaml`](%s), which tests check against every route and handler response.\n\n",
		str(get(d.root, "openapi")), openapiSourceURL)
	if desc := strings.TrimSpace(str(get(info, "description"))); desc != "" {
		b.WriteString(desc + "\n\n")
	}

	if schemes := pairs(get(get(d.root, "components"), "securitySchemes")); len(schemes) > 0 {
		b.WriteString("## Authentication\n\n| Scheme | Type | Description |\n|---|---|---|\n")
		for _, sc := range schemes {
			fmt.Fprintf(&b, "| `%s` | %s | %s |\n", sc.key, schemeKind(sc.val), cell(str(get(sc.val, "description"))))
		}
		b.WriteString("\n")
	}

	type op struct {
		method, path string
		pathItem     *yaml.Node
		node         *yaml.Node
	}
	byTag := map[string][]op{}
	for _, p := range pairs(get(d.root, "paths")) {
		for _, m := range pairs(p.val) {
			if !slices.Contains(httpMethods, m.key) {
				continue
			}
			tag := "other"
			if tags := strs(get(m.val, "tags")); len(tags) > 0 {
				tag = tags[0]
			}
			byTag[tag] = append(byTag[tag], op{m.key, p.key, p.val, m.val})
		}
	}
	var tagOrder []string
	tagDesc := map[string]string{}
	for _, t := range get(d.root, "tags").Content {
		tagOrder = append(tagOrder, str(get(t, "name")))
		tagDesc[str(get(t, "name"))] = str(get(t, "description"))
	}
	if _, ok := byTag["other"]; ok {
		tagOrder = append(tagOrder, "other")
	}
	for _, tag := range tagOrder {
		ops := byTag[tag]
		if len(ops) == 0 {
			continue
		}
		fmt.Fprintf(&b, "## %s\n\n", tag)
		if desc := strings.TrimSpace(tagDesc[tag]); desc != "" {
			b.WriteString(desc + "\n\n")
		}
		for _, o := range ops {
			d.writeOperation(&b, strings.ToUpper(o.method), o.path, o.pathItem, o.node)
		}
	}

	b.WriteString("## Schemas\n\n")
	for _, sc := range pairs(get(get(d.root, "components"), "schemas")) {
		d.writeSchema(&b, sc.key, sc.val)
	}
	return bytes.TrimRight(b.Bytes(), "\n"), nil
}

func schemeKind(n *yaml.Node) string {
	if s := str(get(n, "scheme")); s != "" {
		return s
	}
	return str(get(n, "type"))
}

func (d apiDoc) writeOperation(b *bytes.Buffer, method, path string, pathItem, op *yaml.Node) {
	fmt.Fprintf(b, "### `%s %s`\n\n", method, path)
	summary := strings.TrimSpace(str(get(op, "summary")))
	desc := strings.TrimSpace(str(get(op, "description")))
	switch {
	case summary != "" && desc != "":
		fmt.Fprintf(b, "**%s.** %s\n\n", strings.TrimSuffix(summary, "."), desc)
	case summary != "":
		fmt.Fprintf(b, "**%s.**\n\n", strings.TrimSuffix(summary, "."))
	case desc != "":
		b.WriteString(desc + "\n\n")
	}

	security := get(op, "security")
	if security == nil {
		security = get(d.root, "security")
	}
	var auth []string
	for _, req := range security.Content {
		for _, p := range pairs(req) {
			scheme := get(get(get(d.root, "components"), "securitySchemes"), p.key)
			auth = append(auth, fmt.Sprintf("`%s` (%s)", p.key, schemeKind(scheme)))
		}
	}
	if len(auth) == 0 {
		b.WriteString("Auth: none.\n\n")
	} else {
		fmt.Fprintf(b, "Auth: %s.\n\n", strings.Join(auth, " or "))
	}

	var params []*yaml.Node
	if pp := get(pathItem, "parameters"); pp != nil {
		params = append(params, pp.Content...)
	}
	if pp := get(op, "parameters"); pp != nil {
		params = append(params, pp.Content...)
	}
	if len(params) > 0 {
		b.WriteString("| Parameter | In | Type | Required | Description |\n|---|---|---|---|---|\n")
		for _, p := range params {
			p = d.resolve(p)
			fmt.Fprintf(b, "| `%s` | %s | %s | %s | %s |\n", str(get(p, "name")), str(get(p, "in")),
				cell(d.typeOf(get(p, "schema"))), yesNo(str(get(p, "required")) == "true"), cell(str(get(p, "description"))))
		}
		b.WriteString("\n")
	}

	if body := d.resolve(get(op, "requestBody")); body != nil {
		for _, c := range pairs(get(body, "content")) {
			fmt.Fprintf(b, "Request body (`%s`): %s\n\n", c.key, d.typeOf(get(c.val, "schema")))
		}
	}

	b.WriteString("| Status | Content type | Schema | Description |\n|---|---|---|---|\n")
	for _, r := range pairs(get(op, "responses")) {
		resp := d.resolve(r.val)
		desc := cell(str(get(resp, "description")))
		content := pairs(get(resp, "content"))
		if len(content) == 0 {
			fmt.Fprintf(b, "| %s | — | — | %s |\n", r.key, desc)
			continue
		}
		for _, c := range content {
			fmt.Fprintf(b, "| %s | `%s` | %s | %s |\n", r.key, c.key, cell(d.typeOf(get(c.val, "schema"))), desc)
		}
	}
	b.WriteString("\n")
}

func (d apiDoc) writeSchema(b *bytes.Buffer, name string, s *yaml.Node) {
	fmt.Fprintf(b, "### %s\n\n", name)
	if desc := strings.TrimSpace(str(get(s, "description"))); desc != "" {
		b.WriteString(desc + "\n\n")
	}
	fs := d.fields(s)
	if len(fs) == 0 {
		line := "Type: " + d.typeOf(s)
		if p := str(get(s, "pattern")); p != "" {
			line += ", pattern `" + p + "`"
		}
		b.WriteString(strings.ReplaceAll(line, " | ", ", ") + ".\n\n")
		return
	}
	b.WriteString("| Field | Type | Required | Description |\n|---|---|---|---|\n")
	for _, f := range fs {
		fmt.Fprintf(b, "| `%s` | %s | %s | %s |\n", f.name, cell(d.typeOf(f.schema)), yesNo(f.required), cell(str(get(f.schema, "description"))))
	}
	b.WriteString("\n")
}
