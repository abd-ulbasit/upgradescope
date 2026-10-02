package junittest

import (
	"strings"
	"testing"
)

const valid = `<?xml version="1.0" encoding="UTF-8"?>
<testsuites name="s" tests="2" failures="1" errors="0" time="0">
  <testsuite name="a" tests="2" failures="1" errors="0" skipped="0" time="1.5">
    <testcase name="x" classname="s.a" time="0">
      <failure message="m" type="blocker">text</failure>
      <system-out>out</system-out>
    </testcase>
    <testcase name="y"></testcase>
  </testsuite>
</testsuites>
`

// Validate accepts what junit-10.xsd accepts and rejects, with the
// element and attribute named, what it does not; where xmllint is
// installed, libxml2's schema validator agrees on every case.
func TestValidate(t *testing.T) {
	if errs := Validate([]byte(valid)); len(errs) > 0 {
		t.Fatalf("valid document rejected: %v", errs)
	}
	if out, ok, ran := Xmllint(t, []byte(valid)); ran && !ok {
		t.Fatalf("xmllint rejects the valid document:\n%s", out)
	}
	for _, tc := range []struct {
		name, from, to, want string
	}{
		{"skipped on testsuites", `<testsuites name="s"`, `<testsuites skipped="0" name="s"`, `testsuites: attribute "skipped" is not allowed`},
		{"unknown attribute on testcase", `<testcase name="y"`, `<testcase file="f" name="y"`, `testcase: attribute "file" is not allowed`},
		{"unknown attribute on failure", `type="blocker"`, `type="blocker" line="1"`, `failure: attribute "line" is not allowed`},
		{"missing required attribute", ` failures="1" errors="0" skipped="0"`, ` failures="1" skipped="0"`, `testsuite: required attribute "errors" is missing`},
		{"missing testcase name", `<testcase name="y">`, `<testcase>`, `testcase: required attribute "name" is missing`},
		{"time not a SUREFIRE_TIME", `time="1.5"`, `time="1.5s"`, `testsuite: attribute "time" value "1.5s" does not match`},
		{"unknown element", `<system-out>out</system-out>`, `<stdout>out</stdout>`, `testcase: element "stdout" is not allowed`},
		{"element in the wrong parent", `<testcase name="y"></testcase>`, `<testcase name="y"></testcase><failure message="m"/>`, `testsuite: element "failure" is not allowed`},
		{"text where none is allowed", `<testcase name="y"></testcase>`, `<testcase name="y">loose</testcase>`, `testcase: text is not allowed`},
		{"undeclared root", `<testsuites name="s" tests="2" failures="1" errors="0" time="0">`, `<report>`, `"report" is not a declared element`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			doc := strings.Replace(valid, tc.from, tc.to, 1)
			if tc.name == "undeclared root" {
				doc = strings.Replace(doc, "</testsuites>", "</report>", 1)
			}
			if doc == valid {
				t.Fatalf("%q not in the document", tc.from)
			}
			errs := Validate([]byte(doc))
			if !strings.Contains(strings.Join(errs, "\n"), tc.want) {
				t.Errorf("errors = %q, want one containing %q", errs, tc.want)
			}
			if out, ok, ran := Xmllint(t, []byte(doc)); ran && ok {
				t.Errorf("xmllint accepts what Validate rejects:\n%s\n%s", doc, out)
			}
		})
	}
}
