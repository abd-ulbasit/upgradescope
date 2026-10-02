// Package junittest reads JUnit XML as CI systems do and validates it
// against the Jenkins JUnit schema (the xUnit plugin's junit-10.xsd,
// vendored under testdata), for every package that emits JUnit (the CLI's
// --output junit and the server's /api/v1/gate?format=junit).
package junittest

import (
	"bytes"
	_ "embed"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// schemaXSD is the Jenkins xUnit plugin's JUnit schema, vendored verbatim;
// testdata/README.md has its source, revision and license.
//
//go:embed testdata/junit-10.xsd
var schemaXSD []byte

// Doc is the JUnit dialect CI systems read, decoded independently of the
// writer's model so a field the writer drops is caught. Its shape is the
// Jenkins JUnit schema's (testdata/junit-10.xsd, which the xUnit plugin
// validates against; GitLab, Azure Pipelines' PublishTestResults and
// Jenkins' junit step read the same dialect): a <testsuites> root with
// name, tests, failures, errors and time (the schema allows no skipped
// count there, only on <testsuite>), <testsuite> with name, tests,
// failures and errors (plus skipped and time), <testcase> with name,
// classname and time, and at most one <failure>, <error> or <skipped> per
// test case, each with a message attribute.
type Doc struct {
	XMLName  xml.Name `xml:"testsuites"`
	Name     string   `xml:"name,attr"`
	Tests    *int     `xml:"tests,attr"`
	Failures *int     `xml:"failures,attr"`
	Errors   *int     `xml:"errors,attr"`
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

// Read parses raw as a JUnit reader does, validates it against the
// Jenkins JUnit schema (Validate, and xmllint where it is installed), and
// checks what the schema leaves to readers: an XML declaration, a name on
// every suite and a classname on every test case, counts that match the
// test cases, one outcome per test case, each with a message, numeric
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
	for _, e := range Validate(raw) {
		t.Errorf("not valid against the Jenkins JUnit schema (junit-10.xsd): %s", e)
	}
	if out, ok, ran := Xmllint(t, raw); ran && !ok {
		t.Errorf("xmllint rejects it against junit-10.xsd:\n%s", out)
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
	var total, failures, errs int
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
		total, failures, errs = total+len(s.Cases), failures+f, errs+e
	}
	if total == 0 {
		t.Error("no <testcase>: Jenkins' junit step fails a build whose reports hold no tests")
	}
	count("<testsuites>", "tests", doc.Tests, total)
	count("<testsuites>", "failures", doc.Failures, failures)
	count("<testsuites>", "errors", doc.Errors, errs)
	return doc
}

// Validate checks raw against the declarations in testdata/junit-10.xsd
// and returns what it violates: the root must be <testsuites>, every
// element declared and allowed in its parent, every attribute declared on
// its element, every required attribute present, every time a match for
// the schema's SUREFIRE_TIME pattern, and text only where the schema
// allows it. Child order and occurrence counts are not checked; Read
// checks the ones readers rely on. Malformed XML is one error.
func Validate(raw []byte) []string {
	schema, err := parseXSD(schemaXSD)
	if err != nil {
		return []string{"testdata/junit-10.xsd: " + err.Error()}
	}
	var errs []string
	var stack []*xsdElement // nil: inside an element already reported
	var path []string
	dec := xml.NewDecoder(bytes.NewReader(raw))
	dec.Strict = true
	for {
		tok, err := dec.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return append(errs, "not well-formed XML: "+err.Error())
		}
		switch tok := tok.(type) {
		case xml.StartElement:
			name := tok.Name.Local
			var el *xsdElement
			switch {
			case len(stack) == 0:
				if el = schema[name]; el == nil || name != "testsuites" {
					errs = append(errs, fmt.Sprintf("root: %q is not a declared element JUnit readers start from (<testsuites>)", name))
					el = nil
				}
			case stack[len(stack)-1] != nil:
				if el = stack[len(stack)-1].child(name, schema); el == nil {
					errs = append(errs, fmt.Sprintf("%s: element %q is not allowed", strings.Join(path, "/"), name))
				}
			}
			stack, path = append(stack, el), append(path, name)
			if el == nil {
				continue
			}
			where := strings.Join(path, "/")
			seen := map[string]bool{}
			for _, a := range tok.Attr {
				seen[a.Name.Local] = true
				decl, ok := el.attrs[a.Name.Local]
				switch {
				case !ok || a.Name.Space != "":
					errs = append(errs, fmt.Sprintf("%s: attribute %q is not allowed", where, a.Name.Local))
				case decl != nil && !decl.MatchString(a.Value):
					errs = append(errs, fmt.Sprintf("%s: attribute %q value %q does not match %s", where, a.Name.Local, a.Value, decl))
				}
			}
			for _, n := range el.required {
				if !seen[n] {
					errs = append(errs, fmt.Sprintf("%s: required attribute %q is missing", where, n))
				}
			}
		case xml.EndElement:
			stack, path = stack[:len(stack)-1], path[:len(path)-1]
		case xml.CharData:
			if len(stack) == 0 || len(bytes.TrimSpace(tok)) == 0 {
				continue
			}
			if el := stack[len(stack)-1]; el != nil && !el.text {
				errs = append(errs, fmt.Sprintf("%s: text is not allowed", strings.Join(path, "/")))
			}
		}
	}
	return errs
}

// xsdSchema is the global element declarations of an XML Schema, by name.
type xsdSchema map[string]*xsdElement

// xsdElement is what Validate checks of one element declaration.
type xsdElement struct {
	// attrs are the declared attributes, each with its type's pattern
	// (nil: any string).
	attrs    map[string]*regexp.Regexp
	required []string
	// children are the elements it may contain: a local declaration, or
	// nil for a reference to a global one.
	children map[string]*xsdElement
	// text: mixed content or a string type.
	text bool
}

func (e *xsdElement) child(name string, s xsdSchema) *xsdElement {
	local, ok := e.children[name]
	if !ok {
		return nil
	}
	if local != nil {
		return local
	}
	return s[name]
}

// xsdNode is any schema element, decoded generically.
type xsdNode struct {
	XMLName  xml.Name
	Attrs    []xml.Attr `xml:",any,attr"`
	Children []xsdNode  `xml:",any"`
}

func (n xsdNode) attr(name string) string {
	for _, a := range n.Attrs {
		if a.Name.Local == name {
			return a.Value
		}
	}
	return ""
}

// parseXSD reads the constructs junit-10.xsd uses: simple types that
// restrict a string by a pattern; named and inline complex types, mixed or
// not, with attributes, element references and local string elements
// inside sequences and choices; and global elements of a named, inline or
// string type. Anything else is an error, so a schema that needs more is
// not half-checked in silence.
func parseXSD(raw []byte) (xsdSchema, error) {
	var root xsdNode
	if err := xml.Unmarshal(raw, &root); err != nil {
		return nil, err
	}
	patterns := map[string]*regexp.Regexp{}
	complexTypes := map[string]xsdNode{}
	var elements []xsdNode
	for _, n := range root.Children {
		switch n.XMLName.Local {
		case "simpleType":
			re, err := simplePattern(n)
			if err != nil {
				return nil, err
			}
			patterns[n.attr("name")] = re
		case "complexType":
			complexTypes[n.attr("name")] = n
		case "element":
			elements = append(elements, n)
		default:
			return nil, fmt.Errorf("unsupported top-level <xs:%s>", n.XMLName.Local)
		}
	}

	var element func(n xsdNode) (*xsdElement, error)
	complexType := func(ct xsdNode) (*xsdElement, error) {
		el := &xsdElement{attrs: map[string]*regexp.Regexp{}, children: map[string]*xsdElement{}, text: ct.attr("mixed") == "true"}
		var walk func(n xsdNode) error
		walk = func(n xsdNode) error {
			for _, c := range n.Children {
				switch c.XMLName.Local {
				case "sequence", "choice":
					if err := walk(c); err != nil {
						return err
					}
				case "element":
					if ref := c.attr("ref"); ref != "" {
						el.children[ref] = nil
						continue
					}
					local, err := element(c)
					if err != nil {
						return err
					}
					el.children[c.attr("name")] = local
				case "attribute":
					name, typ := c.attr("name"), c.attr("type")
					var re *regexp.Regexp
					if typ != "xs:string" {
						var ok bool
						if re, ok = patterns[typ]; !ok {
							return fmt.Errorf("attribute %q: unsupported type %q", name, typ)
						}
					}
					el.attrs[name] = re
					if c.attr("use") == "required" {
						el.required = append(el.required, name)
					}
				default:
					return fmt.Errorf("unsupported <xs:%s> in a complex type", c.XMLName.Local)
				}
			}
			return nil
		}
		return el, walk(ct)
	}
	element = func(n xsdNode) (*xsdElement, error) {
		switch typ := n.attr("type"); {
		case typ == "xs:string" && len(n.Children) == 0:
			return &xsdElement{text: true}, nil
		case typ != "" && len(n.Children) == 0:
			ct, ok := complexTypes[typ]
			if !ok {
				return nil, fmt.Errorf("element %q: unsupported type %q", n.attr("name"), typ)
			}
			return complexType(ct)
		case typ == "" && len(n.Children) == 1 && n.Children[0].XMLName.Local == "complexType":
			return complexType(n.Children[0])
		}
		return nil, fmt.Errorf("element %q: unsupported declaration", n.attr("name"))
	}

	s := xsdSchema{}
	for _, n := range elements {
		el, err := element(n)
		if err != nil {
			return nil, err
		}
		s[n.attr("name")] = el
	}
	return s, nil
}

// simplePattern is a pattern restriction of a string as an anchored
// regexp: an XML Schema pattern matches the whole value.
func simplePattern(n xsdNode) (*regexp.Regexp, error) {
	if len(n.Children) == 1 {
		r := n.Children[0]
		if r.XMLName.Local == "restriction" && r.attr("base") == "xs:string" && len(r.Children) == 1 && r.Children[0].XMLName.Local == "pattern" {
			return regexp.Compile("^(?:" + r.Children[0].attr("value") + ")$")
		}
	}
	return nil, fmt.Errorf("simple type %q: only a pattern restriction of xs:string is supported", n.attr("name"))
}

// Xmllint validates raw against the vendored junit-10.xsd with libxml2's
// xmllint, an implementation independent of Validate: valid is its
// verdict, out what it printed. ran is false, and the check logged as
// skipped, when xmllint is not installed.
func Xmllint(t *testing.T, raw []byte) (out string, valid, ran bool) {
	t.Helper()
	bin, err := exec.LookPath("xmllint")
	if err != nil {
		t.Log("xmllint not installed; skipping the libxml2 cross-check")
		return "", false, false
	}
	dir := t.TempDir()
	xsd, doc := filepath.Join(dir, "junit-10.xsd"), filepath.Join(dir, "report.xml")
	if err := os.WriteFile(xsd, schemaXSD, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(doc, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	b, err := exec.Command(bin, "--noout", "--schema", xsd, doc).CombinedOutput()
	var exit *exec.ExitError
	if err != nil && !errors.As(err, &exit) {
		t.Fatalf("xmllint: %v\n%s", err, b)
	}
	return string(b), err == nil, true
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
