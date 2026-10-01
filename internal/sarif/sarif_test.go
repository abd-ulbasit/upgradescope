package sarif

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/abd-ulbasit/upgradescope/internal/engine"
	"github.com/abd-ulbasit/upgradescope/internal/inventory"
	"github.com/abd-ulbasit/upgradescope/internal/sarif/sariftest"
)

const deprecationGuide = "https://kubernetes.io/docs/reference/using-api/deprecation-guide/"

// testReport mixes file-anchored API findings with findings that have no
// file (add-on, skew, kb-stale, a live-cluster API finding).
func testReport() engine.Report {
	return engine.Report{
		ClusterID: "files",
		Target:    inventory.Version{Major: 1, Minor: 36},
		KBVersion: "test-kb",
		Score:     40,
		Findings: []engine.Finding{
			{
				Category: engine.CatEOLAddon, Severity: engine.SevBlocker,
				Key:   "eol-addon/ingress-nginx",
				Title: "ingress-nginx is past end of life", Detail: "EOL 2026-03-24",
				Remediation: "migrate to a supported ingress controller",
				Namespaces:  []string{"ingress-nginx"},
				Citations:   []string{"https://kubernetes.github.io/ingress-nginx/"},
			},
			{
				Category: engine.CatRemovedAPI, Severity: engine.SevBlocker,
				Key:            "removed-api/networking.k8s.io/v1beta1/Ingress",
				Title:          "networking.k8s.io/v1beta1 Ingress removed in 1.22 (10 objects)",
				Detail:         "10 manifest object(s) use this API: namespace unset (1), shop (9).",
				Remediation:    "migrate to networking.k8s.io/v1 Ingress",
				ObjectsOmitted: 7,
				Citations:      []string{deprecationGuide},
				Objects: []inventory.ObjectRef{
					{Name: "admin", File: "legacy/ingress v1.yaml", Line: 4},
					{Namespace: "shop", Name: "web", File: "rendered/all.yaml", Line: 3, RenderedFrom: "shop/templates/ingress.yaml"},
					{Namespace: "shop", Name: "posted", Line: 9}, // gate stream: no file
				},
			},
			{
				Category: engine.CatRemovedAPI, Severity: engine.SevBlocker,
				Key:   "removed-api/policy/v1beta1/PodSecurityPolicy",
				Title: "policy/v1beta1 PodSecurityPolicy removed in 1.25 (2 objects)",
				// live cluster: identity, no file
				Objects: []inventory.ObjectRef{{Name: "restricted"}},
			},
			{
				Category: engine.CatKBStale, Severity: engine.SevWarning,
				Key:   "kb-stale",
				Title: "knowledge base does not cover Kubernetes 1.37",
			},
			{
				Category: engine.CatDeprecatedAPI, Severity: engine.SevInfo,
				Key:     "deprecated-api/core/v1/ComponentStatus",
				Title:   "v1 ComponentStatus deprecated since 1.19 (1 object)",
				Objects: []inventory.ObjectRef{{Name: "cs", File: "cs.json", Line: 1}},
			},
		},
	}
}

func writeLog(t *testing.T, r engine.Report) (sarifLog, []byte) {
	t.Helper()
	var buf bytes.Buffer
	if err := Write(&buf, r, "v-test"); err != nil {
		t.Fatalf("Write: %v", err)
	}
	var log sarifLog
	if err := json.Unmarshal(buf.Bytes(), &log); err != nil {
		t.Fatalf("output is not valid JSON: %v\n%s", err, buf.String())
	}
	return log, buf.Bytes()
}

func TestWriteGitHubAcceptable(t *testing.T) {
	_, raw := writeLog(t, testReport())
	sariftest.AssertGitHubAcceptable(t, raw)
}

func TestWrite(t *testing.T) {
	log, _ := writeLog(t, testReport())
	if log.Version != "2.1.0" || len(log.Runs) != 1 {
		t.Fatalf("envelope = %+v", log)
	}
	run := log.Runs[0]
	d := run.Tool.Driver
	if d.Name != "upgradescope" || d.Version != "v-test" || d.InformationURI != "https://github.com/abd-ulbasit/upgradescope" {
		t.Errorf("driver = %+v", d)
	}

	// One result per (finding, file location); findings without any file
	// location (add-on, kb-stale, live PSP) are omitted, as is the
	// posted-stream ref with no file.
	type got struct{ rule, level, uri string }
	var results []got
	for _, res := range run.Results {
		pl := res.Locations[0].PhysicalLocation
		results = append(results, got{res.RuleID, res.Level, pl.ArtifactLocation.URI})
	}
	want := []got{
		{"removed-api/networking.k8s.io/v1beta1/Ingress", "error", "legacy/ingress%20v1.yaml"},
		{"removed-api/networking.k8s.io/v1beta1/Ingress", "error", "rendered/all.yaml"},
		{"deprecated-api/core/v1/ComponentStatus", "note", "cs.json"},
	}
	if len(results) != len(want) {
		t.Fatalf("results = %+v, want %+v", results, want)
	}
	for i := range want {
		if results[i] != want[i] {
			t.Errorf("results[%d] = %+v, want %+v", i, results[i], want[i])
		}
	}
	if line := run.Results[1].Locations[0].PhysicalLocation.Region.StartLine; line != 3 {
		t.Errorf("startLine = %d, want 3", line)
	}

	// Only rules that have results are listed.
	if len(d.Rules) != 2 {
		t.Fatalf("rules = %+v, want 2", d.Rules)
	}
	ing := d.Rules[0]
	if ing.ShortDescription.Text != "Removed Kubernetes API: networking.k8s.io/v1beta1/Ingress" {
		t.Errorf("shortDescription = %q", ing.ShortDescription.Text)
	}
	if ing.HelpURI != deprecationGuide {
		t.Errorf("helpUri = %q, want the first citation", ing.HelpURI)
	}
	if !strings.Contains(ing.Help.Text, "migrate to networking.k8s.io/v1 Ingress") ||
		!strings.Contains(ing.Help.Markdown, "migrate to networking.k8s.io/v1 Ingress") {
		t.Errorf("help = %+v, want the remediation", ing.Help)
	}
	if ing.DefaultConfiguration.Level != "error" {
		t.Errorf("defaultConfiguration.level = %q", ing.DefaultConfiguration.Level)
	}
	if cs := d.Rules[1]; !strings.HasPrefix(cs.HelpURI, "https://github.com/abd-ulbasit/upgradescope") {
		t.Errorf("helpUri without citations = %q, want the docs link", cs.HelpURI)
	}

	// The message names the object and where it was rendered from.
	msg := run.Results[1].Message.Text
	for _, part := range []string{"networking.k8s.io/v1beta1 Ingress removed in 1.22", "shop/web", "shop/templates/ingress.yaml", "migrate to networking.k8s.io/v1 Ingress"} {
		if !strings.Contains(msg, part) {
			t.Errorf("message %q lacks %q", msg, part)
		}
	}

	// Fingerprints are per (finding, object) and stable across line moves.
	fp := func(r sarifResult) string { return r.PartialFingerprints[fingerprintKey] }
	if fp(run.Results[0]) == fp(run.Results[1]) {
		t.Error("distinct objects share a fingerprint")
	}
	moved := testReport()
	moved.Findings[1].Objects[1].Line = 40
	movedLog, _ := writeLog(t, moved)
	if fp(movedLog.Runs[0].Results[1]) != fp(run.Results[1]) {
		t.Error("fingerprint changed when only the line moved")
	}
}

// Objects with the same identity in the same file (unnamed, or one name
// reused by two documents) still get distinct fingerprints, in file
// order; the first keeps the plain identity fingerprint.
func TestWriteFingerprintsDistinctForSameIdentity(t *testing.T) {
	r := testReport()
	r.Findings = []engine.Finding{r.Findings[4]}
	r.Findings[0].Objects = []inventory.ObjectRef{
		{File: "a.yaml", Line: 1}, {File: "a.yaml", Line: 9}, {File: "a.yaml", Line: 20},
	}
	log, _ := writeLog(t, r)
	seen := map[string]bool{}
	for i, res := range log.Runs[0].Results {
		fp := res.PartialFingerprints[fingerprintKey]
		if seen[fp] {
			t.Errorf("results[%d] repeats fingerprint %s", i, fp)
		}
		seen[fp] = true
	}
	if got := log.Runs[0].Results[0].PartialFingerprints[fingerprintKey]; got != fingerprint(r.Findings[0].Key, r.Findings[0].Objects[0], 0) {
		t.Errorf("first occurrence fingerprint = %s, want the plain identity fingerprint", got)
	}
}

func TestUnanchored(t *testing.T) {
	if got := Unanchored(testReport()); got != 3 { // eol-addon, live PSP, kb-stale
		t.Errorf("Unanchored = %d, want 3", got)
	}
}

// A report with nothing anchorable is still a valid, empty SARIF log.
func TestWriteNoAnchoredFindings(t *testing.T) {
	r := testReport()
	r.Findings = r.Findings[:1]
	log, raw := writeLog(t, r)
	sariftest.AssertGitHubAcceptable(t, raw)
	if len(log.Runs[0].Results) != 0 || len(log.Runs[0].Tool.Driver.Rules) != 0 {
		t.Errorf("run = %+v, want no rules or results", log.Runs[0])
	}
	if !bytes.Contains(raw, []byte(`"results": []`)) || !bytes.Contains(raw, []byte(`"rules": []`)) {
		t.Errorf("results/rules must render as empty arrays:\n%s", raw)
	}
	if n := log.Runs[0].Invocations; len(n) != 1 || len(n[0].ToolExecutionNotifications) != 1 || n[0].ToolExecutionNotifications[0].Level != "error" {
		t.Errorf("invocations = %+v, want the omitted blocker as an error notification", n)
	}
}

// Findings without a file location cannot be results (GitHub rejects the
// upload), but the document itself must say they exist: a /gate response
// whose blockers are all unanchored would otherwise read as a clean pass.
// Each one becomes a tool execution notification at its severity's level,
// objects beyond the results (capped refs, refs without a file) are
// counted in a note, and run.properties carries the verdict.
func TestWriteReportsOmittedFindings(t *testing.T) {
	log, raw := writeLog(t, testReport())
	sariftest.AssertGitHubAcceptable(t, raw)
	run := log.Runs[0]
	if p := run.Properties; p == nil || p.Ready || p.Score != 40 || p.Findings != 5 || p.OmittedFindings != 3 {
		t.Errorf("run.properties = %+v, want ready=false score=40 findings=5 omittedFindings=3", p)
	}
	if len(run.Invocations) != 1 || !run.Invocations[0].ExecutionSuccessful {
		t.Fatalf("invocations = %+v, want one successful invocation", run.Invocations)
	}
	notes := run.Invocations[0].ToolExecutionNotifications
	want := []struct {
		level, key string
		parts      []string
	}{
		{"error", "eol-addon/ingress-nginx", []string{"no file location", "ingress-nginx is past end of life", "EOL 2026-03-24", "Fix: migrate to a supported ingress controller."}},
		{"note", "removed-api/networking.k8s.io/v1beta1/Ingress", []string{"networking.k8s.io/v1beta1 Ingress removed in 1.22", "8 more affected object(s)", "shop/posted (line 9)"}},
		{"error", "removed-api/policy/v1beta1/PodSecurityPolicy", []string{"policy/v1beta1 PodSecurityPolicy removed in 1.25", "Objects: restricted."}},
		{"warning", "kb-stale", []string{"knowledge base does not cover Kubernetes 1.37"}},
	}
	if len(notes) != len(want) {
		t.Fatalf("notifications = %+v, want %d", notes, len(want))
	}
	for i, w := range want {
		n := notes[i]
		if n.Level != w.level || n.Properties["findingKey"] != w.key {
			t.Errorf("notification %d = %+v, want level %s for %s", i, n, w.level, w.key)
		}
		for _, part := range w.parts {
			if !strings.Contains(n.Message.Text, part) {
				t.Errorf("notification %d message %q lacks %q", i, n.Message.Text, part)
			}
		}
	}
}

// A clean report still records the verdict and a successful invocation.
func TestWriteCleanReport(t *testing.T) {
	log, raw := writeLog(t, engine.Report{Score: 100, Ready: true})
	sariftest.AssertGitHubAcceptable(t, raw)
	run := log.Runs[0]
	if p := run.Properties; p == nil || !p.Ready || p.Score != 100 || p.OmittedFindings != 0 {
		t.Errorf("run.properties = %+v, want ready, score 100", p)
	}
	if len(run.Invocations) != 1 || !run.Invocations[0].ExecutionSuccessful || len(run.Invocations[0].ToolExecutionNotifications) != 0 {
		t.Errorf("invocations = %+v, want one successful invocation without notifications", run.Invocations)
	}
}
