package junit

import (
	"bytes"
	"encoding/json"
	"encoding/xml"
	"errors"
	"flag"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/abd-ulbasit/upgradescope/internal/engine"
	"github.com/abd-ulbasit/upgradescope/internal/inventory"
)

var update = flag.Bool("update", false, "rewrite golden files under testdata")

// The JUnit dialect CI systems read, decoded independently of the
// writer's model so a field the writer drops is caught. It follows the
// Jenkins JUnit schema (jenkins-junit.xsd, the de facto reference that
// GitLab, Azure Pipelines' PublishTestResults and Jenkins' junit step
// accept): a <testsuites> root, <testsuite> with name and tests (plus the
// failures, errors, skipped and time counts), <testcase> with name,
// classname and time, and at most one <failure>, <error> or <skipped> per
// test case, each with a message attribute.
type reader struct {
	XMLName  xml.Name      `xml:"testsuites"`
	Name     string        `xml:"name,attr"`
	Tests    *int          `xml:"tests,attr"`
	Failures *int          `xml:"failures,attr"`
	Errors   *int          `xml:"errors,attr"`
	Skipped  *int          `xml:"skipped,attr"`
	Time     string        `xml:"time,attr"`
	Suites   []readerSuite `xml:"testsuite"`
}

type readerSuite struct {
	Name     string       `xml:"name,attr"`
	Tests    *int         `xml:"tests,attr"`
	Failures *int         `xml:"failures,attr"`
	Errors   *int         `xml:"errors,attr"`
	Skipped  *int         `xml:"skipped,attr"`
	Time     string       `xml:"time,attr"`
	Cases    []readerCase `xml:"testcase"`
	Other    []xml.Name   `xml:",any"`
}

type readerCase struct {
	Name      string         `xml:"name,attr"`
	Classname string         `xml:"classname,attr"`
	Time      string         `xml:"time,attr"`
	Failure   []readerResult `xml:"failure"`
	Error     []readerResult `xml:"error"`
	Skipped   []readerResult `xml:"skipped"`
	SystemOut []string       `xml:"system-out"`
	Other     []xml.Name     `xml:",any"`
}

type readerResult struct {
	Message *string `xml:"message,attr"`
	Type    string  `xml:"type,attr"`
	Text    string  `xml:",chardata"`
}

// status is "failure", "error", "skipped" or "passed".
func (c readerCase) status() string {
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

// readJUnit parses raw as a JUnit reader does and checks it against the
// Jenkins JUnit schema's rules: required attributes, counts that match the
// test cases, one outcome per test case, numeric times.
func readJUnit(t *testing.T, raw []byte) reader {
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
	var doc reader
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
			for _, r := range append(append(append([]readerResult{}, c.Failure...), c.Error...), c.Skipped...) {
				if r.Message == nil || *r.Message == "" {
					t.Errorf("%s: outcome without a message", cw)
				}
			}
			switch c.status() {
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
	count("<testsuites>", "tests", doc.Tests, total)
	count("<testsuites>", "failures", doc.Failures, failures)
	count("<testsuites>", "errors", doc.Errors, errs)
	count("<testsuites>", "skipped", doc.Skipped, skipped)
	return doc
}

func write(t *testing.T, r engine.Report, opts Options) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := Write(&buf, r, opts); err != nil {
		t.Fatalf("Write: %v", err)
	}
	return buf.Bytes()
}

// outcomes maps "suite/testcase" to its status.
func outcomes(doc reader) map[string]string {
	out := map[string]string{}
	for _, s := range doc.Suites {
		for _, c := range s.Cases {
			out[s.Name+"/"+c.Name] = c.status()
		}
	}
	return out
}

// TestWriteGolden renders every engine golden report (the reports
// internal/engine's TestEvaluateGolden pins) as JUnit with the default
// gate, twice: the output is stable, a JUnit reader accepts it, and it
// matches testdata/<case>.xml (go test ./internal/junit -update rewrites
// them).
func TestWriteGolden(t *testing.T) {
	entries, err := os.ReadDir("../engine/testdata")
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		n++
		t.Run(name, func(t *testing.T) {
			raw, err := os.ReadFile(filepath.Join("../engine/testdata", name, "expected.json"))
			if err != nil {
				t.Fatal(err)
			}
			var r engine.Report
			if err := json.Unmarshal(raw, &r); err != nil {
				t.Fatal(err)
			}
			got := write(t, r, Options{FailOn: "blocker"})
			if again := write(t, r, Options{FailOn: "blocker"}); !bytes.Equal(got, again) {
				t.Fatalf("two runs differ:\n%s\n---\n%s", got, again)
			}
			doc := readJUnit(t, got)
			// Every finding is a test case, failing exactly when it is a
			// blocker (the default gate).
			var blockers int
			for _, f := range r.Findings {
				if f.Severity == engine.SevBlocker {
					blockers++
				}
			}
			if *doc.Failures != blockers {
				t.Errorf("failures = %d, want one per blocker (%d)", *doc.Failures, blockers)
			}
			golden := filepath.Join("testdata", name+".xml")
			if *update {
				if err := os.WriteFile(golden, got, 0o644); err != nil {
					t.Fatal(err)
				}
				return
			}
			want, err := os.ReadFile(golden)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, want) {
				t.Errorf("%s differs (rerun with -update and inspect the diff):\n got:\n%s\nwant:\n%s", golden, got, want)
			}
		})
	}
	if n < 10 {
		t.Fatalf("found %d engine golden cases; the fixture walk is broken", n)
	}
}

// gateReport has a finding of every kind the gate treats differently.
func gateReport() engine.Report {
	return engine.Report{
		Target:  inventory.Version{Major: 1, Minor: 36},
		Score:   10,
		Verdict: engine.VerdictBlocked,
		Findings: []engine.Finding{
			{Category: engine.CatRemovedAPI, Severity: engine.SevBlocker, Key: "removed-api/batch/v1beta1/CronJob",
				Title: "batch/v1beta1 CronJob removed in 1.25 (1 object)", Detail: "1 manifest object(s) use this API.",
				Remediation: "migrate to batch/v1 CronJob", Citations: []string{"https://kubernetes.io/docs/reference/using-api/deprecation-guide/"},
				Objects: []inventory.ObjectRef{{Name: "nightly", File: "rendered/all.yaml", Line: 14, RenderedFrom: "shop/templates/cronjob.yaml"}}},
			{Category: engine.CatEOLAddon, Severity: engine.SevBlocker, Key: "eol-addon/ingress-nginx",
				Title: "ingress-nginx is end-of-life", BaselineState: engine.BaselineUnchanged},
			{Category: engine.CatDeprecatedAPI, Severity: engine.SevWarning, Key: "deprecated-api/flowcontrol.apiserver.k8s.io/v1beta3/FlowSchema",
				Title: "flowcontrol v1beta3 FlowSchema deprecated"},
			{Category: engine.CatAddOnNoData, Severity: engine.SevInfo, Key: "addon-no-data/external-dns", Title: "no lifecycle data for external-dns"},
		},
		Suppressed: []engine.SuppressedFinding{{
			Finding: engine.Finding{Category: engine.CatRemovedAPI, Severity: engine.SevBlocker, Key: "removed-api/policy/v1beta1/PodSecurityPolicy",
				Title: "policy/v1beta1 PodSecurityPolicy removed in 1.25 (1 object)"},
			Reason: "deleted in the next release", Source: ".upgradescope.yaml", Expires: "2026-12-31",
		}},
		NotAssessed: []engine.CapabilityGap{
			{Capability: inventory.CapAddOns, Reason: "list pods: forbidden", Required: true},
			{Capability: inventory.CapHelm, Reason: "files mode"},
		},
	}
}

// TestWriteFollowsTheGate: a test case fails exactly when the finding it
// stands for fails `scan --fail-on` — at or above the threshold and not
// unchanged since the baseline. Below the threshold it passes, with the
// finding on system-out; baseline-unchanged and suppressed findings are
// skipped with the reason; a required gap that makes the gate fail is an
// error, so an unknown verdict reads as a broken build, not a green one.
func TestWriteFollowsTheGate(t *testing.T) {
	const (
		removed    = "removed-api/removed-api/batch/v1beta1/CronJob"
		eol        = "eol-addon/eol-addon/ingress-nginx"
		deprecated = "deprecated-api/deprecated-api/flowcontrol.apiserver.k8s.io/v1beta3/FlowSchema"
		noData     = "addon-no-data/addon-no-data/external-dns"
		suppressed = "removed-api/removed-api/policy/v1beta1/PodSecurityPolicy (suppressed)"
		addons     = "not-assessed/addons"
		helm       = "not-assessed/helm"
	)
	cases := []struct {
		name string
		opts Options
		want map[string]string
	}{
		{"blocker", Options{FailOn: "blocker"}, map[string]string{
			removed: "failure", eol: "skipped", deprecated: "passed", noData: "passed", suppressed: "skipped", addons: "error", helm: "skipped"}},
		{"default is blocker", Options{}, map[string]string{
			removed: "failure", eol: "skipped", deprecated: "passed", noData: "passed", suppressed: "skipped", addons: "error", helm: "skipped"}},
		{"warning", Options{FailOn: "warning"}, map[string]string{
			removed: "failure", eol: "skipped", deprecated: "failure", noData: "passed", suppressed: "skipped", addons: "error", helm: "skipped"}},
		{"allow incomplete", Options{FailOn: "blocker", AllowIncomplete: true}, map[string]string{
			removed: "failure", eol: "skipped", deprecated: "passed", noData: "passed", suppressed: "skipped", addons: "skipped", helm: "skipped"}},
		{"never", Options{FailOn: "never"}, map[string]string{
			removed: "passed", eol: "skipped", deprecated: "passed", noData: "passed", suppressed: "skipped", addons: "skipped", helm: "skipped"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			raw := write(t, gateReport(), tc.opts)
			got := outcomes(readJUnit(t, raw))
			for k, want := range tc.want {
				if got[k] != want {
					t.Errorf("%s = %q, want %q", k, got[k], want)
				}
			}
			if len(got) != len(tc.want) {
				t.Errorf("test cases = %v, want %v", got, tc.want)
			}
		})
	}

	// What each outcome says.
	doc := readJUnit(t, write(t, gateReport(), Options{FailOn: "blocker"}))
	byName := map[string]readerCase{}
	for _, s := range doc.Suites {
		for _, c := range s.Cases {
			byName[s.Name+"/"+c.Name] = c
		}
	}
	f := byName[removed].Failure[0]
	for _, want := range []string{"rendered/all.yaml:14", "nightly", "shop/templates/cronjob.yaml", "Fix: migrate to batch/v1 CronJob.", "deprecation-guide"} {
		if !strings.Contains(f.Text, want) {
			t.Errorf("failure text does not name %q:\n%s", want, f.Text)
		}
	}
	if *f.Message != "batch/v1beta1 CronJob removed in 1.25 (1 object)" || f.Type != "blocker" || byName[removed].Classname != "upgradescope.removed-api" {
		t.Errorf("failure = %+v (classname %s), want the title as message, the severity as type", f, byName[removed].Classname)
	}
	if m := *byName[eol].Skipped[0].Message; !strings.Contains(m, "unchanged since the baseline") {
		t.Errorf("baseline skip message = %q", m)
	}
	if m := *byName[suppressed].Skipped[0].Message; !strings.Contains(m, "deleted in the next release") || !strings.Contains(m, "2026-12-31") || !strings.Contains(m, ".upgradescope.yaml") {
		t.Errorf("suppressed skip message = %q, want the reason, expiry and source", m)
	}
	if out := byName[deprecated].SystemOut; len(out) != 1 || !strings.Contains(out[0], "warning") {
		t.Errorf("a passing finding's system-out = %q, want its severity and text", out)
	}
	if e := byName[addons].Error[0]; !strings.Contains(*e.Message, "list pods: forbidden") || !strings.Contains(e.Text, "unknown") {
		t.Errorf("required gap error = %+v, want the reason and the unknown verdict", e)
	}
}

// A target that is not an upgrade fails `scan` even with
// --allow-incomplete; only --fail-on never passes it.
func TestWriteTargetGap(t *testing.T) {
	r := engine.Report{Verdict: engine.VerdictUnknown, NotAssessed: []engine.CapabilityGap{
		{Capability: engine.GapTarget, Reason: "target 1.4 is not an upgrade of v1.36.4", Required: true}}}
	for _, tc := range []struct {
		opts Options
		want string
	}{
		{Options{FailOn: "blocker", AllowIncomplete: true}, "error"},
		{Options{FailOn: "warning"}, "error"},
		{Options{FailOn: "never"}, "skipped"},
	} {
		got := outcomes(readJUnit(t, write(t, r, tc.opts)))["not-assessed/target"]
		if got != tc.want {
			t.Errorf("%+v: target gap = %q, want %q", tc.opts, got, tc.want)
		}
	}
}

// A clean report still holds one passing test: Jenkins' junit step fails
// the build on reports without tests, and Azure warns.
func TestWriteCleanReport(t *testing.T) {
	r := engine.Report{Target: inventory.Version{Major: 1, Minor: 36}, Score: 100, Ready: true, Verdict: engine.VerdictReady, KBVersion: "kb-1"}
	doc := readJUnit(t, write(t, r, Options{FailOn: "blocker"}))
	if *doc.Tests != 1 || *doc.Failures != 0 || doc.Suites[0].Cases[0].status() != "passed" {
		t.Fatalf("clean report = %+v, want one passing test", doc)
	}
	if out := doc.Suites[0].Cases[0].SystemOut; len(out) != 1 || !strings.Contains(out[0], "ready") || !strings.Contains(out[0], "1.36") {
		t.Errorf("clean report system-out = %q, want the verdict and target", out)
	}
}

// Titles, names and reasons come from scanned manifests and config files:
// markup and characters XML 1.0 cannot carry must not break the document.
func TestWriteEscapes(t *testing.T) {
	r := engine.Report{Verdict: engine.VerdictBlocked, Findings: []engine.Finding{{
		Category: engine.CatRemovedAPI, Severity: engine.SevBlocker, Key: `removed-api/x/v1/K"<&>`,
		Title:   "a <b> & \"c\" \x01 ]]> d",
		Objects: []inventory.ObjectRef{{Name: "n]]>\x0b", File: "f&.yaml", Line: 1}},
	}}}
	doc := readJUnit(t, write(t, r, Options{}))
	c := doc.Suites[0].Cases[0]
	if c.Name != `removed-api/x/v1/K"<&>` || !strings.Contains(*c.Failure[0].Message, `a <b> & "c"`) || !strings.Contains(c.Failure[0].Text, "f&.yaml:1") {
		t.Errorf("escaped case = %+v", c)
	}
}

// A failed write is returned, so the scan exits 1 instead of passing or
// failing the gate with the report lost.
func TestWritePropagatesWriteErrors(t *testing.T) {
	if err := Write(failingWriter{}, gateReport(), Options{}); err == nil {
		t.Fatal("Write to a failing writer returned nil")
	}
}

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errors.New("no space left on device") }
