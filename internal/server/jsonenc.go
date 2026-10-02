package server

import (
	"bytes"
	"encoding/json"
	"io"
)

// The server's JSON (API responses and the reports it stores) is never
// longer than the UTF-8 of the strings in it, escaped only where JSON
// requires. encoding/json also escapes <, > and & (for HTML <script>
// blocks; every response here is application/json with nosniff) and
// U+2028/U+2029 (for JavaScript before ES2019; JSON.parse accepts them),
// as six bytes each: names of `<` in a snapshot made its stored reports
// six times their size. marshalJSON and writeJSON write all of these as
// themselves.

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

// lineSeparatorWriter writes JSON from json.Encoder with each   and
//   escape replaced by the character itself. json.Encoder writes a
// whole value per Write, so no escape spans two. Outside strings JSON has
// no backslash, and inside one a backslash always starts a two-character
// escape (\\ included) or a \uXXXX one, so stepping over escapes finds
// exactly the encoder's.
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
			sep := " "
			if p[i+5] == '9' {
				sep = " "
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
