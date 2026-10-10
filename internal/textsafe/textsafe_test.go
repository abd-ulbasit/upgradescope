package textsafe

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestEscape(t *testing.T) {
	for _, tc := range []struct{ name, in, want string }{
		{"plain", "ingress-nginx/controller", "ingress-nginx/controller"},
		{"unicode text is kept", "名前 ñ é → …", "名前 ñ é → …"},
		{"ESC", "a\x1b[2Jb", `a\x1b[2Jb`},
		{"CSI colour", "\x1b[31mRED", `\x1b[31mRED`},
		{"BEL", "a\ab", `a\x07b`},
		{"CR overwrites a line", "real\rfake", `real\rfake`},
		{"newline", "a\nb", `a\nb`},
		{"tab", "a\tb", `a\tb`},
		{"NUL", "a\x00b", `a\x00b`},
		{"DEL", "a\x7fb", `a\x7fb`},
		{"C1 CSI", "a\u009bb", `a\u009bb`},
		{"C1 NEL", "a\u0085b", `a\u0085b`},
		{"RLO", "ns\u202eabc", `ns\u202eabc`},
		{"bidi isolates", "\u2066x\u2069", `\u2066x\u2069`},
		{"line and paragraph separators", "a\u2028b\u2029c", `a\u2028b\u2029c`},
		{"direction marks", "a\u200eb\u200fc\u061cd", `a\u200eb\u200fc\u061cd`},
		{"invalid UTF-8", "a\xffb", `a\xffb`},
		{"backslash is kept", `C:\dir\n`, `C:\dir\n`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := Escape(tc.in); got != tc.want {
				t.Errorf("Escape(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestEscapeLeavesNothingUnsafe(t *testing.T) {
	var b strings.Builder
	for r := rune(0); r < 0x10000; r++ {
		if utf8.ValidRune(r) {
			b.WriteRune(r)
		}
	}
	got := Escape(b.String())
	for _, r := range got {
		if Unsafe(r) {
			t.Fatalf("Escape left %U in its output", r)
		}
	}
	if !utf8.ValidString(got) {
		t.Fatal("Escape's output is not valid UTF-8")
	}
}

func TestEscapeNoAllocWhenSafe(t *testing.T) {
	s := "ingress-nginx"
	if n := testing.AllocsPerRun(100, func() { _ = Escape(s) }); n != 0 {
		t.Errorf("Escape of a safe string allocates %v times", n)
	}
}
