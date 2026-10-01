package sarif

import (
	"bytes"
	"encoding/json"
	"net/url"
	"strings"
	"testing"

	"github.com/abd-ulbasit/upgradescope/internal/engine"
	"github.com/abd-ulbasit/upgradescope/internal/inventory"
)

const deprecationGuide = "https://kubernetes.io/docs/reference/using-api/deprecation-guide/"

// testReport mixes file-anchored API findings with findings that have no
// file (add-on, skew, kb-stale, a live-cluster API finding).
func testReport() engine.Report {
	return engine.Report{
		ClusterID: "files",
		Target:    inventory.Version{Major: 1, Minor: 36},
		KBVersion: "test-kb",
		Findings: []engine.Finding{
			{
				Category: engine.CatEOLAddon, Severity: engine.SevBlocker,
				Key:   "eol-addon/ingress-nginx",
				Title: "ingress-nginx is past end of life", Detail: "EOL 2026-03-24",
				Namespaces: []string{"ingress-nginx"},
				Citations:  []string{"https://kubernetes.github.io/ingress-nginx/"},
			},
			{
				Category: engine.CatRemovedAPI, Severity: engine.SevBlocker,
				Key:         "removed-api/networking.k8s.io/v1beta1/Ingress",
				Title:       "networking.k8s.io/v1beta1 Ingress removed in 1.22 (3 objects)",
				Detail:      "3 manifest object(s) use this API: namespace unset (1), shop (2).",
				Remediation: "migrate to networking.k8s.io/v1 Ingress",
				Citations:   []string{deprecationGuide},
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

// assertGitHubAcceptable checks every property GitHub code scanning marks
// as required, and the limits it enforces, per
// https://docs.github.com/en/code-security/code-scanning/integrating-with-code-scanning/sarif-support-for-code-scanning
// (the official SARIF 2.1.0 JSON schema is not vendored: its OASIS IPR
// terms are not a clear redistribution license).
func assertGitHubAcceptable(t *testing.T, raw []byte) {
	t.Helper()
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	if doc["$schema"] == "" || doc["$schema"] == nil {
		t.Error("$schema missing")
	}
	if doc["version"] != "2.1.0" {
		t.Errorf("version = %v, want 2.1.0", doc["version"])
	}
	var log sarifLog
	if err := json.Unmarshal(raw, &log); err != nil {
		t.Fatal(err)
	}
	if n := len(log.Runs); n < 1 || n > 20 {
		t.Fatalf("runs = %d, want 1..20", n)
	}
	for _, run := range log.Runs {
		d := run.Tool.Driver
		if d.Name == "" || d.Rules == nil {
			t.Errorf("tool.driver name/rules missing: %+v", d)
		}
		if len(d.Rules) > 25000 || len(run.Results) > 25000 {
			t.Errorf("rules=%d results=%d exceed GitHub limits", len(d.Rules), len(run.Results))
		}
		ids := map[string]bool{}
		for _, rule := range d.Rules {
			if rule.ID == "" || ids[rule.ID] {
				t.Errorf("rule id %q empty or duplicated", rule.ID)
			}
			ids[rule.ID] = true
			if rule.ShortDescription.Text == "" || rule.ShortDescription.Text == rule.ID || len(rule.ShortDescription.Text) > 1024 {
				t.Errorf("rule %s shortDescription %q: want human text ≤1024 chars, not the id", rule.ID, rule.ShortDescription.Text)
			}
			if rule.FullDescription.Text == "" || len(rule.FullDescription.Text) > 1024 {
				t.Errorf("rule %s fullDescription %q: want text ≤1024 chars", rule.ID, rule.FullDescription.Text)
			}
			if rule.Help.Text == "" || rule.Help.Markdown == "" {
				t.Errorf("rule %s help = %+v, want text and markdown", rule.ID, rule.Help)
			}
			if u, err := url.Parse(rule.HelpURI); err != nil || !u.IsAbs() {
				t.Errorf("rule %s helpUri %q: want an absolute URL", rule.ID, rule.HelpURI)
			}
			if n := len(rule.Properties.Tags); n == 0 || n > 20 {
				t.Errorf("rule %s tags = %v, want 1..20", rule.ID, rule.Properties.Tags)
			}
			switch rule.DefaultConfiguration.Level {
			case "note", "warning", "error":
			default:
				t.Errorf("rule %s defaultConfiguration.level = %q", rule.ID, rule.DefaultConfiguration.Level)
			}
		}
		for i, res := range run.Results {
			if !ids[res.RuleID] || d.Rules[res.RuleIndex].ID != res.RuleID {
				t.Errorf("results[%d] ruleId %q / ruleIndex %d do not match a rule", i, res.RuleID, res.RuleIndex)
			}
			if res.Message.Text == "" {
				t.Errorf("results[%d] message.text empty", i)
			}
			if len(res.PartialFingerprints) == 0 {
				t.Errorf("results[%d] partialFingerprints empty", i)
			}
			if n := len(res.Locations); n < 1 || n > 1000 {
				t.Fatalf("results[%d] has %d locations, GitHub requires 1..1000", i, n)
			}
			for _, loc := range res.Locations {
				pl := loc.PhysicalLocation
				if pl == nil {
					t.Fatalf("results[%d]: location without physicalLocation", i)
				}
				u, err := url.Parse(pl.ArtifactLocation.URI)
				if err != nil || pl.ArtifactLocation.URI == "" || u.IsAbs() || strings.HasPrefix(u.Path, "/") {
					t.Errorf("results[%d] uri %q: want a repository-relative URI reference", i, pl.ArtifactLocation.URI)
				}
				if pl.Region == nil || pl.Region.StartLine < 1 {
					t.Errorf("results[%d] region %+v: want startLine ≥ 1", i, pl.Region)
				}
			}
		}
	}
}

func TestWriteGitHubAcceptable(t *testing.T) {
	_, raw := writeLog(t, testReport())
	assertGitHubAcceptable(t, raw)
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
	assertGitHubAcceptable(t, raw)
	if len(log.Runs[0].Results) != 0 || len(log.Runs[0].Tool.Driver.Rules) != 0 {
		t.Errorf("run = %+v, want no rules or results", log.Runs[0])
	}
	if !bytes.Contains(raw, []byte(`"results": []`)) || !bytes.Contains(raw, []byte(`"rules": []`)) {
		t.Errorf("results/rules must render as empty arrays:\n%s", raw)
	}
}
