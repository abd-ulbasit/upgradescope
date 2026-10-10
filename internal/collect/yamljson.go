package collect

import (
	"bytes"
	"encoding/json"
	"strconv"

	yaml "go.yaml.in/yaml/v3"
	sigsyaml "sigs.k8s.io/yaml"
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
//     numbers many ways), so that is done as v2 does it (v2Scalar), or put
//     to kubectl's decoder itself (ask).
//   - Anything else that could change the reading is left to kubectl's
//     decoder: anchors, aliases and tags, merge keys, keys that are not
//     strings, duplicate keys, and text the line-by-line reader of
//     kubectl's decoder changes (readsTheSame).
//
// TestTreeJSON_MatchesKubectl and the tests after it hold the bytes to
// sigs.k8s.io/yaml's on documents of every kind this answers or declines,
// and FuzzTreeJSONMatchesKubectl on arbitrary text.
type treeJSONConverter struct {
	// oracle remembers what kubectl's own decoder made of a plain
	// scalar that v2 may type as a number (nil when it could not be
	// typed), by the scalar's text.
	oracle map[string]*oracleAnswer
	// asked counts the questions put to kubectl's decoder for the
	// document in hand.
	asked int
}

// oracleAnswer is the JSON kubectl's decoder makes of one plain scalar.
type oracleAnswer struct {
	json  []byte
	isStr bool   // json is a string literal...
	str   string // ...with this value
}

// maxOracleQuestions bounds how many distinct plain scalars of one document
// are put to kubectl's decoder one by one (each costs about what decoding a
// tiny document does); a document with more is decoded whole by it.
const maxOracleQuestions = 256

// maxOracleEntries bounds the converter's memory of answers.
const maxOracleEntries = 4096

// json returns the JSON of the document text, whose first node is root.
func (c *treeJSONConverter) json(root *yaml.Node, text []byte) ([]byte, bool) {
	if !readsTheSame(text) {
		return nil, false
	}
	c.asked = 0
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
// sees what v2 sees when kubectl's decoder reads it. Three things differ in
// the text itself, not in the parsers:
//
//   - kubectl's decoder reads text a line at a time and ends every line
//     with "\n" (a last line without one gets it; "\r\n" becomes "\n"),
//     which a block scalar and a UTF-16 byte-order mark can tell. Text that
//     does not end in a newline, holds a "\r" or starts like UTF-16 is
//     left to it.
//   - A line that starts with "---" is a document separator to it, which
//     text from the walk's own splitting never holds.
//   - An explicit tag shows in the tree, but the bare "!" (a non-specific
//     tag, which makes a plain scalar a string for v2) does not: v3 reads
//     "! 12" as the integer. So text with a "!" that starts a word, where
//     YAML puts a tag, is left to it too.
func readsTheSame(text []byte) bool {
	if len(text) == 0 || text[len(text)-1] != '\n' || bytes.IndexByte(text, '\r') >= 0 ||
		bytes.HasPrefix(text, []byte("---")) || bytes.Contains(text, []byte("\n---")) {
		return false
	}
	if len(text) >= 2 && (text[0] == 0xFF && text[1] == 0xFE || text[0] == 0xFE && text[1] == 0xFF) {
		return false
	}
	for i := 0; i < len(text); i++ {
		if text[i] != '!' {
			continue
		}
		if i == 0 {
			return false
		}
		switch text[i-1] {
		case ' ', '\t', '\n', '[', '{', ',':
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
// 2/8/16, underscores, floats, timestamps that stay strings): a decimal
// integer is read here, anything else is put to kubectl's own decoder.
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
		if v, ok := decimalInt(s); ok {
			return v, true
		}
		return c.ask(s)
	}
	return s, true
}

// decimalInt reads 0 or an optionally negative decimal integer without a
// leading zero, of at most 18 digits (so it fits an int64 and a JSON number
// exactly): what v2 reads as an int, and every parser the same.
func decimalInt(s string) (int64, bool) {
	d := s
	if d[0] == '-' {
		d = d[1:]
	}
	if d == "" || len(d) > 18 || (d[0] == '0' && len(s) > 1) {
		return 0, false
	}
	for i := 0; i < len(d); i++ {
		if d[i] < '0' || d[i] > '9' {
			return 0, false
		}
	}
	v, err := strconv.ParseInt(s, 10, 64)
	return v, err == nil
}

// ask puts a plain scalar that starts like a number to kubectl's own
// decoder, as a document of its own, and returns what it made of it: a
// number as raw JSON, a string as a string. A scalar of characters that
// could not stand alone as a plain scalar, an answer that is neither (null,
// a boolean, an error), or a string that is not the text itself, is not
// answered.
func (c *treeJSONConverter) ask(s string) (any, bool) {
	a, seen := c.oracle[s]
	if !seen {
		if c.asked++; c.asked > maxOracleQuestions || !askable(s) {
			return nil, false
		}
		a = askKubectl(s)
		if c.oracle == nil {
			c.oracle = map[string]*oracleAnswer{}
		}
		if len(c.oracle) < maxOracleEntries {
			c.oracle[s] = a
		}
	}
	switch {
	case a == nil:
		return nil, false
	case a.isStr:
		if a.str != s {
			return nil, false
		}
		return a.str, true
	}
	return json.RawMessage(a.json), true
}

// askable reports whether s, a plain scalar's text, reads the same as a
// document by itself: letters, digits and the punctuation of versions,
// units, ratios, ports and CIDRs, with no break, quote, comment or
// indicator.
func askable(s string) bool {
	for i := 0; i < len(s); i++ {
		switch c := s[i]; {
		case c >= '0' && c <= '9', c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z':
		case c == '.', c == '+', c == '-', c == '_', c == ':', c == '/', c == '%', c == ',', c == ' ':
		default:
			return false
		}
	}
	return s[len(s)-1] != ' '
}

// askKubectl returns the JSON sigs.k8s.io/yaml makes of s alone, which is
// what the whole-document conversion makes of that scalar; nil when it
// fails or makes anything but a number or a string.
func askKubectl(s string) *oracleAnswer {
	raw, err := sigsyaml.YAMLToJSON([]byte(s))
	if err != nil || len(raw) == 0 {
		return nil
	}
	switch raw[0] {
	case '"':
		var str string
		if json.Unmarshal(raw, &str) != nil {
			return nil
		}
		return &oracleAnswer{json: raw, isStr: true, str: str}
	case '-', '0', '1', '2', '3', '4', '5', '6', '7', '8', '9':
		return &oracleAnswer{json: raw}
	}
	return nil
}
