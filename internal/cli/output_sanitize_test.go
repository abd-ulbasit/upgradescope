package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/abd-ulbasit/upgradescope/internal/engine"
	"github.com/abd-ulbasit/upgradescope/internal/inventory"
	"github.com/abd-ulbasit/upgradescope/internal/server/store"
	"github.com/abd-ulbasit/upgradescope/internal/textsafe"
)

// hostile holds what a manifest author can put in a name: an ANSI clear
// and colour, a BEL, a bare CR (overwrites the line), a GitHub workflow
// command, a right-to-left override, a C1 CSI, a line separator and a
// newline.
const hostile = "evil\x1b[2J\x1b[31mRED\a\r::error::pwned\u202eabc\u009b\u2028\nnext"

// assertTerminalSafe fails when out holds a byte or rune the terminal
// would act on. Only the newlines that end the renderer's own lines may
// remain, so a line break inside a field is a failure too: the fields
// carry the sentinel "next" on the line after the break.
func assertTerminalSafe(t *testing.T, what, out string) {
	t.Helper()
	if !utf8.ValidString(out) {
		t.Errorf("%s: not valid UTF-8", what)
	}
	for i, r := range out {
		if r == '\n' {
			continue
		}
		if textsafe.Unsafe(r) {
			lo := max(0, i-30)
			t.Fatalf("%s: holds %U at byte %d, after %q", what, r, i, out[lo:i])
		}
	}
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "next") {
			t.Errorf("%s: a field's newline survived: %q", what, line)
		}
	}
}

// fillHostile sets every plain string and []string below v to the hostile
// text (named string types, the enumerations the renderers switch on, are
// left alone), so a field a renderer forgets to escape shows up as output.
func fillHostile(v reflect.Value) {
	switch v.Kind() {
	case reflect.Pointer:
		if v.IsNil() {
			v.Set(reflect.New(v.Type().Elem()))
		}
		fillHostile(v.Elem())
	case reflect.Struct:
		for i := range v.NumField() {
			if v.Type().Field(i).IsExported() {
				fillHostile(v.Field(i))
			}
		}
	case reflect.Slice:
		if v.Type().Elem().Kind() == reflect.String && v.Type().Elem() != reflect.TypeOf("") {
			return
		}
		if v.Len() == 0 {
			v.Set(reflect.MakeSlice(v.Type(), 1, 1))
		}
		for i := range v.Len() {
			fillHostile(v.Index(i))
		}
	case reflect.String:
		if v.Type() == reflect.TypeOf("") {
			v.SetString(hostile)
		}
	}
}

// hostileReport is a report whose every free-text field is hostile, with
// the structure the renderers branch on (a baseline state, a suppression,
// a hop with each kind of entry, a gap with skipped entries) in place.
func hostileReport() engine.Report {
	f := engine.Finding{
		Category: engine.CatRemovedAPI, Severity: engine.SevBlocker,
		Objects: []inventory.ObjectRef{{Name: "x", Namespace: "y", File: "f", Line: 3, RenderedFrom: "t"}},
	}
	f.BaselineState = engine.BaselineNew
	r := engine.Report{
		Verdict: engine.VerdictBlocked, Findings: []engine.Finding{f},
		Suppressed: []engine.SuppressedFinding{{Finding: f, Reason: "r", Source: "s", Expires: "2999-01-01"}},
		NotAssessed: []engine.CapabilityGap{{
			Capability: inventory.CapDeprecatedCalls, Reason: "r", Required: true, Skipped: []string{"a"},
		}},
		Support:            &engine.SupportStatus{},
		UnrecognizedImages: []string{"i"},
		Hops: []engine.Hop{{
			Findings: []engine.Finding{f}, Changed: []engine.FindingRef{{Severity: engine.SevWarning, Was: engine.SevInfo}}, Carried: []engine.FindingRef{{}},
		}},
	}
	fillHostile(reflect.ValueOf(&r).Elem())
	// The enumerations and counts stay what the renderers branch on.
	r.Findings[0].Category, r.Findings[0].Severity = engine.CatRemovedAPI, engine.SevBlocker
	r.Findings[0].BaselineState = engine.BaselineNew
	return r
}

func TestTableAndMarkdownEscapeEveryField(t *testing.T) {
	r := hostileReport()
	var tbl, md bytes.Buffer
	if err := WriteTable(&tbl, r); err != nil {
		t.Fatal(err)
	}
	WriteMarkdown(&md, r)
	if !strings.Contains(tbl.String(), `\x1b[2J`) || !strings.Contains(md.String(), `\\x1b`) {
		t.Fatalf("the escapes are not shown:\n%s\n%s", tbl.String(), md.String())
	}
	assertTerminalSafe(t, "table", tbl.String())
	assertTerminalSafe(t, "markdown", md.String())
}

func TestTableEscapesObjectNamesTitlesDetailsAndFiles(t *testing.T) {
	obj := inventory.ObjectRef{Name: "n\x1b[2J", Namespace: "ns\u202eabc", File: "dir\r/f.yaml", Line: 4, RenderedFrom: "t\a"}
	f := engine.Finding{
		Category: engine.CatRemovedAPI, Severity: engine.SevBlocker, Title: "t\x1b[31m\ritle",
		Detail: "d\x1bd", Remediation: "fix\u009b", Citations: []string{"https://x/\x1b"}, Teams: []string{"te\ram"},
		Objects: []inventory.ObjectRef{obj},
	}
	r := engine.Report{Verdict: engine.VerdictBlocked, Findings: []engine.Finding{f}}
	var tbl bytes.Buffer
	if err := WriteTable(&tbl, r); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`[removed-api] t\x1b[31m\ritle`, `      d\x1bd`, `- ns\u202eabc/n\x1b[2J  dir\r/f.yaml:4 (rendered from t\x07)`,
		`fix: fix\u009b`, `see: https://x/\x1b`, `teams: te\ram`,
	} {
		if !strings.Contains(tbl.String(), want) {
			t.Errorf("table lacks %q:\n%s", want, tbl.String())
		}
	}
	assertTerminalSafe(t, "table", tbl.String())
}

// A cell's escapes survive Markdown: the backslash of an escape is itself
// escaped, so the page shows \x1b and not the control character.
func TestMarkdownShowsTheEscapeNotTheCharacter(t *testing.T) {
	r := engine.Report{Verdict: engine.VerdictBlocked, Findings: []engine.Finding{{
		Category: engine.CatRemovedAPI, Severity: engine.SevBlocker, Title: "a\x1bb\u202ec",
		Objects: []inventory.ObjectRef{{Name: "n\x1b", File: "f\x1b.yaml", Line: 1}},
	}}}
	var md bytes.Buffer
	WriteMarkdown(&md, r)
	for _, want := range []string{`a\\x1bb\\u202ec`, `n\\x1b`, "`f\\x1b.yaml:1`"} {
		if !strings.Contains(md.String(), want) {
			t.Errorf("markdown lacks %q:\n%s", want, md.String())
		}
	}
	assertTerminalSafe(t, "markdown", md.String())
}

// The real pipeline: a manifest, a config rule and a file name that all
// carry control characters, through the table, Markdown and the stderr
// warnings. SARIF and JUnit stay valid; JSON round-trips the exact text.
func TestScanFilesNeutralisesHostileManifestText(t *testing.T) {
	name := "evil\x1b[2J\x1b[31mRED\a\r::error::pwned"
	ns := "ns\u202eabc"
	manifest := fmt.Sprintf(`apiVersion: networking.k8s.io/v1beta1
kind: Ingress
metadata:
  name: %q
  namespace: %q
---
apiVersion: networking.k8s.io/v1beta1
kind: Ingress
metadata:
  name: annotated
  annotations:
    upgradescope.dev/ignore: removed-api
    upgradescope.dev/ignore-reason: %q
`, name, ns, "why\x1b[2J\r\u202e")
	cfg := fmt.Sprintf("ignore:\n  - category: eol-addon\n    reason: %q\n    expires: 2000-01-01\n", "cfg\x1b[2J\u202e")
	dir := writeFiles(t, map[string]string{
		"a.yaml": manifest, "bad\x1b[2J\u202e.yaml": "kind: [unterminated\n", ".upgradescope.yaml": cfg,
	})
	for _, format := range []string{"table", "markdown"} {
		t.Run(format, func(t *testing.T) {
			out, errOut, err := execScanFiles(t, "--files", dir, "--output", format)
			t.Logf("err: %v", err)
			assertTerminalSafe(t, format+" stdout", out)
			assertTerminalSafe(t, format+" stderr", errOut)
			if !strings.Contains(errOut, "warning:") {
				t.Errorf("no warning on stderr to check:\n%s", errOut)
			}
			// Markdown doubles the escape's backslash so the page shows it.
			if !strings.Contains(out, `x1b`) || !strings.Contains(out, `u202eabc`) {
				t.Errorf("the name is not shown escaped:\n%s", out)
			}
			if !strings.Contains(errOut, `\x1b[2J`) {
				t.Errorf("the warning does not show the escape:\n%s", errOut)
			}
		})
	}

	t.Run("json keeps the exact text", func(t *testing.T) {
		out, _, _ := execScanFiles(t, "--files", dir, "--output", "json")
		var rep engine.Report
		if err := json.Unmarshal([]byte(out), &rep); err != nil {
			t.Fatalf("json: %v\n%s", err, out)
		}
		found := false
		for _, f := range rep.Findings {
			for _, o := range f.Objects {
				found = found || (o.Name == name && o.Namespace == ns)
			}
		}
		if !found {
			t.Errorf("JSON lost the exact name %q:\n%s", name, out)
		}
		// Raw bytes below 0x20 never appear in JSON, so a terminal sees none.
		if bytes.ContainsAny([]byte(out), "\x1b\a\r\x00") {
			t.Errorf("JSON holds a raw control byte")
		}
	})

	t.Run("sarif is valid JSON with no raw control byte", func(t *testing.T) {
		out, _, _ := execScanFiles(t, "--files", dir, "--output", "sarif")
		if !json.Valid([]byte(out)) {
			t.Fatalf("SARIF is not valid JSON:\n%s", out)
		}
		if bytes.ContainsAny([]byte(out), "\x1b\a\r\x00") {
			t.Errorf("SARIF holds a raw control byte")
		}
	})

	t.Run("junit is well-formed XML with no control character", func(t *testing.T) {
		out, _, _ := execScanFiles(t, "--files", dir, "--output", "junit")
		if err := xml.Unmarshal([]byte(out), new(struct{ XMLName xml.Name })); err != nil {
			t.Fatalf("JUnit is not well-formed XML: %v\n%s", err, out)
		}
		if bytes.ContainsAny([]byte(out), "\x1b\a\r\x00") {
			t.Errorf("JUnit holds a raw control byte")
		}
	})
}

// The list commands print names that come from the database, which a
// cluster's agent chose (older rows were never validated) or an operator
// typed; the tables show them escaped. The messages that quote a name use
// %q, which escapes already: a test keeps it that way.
func TestAdminListsEscapeNames(t *testing.T) {
	db := filepath.Join(t.TempDir(), "admin.db")
	st, err := store.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := st.UpsertCluster(ctx, store.Cluster{Name: hostile, ClusterUID: "uid\x1b[2J\u202e"}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateToken(ctx, hostile, "tok-1234567890abcdef"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateReadToken(ctx, []string{hostile}, "read-1234567890abcdef"); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	cl, err := execClusters(t, "list", "--db", db)
	if err != nil {
		t.Fatal(err)
	}
	tl, _, err := execTokens(t, "list", "--db", db)
	if err != nil {
		t.Fatal(err)
	}
	rl, _, err := execTokens(t, "list", "--read", "--db", db)
	if err != nil {
		t.Fatal(err)
	}
	for what, out := range map[string]string{"clusters list": cl, "tokens list": tl, "tokens list --read": rl} {
		if !strings.Contains(out, `\x1b[2J`) {
			t.Errorf("%s does not show the escape:\n%s", what, out)
		}
		assertTerminalSafe(t, what, out)
	}
	msg, _, err := execTokens(t, "revoke", hostile, "--all", "--db", db)
	if err != nil {
		t.Fatal(err)
	}
	assertTerminalSafe(t, "tokens revoke", msg)
}

// A scan error that quotes a name a pull request chose is one line on
// stderr, however the name was spelled: a newline in a file name must not
// start a line of its own that looks like a warning.
func TestErrorTextEscapesNewlinesInQuotedNames(t *testing.T) {
	dir := writeFiles(t, map[string]string{"app.yaml": removedAPIs})
	// A dangling symlink: the walk fails to open it, and the error quotes its path.
	name := "x\nwarning: FAKE\x1b[2J\r\u202e.yaml"
	if err := os.Symlink(filepath.Join(dir, "missing-target"), filepath.Join(dir, name)); err != nil {
		t.Skipf("no symlinks here: %v", err)
	}
	_, _, err := execScanFiles(t, "--files", dir)
	if err == nil {
		t.Fatal("want an error from the dangling symlink")
	}
	got := ErrorText(err)
	assertTerminalSafe(t, "error", got)
	if strings.Contains(got, "\n") {
		t.Errorf("one failing file is one line, got %q", got)
	}
	if !strings.Contains(got, `x\nwarning: FAKE\x1b[2J\r\u202e.yaml`) {
		t.Errorf("the name is not shown as escapes: %q", got)
	}
	if strings.Contains(err.Error(), "\n") == false {
		t.Fatal("the test no longer reproduces: the raw error has no newline")
	}
}

// ErrorText keeps exactly the newlines errors.Join writes between errors
// (also under a "context: %w" prefix) and escapes every other one.
func TestErrorTextKeepsOnlyTheJoinSeparators(t *testing.T) {
	a := errors.New("first\nforged\x1b[2J")
	b := fmt.Errorf("second %q\nforged: %w", "x", errors.New("inner\rpart"))
	for _, tc := range []struct {
		name string
		err  error
		want string
	}{
		{"plain", a, `first\nforged\x1b[2J`},
		{"join", errors.Join(a, b), "first\\nforged\\x1b[2J\nsecond \"x\"\\nforged: inner\\rpart"},
		{"wrapped join", fmt.Errorf("ctx\n%s: %w", "p", errors.Join(a, errors.New("two"))), "ctx\\np: first\\nforged\\x1b[2J\ntwo"},
		{"several %w, newline-separated, read as a join", fmt.Errorf("%w\n%w", a, errors.New("two")), "first\\nforged\\x1b[2J\ntwo"},
		{"several %w with a prefix are escaped whole", fmt.Errorf("p\n%w %w", a, errors.New("two")), `p\nfirst\nforged\x1b[2J two`},
		{"wrapped", fmt.Errorf("ctx\nfake: %w", errors.New("in\x00ner")), `ctx\nfake: in\x00ner`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := ErrorText(tc.err)
			if got != tc.want {
				t.Errorf("ErrorText = %q, want %q", got, tc.want)
			}
			assertTerminalSafe(t, tc.name, got)
		})
	}
}

// The whitespace controls are shown as escapes in Markdown too, never
// folded into a space: a reviewer sees that the name held a CR or a tab.
func TestMarkdownShowsWhitespaceControlsAsEscapes(t *testing.T) {
	name := "RED\a\r::error::pwned\u0085x\tT\vU\fV\nW\u2028X\u2029Y"
	r := engine.Report{Verdict: engine.VerdictBlocked, Findings: []engine.Finding{{
		Category: engine.CatRemovedAPI, Severity: engine.SevBlocker, Title: name,
		Objects: []inventory.ObjectRef{{Name: name, File: name + ".yaml", Line: 1}},
	}}}
	var md bytes.Buffer
	WriteMarkdown(&md, r)
	// In text the backslash of the escape is itself escaped; in a code span it is not.
	text := `RED\\x07\\r::error::pwned\\u0085x\\tT\\x0bU\\x0cV\\nW\\u2028X\\u2029Y`
	code := "`" + `RED\x07\r::error::pwned\u0085x\tT\x0bU\x0cV\nW\u2028X\u2029Y.yaml:1` + "`"
	for _, want := range []string{text, code} {
		if strings.Count(md.String(), want) < 1 {
			t.Errorf("markdown lacks %q:\n%s", want, md.String())
		}
	}
	assertTerminalSafe(t, "markdown", md.String())
}
