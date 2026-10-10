package mcp

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// A report from a file or the fleet server is judged by api/report.schema.json,
// which leaves its objects open (no additionalProperties: false), so a
// document can carry a member beside a declared one: a "FINDINGS" beside
// "findings", a second "target". encoding/json matches a struct's fields
// without regard to case and lets the last of equal names win, so a reader
// that decodes into a struct reads a member the schema check never judged,
// and the tools could disagree with each other about one document. Two
// rules close that: the readers below decode exactly the declared names, and
// checkMembers refuses a report in which such a member exists at all.

// reportView is what list_findings reads of a report: the members it
// returns and filters on, each read by its exact name.
type reportView struct {
	Target    string
	KBVersion string
	Findings  []json.RawMessage
}

// decodeReport reads doc's "target", "kbVersion" and "findings" members by
// those exact names, as map[string]json.RawMessage keeps them. A member that
// is absent or not the type the schema gives it reads as empty: checkReport
// has refused such a report already.
func decodeReport(doc json.RawMessage) (reportView, error) {
	var members map[string]json.RawMessage
	if err := json.Unmarshal(doc, &members); err != nil {
		return reportView{}, errNotJSON
	}
	var v reportView
	if raw, ok := members["target"]; ok {
		if err := json.Unmarshal(raw, &v.Target); err != nil {
			return reportView{}, errNotJSON
		}
	}
	if raw, ok := members["kbVersion"]; ok {
		if err := json.Unmarshal(raw, &v.KBVersion); err != nil {
			return reportView{}, errNotJSON
		}
	}
	if raw, ok := members["findings"]; ok {
		if err := json.Unmarshal(raw, &v.Findings); err != nil {
			return reportView{}, errNotJSON
		}
	}
	return v, nil
}

// findingKey is a finding's "severity" and "category", each read by its
// exact name.
func findingKey(raw json.RawMessage) (severity, category string, err error) {
	var members map[string]json.RawMessage
	if err := json.Unmarshal(raw, &members); err != nil {
		return "", "", errNotJSON
	}
	for name, dst := range map[string]*string{"severity": &severity, "category": &category} {
		if m, ok := members[name]; ok {
			if err := json.Unmarshal(m, dst); err != nil {
				return "", "", errNotJSON
			}
		}
	}
	return severity, category, nil
}

// reportTarget is the "target" member of a report that passed checkReport.
func reportTarget(doc json.RawMessage) string {
	var members map[string]json.RawMessage
	var target string
	if json.Unmarshal(doc, &members) == nil {
		_ = json.Unmarshal(members["target"], &target)
	}
	return target
}

// checkMembers refuses a report in which an object repeats a member name, or
// has a member whose name differs only in case from a property the schema
// declares for it, and a string the schema gives a pattern that is longer
// than MaxClusterTextBytes (markClusterText would cut it, and a cut value no
// longer has the form the pattern requires). The reason names the rule and
// where in the report it is, through the schema's own property names and
// array indexes ("*" for a member the schema does not name), and quotes
// nothing of the document.
func checkMembers(doc []byte) error {
	dec := json.NewDecoder(bytes.NewReader(doc))
	w := memberWalker{dec: dec}
	root := reportDoc()
	if err := w.value("", w.expand(root)); err != nil {
		var re *memberError
		if errors.As(err, &re) {
			return re
		}
		return errNotJSON
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return errNotJSON
	}
	return nil
}

// memberError is a rule a report breaks, at a location.
type memberError struct{ msg string }

func (e *memberError) Error() string { return e.msg }

func rule(at, format string, args ...any) error {
	if at == "" {
		at = "the top level"
	} else {
		at = "at " + at
	}
	return &memberError{fmt.Sprintf("it has "+format+" (%s)", append(args, at)...)}
}

type memberWalker struct{ dec *json.Decoder }

// expand is the schema objects that apply to a value of schema node n: n
// itself and, followed through $ref (to this document's $defs) and allOf,
// those it refers to.
func (w *memberWalker) expand(n any) []map[string]any {
	var out []map[string]any
	var add func(n any, depth int)
	add = func(n any, depth int) {
		m, ok := n.(map[string]any)
		if !ok || depth > 16 {
			return
		}
		out = append(out, m)
		if ref, ok := m["$ref"].(string); ok {
			if name, ok := strings.CutPrefix(ref, "#/$defs/"); ok {
				if defs, ok := reportDoc()["$defs"].(map[string]any); ok {
					add(defs[name], depth+1)
				}
			}
		}
		if all, ok := m["allOf"].([]any); ok {
			for _, a := range all {
				add(a, depth+1)
			}
		}
	}
	add(n, 0)
	return out
}

// value walks one JSON value at path, which the schema objects nodes apply to.
func (w *memberWalker) value(path string, nodes []map[string]any) error {
	tok, err := w.dec.Token()
	if err != nil {
		return err
	}
	switch t := tok.(type) {
	case json.Delim:
		switch t {
		case '{':
			if err := w.object(path, nodes); err != nil {
				return err
			}
		case '[':
			var items []map[string]any
			for _, n := range nodes {
				items = append(items, w.expand(n["items"])...)
			}
			for i := 0; w.dec.More(); i++ {
				if err := w.value(path+"/"+strconv.Itoa(i), items); err != nil {
					return err
				}
			}
		default:
			return errNotJSON
		}
		_, err := w.dec.Token() // the closing delimiter
		return err
	case string:
		if len(t) > MaxClusterTextBytes {
			for _, n := range nodes {
				if _, ok := n["pattern"].(string); ok {
					return rule(path, "a string longer than %d bytes where api/report.schema.json gives a pattern, which cutting it would break (rule: a value with a pattern is at most %d bytes)",
						MaxClusterTextBytes, MaxClusterTextBytes)
				}
			}
		}
	}
	return nil
}

// object walks the members of an object whose opening brace was read.
func (w *memberWalker) object(path string, nodes []map[string]any) error {
	props := map[string][]map[string]any{}
	var extra []map[string]any
	for _, n := range nodes {
		if ps, ok := n["properties"].(map[string]any); ok {
			for name, sub := range ps {
				props[name] = append(props[name], w.expand(sub)...)
			}
		}
		extra = append(extra, w.expand(n["additionalProperties"])...)
	}
	seen := map[string]bool{}
	for w.dec.More() {
		k, err := w.dec.Token()
		if err != nil {
			return err
		}
		name, ok := k.(string)
		if !ok {
			return errNotJSON
		}
		if seen[name] {
			return rule(path, "a member name that is repeated (rule: the names of an object are distinct)")
		}
		seen[name] = true
		sub, declared := props[name]
		seg := "*"
		if declared {
			seg = escapePointer(name)
		} else {
			for p := range props {
				if strings.EqualFold(p, name) {
					return rule(path, "a member name that differs only in case from the property %q of api/report.schema.json (rule: member names match the schema's exactly)", p)
				}
			}
			sub = extra
		}
		if err := w.value(path+"/"+seg, sub); err != nil {
			return err
		}
	}
	return nil
}
