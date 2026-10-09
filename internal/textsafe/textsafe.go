// Package textsafe makes text that a scanned manifest or cluster controls
// safe to print to a terminal or a CI log.
//
// Object names, namespaces, file names, annotation values and the reasons
// and titles built from them reach the table, Markdown and warning output
// as they are in the YAML. A name such as "x\e[2J\e[31mRED\r" would clear
// the reviewer's screen, colour what follows, or (with a bare CR) overwrite
// the line a finding was printed on; a right-to-left override reorders what
// is shown. Escape replaces those characters with visible escapes, so the
// text is shown as what it holds.
package textsafe

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

// Unsafe reports whether r must not reach a terminal as itself: the C0
// controls (tab, newline and CR included, so a field stays on one line and
// one column), DEL, the C1 controls, the bidirectional controls (the
// embeddings and overrides U+202A to U+202E, the isolates U+2066 to U+2069,
// the marks U+200E, U+200F and U+061C) and the line and paragraph
// separators U+2028 and U+2029.
func Unsafe(r rune) bool {
	switch {
	case r < 0x20, r == 0x7f, r >= 0x80 && r <= 0x9f:
		return true
	case r == 0x061c, r == 0x200e, r == 0x200f, r == 0x2028, r == 0x2029:
		return true
	case r >= 0x202a && r <= 0x202e, r >= 0x2066 && r <= 0x2069:
		return true
	}
	return false
}

// Escape returns s with every Unsafe rune, and every byte that is not valid
// UTF-8, written as a visible escape: \n, \r and \t for those three, \xNN
// for the other C0 controls, DEL and stray bytes, \uNNNN for the rest. A
// backslash is left as it is, so a Windows path stays readable; the escapes
// are for a person reading, not for parsing back. A string with nothing to
// escape is returned as it is, without allocating.
func Escape(s string) string { return escape(s, true) }

// Lines is Escape for text that is already several lines, such as an error
// from errors.Join: a newline is kept as the line break it is.
func Lines(s string) string { return escape(s, false) }

func escape(s string, newlines bool) string {
	clean := true
	for i := 0; i < len(s); {
		r, n := utf8.DecodeRuneInString(s[i:])
		if (r == utf8.RuneError && n == 1) || (Unsafe(r) && (newlines || r != '\n')) {
			clean = false
			break
		}
		i += n
	}
	if clean {
		return s
	}
	var b strings.Builder
	b.Grow(len(s) + 16)
	for i := 0; i < len(s); {
		r, n := utf8.DecodeRuneInString(s[i:])
		switch {
		case r == utf8.RuneError && n == 1:
			fmt.Fprintf(&b, `\x%02x`, s[i])
		case r == '\n' && !newlines:
			b.WriteByte('\n')
		case r == '\n':
			b.WriteString(`\n`)
		case r == '\r':
			b.WriteString(`\r`)
		case r == '\t':
			b.WriteString(`\t`)
		case Unsafe(r) && r < 0x80:
			fmt.Fprintf(&b, `\x%02x`, r)
		case Unsafe(r):
			fmt.Fprintf(&b, `\u%04x`, r)
		default:
			b.WriteString(s[i : i+n])
		}
		i += n
	}
	return b.String()
}
