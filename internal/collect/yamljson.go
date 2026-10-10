package collect

import (
	"bytes"
	"encoding/json"
	"regexp"
	"strconv"
	"strings"

	yaml "go.yaml.in/yaml/v3"
)

// treeJSONConverter makes kubectl's YAML-to-JSON step (apimachinery's
// YAMLToJSONDecoder: go-yaml v2 reads the document, sigs.k8s.io/yaml turns
// what it read into JSON) from the yaml.v3 node tree the walk has already
// built, so a document is parsed once instead of twice (#285).
//
// What it returns is the JSON kubectl's decoder makes of the same text,
// byte for byte, or ok false, and then the caller runs kubectl's decoder on
// the text, as before. It answers only where go-yaml v2 (YAML 1.1, as
// kubectl reads) and v3 cannot read the text differently:
//
//   - The two share a scanner and parser, apart from how they keep
//     comments, which does not touch the tree. What differs is how a plain
//     scalar is typed (YAML 1.1 reads yes, no, on and off as booleans, and
//     numbers many ways), so that is done as v2 does it (v2Scalar and
//     v2Number: v2's own resolve, with the same standard-library calls),
//     never by asking kubectl's decoder: a question per scalar costs more
//     than the second parse it saves (see the scale guide).
//   - Anything else that could change the reading is left to kubectl's
//     decoder: anchors, aliases and tags, merge keys, keys that are not
//     strings, duplicate keys, and text the line-by-line reader of
//     kubectl's decoder changes (readsTheSame).
//
// TestTreeJSON_MatchesKubectl and the tests after it hold the bytes to
// sigs.k8s.io/yaml's on documents of every kind this answers or declines,
// and FuzzTreeJSONMatchesKubectl on arbitrary text.
type treeJSONConverter struct {
	// answered and declined count the documents the converter made JSON of
	// and left to kubectl's decoder: what a test reads to say how often
	// the shortcut applies.
	answered, declined int
}

// json returns the JSON of the document text, whose first node is root.
func (c *treeJSONConverter) json(root *yaml.Node, text []byte) ([]byte, bool) {
	raw, ok := c.convert(root, text)
	if ok {
		c.answered++
	} else {
		c.declined++
	}
	return raw, ok
}

func (c *treeJSONConverter) convert(root *yaml.Node, text []byte) ([]byte, bool) {
	if !readsTheSame(text) {
		return nil, false
	}
	v, ok := c.value(root)
	if !ok {
		return nil, false
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, false // kubectl's decoder reports it
	}
	return raw, true
}

// readsTheSame reports whether go-yaml v3, reading text as the walk does,
// sees what v2 sees when kubectl's decoder reads it. Four things differ in
// the text itself, not in the parsers:
//
//   - kubectl's decoder reads text a line at a time and ends every line
//     with "\n" (a last line without one gets it; "\r\n" becomes "\n"),
//     which a block scalar and a UTF-16 byte-order mark can tell. Text that
//     does not end in a newline, holds a "\r" or starts like UTF-16 is
//     left to it.
//   - A line that starts with "---" is a document separator to it, which
//     text from the walk's own splitting never holds.
//   - go-yaml reads U+0085 (NEL), U+2028 (LS) and U+2029 (PS) as line
//     breaks, as well as "\n", and the "!" scan below finds a node's start
//     by the line it is on. Text with any of the three is left to kubectl's
//     decoder rather than have the scan count them, since a value that holds
//     one is rare.
//   - An explicit tag shows in the tree, but the bare "!" (a non-specific
//     tag, which makes a plain scalar a string for v2) does not: v3 reads
//     "! 12" as the integer. So text with a "!" where YAML puts a tag, at
//     the start of a node, is left to it too.
func readsTheSame(text []byte) bool {
	if len(text) == 0 || text[len(text)-1] != '\n' || bytes.IndexByte(text, '\r') >= 0 ||
		bytes.HasPrefix(text, []byte("---")) || bytes.Contains(text, []byte("\n---")) {
		return false
	}
	if bytes.Contains(text, []byte("\u0085")) || bytes.Contains(text, []byte("\u2028")) || bytes.Contains(text, []byte("\u2029")) {
		return false
	}
	if len(text) >= 2 && (text[0] == 0xFF && text[1] == 0xFE || text[0] == 0xFE && text[1] == 0xFF) {
		return false
	}
	for i := 0; i < len(text); i++ {
		if text[i] != '!' {
			continue
		}
		// A tag starts a node: what comes before it on its line is
		// nothing, or an indicator (: - ? [ { ,). Anywhere else the "!" is
		// text, as in a CEL rule's "a && !b".
		j := i - 1
		for j >= 0 && (text[j] == ' ' || text[j] == '\t') {
			j--
		}
		if j < 0 || text[j] == '\n' || strings.IndexByte(":-?[{,", text[j]) >= 0 {
			return false
		}
	}
	return true
}

// untagged reports whether n carries no tag of its own: no anchor, no
// explicit tag (the bare "!" too, which v3 records without the tagged
// style), only the tag v3 resolves for its kind.
func untagged(n *yaml.Node) bool {
	if n.Anchor != "" || n.Style&yaml.TaggedStyle != 0 {
		return false
	}
	switch n.Kind {
	case yaml.MappingNode:
		return n.Tag == "!!map"
	case yaml.SequenceNode:
		return n.Tag == "!!seq"
	case yaml.ScalarNode:
		switch n.Tag {
		case "!!str", "!!int", "!!float", "!!bool", "!!null", "!!timestamp", "!!merge":
			return true
		}
	}
	return false
}

func (c *treeJSONConverter) value(n *yaml.Node) (any, bool) {
	if !untagged(n) {
		return nil, false
	}
	switch n.Kind {
	case yaml.MappingNode:
		if len(n.Content)%2 != 0 {
			return nil, false
		}
		m := make(map[string]any, len(n.Content)/2)
		for i := 0; i < len(n.Content); i += 2 {
			k, ok := c.key(n.Content[i])
			if !ok {
				return nil, false
			}
			if _, dup := m[k]; dup {
				return nil, false
			}
			v, ok := c.value(n.Content[i+1])
			if !ok {
				return nil, false
			}
			m[k] = v
		}
		return m, true
	case yaml.SequenceNode:
		s := make([]any, len(n.Content))
		for i, e := range n.Content {
			v, ok := c.value(e)
			if !ok {
				return nil, false
			}
			s[i] = v
		}
		return s, true
	case yaml.ScalarNode:
		return c.scalar(n)
	}
	return nil, false // an alias, or a document inside a document
}

// key is a mapping key: only a string, which both parsers read the same.
func (c *treeJSONConverter) key(n *yaml.Node) (string, bool) {
	if n.Kind != yaml.ScalarNode || !untagged(n) {
		return "", false
	}
	if n.Tag == "!!merge" {
		return "", false // a merge key
	}
	v, ok := c.scalar(n)
	if !ok {
		return "", false
	}
	s, isStr := v.(string)
	return s, isStr
}

// scalar is the value go-yaml v2 reads a scalar as, as sigs.k8s.io/yaml
// then passes it on to JSON.
func (c *treeJSONConverter) scalar(n *yaml.Node) (any, bool) {
	if n.Style&(yaml.DoubleQuotedStyle|yaml.SingleQuotedStyle|yaml.LiteralStyle|yaml.FoldedStyle) != 0 {
		return n.Value, true // quoted and block scalars are strings in YAML 1.1 too
	}
	return c.v2Scalar(n.Value)
}

// v2Scalar types a plain scalar as go-yaml v2's resolve does: the first
// byte is its hint. A byte with none (a letter other than those below, a
// quote's opposite, anything non-ASCII) makes a string. y, n, t, f, o and ~
// start the words YAML 1.1 reads as booleans and null (v2's resolveMap).
// + - . and the digits start the numbers, which v2 reads many ways (base
// 2/8/16, underscores, floats, timestamps that stay strings): v2Number. A
// scalar with a byte other than a letter, a digit or one of _ . + - cannot
// be any of them (v2 tries ParseInt, ParseUint, a float pattern, ParseFloat
// and a timestamp that stays a string, and none accepts such a byte), so it
// is a string.
func (c *treeJSONConverter) v2Scalar(s string) (any, bool) {
	if s == "" {
		return nil, true
	}
	switch s[0] {
	case 'y', 'Y', 'n', 'N', 't', 'T', 'f', 'F', 'o', 'O', '~':
		switch s {
		case "~", "null", "Null", "NULL":
			return nil, true
		case "y", "Y", "yes", "Yes", "YES", "true", "True", "TRUE", "on", "On", "ON":
			return true, true
		case "n", "N", "no", "No", "NO", "false", "False", "FALSE", "off", "Off", "OFF":
			return false, true
		}
		return s, true
	case '+', '-', '.', '0', '1', '2', '3', '4', '5', '6', '7', '8', '9':
		if !numberChars(s) {
			return s, true
		}
		return v2Number(s)
	}
	return s, true
}

// yamlStyleFloat is the pattern go-yaml v2 (resolve.go, v2.4.4) requires
// before it reads a scalar as a float.
var yamlStyleFloat = regexp.MustCompile(`^[-+]?(\.[0-9]+|[0-9]+(\.[0-9]*)?)([eE][-+]?[0-9]+)?$`)

// v2Number types a plain scalar that starts with + - . or a digit and has
// only letters, digits and _ . + - as go-yaml v2's resolve does, in the same
// order and with the same standard-library calls, so the value (and then the
// JSON) is the same: an int64 or uint64, a float64, or the text itself where
// v2 reads a string (a timestamp too). It reports false for the values JSON cannot hold, which
// kubectl's decoder fails on: .nan, .inf and their signed forms.
//
// This is a copy of what v2's resolve does, so it is held to
// sigs.k8s.io/yaml by TestV2Number_MatchesKubectl over a generated set of
// scalars, by TestTreeJSON_* and the fuzz target, and
// TestV2Number_TracksV2Version names the v2 version it was checked against.
func v2Number(s string) (any, bool) {
	switch s {
	case ".nan", ".NaN", ".NAN", ".inf", ".Inf", ".INF", "+.inf", "+.Inf", "+.INF", "-.inf", "-.Inf", "-.INF":
		return nil, false
	}
	if s[0] == '.' {
		if f, err := strconv.ParseFloat(s, 64); err == nil {
			return f, true
		}
		return s, true
	}
	// v2 tries a timestamp here, "2001-12-14" and the like, and hands it on as
	// the string it is; no number is written that way, so it falls through to
	// the string at the end all the same.
	plain := strings.ReplaceAll(s, "_", "")
	if i, err := strconv.ParseInt(plain, 0, 64); err == nil {
		return i, true
	}
	if u, err := strconv.ParseUint(plain, 0, 64); err == nil {
		return u, true
	}
	if yamlStyleFloat.MatchString(plain) {
		if f, err := strconv.ParseFloat(plain, 64); err == nil {
			return f, true
		}
	}
	if strings.HasPrefix(plain, "0b") {
		if i, err := strconv.ParseInt(plain[2:], 2, 64); err == nil {
			return i, true
		}
		if u, err := strconv.ParseUint(plain[2:], 2, 64); err == nil {
			return u, true
		}
	} else if strings.HasPrefix(plain, "-0b") {
		if i, err := strconv.ParseInt("-"+plain[3:], 2, 64); err == nil {
			return i, true
		}
	}
	return s, true
}

// numberChars reports whether s has only letters, digits and _ . + -: the
// bytes of everything go-yaml v2 reads as a number.
func numberChars(s string) bool {
	for i := 0; i < len(s); i++ {
		switch c := s[i]; {
		case c >= '0' && c <= '9', c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z':
		case c == '.', c == '+', c == '-', c == '_':
		default:
			return false
		}
	}
	return true
}
