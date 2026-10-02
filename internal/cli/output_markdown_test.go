package cli

import (
	"bytes"
	"fmt"
	"strings"
	"testing"

	"github.com/abd-ulbasit/upgradescope/internal/engine"
	"github.com/abd-ulbasit/upgradescope/internal/inventory"
	"github.com/abd-ulbasit/upgradescope/internal/suppress"
)

// The GitHub Action's step summary: a header (verdict, target, score,
// counts, KB), then one table row per finding with its objects (file:line,
// first few, then a count) and the fix.
func TestWriteMarkdownGolden(t *testing.T) {
	var many []inventory.ObjectRef
	for i := range 6 {
		many = append(many, inventory.ObjectRef{Name: fmt.Sprintf("web-%d", i), File: "rendered/ingress.yaml", Line: 1 + 7*i})
	}
	many[0].Namespace = "shop"
	r := engine.Report{
		ClusterID: "files",
		Target:    inventory.Version{Major: 1, Minor: 36},
		KBVersion: "test-kb",
		Score:     45,
		Verdict:   engine.VerdictBlocked,
		Findings: []engine.Finding{
			{
				Category: engine.CatRemovedAPI, Severity: engine.SevBlocker,
				Title:       "batch/v1beta1 CronJob removed in 1.25 (1 object)",
				Remediation: "migrate to batch/v1 CronJob",
				Objects:     []inventory.ObjectRef{{Name: "nightly", File: "rendered/all.yaml", Line: 3, RenderedFrom: "demo/templates/cronjob.yaml"}},
			},
			{
				Category: engine.CatRemovedAPI, Severity: engine.SevBlocker,
				Title:          "networking.k8s.io/v1beta1 Ingress removed in 1.22 (8 objects)",
				Remediation:    "migrate to networking.k8s.io/v1 Ingress",
				Objects:        many,
				ObjectsOmitted: 2,
			},
			{
				Category: engine.CatVersionSkew, Severity: engine.SevWarning,
				Title: "kubelet 3 minors behind apiserver",
			},
		},
		NotAssessed: []engine.CapabilityGap{{Capability: inventory.CapVersions, Reason: "files mode"}},
	}

	var buf bytes.Buffer
	WriteMarkdown(&buf, r)

	want := "### upgradescope: blocked\n" +
		"\n" +
		"Target **1.36** · score **45/100** · 2 blockers, 1 warning, 0 info · KB `test-kb`\n" +
		"\n" +
		"| Severity | Finding | Objects | Remediation |\n" +
		"|---|---|---|---|\n" +
		"| blocker | batch/v1beta1 CronJob removed in 1.25 (1 object) | `rendered/all.yaml:3` nightly (rendered from `demo/templates/cronjob.yaml`) | migrate to batch/v1 CronJob |\n" +
		"| blocker | networking.k8s.io/v1beta1 Ingress removed in 1.22 (8 objects) | `rendered/ingress.yaml:1` shop/web-0<br>`rendered/ingress.yaml:8` web-1<br>`rendered/ingress.yaml:15` web-2<br>`rendered/ingress.yaml:22` web-3<br>`rendered/ingress.yaml:29` web-4<br>…and 3 more | migrate to networking.k8s.io/v1 Ingress |\n" +
		"| warning | kubelet 3 minors behind apiserver |  |  |\n" +
		"\n" +
		"**Not assessed**\n" +
		"\n" +
		"- versions: files mode\n"
	if got := buf.String(); got != want {
		t.Errorf("markdown output mismatch\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

func TestWriteMarkdownNoFindings(t *testing.T) {
	r := engine.Report{
		Target:    inventory.Version{Major: 1, Minor: 36},
		KBVersion: "test-kb",
		Score:     100,
		Ready:     true,
		Verdict:   engine.VerdictReady,
	}
	var buf bytes.Buffer
	WriteMarkdown(&buf, r)
	want := "### upgradescope: ready\n" +
		"\n" +
		"Target **1.36** · score **100/100** · 0 blockers, 0 warnings, 0 info · KB `test-kb`\n" +
		"\n" +
		"No findings.\n"
	if got := buf.String(); got != want {
		t.Errorf("markdown output mismatch\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

// An unknown verdict names the required gaps that make it unknown.
func TestWriteMarkdownUnknownVerdictMarksRequiredGaps(t *testing.T) {
	r := engine.Report{
		Target:  inventory.Version{Major: 1, Minor: 38},
		Score:   100,
		Verdict: engine.VerdictUnknown,
		NotAssessed: []engine.CapabilityGap{
			{Capability: engine.GapKBCoverage, Reason: "target 1.38 is newer than the knowledge base (1.37)", Required: true},
			{Capability: inventory.CapVersions, Reason: "files mode"},
		},
	}
	var buf bytes.Buffer
	WriteMarkdown(&buf, r)
	out := buf.String()
	for _, want := range []string{
		"### upgradescope: unknown (required checks were not assessed)\n",
		"- kb-coverage (required): target 1.38 is newer than the knowledge base (1.37)\n",
		"- versions: files mode\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("markdown lacks %q:\n%s", want, out)
		}
	}
}

// Issue #122: partial gaps are marked partial and name what they skipped.
func TestWriteMarkdownPartialGaps(t *testing.T) {
	r := engine.Report{
		Target:  inventory.Version{Major: 1, Minor: 25},
		Score:   100,
		Verdict: engine.VerdictUnknown,
		NotAssessed: []engine.CapabilityGap{
			{Capability: inventory.CapAPIUsage, Reason: "list policy/v1beta1 podsecuritypolicies: forbidden", Partial: true, Required: true,
				Skipped: []string{"policy/v1beta1 PodSecurityPolicy"}},
		},
	}
	var buf bytes.Buffer
	WriteMarkdown(&buf, r)
	const want = "- api-usage (partial, required): list policy/v1beta1 podsecuritypolicies: forbidden. Skipped: policy/v1beta1 PodSecurityPolicy\n"
	if !strings.Contains(buf.String(), want) {
		t.Errorf("markdown lacks %q:\n%s", want, buf.String())
	}
}

// Manifest-controlled strings (object names, file paths, rendered-from
// templates) cannot break the table or inject markup into the summary.
func TestWriteMarkdownEscapes(t *testing.T) {
	r := engine.Report{
		Target:  inventory.Version{Major: 1, Minor: 36},
		Verdict: engine.VerdictBlocked,
		Findings: []engine.Finding{{
			Severity:    engine.SevBlocker,
			Title:       "a | b <img src=x> *bold* [link](http://x)",
			Remediation: "line one\nline two",
			Objects:     []inventory.ObjectRef{{Name: "n|x", File: "dir/we`ird|name.yaml", Line: 2}},
		}},
	}
	var buf bytes.Buffer
	WriteMarkdown(&buf, r)
	out := buf.String()
	row := ""
	for _, l := range strings.Split(out, "\n") {
		if strings.HasPrefix(l, "| blocker") {
			row = l
		}
	}
	want := `| blocker | a \| b \<img src=x\> \*bold\* \[link\](http://x) | ` +
		"``dir/we`ird\\|name.yaml:2`` n\\|x | line one line two |"
	if row != want {
		t.Errorf("row\n got: %s\nwant: %s", row, want)
	}
}

// A backslash before a pipe in a path keeps the pipe escaped and is not
// doubled: GitHub renders "`a\\|b`" in a cell as the code span a\|b.
func TestMdCodeBackslashBeforePipe(t *testing.T) {
	if got, want := mdCode(`a\|b.yaml:1`), "`a\\\\|b.yaml:1`"; got != want {
		t.Errorf("mdCode = %s, want %s", got, want)
	}
}

// Suppressed findings are not scored, so the verdict can be ready with
// removed APIs in the manifests: the summary says so and lists each one
// with the reason it was accepted and what accepted it.
func TestWriteMarkdownSuppressed(t *testing.T) {
	r := engine.Report{
		Target:    inventory.Version{Major: 1, Minor: 36},
		KBVersion: "test-kb",
		Score:     100,
		Ready:     true,
		Verdict:   engine.VerdictReady,
		Suppressed: []engine.SuppressedFinding{
			{
				Finding: engine.Finding{
					Category: engine.CatRemovedAPI, Severity: engine.SevBlocker,
					Title:   "networking.k8s.io/v1beta1 Ingress removed in 1.22 (1 object)",
					Objects: []inventory.ObjectRef{{Namespace: "shop", Name: "web", File: "rendered/all.yaml", Line: 2}},
				},
				Reason:  "shop's Ingress is replaced in PLAT-7",
				Source:  "ci/upgradescope.yaml",
				Expires: "2099-12-31",
			},
			{
				Finding: engine.Finding{
					Category: engine.CatRemovedAPI, Severity: engine.SevBlocker,
					Title:   "batch/v1beta1 CronJob removed in 1.25 (1 object)",
					Objects: []inventory.ObjectRef{{Name: "nightly", File: "rendered/all.yaml", Line: 12}},
				},
				Reason: "retired with the batch cluster",
				Source: suppress.AnnotationSource,
			},
		},
	}
	var buf bytes.Buffer
	WriteMarkdown(&buf, r)
	want := "### upgradescope: ready\n" +
		"\n" +
		"Target **1.36** · score **100/100** · 0 blockers, 0 warnings, 0 info · 2 suppressed · KB `test-kb`\n" +
		"\n" +
		"No findings left after suppression.\n" +
		"\n" +
		"**Suppressed (2).** Accepted by an ignore rule or annotation, so they are not scored and do not fail the gate.\n" +
		"\n" +
		"| Severity | Finding | Objects | Reason | Suppressed by |\n" +
		"|---|---|---|---|---|\n" +
		"| blocker | networking.k8s.io/v1beta1 Ingress removed in 1.22 (1 object) | `rendered/all.yaml:2` shop/web | shop's Ingress is replaced in PLAT-7 | `ci/upgradescope.yaml` until 2099-12-31 |\n" +
		"| blocker | batch/v1beta1 CronJob removed in 1.25 (1 object) | `rendered/all.yaml:12` nightly | retired with the batch cluster | `upgradescope.dev/ignore` annotation |\n"
	if got := buf.String(); got != want {
		t.Errorf("markdown output mismatch\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

// Against a baseline, each finding says whether it is new, and a line
// under the header says only new ones fail the gate: a blocked verdict
// whose blockers are all unchanged is a passing gate.
func TestWriteMarkdownBaseline(t *testing.T) {
	r := engine.Report{
		Target:    inventory.Version{Major: 1, Minor: 36},
		KBVersion: "test-kb",
		Score:     50,
		Verdict:   engine.VerdictBlocked,
		Findings: []engine.Finding{
			{
				Category: engine.CatRemovedAPI, Severity: engine.SevBlocker,
				Title:         "batch/v1beta1 CronJob removed in 1.25 (1 object)",
				Remediation:   "migrate to batch/v1 CronJob",
				Objects:       []inventory.ObjectRef{{Name: "nightly", File: "rendered/all.yaml", Line: 12}},
				BaselineState: engine.BaselineUnchanged,
			},
			{
				Category: engine.CatRemovedAPI, Severity: engine.SevBlocker,
				Title:         "networking.k8s.io/v1beta1 Ingress removed in 1.22 (1 object)",
				Remediation:   "migrate to networking.k8s.io/v1 Ingress",
				Objects:       []inventory.ObjectRef{{Namespace: "shop", Name: "web", File: "rendered/all.yaml", Line: 2}},
				BaselineState: engine.BaselineNew,
			},
		},
	}
	var buf bytes.Buffer
	WriteMarkdown(&buf, r)
	want := "### upgradescope: blocked\n" +
		"\n" +
		"Target **1.36** · score **50/100** · 2 blockers, 0 warnings, 0 info · KB `test-kb`\n" +
		"\n" +
		"**Baseline:** 1 new, 1 unchanged. Only new findings fail the gate; score and verdict count both.\n" +
		"\n" +
		"| Severity | Baseline | Finding | Objects | Remediation |\n" +
		"|---|---|---|---|---|\n" +
		"| blocker | unchanged | batch/v1beta1 CronJob removed in 1.25 (1 object) | `rendered/all.yaml:12` nightly | migrate to batch/v1 CronJob |\n" +
		"| blocker | **new** | networking.k8s.io/v1beta1 Ingress removed in 1.22 (1 object) | `rendered/all.yaml:2` shop/web | migrate to networking.k8s.io/v1 Ingress |\n"
	if got := buf.String(); got != want {
		t.Errorf("markdown output mismatch\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}

	// Every finding unchanged: the line explains the passing gate.
	for i := range r.Findings {
		r.Findings[i].BaselineState = engine.BaselineUnchanged
	}
	buf.Reset()
	WriteMarkdown(&buf, r)
	if line := "**Baseline:** 0 new, 2 unchanged."; !strings.Contains(buf.String(), line) {
		t.Errorf("markdown lacks %q:\n%s", line, buf.String())
	}
}

// Reasons and sources come from a repository's config file or a manifest's
// annotation: they are escaped like every other cell.
func TestWriteMarkdownSuppressedEscapes(t *testing.T) {
	r := engine.Report{
		Target:  inventory.Version{Major: 1, Minor: 36},
		Verdict: engine.VerdictReady,
		Suppressed: []engine.SuppressedFinding{{
			Finding: engine.Finding{Severity: engine.SevWarning, Title: "t"},
			Reason:  "see <script>x</script> | *now*\nsecond line",
			Source:  "dir/we`ird|.yaml",
			Expires: "2099-12-31",
		}},
	}
	var buf bytes.Buffer
	WriteMarkdown(&buf, r)
	want := `| warning | t |  | see \<script\>x\</script\> \| \*now\* second line | ` + "``dir/we`ird\\|.yaml`` until 2099-12-31 |\n"
	if !strings.HasSuffix(buf.String(), want) {
		t.Errorf("suppressed row\n got: %s\nwant suffix: %s", buf.String(), want)
	}
}

// #94: a live scan's summary says which cluster it read; the context
// name comes from a kubeconfig, so it is escaped like every other value.
func TestWriteMarkdownNamesTheCluster(t *testing.T) {
	r := engine.Report{
		Target:      inventory.Version{Major: 1, Minor: 36},
		KBVersion:   "test-kb",
		Score:       100,
		Ready:       true,
		Verdict:     engine.VerdictReady,
		KubeContext: "prod|eu",
		APIServer:   "https://10.0.0.1:6443",
	}
	var buf bytes.Buffer
	WriteMarkdown(&buf, r)
	want := "### upgradescope: ready\n" +
		"\n" +
		"Target **1.36** · score **100/100** · 0 blockers, 0 warnings, 0 info · KB `test-kb`\n" +
		"\n" +
		"Context `prod\\|eu` · API server `https://10.0.0.1:6443`\n" +
		"\n" +
		"No findings.\n"
	if got := buf.String(); got != want {
		t.Errorf("markdown output mismatch\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}
