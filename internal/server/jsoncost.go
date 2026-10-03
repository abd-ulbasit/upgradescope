package server

// jsonCost is an upper bound, read from the raw bytes, on what decoding a
// snapshot push builds. Like YAML on /gate, decoding costs memory per
// value, not per byte: `"objects":[{},{},…]` decodes every 3-byte `{}`
// into a ~140-byte ObjectRef, so a 20 MiB push of them took ~2.6 GB of
// heap, where 20 MiB of one string took ~70 MB.
type jsonCost struct {
	objects int // '{': each may become a struct or a map
	values  int // every other value, object keys included
}

// units weighs a cost against the snapshot node budget: an object counts
// eight units (a decoded struct costs up to ~8x a decoded string), every
// other value one (TestIngestDecodeHeapIsBounded).
func (c jsonCost) units() int { return 8*c.objects + c.values }

// jsonMeter counts the values of a JSON text fed in pieces cut anywhere.
// For valid JSON the count is exact; what is not valid JSON never decodes
// (encoding/json checks the whole text before it decodes any of it).
type jsonMeter struct {
	cost    jsonCost
	inStr   bool // inside a string
	escaped bool // the last byte was a '\' inside a string
	inWord  bool // inside a number or a literal
}

func (m *jsonMeter) feed(b []byte) {
	for _, c := range b {
		switch {
		case m.inStr:
			switch {
			case m.escaped:
				m.escaped = false
			case c == '\\':
				m.escaped = true
			case c == '"':
				m.inStr = false
			}
		case c == '"':
			m.inStr, m.inWord = true, false
			m.cost.values++
		case c == '{':
			m.inWord = false
			m.cost.objects++
		case c == '[':
			m.inWord = false
			m.cost.values++
		case c >= '0' && c <= '9' || c == '-' || c == '+' || c == '.' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z':
			if !m.inWord {
				m.inWord = true
				m.cost.values++
			}
		default: // , : ] } and white space
			m.inWord = false
		}
	}
}
