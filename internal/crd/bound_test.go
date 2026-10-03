package crd

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/abd-ulbasit/upgradescope/internal/engine"
	"github.com/abd-ulbasit/upgradescope/internal/inventory"
)

// spec.targets is bounded in the schema: every target costs a status row, so
// an unbounded list could grow the status past the apiserver's request limit
// and wedge the write (issue #191). The constant the agent caps at must
// equal the schema's maxItems, and each item must be short.
func TestManifestBoundsSpecTargets(t *testing.T) {
	c := parseManifest(t)
	targets := c.Spec.Versions[0].Schema.OpenAPIV3Schema.Properties["spec"].Properties["targets"]
	if targets.MaxItems == nil || *targets.MaxItems != int64(MaxTargets) {
		t.Errorf("spec.targets maxItems = %v, want MaxTargets (%d)", targets.MaxItems, MaxTargets)
	}
	if item := targets.Items.Schema; item.MaxLength == nil || *item.MaxLength > 8 {
		t.Errorf("spec.targets[] maxLength = %v, want at most 8 (\"1.123456\")", item.MaxLength)
	}
}

// A finding title or remediation can carry object names or free text of any
// length; what lands in status is clipped to a budget of encoded bytes (what
// the text costs in the stored JSON), on a rune boundary.
func TestTargetStatusFromReportClipsLongText(t *testing.T) {
	for _, tc := range []struct{ name, ch string }{
		{"ascii", "x"}, {"two-byte", "é"}, {"four-byte", "😀"}, {"escaped", "<"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			long := strings.Repeat(tc.ch, 5000)
			r := engine.Report{Target: inventory.Version{Major: 1, Minor: 36}, Findings: []engine.Finding{
				{Category: engine.CatDeprecatedAPI, Severity: engine.SevWarning, Title: long, Remediation: long},
			}}
			f := TargetStatusFromReport(r).TopFindings[0]
			if !utf8.ValidString(f.Title) || !utf8.ValidString(f.Remediation) {
				t.Error("clipped text is not valid UTF-8")
			}
			for _, c := range []struct {
				name, got string
				limit     int
			}{{"title", f.Title, maxTitleLen}, {"remediation", f.Remediation, maxRemediationLen}} {
				if !strings.HasSuffix(c.got, "…") {
					t.Errorf("%s does not end in …", c.name)
				}
				// The text is cut at the last whole character that fits, so it
				// comes within one character (6 bytes at most) of the budget.
				if n := encodedLen(t, strings.TrimSuffix(c.got, "…")); n > c.limit || n < c.limit-6 {
					t.Errorf("%s encodes to %d bytes before the ellipsis, want %d-%d", c.name, n, c.limit-6, c.limit)
				}
			}
		})
	}
	short := TargetStatusFromReport(engine.Report{Findings: []engine.Finding{{Title: "short"}}}).TopFindings[0].Title
	if short != "short" {
		t.Errorf("short title = %q, want it untouched", short)
	}
	// Text that fits the budget is untouched even when it is multi-byte.
	fits := strings.Repeat("é", maxTitleLen/2)
	if got := clip(fits, maxTitleLen); got != fits {
		t.Errorf("a title that exactly fits the budget was clipped to %d bytes", len(got))
	}
}

// encodedLen is the length of s as the apiserver stores it: a JSON string
// with Go's escaping (< > & and U+2028/9 as \uXXXX), without the quotes.
func encodedLen(t *testing.T, s string) int {
	t.Helper()
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	return len(b) - 2
}

// The status is bounded by construction: MaxTargets targets, each with
// maxTopFindings findings of the longest title and remediation, plus the
// longest and most numerous notAssessed notes, written through WriteStatus,
// stay under 1 MiB as the apiserver stores them (JSON, with its escaping),
// whatever characters the text is made of: a '<' is six bytes there and an
// emoji four, so a bound counted in characters would reach 1.5 MiB. The
// apiserver's request limit is 3 MiB and etcd's object limit 1.5 MiB.
func TestWorstCaseStatusStaysUnder1MiB(t *testing.T) {
	for _, tc := range []struct{ name, ch string }{
		{"ascii", "x"}, {"two-byte", "é"}, {"four-byte", "😀"}, {"escaped", "<"},
		{"quote", `"`}, {"control", "\x01"}, {"line-separator", " "}, {"invalid-utf8", "\xff"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			huge := strings.Repeat(tc.ch, 100_000)
			var reports []engine.Report
			for i := 0; i < MaxTargets; i++ {
				r := engine.Report{Target: inventory.Version{Major: 1, Minor: 30 + i}}
				for j := 0; j < 5*maxTopFindings; j++ {
					r.Findings = append(r.Findings, engine.Finding{
						Category: engine.CatDeprecatedAPI, Severity: engine.SevBlocker,
						Title: huge, Remediation: huge,
					})
				}
				for j := 0; j < 100; j++ {
					r.NotAssessed = append(r.NotAssessed, engine.CapabilityGap{
						Capability: inventory.Capability(fmt.Sprintf("capability-%d", j)), Reason: huge, Partial: true,
						Skipped: []string{huge, huge},
					})
				}
				reports = append(reports, r)
			}
			st := StatusFromReports(reports, "v1.29.0", "dev", time.Now())
			st.NotAssessed = append(st.NotAssessed, huge, huge)

			dyn := newDynFake(newCRObject(DefaultName))
			if err := WriteStatus(context.Background(), dyn, DefaultName, st); err != nil {
				t.Fatal(err)
			}
			obj, err := dyn.Resource(GVR()).Get(context.Background(), DefaultName, metav1.GetOptions{})
			if err != nil {
				t.Fatal(err)
			}
			var buf bytes.Buffer
			err = unstructured.UnstructuredJSONScheme.Encode(obj, &buf)
			raw := buf.Bytes()
			if err != nil {
				t.Fatal(err)
			}
			if len(raw) >= 1<<20 {
				t.Errorf("worst-case status is %d bytes on the wire, want under 1 MiB (%d)", len(raw), 1<<20)
			}
			t.Logf("%s: %d bytes", tc.name, len(raw))
			got := readStatus(t, dyn, DefaultName)
			if len(got.Targets) != MaxTargets || len(got.Targets[0].TopFindings) != maxTopFindings {
				t.Errorf("bounding dropped content it should keep: %d targets, %d top findings", len(got.Targets), len(got.Targets[0].TopFindings))
			}
			if len(got.NotAssessed) > maxNotAssessed+1 {
				t.Errorf("notAssessed has %d entries, want at most %d (+1 summary line)", len(got.NotAssessed), maxNotAssessed)
			}
		})
	}
}

// Clipping notAssessed keeps the head and says how many entries it left out.
func TestBoundNotAssessedSummarizesTheRest(t *testing.T) {
	var notes []string
	for i := 0; i < maxNotAssessed+7; i++ {
		notes = append(notes, fmt.Sprintf("note-%d", i))
	}
	got := boundNotAssessed(notes)
	if len(got) != maxNotAssessed+1 || got[0] != "note-0" || got[maxNotAssessed-1] != fmt.Sprintf("note-%d", maxNotAssessed-1) {
		t.Fatalf("kept %d entries (first %q), want the first %d plus a summary", len(got), got[0], maxNotAssessed)
	}
	if last := got[len(got)-1]; !strings.Contains(last, "7 more") {
		t.Errorf("summary = %q, want it to count the 7 omitted entries", last)
	}
	if got := boundNotAssessed([]string{"a", "b"}); !reflect.DeepEqual(got, []string{"a", "b"}) {
		t.Errorf("under the cap = %v, want it unchanged", got)
	}
}

// jsonCost must never undercount: the bound on the stored size rests on it.
func TestJSONCostNeverUndercounts(t *testing.T) {
	for r := rune(0); r <= utf8.MaxRune; r++ {
		if r >= 0xD800 && r < 0xE000 { // surrogates are not runes
			continue
		}
		if got, want := jsonCost(r), encodedLen(t, string(r)); got < want {
			t.Fatalf("jsonCost(%U) = %d, but it encodes to %d bytes", r, got, want)
		}
	}
	if got, want := jsonCost(utf8.RuneError), encodedLen(t, "\xff"); got < want {
		t.Errorf("jsonCost for a byte that is not UTF-8 = %d, but it encodes to %d bytes", got, want)
	}
}
