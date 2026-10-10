// Package junit renders engine reports as JUnit XML, the test-report
// format Jenkins (the junit step), GitLab (artifacts:reports:junit) and
// Azure Pipelines (PublishTestResults) display. Shared by the CLI's
// --output junit and the server's /api/v1/gate?format=junit endpoint.
package junit

import (
	"encoding/xml"
	"fmt"
	"io"
	"slices"
	"strings"

	"github.com/abd-ulbasit/upgradescope/internal/engine"
	"github.com/abd-ulbasit/upgradescope/internal/inventory"
)

// Options are the gate the test outcomes follow, as `scan` takes it.
type Options struct {
	// FailOn is blocker (the default when empty), warning or never.
	FailOn string
	// AllowIncomplete: a required gap other than the target does not fail.
	AllowIncomplete bool
}

// The JUnit model: only the fields we emit, valid against the Jenkins
// JUnit schema (the xUnit plugin's junit-10.xsd; the package tests check
// it). That schema has no skipped count on <testsuites>, only on each
// <testsuite>.
type testsuites struct {
	XMLName  xml.Name    `xml:"testsuites"`
	Name     string      `xml:"name,attr"`
	Tests    int         `xml:"tests,attr"`
	Failures int         `xml:"failures,attr"`
	Errors   int         `xml:"errors,attr"`
	Time     string      `xml:"time,attr"`
	Suites   []testsuite `xml:"testsuite"`
}

type testsuite struct {
	Name     string     `xml:"name,attr"`
	Tests    int        `xml:"tests,attr"`
	Failures int        `xml:"failures,attr"`
	Errors   int        `xml:"errors,attr"`
	Skipped  int        `xml:"skipped,attr"`
	Time     string     `xml:"time,attr"`
	Cases    []testcase `xml:"testcase"`
}

type testcase struct {
	Name      string   `xml:"name,attr"`
	Classname string   `xml:"classname,attr"`
	Time      string   `xml:"time,attr"`
	Failure   *outcome `xml:"failure"`
	Error     *outcome `xml:"error"`
	Skipped   *outcome `xml:"skipped"`
	SystemOut string   `xml:"system-out,omitempty"`
}

type outcome struct {
	Message string `xml:"message,attr"`
	Type    string `xml:"type,attr,omitempty"`
	Text    string `xml:",chardata"`
}

const (
	name = "upgradescope"
	// zero is every time attribute: the scan is not timed per check, and
	// a fixed value keeps the output reproducible.
	zero = "0"
	// notAssessed is the suite of the capability gaps.
	notAssessed = "not-assessed"
)

// categoryOrder is the suite order; a category it does not list follows,
// sorted.
var categoryOrder = []engine.Category{
	engine.CatRemovedAPI, engine.CatDeprecatedAPI, engine.CatDeprecatedAPIInUse, engine.CatUnknownAPI,
	engine.CatEOLAddon, engine.CatEOLApproaching, engine.CatChartIncompat, engine.CatAddOnNoData,
	engine.CatVersionSkew, engine.CatKBStale,
}

// Write renders the report as JUnit XML: one <testsuite> per finding
// category, in a fixed order, with one <testcase> per finding (name: its
// key; classname: upgradescope.<category>), and a not-assessed suite with
// one test case per capability gap. Outcomes follow the gate in opts, so
// the test report fails exactly when `scan --fail-on` does:
//
//   - a finding at or above FailOn is a <failure> (type: its severity;
//     message: its title; text: evidence, objects as file:line, fix and
//     references); below it, the test case passes and the same text is
//     its <system-out>;
//   - a finding unchanged since the baseline, and a suppressed finding
//     (test case "<key> (suppressed)"), is <skipped> with the reason;
//   - a required gap the gate fails on (the target, always; any other
//     unless AllowIncomplete) is an <error>: the verdict is unknown;
//     every other gap is <skipped>, the check not having run.
//
// A report with none of these is one passing test case ("no findings",
// suite readiness), since Jenkins fails a build whose reports hold no
// tests. Any gap, even a skipped optional one, adds a not-assessed suite
// instead, so that case appears only when every check ran: a live scan.
// A clean files-mode scan is one skipped test per optional check that did
// not run (deprecated-calls, helm, versions) and none passed. Times are 0.
// A failed write is returned.
func Write(w io.Writer, r engine.Report, opts Options) error {
	failOn := opts.FailOn
	if failOn == "" {
		failOn = "blocker"
	}
	suites := map[string]*testsuite{}
	var order []string
	add := func(suite string, c testcase) {
		s, ok := suites[suite]
		if !ok {
			s = &testsuite{Name: suite, Time: zero}
			suites[suite] = s
			order = append(order, suite)
		}
		c.Classname, c.Time = name+"."+suite, zero
		s.Cases = append(s.Cases, c)
	}

	for _, f := range r.Findings {
		text := findingText(f)
		c := testcase{Name: key(f)}
		switch {
		case f.BaselineState == engine.BaselineUnchanged:
			c.Skipped = &outcome{Message: "unchanged since the baseline: " + f.Title}
			c.SystemOut = string(f.Severity) + ": " + text
		case reaches(f.Severity, failOn):
			c.Failure = &outcome{Message: f.Title, Type: string(f.Severity), Text: text}
		default:
			c.SystemOut = string(f.Severity) + ": " + text
		}
		add(string(f.Category), c)
	}
	for _, s := range r.Suppressed {
		why := s.Reason
		if s.Expires != "" {
			why += "; until " + s.Expires
		}
		if s.Source != "" {
			why += "; by " + s.Source
		}
		add(string(s.Category), testcase{
			Name:      key(s.Finding) + " (suppressed)",
			Skipped:   &outcome{Message: "suppressed (" + why + "): " + s.Title},
			SystemOut: string(s.Severity) + ": " + findingText(s.Finding),
		})
	}
	sortSuites(order)
	for _, g := range r.NotAssessed {
		msg := "not assessed: " + gapText(g)
		c := testcase{Name: string(g.Capability)}
		if g.Required && failOn != "never" && (g.Capability == engine.GapTarget || !opts.AllowIncomplete) {
			why := "The verdict is unknown: a required check did not run, so a blocker may have been missed."
			if g.Capability == engine.GapTarget {
				why = "The verdict is unknown: the target is not an upgrade of this cluster."
			}
			c.Error = &outcome{Message: msg, Type: notAssessed, Text: why}
		} else {
			c.Skipped = &outcome{Message: msg}
		}
		add(notAssessed, c)
	}
	if len(order) == 0 {
		add("readiness", testcase{Name: "no findings", SystemOut: fmt.Sprintf(
			"verdict %s, score %d/100, target %s, knowledge base %s: no findings, nothing suppressed, every check assessed.",
			r.Verdict, r.Score, r.Target, r.KBVersion)})
	}

	doc := testsuites{Name: name, Time: zero}
	for _, n := range order {
		s := suites[n]
		for _, c := range s.Cases {
			s.Tests++
			switch {
			case c.Failure != nil:
				s.Failures++
			case c.Error != nil:
				s.Errors++
			case c.Skipped != nil:
				s.Skipped++
			}
		}
		doc.Tests += s.Tests
		doc.Failures += s.Failures
		doc.Errors += s.Errors
		doc.Suites = append(doc.Suites, *s)
	}

	if _, err := io.WriteString(w, xml.Header); err != nil {
		return err
	}
	enc := xml.NewEncoder(w)
	enc.Indent("", "  ")
	if err := enc.Encode(doc); err != nil {
		return err
	}
	_, err := io.WriteString(w, "\n")
	return err
}

// sortSuites orders category suite names: categoryOrder first, then the
// rest by name. Gaps are added after it, so not-assessed stays last.
func sortSuites(order []string) {
	rank := func(s string) int {
		if i := slices.Index(categoryOrder, engine.Category(s)); i >= 0 {
			return i
		}
		return len(categoryOrder)
	}
	slices.SortStableFunc(order, func(a, b string) int {
		if ra, rb := rank(a), rank(b); ra != rb {
			return ra - rb
		}
		return strings.Compare(a, b)
	})
}

// reaches reports whether severity s fails the gate at failOn.
func reaches(s engine.Severity, failOn string) bool {
	switch failOn {
	case "never":
		return false
	case "warning":
		return s == engine.SevBlocker || s == engine.SevWarning
	default: // blocker
		return s == engine.SevBlocker
	}
}

func key(f engine.Finding) string {
	if f.Key != "" {
		return f.Key
	}
	return string(f.Category)
}

// findingText is a finding as a test report shows it: title, evidence,
// objects (file:line, rendered from), teams, fix and references.
func findingText(f engine.Finding) string {
	var b strings.Builder
	b.WriteString(sentence(f.Title))
	if f.Detail != "" {
		b.WriteString("\n" + sentence(f.Detail))
	}
	if len(f.Objects) > 0 {
		b.WriteString("\nObjects:")
		for _, o := range f.Objects {
			b.WriteString("\n- " + objectText(o))
		}
		if f.ObjectsOmitted > 0 {
			fmt.Fprintf(&b, "\n- and %d more (at most %d objects are recorded per finding)", f.ObjectsOmitted, inventory.MaxObjectRefs)
		}
	}
	if len(f.Teams) > 0 {
		b.WriteString("\nTeams: " + strings.Join(f.Teams, ", "))
	}
	if f.Remediation != "" {
		b.WriteString("\nFix: " + sentence(f.Remediation))
	}
	if len(f.Citations) > 0 {
		b.WriteString("\nReferences:")
		for _, c := range f.Citations {
			b.WriteString("\n- " + c)
		}
	}
	return b.String()
}

// objectText is "namespace/name (file:line), rendered from X".
func objectText(o inventory.ObjectRef) string {
	s := o.Name
	if s == "" {
		s = "(unnamed)"
	}
	if o.Namespace != "" {
		s = o.Namespace + "/" + s
	}
	switch {
	case o.File != "" && o.Line > 0:
		s += fmt.Sprintf(" (%s:%d)", o.File, o.Line)
	case o.File != "":
		s += " (" + o.File + ")"
	case o.Line > 0:
		s += fmt.Sprintf(" (line %d)", o.Line)
	}
	if o.RenderedFrom != "" {
		s += ", rendered from " + o.RenderedFrom
	}
	return s
}

// gapText is the gap's label, reason and what it skipped.
func gapText(g engine.CapabilityGap) string {
	s := g.Label() + ": " + g.Reason
	if len(g.Skipped) > 0 {
		s += " (skipped: " + strings.Join(g.Skipped, ", ") + ")"
	}
	return s
}

// sentence ends s with a full stop unless it already ends a sentence.
func sentence(s string) string {
	s = strings.TrimSpace(s)
	if strings.HasSuffix(s, ".") || strings.HasSuffix(s, "!") || strings.HasSuffix(s, "?") {
		return s
	}
	return s + "."
}
