package server

import "sort"

// yamlCost is an upper bound, read from the raw bytes, on what decoding a
// YAML (or JSON) document builds. Decoding is what costs memory: each
// document becomes a yaml.v3 node tree and kubectl's generic tree, and
// list items become objects, so the heap a document takes follows how
// many nodes it holds, not its size: a 4 MiB flow sequence `[1,1,…]` is
// two million nodes, a 4 MiB string is one.
type yamlCost struct {
	nodes   int // scalars, collections and aliases (aliases are not expanded)
	entries int // sequence entries: each may be a list item, i.e. an object
}

// add sums two costs.
func (c yamlCost) add(o yamlCost) yamlCost {
	return yamlCost{nodes: c.nodes + o.nodes, entries: c.entries + o.entries}
}

// units weighs a cost against the node budget. A sequence entry counts
// three units on top of its node: one that is a list item also becomes an
// object, and a List of empty items costs about four times as much per
// node as a mapping of plain scalars (TestGateDecodeHeapIsBounded).
func (c yamlCost) units() int { return c.nodes + 3*c.entries }

// byteSource is a byte sequence read by offset: one slice, or a buffered
// request body's chunks, without copying them together.
type byteSource struct {
	chunks [][]byte
	starts []int // offset of each chunk
	size   int
	cur    int // chunk of the last read
}

func newByteSource(chunks [][]byte) *byteSource {
	s := &byteSource{chunks: chunks, starts: make([]int, len(chunks))}
	for i, c := range chunks {
		s.starts[i] = s.size
		s.size += len(c)
	}
	return s
}

// at returns the byte at offset i, or 0 past the end (a NUL ends a YAML
// stream too).
func (s *byteSource) at(i int) byte {
	if i < 0 || i >= s.size {
		return 0
	}
	if c := s.cur; i < s.starts[c] || i >= s.starts[c]+len(s.chunks[c]) {
		s.cur = sort.Search(len(s.starts), func(k int) bool { return s.starts[k] > i }) - 1
	}
	return s.chunks[s.cur][i-s.starts[s.cur]]
}

// measureYAML measures one whole document.
func measureYAML(doc []byte) yamlCost {
	return measureYAMLRange(newByteSource([][]byte{doc}), 0, len(doc))
}

// measureYAMLRange measures the document at src[start:end] by scanning it
// the way yaml.v3 (and go-yaml v2, its sibling port of libyaml, which
// kubectl's decoder uses) scans it — the same indentation stack, simple
// keys, and plain, quoted and block scalar rules — and counting the nodes
// its parser builds from those tokens: every scalar, alias and collection,
// and every empty (null) node an indicator implies (`a:`, `- `, `{a, b}`).
// Aliases count once: yaml.v3 does not expand them, and go-yaml v2 refuses
// excessive aliasing. Where the scanner would stop with an error, this
// goes on and counts the rest too, so the count is an upper bound for
// valid and invalid documents alike; the remaining over-counting is small
// (a property such as an anchor or tag counts inside a flow collection,
// an explicit '?' key always counts an empty value).
func measureYAMLRange(src *byteSource, start, end int) yamlCost {
	s := &yamlScanner{src: src, pos: start, end: end, indent: -1, keyAllowed: true, expect: true}
	s.keys = []simpleKey{{}}
	s.run()
	return s.cost
}

// simpleKey is a token that may turn out to be an implicit mapping key,
// once a ':' follows it on the same line.
type simpleKey struct {
	possible   bool
	line, idx  int // line and character index where it starts
	col        int
	slotBefore bool // the block node slot open before it was still empty
	nodes      int  // nodes counted before it: none since means it is only properties
}

// flowItem is the counting state of one open flow collection's current
// entry.
type flowItem struct {
	seq                    bool
	started                bool // the entry has content (seq: counted in entries)
	explicitKey            bool // it began with '?'
	keyFilled, sawValue    bool
	valueFilled, pairCount bool
}

// yamlScanner mirrors yaml.v3's scanner (scannerc.go) closely enough to see
// the same tokens, and counts nodes. In the block context the parser fills
// one node slot at a time — the root, a mapping value, a sequence entry —
// so expect records whether the open slot is still empty; an indicator
// or block end that closes an empty slot counts the empty node.
type yamlScanner struct {
	src       *byteSource
	pos, end  int
	line, col int // col and idx count characters, as yaml.v3's marks do
	idx       int

	flowLevel  int
	indent     int
	indents    []int
	keyAllowed bool
	keys       []simpleKey // one per flow level; [0] is the block context's
	flows      []flowItem

	expect bool // the open block node slot is empty
	cost   yamlCost
}

func (s *yamlScanner) at(k int) byte {
	if i := s.pos + k; i < s.end {
		return s.src.at(i)
	}
	return 0
}

func isYAMLBlank(b byte) bool { return b == ' ' || b == '\t' }

// breakLen is the length of the line break at offset k, 0 if none: CR,
// LF, CRLF, NEL, LS or PS, as yaml.v3's is_break.
func (s *yamlScanner) breakLen(k int) int {
	switch b := s.at(k); {
	case b == '\r' && s.at(k+1) == '\n':
		return 2
	case b == '\r' || b == '\n':
		return 1
	case b == 0xC2 && s.at(k+1) == 0x85:
		return 2
	case b == 0xE2 && s.at(k+1) == 0x80 && (s.at(k+2) == 0xA8 || s.at(k+2) == 0xA9):
		return 3
	}
	return 0
}

func (s *yamlScanner) isZ(k int) bool { return s.pos+k >= s.end || s.at(k) == 0 }
func (s *yamlScanner) isBlankZ(k int) bool {
	return isYAMLBlank(s.at(k)) || s.breakLen(k) > 0 || s.isZ(k)
}

// skip moves past one character.
func (s *yamlScanner) skip() {
	s.pos++
	s.col++
	s.idx++
	for s.pos < s.end && s.src.at(s.pos)&0xC0 == 0x80 { // UTF-8 continuation bytes
		s.pos++
	}
}

// skipLine moves past the line break at pos, if there is one.
func (s *yamlScanner) skipLine() {
	if n := s.breakLen(0); n > 0 {
		if s.at(0) == '\r' && n == 2 {
			s.idx++ // CRLF is two characters; NEL, LS and PS are one each
		}
		s.pos += n
		s.idx++
		s.col = 0
		s.line++
	}
}

func (s *yamlScanner) run() {
	for {
		s.toNextToken()
		s.unroll(s.col)
		if s.isZ(0) {
			s.unroll(-1)
			s.closeSlot()
			return
		}
		s.token()
	}
}

// toNextToken skips white space, comments and line breaks.
func (s *yamlScanner) toNextToken() {
	for {
		if s.col == 0 && s.at(0) == 0xEF && s.at(1) == 0xBB && s.at(2) == 0xBF {
			s.skip()
		}
		for s.at(0) == ' ' || (s.flowLevel > 0 || !s.keyAllowed) && s.at(0) == '\t' {
			s.skip()
		}
		if s.at(0) == '#' {
			for s.breakLen(0) == 0 && !s.isZ(0) {
				s.skip()
			}
		}
		if s.breakLen(0) == 0 {
			return
		}
		s.skipLine()
		if s.flowLevel == 0 {
			s.keyAllowed = true
		}
	}
}

// token scans one token at pos (yaml_parser_fetch_next_token).
func (s *yamlScanner) token() {
	c, next := s.at(0), s.at(1)
	switch {
	case s.col == 0 && c == '%':
		s.unroll(-1)
		s.removeKey()
		s.keyAllowed = false
		for s.breakLen(0) == 0 && !s.isZ(0) {
			s.skip()
		}
	case s.col == 0 && (c == '-' && next == '-' && s.at(2) == '-' || c == '.' && next == '.' && s.at(2) == '.') && s.isBlankZ(3):
		// A document marker: what follows is another document, which
		// yaml.v3 decodes too.
		s.unroll(-1)
		s.closeSlot()
		s.expect = true
		s.removeKey()
		s.keyAllowed = false
		s.skip()
		s.skip()
		s.skip()
	case c == '[' || c == '{':
		s.saveKey()
		s.node()
		s.flowLevel++
		s.keys = append(s.keys, simpleKey{})
		s.flows = append(s.flows, flowItem{seq: c == '['})
		s.keyAllowed = true
		s.skip()
	case c == ']' || c == '}':
		s.removeKey()
		if s.flowLevel > 0 {
			s.endFlowItem()
			s.flowLevel--
			s.keys = s.keys[:len(s.keys)-1]
			s.flows = s.flows[:len(s.flows)-1]
		}
		s.keyAllowed = false
		s.skip()
	case c == ',':
		s.removeKey()
		s.keyAllowed = true
		if s.flowLevel > 0 {
			s.endFlowItem()
		}
		s.skip()
	case c == '-' && s.isBlankZ(1):
		if s.flowLevel == 0 {
			s.roll(s.col, true)
			if s.expect {
				s.cost.nodes++ // the previous entry was empty, or this starts an indentless sequence
			}
			s.cost.entries++
			s.expect = true
		}
		s.removeKey()
		s.keyAllowed = true
		s.skip()
	case c == '?' && (s.flowLevel > 0 || s.isBlankZ(1)):
		if s.flowLevel == 0 {
			s.roll(s.col, true)
			s.closeSlot()
			s.cost.nodes++ // the value, which may never come
			s.expect = true
		} else {
			f := s.flowStart()
			f.explicitKey = true
			if f.seq {
				s.cost.nodes++ // `[? a]`: a one-pair mapping
			}
		}
		s.removeKey()
		s.keyAllowed = s.flowLevel == 0
		s.skip()
	case c == ':' && (s.flowLevel > 0 || s.isBlankZ(1)):
		s.value()
		s.skip()
	case c == '*' || c == '&':
		s.saveKey()
		s.keyAllowed = false
		s.skip()
		for b := s.at(0); b >= '0' && b <= '9' || b >= 'A' && b <= 'Z' || b >= 'a' && b <= 'z' || b == '_' || b == '-'; b = s.at(0) {
			s.skip()
		}
		if c == '*' {
			s.node()
		} else {
			s.property()
		}
	case c == '!':
		s.saveKey()
		s.keyAllowed = false
		for !s.isBlankZ(0) {
			s.skip()
		}
		s.property()
	case (c == '|' || c == '>') && s.flowLevel == 0:
		s.removeKey()
		s.keyAllowed = true
		s.blockScalar()
		s.node()
	case c == '\'' || c == '"':
		s.saveKey()
		s.keyAllowed = false
		s.quoted(c)
		s.node()
	case !(s.isBlankZ(0) || isIndicatorByte(c)) ||
		c == '-' && !isYAMLBlank(next) ||
		s.flowLevel == 0 && (c == '?' || c == ':') && !s.isBlankZ(1):
		s.saveKey()
		s.keyAllowed = false
		s.plain()
		s.node()
	default:
		s.skip() // a character that cannot start a token: yaml.v3 stops here
	}
}

func isIndicatorByte(c byte) bool {
	switch c {
	case '-', '?', ':', ',', '[', ']', '{', '}', '#', '&', '*', '!', '|', '>', '\'', '"', '%', '@', '`':
		return true
	}
	return false
}

// roll pushes a block collection whose entries start at column col, as
// yaml_parser_roll_indent does, and counts its node. It reports whether it
// did.
func (s *yamlScanner) roll(col int, fill bool) bool {
	if s.flowLevel > 0 || s.indent >= col {
		return false
	}
	s.indents = append(s.indents, s.indent)
	s.indent = col
	s.cost.nodes++ // BLOCK-MAPPING-START or BLOCK-SEQUENCE-START: the collection
	if fill {
		s.expect = false
	}
	return true
}

// unroll pops block collections indented deeper than col; each one's end
// closes its open slot.
func (s *yamlScanner) unroll(col int) {
	if s.flowLevel > 0 {
		return
	}
	for s.indent > col {
		s.closeSlot()
		s.indent = s.indents[len(s.indents)-1]
		s.indents = s.indents[:len(s.indents)-1]
	}
}

// closeSlot counts the empty node of a block slot nothing filled.
func (s *yamlScanner) closeSlot() {
	if s.expect {
		s.cost.nodes++
		s.expect = false
	}
}

func (s *yamlScanner) saveKey() {
	if s.keyAllowed {
		s.keys[len(s.keys)-1] = simpleKey{possible: true, line: s.line, idx: s.idx, col: s.col, slotBefore: s.expect, nodes: s.cost.nodes}
	}
}

func (s *yamlScanner) removeKey() { s.keys[len(s.keys)-1].possible = false }

// value handles a ':' indicator (yaml_parser_fetch_value).
func (s *yamlScanner) value() {
	k := &s.keys[len(s.keys)-1]
	if k.possible && k.line == s.line && k.idx+1024 >= s.idx { // a simple key
		k.possible = false
		s.keyAllowed = false
		if s.flowLevel == 0 {
			if s.cost.nodes == k.nodes {
				s.cost.nodes++ // `&a: b`: a key of properties only is an empty node
			}
			// The key filled the slot that was open before it, unless the
			// slot was empty and this is the same mapping's next key.
			if !s.roll(k.col, false) && k.slotBefore {
				s.cost.nodes++
			}
			s.expect = true
		} else {
			s.flowValue()
		}
		return
	}
	if s.flowLevel == 0 { // a complex value, after `? key` or alone
		s.roll(s.col, true)
		s.closeSlot()
		s.expect = true
	} else {
		s.flowValue()
	}
	s.keyAllowed = s.flowLevel == 0
}

// node counts a scalar, alias or flow collection: it fills the open block
// slot, or the current flow entry's key or value.
func (s *yamlScanner) node() {
	s.cost.nodes++
	if s.flowLevel == 0 {
		s.expect = false
		return
	}
	f := s.flowStart()
	if f.sawValue {
		f.valueFilled = true
	} else {
		f.keyFilled = true
	}
}

// property counts an anchor or tag. In the block context it belongs to
// the next node or, with none, to the empty node its slot gets anyway; in
// a flow collection it is counted as a node, since it may be all an entry
// holds.
func (s *yamlScanner) property() {
	if s.flowLevel > 0 {
		s.node()
	}
}

// flowStart returns the current flow entry, counting it in entries when it
// is a new sequence entry.
func (s *yamlScanner) flowStart() *flowItem {
	f := &s.flows[len(s.flows)-1]
	if !f.started {
		f.started = true
		if f.seq {
			s.cost.entries++
		}
	}
	return f
}

// flowValue handles a ':' inside a flow collection.
func (s *yamlScanner) flowValue() {
	f := s.flowStart()
	if f.seq && !f.explicitKey {
		s.cost.nodes++ // `[a: b]`: a one-pair mapping
	}
	if !f.keyFilled {
		s.cost.nodes++ // an empty key
		f.keyFilled = true
	}
	f.sawValue, f.valueFilled = true, false
}

// endFlowItem counts the empty nodes of the flow entry a ',' or the
// collection's end closes.
func (s *yamlScanner) endFlowItem() {
	f := &s.flows[len(s.flows)-1]
	if f.started && (!f.seq || f.explicitKey || f.sawValue) {
		if !f.keyFilled {
			s.cost.nodes++
		}
		if !f.valueFilled {
			s.cost.nodes++ // `{a, b}`, `{a: }`, `[? a]`
		}
	}
	*f = flowItem{seq: f.seq}
}

// plain scans a plain scalar (yaml_parser_scan_plain_scalar), which in the
// block context goes on over lines indented deeper than the enclosing
// collection.
func (s *yamlScanner) plain() {
	indent := s.indent + 1
	leadingBlanks := false
	for {
		if s.col == 0 && (s.at(0) == '-' && s.at(1) == '-' && s.at(2) == '-' || s.at(0) == '.' && s.at(1) == '.' && s.at(2) == '.') && s.isBlankZ(3) {
			break
		}
		if s.at(0) == '#' {
			break
		}
		for !s.isBlankZ(0) {
			c := s.at(0)
			if c == ':' && s.isBlankZ(1) || s.flowLevel > 0 && (c == ',' || c == '?' || c == '[' || c == ']' || c == '{' || c == '}') {
				break
			}
			s.skip()
		}
		if !isYAMLBlank(s.at(0)) && s.breakLen(0) == 0 {
			break
		}
		for isYAMLBlank(s.at(0)) || s.breakLen(0) > 0 {
			if isYAMLBlank(s.at(0)) {
				s.skip()
			} else {
				s.skipLine()
				leadingBlanks = true
			}
		}
		if s.flowLevel == 0 && s.col < indent {
			break
		}
	}
	if leadingBlanks {
		s.keyAllowed = true
	}
}

// quoted scans a single- or double-quoted scalar, which ends only at its
// closing quote, whatever the lines' indentation.
func (s *yamlScanner) quoted(q byte) {
	s.skip()
	for !s.isZ(0) {
		switch c := s.at(0); {
		case q == '\'' && c == '\'' && s.at(1) == '\'':
			s.skip()
			s.skip()
		case c == q:
			s.skip()
			return
		case q == '"' && c == '\\':
			s.skip()
			if s.breakLen(0) > 0 {
				s.skipLine()
			} else if !s.isZ(0) {
				s.skip()
			}
		case s.breakLen(0) > 0:
			s.skipLine()
		default:
			s.skip()
		}
	}
}

// blockScalar scans a literal or folded scalar (yaml_parser_scan_block_scalar):
// the header line, then every line indented at least as deep as its
// content, which is the explicit indentation indicator or the first
// non-empty line's, and deeper than the enclosing collection.
func (s *yamlScanner) blockScalar() {
	s.skip()
	increment := 0
	for range 2 {
		switch c := s.at(0); {
		case c == '+' || c == '-':
			s.skip()
		case c >= '1' && c <= '9' && increment == 0:
			increment = int(c - '0')
			s.skip()
		}
	}
	for s.breakLen(0) == 0 && !s.isZ(0) { // blanks and a comment; anything else is an error
		s.skip()
	}
	s.skipLine()
	indent := 0
	if increment > 0 {
		indent = max(s.indent, 0) + increment
	}
	s.blockBreaks(&indent)
	for s.col == indent && !s.isZ(0) {
		for s.breakLen(0) == 0 && !s.isZ(0) {
			s.skip()
		}
		s.skipLine()
		s.blockBreaks(&indent)
	}
}

func (s *yamlScanner) blockBreaks(indent *int) {
	maxIndent := 0
	for {
		for (*indent == 0 || s.col < *indent) && s.at(0) == ' ' {
			s.skip()
		}
		maxIndent = max(maxIndent, s.col)
		if s.breakLen(0) == 0 {
			break
		}
		s.skipLine()
	}
	if *indent == 0 {
		*indent = max(maxIndent, s.indent+1, 1)
	}
}
