package server

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
)

// The server's JSON (API responses and the reports it stores) is never
// longer than the UTF-8 of the strings in it, escaped only where JSON
// requires. encoding/json also escapes <, > and & (for HTML <script>
// blocks; every response here is application/json with nosniff) and
// U+2028 and U+2029 (for JavaScript before ES2019; JSON.parse accepts
// them), as six-byte backslash-u escapes: names of `<` in a snapshot made
// its stored reports six times their size. marshalJSON and writeJSON
// write all of these as themselves.

// Line and paragraph separators, which encoding/json always escapes.
var (
	lineSeparator      = string(rune(0x2028))
	paragraphSeparator = string(rune(0x2029))
)

// marshalJSON is json.Marshal without HTML or line-separator escapes.
func marshalJSON(v any) ([]byte, error) {
	var b bytes.Buffer
	if err := encodeJSON(&b, v); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(b.Bytes(), []byte("\n")), nil
}

// encodeJSON writes v to w as json.Encoder does, newline included,
// without HTML or line-separator escapes.
func encodeJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(lineSeparatorWriter{w})
	enc.SetEscapeHTML(false)
	return enc.Encode(v)
}

// lineSeparatorWriter writes JSON from json.Encoder with each escape of
// U+2028 or U+2029 replaced by the character itself. json.Encoder writes
// a whole value per Write, so no escape spans two. Outside strings JSON
// has no backslash, and inside one a backslash always starts a
// two-character escape (an escaped backslash included) or a six-character
// unicode one, so stepping over escapes finds exactly the encoder's.
type lineSeparatorWriter struct{ w io.Writer }

func (l lineSeparatorWriter) Write(p []byte) (int, error) {
	start := 0
	for i := 0; i < len(p); i++ {
		if p[i] != '\\' {
			continue
		}
		if i+5 < len(p) && p[i+1] == 'u' && p[i+2] == '2' && p[i+3] == '0' && p[i+4] == '2' && (p[i+5] == '8' || p[i+5] == '9') {
			if _, err := l.w.Write(p[start:i]); err != nil {
				return start, err
			}
			sep := lineSeparator
			if p[i+5] == '9' {
				sep = paragraphSeparator
			}
			if _, err := io.WriteString(l.w, sep); err != nil {
				return i, err
			}
			i += 5
			start = i + 1
			continue
		}
		i++ // the escaped character: a second backslash is not an escape
	}
	if _, err := l.w.Write(p[start:]); err != nil {
		return start, err
	}
	return len(p), nil
}

// canonicalHash is the hex SHA-256 of json.Marshal(v), the snapshot dedup
// hash, computed without json.Marshal's output, which escapes <, > and &
// as six bytes each and is then copied: for a push of object names of <
// at the node budget, that was ~150 MiB more heap than the same push of
// x. v is encoded without those escapes, and htmlEscaper restores them on
// the way into the hash, so the hash is json.Marshal's, as stored.
func canonicalHash(v any) (string, error) {
	h := sha256.New()
	enc := json.NewEncoder(htmlEscaper{h})
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// unicodeEscape is c as JSON's six-byte unicode escape, as encoding/json
// writes it (lower-case hex).
func unicodeEscape(c byte) string { return fmt.Sprintf(`\%c%04x`, 'u', c) }

// htmlEscapes are json.Marshal's escapes of the HTML characters.
var htmlEscapes = map[byte]string{'<': unicodeEscape('<'), '>': unicodeEscape('>'), '&': unicodeEscape('&')}

// htmlEscaper writes compact JSON from json.Encoder as json.Marshal
// writes it: each <, > and & (which JSON has only inside strings) as its
// unicode escape, and without the newline that ends the encoder's value
// (the only raw newline in compact JSON).
type htmlEscaper struct{ w io.Writer }

func (e htmlEscaper) Write(p []byte) (int, error) {
	start := 0
	for i, c := range p {
		esc, special := htmlEscapes[c]
		if !special && c != '\n' {
			continue
		}
		if _, err := e.w.Write(p[start:i]); err != nil {
			return start, err
		}
		if _, err := io.WriteString(e.w, esc); err != nil {
			return i, err
		}
		start = i + 1
	}
	if _, err := e.w.Write(p[start:]); err != nil {
		return start, err
	}
	return len(p), nil
}
