package cli

import (
	"encoding/json"
	"errors"
	"regexp"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"

	"github.com/abd-ulbasit/upgradescope/internal/engine"
)

// SE-18: what the gate trusts. On a pull_request the config, the
// annotations and the baseline all come from the pull request's tree, and
// the docs say so (TestDocsSayWhoCanTurnTheGateOff).

const ingressV1beta1 = `apiVersion: networking.k8s.io/v1beta1
kind: Ingress
metadata:
  name: web
`

const ingressAnnotated = ingressV1beta1 + `  annotations:
    upgradescope.dev/ignore: removed-api
    upgradescope.dev/ignore-reason: accepted in this pull request
`

const ignoreRemovedAPIs = "ignore:\n  - category: removed-api\n    reason: x\n"

func TestGateTrustsTheFilesOfThePullRequest(t *testing.T) {
	t.Run("a manifest with a removed API fails the gate", func(t *testing.T) {
		dir := writeFiles(t, map[string]string{"a.yaml": ingressV1beta1})
		_, _, err := execScanFiles(t, "--files", dir, "--target", "1.37")
		if !errors.Is(err, ErrGateFailed) {
			t.Fatalf("err = %v, want the gate to fail", err)
		}
	})
	t.Run("a .upgradescope.yaml the pull request adds suppresses it", func(t *testing.T) {
		dir := writeFiles(t, map[string]string{"a.yaml": ingressV1beta1, ".upgradescope.yaml": ignoreRemovedAPIs})
		out, _, err := execScanFiles(t, "--files", dir, "--target", "1.37")
		if err != nil || !strings.Contains(out, "READY  yes") || !strings.Contains(out, "1 suppressed") {
			t.Fatalf("err = %v, want the gate to pass with 1 suppressed:\n%s", err, out)
		}
	})
	t.Run("an annotation the pull request adds suppresses it", func(t *testing.T) {
		dir := writeFiles(t, map[string]string{"a.yaml": ingressAnnotated})
		out, _, err := execScanFiles(t, "--files", dir, "--target", "1.37")
		if err != nil || !strings.Contains(out, "reason: accepted in this pull request (annotation)") {
			t.Fatalf("err = %v, want the gate to pass and list the reason:\n%s", err, out)
		}
	})
	t.Run("a baseline the pull request commits holds its findings", func(t *testing.T) {
		dir := writeFiles(t, map[string]string{"a.yaml": ingressV1beta1})
		base := t.TempDir() + "/baseline.json"
		if _, _, err := execScanFiles(t, "--files", dir, "--target", "1.37", "--fail-on", "never", "--write-baseline", base); err != nil {
			t.Fatal(err)
		}
		out, _, err := execScanFiles(t, "--files", dir, "--target", "1.37", "--baseline", base)
		if err != nil || !strings.Contains(out, "BASELINE  1 unchanged, 0 new") {
			t.Fatalf("err = %v, want the gate to pass with the finding unchanged:\n%s", err, out)
		}
	})
}

// A config that --config (the Action's config) names is the only one read:
// a .upgradescope.yaml in the scan root, which the pull request could add,
// is not looked for. Annotations are not config, so they still apply, and
// their suppressions carry the source the docs' guard filters on.
func TestGateNamedConfigStopsDiscoveryButNotAnnotations(t *testing.T) {
	trusted := writeConfig(t, "ignore: []\n")
	dir := writeFiles(t, map[string]string{"a.yaml": ingressV1beta1, ".upgradescope.yaml": ignoreRemovedAPIs})
	if _, _, err := execScanFiles(t, "--files", dir, "--target", "1.37", "--config", trusted); !errors.Is(err, ErrGateFailed) {
		t.Fatalf("err = %v, want the gate to fail: the pull request's config must not be read", err)
	}

	dir = writeFiles(t, map[string]string{"a.yaml": ingressAnnotated})
	out, _, err := execScanFiles(t, "--files", dir, "--target", "1.37", "--config", trusted, "--output", "json")
	if err != nil {
		t.Fatalf("err = %v, want the annotation to suppress", err)
	}
	var rep engine.Report
	if err := json.Unmarshal([]byte(out), &rep); err != nil {
		t.Fatal(err)
	}
	if len(rep.Suppressed) != 1 || rep.Suppressed[0].Source != "annotation" {
		t.Fatalf("suppressed = %+v, want one with source \"annotation\"", rep.Suppressed)
	}
}

const heading = "Who can turn the gate off"

// yamlAfterHeading returns the first ```yaml block after the heading.
func yamlAfterHeading(t *testing.T, file string) string {
	t.Helper()
	src := readDoc(t, file)
	_, after, ok := strings.Cut(src, "## "+heading+"\n")
	if !ok {
		_, after, ok = strings.Cut(src, "### "+heading+"\n")
	}
	if !ok {
		t.Fatalf("%s has no %q section", file, heading)
	}
	m := regexp.MustCompile("(?s)```yaml\n(.*?)```").FindStringSubmatch(after)
	if m == nil {
		t.Fatalf("%s: no workflow after %q", file, heading)
	}
	return m[1]
}

// The three pages state the boundary and show the same workflow, which
// reads the config and the baseline from the base commit and hands them to
// inputs the action has.
func TestDocsSayWhoCanTurnTheGateOff(t *testing.T) {
	files := []string{"docs/getting-started/ci-gate.md", "docs/guides/suppressions-and-baselines.md", "action/README.md"}
	var first string
	for _, file := range files {
		src := readDoc(t, file)
		if !strings.Contains(squash(src), "A pull request can suppress its own findings") &&
			!strings.Contains(squash(src), "a pull request can suppress its own findings") {
			t.Errorf("%s does not say that a pull request can suppress its own findings", file)
		}
		block := yamlAfterHeading(t, file)
		if first == "" {
			first = block
		} else if block != first {
			t.Errorf("%s: the workflow differs from the first page's", file)
		}
	}

	type step struct {
		Uses string         `json:"uses"`
		Run  string         `json:"run"`
		With map[string]any `json:"with"`
	}
	var wf struct {
		Jobs map[string]struct {
			Steps []step `json:"steps"`
		} `json:"jobs"`
	}
	if err := yaml.Unmarshal([]byte(first), &wf); err != nil {
		t.Fatalf("the workflow does not parse: %v", err)
	}
	var action struct {
		Inputs map[string]any `json:"inputs"`
	}
	if err := yaml.Unmarshal([]byte(readDoc(t, "action.yml")), &action); err != nil {
		t.Fatal(err)
	}
	var baseCheckout, gate bool
	var cp string
	for _, s := range wf.Jobs["upgrade-gate"].Steps {
		switch {
		case strings.HasPrefix(s.Uses, "actions/checkout@") && s.With["ref"] == "${{ github.event.pull_request.base.sha }}":
			baseCheckout = s.With["path"] == "trusted"
		case strings.HasPrefix(s.Run, "cp trusted/"):
			cp = s.Run
		}
	}
	for _, s := range wf.Jobs["upgrade-gate"].Steps {
		if !strings.HasPrefix(s.Uses, "abd-ulbasit/upgradescope@") {
			continue
		}
		gate = true
		for _, in := range []string{"config", "baseline"} {
			if _, ok := action.Inputs[in]; !ok {
				t.Errorf("action.yml has no input %q", in)
			}
			// The file is copied over the workspace's own, so the config's
			// file globs keep meaning what they mean in the repository.
			v, _ := s.With[in].(string)
			if v == "" || strings.Contains(v, "trusted") || !strings.Contains(cp, "trusted/"+v) {
				t.Errorf("the action's %s = %q: want a workspace path that the copy %q provides from trusted/", in, v, cp)
			}
		}
	}
	if !baseCheckout || !gate || cp == "" {
		t.Errorf("workflow lacks the base checkout (%v), the gate step (%v) or the copy (%q)", baseCheckout, gate, cp)
	}
}

// The guard the docs show filters on the source that annotation
// suppressions carry in the JSON report.
func TestDocsAnnotationGuardMatchesTheReport(t *testing.T) {
	for _, file := range []string{"docs/getting-started/ci-gate.md", "docs/guides/suppressions-and-baselines.md", "action/README.md"} {
		if !strings.Contains(readDoc(t, file), `select(.source == "annotation")`) {
			t.Errorf("%s: no annotation guard", file)
		}
	}
}
