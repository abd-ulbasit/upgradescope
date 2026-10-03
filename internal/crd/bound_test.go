package crd

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

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
// length; what lands in status is clipped, on a rune boundary.
func TestTargetStatusFromReportClipsLongText(t *testing.T) {
	long := strings.Repeat("é", 5000) // multi-byte: a byte cut would split a rune
	r := engine.Report{Target: inventory.Version{Major: 1, Minor: 36}, Findings: []engine.Finding{
		{Category: engine.CatDeprecatedAPI, Severity: engine.SevWarning, Title: long, Remediation: long},
	}}
	f := TargetStatusFromReport(r).TopFindings[0]
	if !utf8.ValidString(f.Title) || !utf8.ValidString(f.Remediation) {
		t.Error("clipped text is not valid UTF-8")
	}
	if n := utf8.RuneCountInString(f.Title); n != maxTitleLen+1 || !strings.HasSuffix(f.Title, "…") {
		t.Errorf("title = %d runes (ends in …: %v), want %d runes ending in …", n, strings.HasSuffix(f.Title, "…"), maxTitleLen+1)
	}
	if n := utf8.RuneCountInString(f.Remediation); n != maxRemediationLen+1 {
		t.Errorf("remediation = %d runes, want %d", n, maxRemediationLen+1)
	}
	short := TargetStatusFromReport(engine.Report{Findings: []engine.Finding{{Title: "short"}}}).TopFindings[0].Title
	if short != "short" {
		t.Errorf("short title = %q, want it untouched", short)
	}
}

// The status is bounded by construction: MaxTargets targets, each with
// maxTopFindings findings of the longest title and remediation, plus the
// longest and most numerous notAssessed notes, written through WriteStatus,
// stay far below the apiserver's 3 MiB request limit. The bound is asserted
// at 1 MiB, which also keeps etcd's 1.5 MiB object limit out of reach.
func TestWorstCaseStatusStaysUnder1MiB(t *testing.T) {
	huge := strings.Repeat("x", 100_000)
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
	raw, err := json.MarshalIndent(obj.Object, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) >= 1<<20 {
		t.Errorf("worst-case status is %d bytes, want under 1 MiB (%d)", len(raw), 1<<20)
	}
	got := readStatus(t, dyn, DefaultName)
	if len(got.Targets) != MaxTargets || len(got.Targets[0].TopFindings) != maxTopFindings {
		t.Errorf("bounding dropped content it should keep: %d targets, %d top findings", len(got.Targets), len(got.Targets[0].TopFindings))
	}
	if len(got.NotAssessed) > maxNotAssessed+1 {
		t.Errorf("notAssessed has %d entries, want at most %d (+1 summary line)", len(got.NotAssessed), maxNotAssessed)
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
