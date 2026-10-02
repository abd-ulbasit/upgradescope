// Package junittest reads JUnit XML as CI systems do and checks it against
// the Jenkins JUnit schema, for every package that emits JUnit (the CLI's
// --output junit and the server's /api/v1/gate?format=junit).
package junittest

import (
	"bytes"
	"encoding/xml"
	"errors"
	"io"
	"strconv"
	"testing"
)

// Doc is the JUnit dialect CI systems read, decoded independently of the
// writer's model so a field the writer drops is caught. It follows the
// Jenkins JUnit schema (jenkins-junit.xsd, the de facto reference that
// GitLab, Azure Pipelines' PublishTestResults and Jenkins' junit step
// accept): a <testsuites> root, <testsuite> with name and tests (plus the
// failures, errors, skipped and time counts), <testcase> with name,
// classname and time, and at most one <failure>, <error> or <skipped> per
// test case, each with a message attribute.
type Doc struct {
	XMLName  xml.Name `xml:"testsuites"`
	Name     string   `xml:"name,attr"`
	Tests    *int     `xml:"tests,attr"`
	Failures *int     `xml:"failures,attr"`
	Errors   *int     `xml:"errors,attr"`
	Skipped  *int     `xml:"skipped,attr"`
	Time     string   `xml:"time,attr"`
	Suites   []Suite  `xml:"testsuite"`
}

type Suite struct {
	Name     string     `xml:"name,attr"`
	Tests    *int       `xml:"tests,attr"`
	Failures *int       `xml:"failures,attr"`
	Errors   *int       `xml:"errors,attr"`
	Skipped  *int       `xml:"skipped,attr"`
	Time     string     `xml:"time,attr"`
	Cases    []Case     `xml:"testcase"`
	Other    []xml.Name `xml:",any"`
}

type Case struct {
	Name      string     `xml:"name,attr"`
	Classname string     `xml:"classname,attr"`
	Time      string     `xml:"time,attr"`
	Failure   []Result   `xml:"failure"`
	Error     []Result   `xml:"error"`
	Skipped   []Result   `xml:"skipped"`
	SystemOut []string   `xml:"system-out"`
	Other     []xml.Name `xml:",any"`
}

type Result struct {
	Message *string `xml:"message,attr"`
	Type    string  `xml:"type,attr"`
	Text    string  `xml:",chardata"`
}

// Status is "failure", "error", "skipped" or "passed".
func (c Case) Status() string {
	switch {
	case len(c.Failure) > 0:
		return "failure"
	case len(c.Error) > 0:
		return "error"
	case len(c.Skipped) > 0:
		return "skipped"
	}
	return "passed"
}

// Read parses raw as a JUnit reader does and checks it against the
// Jenkins JUnit schema's rules: an XML declaration, required attributes,
// counts that match the test cases, one outcome per test case, numeric
// times, and at least one test (Jenkins' junit step fails a build whose
// reports hold none).
func Read(t *testing.T, raw []byte) Doc {
	t.Helper()
	if !bytes.HasPrefix(raw, []byte(xml.Header)) {
		t.Errorf("output does not start with the XML declaration:\n%s", raw)
	}
	// Well-formed, strictly: every token decodes.
	dec := xml.NewDecoder(bytes.NewReader(raw))
	dec.Strict = true
	for {
		_, err := dec.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("not well-formed XML: %v\n%s", err, raw)
		}
	}
	var doc Doc
	if err := xml.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("not a <testsuites> document: %v\n%s", err, raw)
	}
	count := func(where, attr string, got *int, want int) {
		t.Helper()
		if got == nil {
			t.Errorf("%s: no %s attribute", where, attr)
		} else if *got != want {
			t.Errorf("%s: %s=%d, want %d", where, attr, *got, want)
		}
	}
	number := func(where, v string) {
		t.Helper()
		if _, err := strconv.ParseFloat(v, 64); err != nil {
			t.Errorf("%s: time %q is not a number", where, v)
		}
	}
	if doc.Name == "" {
		t.Error("<testsuites> has no name")
	}
	number("<testsuites>", doc.Time)
	if len(doc.Suites) == 0 {
		t.Fatal("no <testsuite>: Jenkins' junit step fails a build whose reports hold no tests")
	}
	var total, failures, errs, skipped int
	for _, s := range doc.Suites {
		where := "testsuite " + s.Name
		if s.Name == "" {
			t.Error("a <testsuite> has no name")
		}
		if len(s.Other) > 0 {
			t.Errorf("%s: unexpected elements %v", where, s.Other)
		}
		number(where, s.Time)
		var f, e, sk int
		for _, c := range s.Cases {
			cw := where + " / " + c.Name
			if c.Name == "" || c.Classname == "" {
				t.Errorf("%s: testcase without name or classname: %+v", where, c)
			}
			if len(c.Other) > 0 {
				t.Errorf("%s: unexpected elements %v", cw, c.Other)
			}
			number(cw, c.Time)
			if n := len(c.Failure) + len(c.Error) + len(c.Skipped); n > 1 {
				t.Errorf("%s: %d outcomes, want at most one", cw, n)
			}
			for _, r := range append(append(append([]Result{}, c.Failure...), c.Error...), c.Skipped...) {
				if r.Message == nil || *r.Message == "" {
					t.Errorf("%s: outcome without a message", cw)
				}
			}
			switch c.Status() {
			case "failure":
				f++
			case "error":
				e++
			case "skipped":
				sk++
			}
		}
		count(where, "tests", s.Tests, len(s.Cases))
		count(where, "failures", s.Failures, f)
		count(where, "errors", s.Errors, e)
		count(where, "skipped", s.Skipped, sk)
		total, failures, errs, skipped = total+len(s.Cases), failures+f, errs+e, skipped+sk
	}
	if total == 0 {
		t.Error("no <testcase>: Jenkins' junit step fails a build whose reports hold no tests")
	}
	count("<testsuites>", "tests", doc.Tests, total)
	count("<testsuites>", "failures", doc.Failures, failures)
	count("<testsuites>", "errors", doc.Errors, errs)
	count("<testsuites>", "skipped", doc.Skipped, skipped)
	return doc
}

// Outcomes maps "suite/testcase" to its status.
func Outcomes(doc Doc) map[string]string {
	out := map[string]string{}
	for _, s := range doc.Suites {
		for _, c := range s.Cases {
			out[s.Name+"/"+c.Name] = c.Status()
		}
	}
	return out
}
