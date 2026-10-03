package server

import (
	"reflect"
	"strings"
	"time"

	"github.com/abd-ulbasit/upgradescope/internal/engine"
	"github.com/abd-ulbasit/upgradescope/internal/inventory"
)

// A /gate answer is refused (413) when it could be larger than
// --max-gate-bytes, before it is encoded. Its size follows the strings the
// stream (and, with ?cluster=, the cluster) put in it, times how each
// format escapes and repeats them, and an encoder holds several copies of
// what it writes while it writes it. Unbounded, 13,600 objects (the KB's
// 136 deprecated or removed GVKs at 100 objects each) with 650-byte names
// of `<` and a 512-byte path answered 72 MB of SARIF from a 9 MB stream,
// and took 347 MiB of heap to encode it.

// gateAnswerLimit is the most a /gate answer may take: --max-gate-bytes,
// the cap on the stream it answers.
func (s *Server) gateAnswerLimit() int64 {
	if s.maxGateAnswer > 0 {
		return s.maxGateAnswer
	}
	return s.maxGateBytes()
}

// gateAnswerBound is an upper bound on the bytes of resp's answer in
// format: JSON as writeJSON writes it, or SARIF, GitLab Code Quality
// (indented JSON, HTML-escaped) or JUnit XML. Every string counts at its
// dearest escape in any of them (stringBound), every value with room for
// its key, punctuation and indentation (valueBound). The other formats
// also repeat things: SARIF and Code Quality write one result per object
// with a file, with its finding's key, title and fix and the file as a
// URI, and every format restates a finding's text and up to five of its
// objects in messages and notifications (findingRepeats).
// TestGateAnswerBoundHolds checks it against every format.
func gateAnswerBound(resp gateResponse, format string) int64 {
	whole := resp
	whole.Report.Findings = nil // shadowed by resp.Findings, written once
	n := valueBound(reflect.ValueOf(whole))
	if format == "" || format == "json" {
		return n
	}
	for _, f := range resp.Findings {
		n += findingRepeats(f.Finding)
	}
	for _, f := range resp.Suppressed {
		n += findingRepeats(f.Finding) + 2*stringBound(f.Reason)
	}
	for _, g := range resp.NotAssessed { // notifications and run properties
		n += 512 + stringBound(g.Reason) + 2*stringBound(strings.Join(g.Skipped, ", "))
	}
	for _, img := range resp.UnrecognizedImages {
		n += stringBound(img)
	}
	return n
}

// findingRepeats bounds what SARIF, Code Quality and JUnit write about f
// beyond f itself: the finding's text in its rule, test case and
// messages, up to five of its objects in a notification, and a result
// per object with a file.
func findingRepeats(f engine.Finding) int64 {
	text := stringBound(f.Key) + stringBound(f.Title) + stringBound(f.Detail) + stringBound(f.Remediation)
	for _, c := range f.Citations {
		text += stringBound(c)
	}
	n := 1024 + 3*text
	for i, o := range f.Objects {
		if i < 5 {
			n += refBound(o)
		}
		if o.File != "" {
			n += 512 + stringBound(f.Key) + stringBound(f.Title) + stringBound(f.Remediation) + refBound(o) + uriBound(o.File)
		}
	}
	return n
}

// refBound bounds the strings of one object ref.
func refBound(o inventory.ObjectRef) int64 {
	return stringBound(o.Namespace) + stringBound(o.Name) + stringBound(o.File) + stringBound(o.RenderedFrom) +
		stringBound(o.Manager) + stringBound(o.Ignore) + stringBound(o.IgnoreReason)
}

// stringBound is the most bytes s takes, quotes included, in any of the
// answer's formats: six for a byte that JSON or XML escape (a control
// character, a quote, &, <, > or a backslash, which become a six-byte
// JSON unicode escape or an XML character reference), two for a byte of
// a non-ASCII character (U+2028 is a six-byte escape of three bytes, and
// XML writes an invalid character as the three bytes of U+FFFD), one for
// any other. Strings here are valid UTF-8: YAML and JSON decode to it,
// and ?path= must be.
func stringBound(s string) int64 {
	n := int64(2)
	for i := 0; i < len(s); i++ {
		switch c := s[i]; {
		case c < 0x20 || c == 0x7f || c == '"' || c == '&' || c == '\'' || c == '<' || c == '>' || c == '\\':
			n += 6
		case c >= 0x80:
			n += 2
		default:
			n++
		}
	}
	return n
}

// uriBound is the most bytes the path p takes as a SARIF artifact URI,
// quotes included: three for a byte that is not a letter, a digit or a
// slash (percent-encoded), six for one JSON then escapes (%25 holds
// none, so this is only the quote and the backslash, which a path here
// cannot hold), one for any other.
func uriBound(p string) int64 {
	n := int64(2)
	for i := 0; i < len(p); i++ {
		switch c := p[i]; {
		case c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '/':
			n++
		default:
			n += 3
		}
	}
	return n
}

var timeType = reflect.TypeFor[time.Time]()

// valueBound bounds v as JSON with two-space indentation at any depth:
// each value, element and field gets 16 bytes for its punctuation and
// indentation, a field its key twice over (the Go name and the tag), a
// number 24 and a time 40.
func valueBound(v reflect.Value) int64 {
	const slack = 16
	switch v.Kind() {
	case reflect.Invalid:
		return slack
	case reflect.Pointer, reflect.Interface:
		if v.IsNil() {
			return slack
		}
		return valueBound(v.Elem())
	case reflect.String:
		return slack + stringBound(v.String())
	case reflect.Bool:
		return slack + 5
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr,
		reflect.Float32, reflect.Float64:
		return slack + 24
	case reflect.Slice, reflect.Array:
		n := int64(slack)
		for i := range v.Len() {
			n += valueBound(v.Index(i))
		}
		return n
	case reflect.Map:
		n := int64(slack)
		for it := v.MapRange(); it.Next(); {
			n += valueBound(it.Key()) + valueBound(it.Value())
		}
		return n
	case reflect.Struct:
		if v.Type() == timeType {
			return slack + 40
		}
		n := int64(slack)
		t := v.Type()
		for i := range t.NumField() {
			sf := t.Field(i)
			if !sf.IsExported() && !sf.Anonymous {
				continue
			}
			name, _, _ := strings.Cut(sf.Tag.Get("json"), ",")
			if name == "-" {
				continue
			}
			n += int64(len(sf.Name)+len(name)) + valueBound(v.Field(i))
		}
		return n
	default: // func, chan, complex: not in an answer
		return slack
	}
}
