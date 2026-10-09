package cli

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"

	"github.com/abd-ulbasit/upgradescope/internal/engine"
)

// SE-19: what the gate trusts. On a pull_request the config, the
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
		// A paragraph pasted twice reads as one long line and passes the
		// workflow comparison below, so no sentence of one may repeat.
		seen := map[string]bool{}
		for _, sentence := range strings.SplitAfter(squash(src), ". ") {
			if len(sentence) < 60 {
				continue
			}
			if seen[sentence] {
				t.Errorf("%s: this sentence appears twice: %.80q", file, sentence)
			}
			seen[sentence] = true
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
	steps := wf.Jobs["upgrade-gate"].Steps
	var baseCheckout, gate bool
	var cp string
	cpAt, gateAt, baseAt, clearAt := -1, -1, -1, -1
	for i, s := range steps {
		switch {
		case strings.HasPrefix(s.Uses, "actions/checkout@") && s.With["ref"] == "${{ github.event.pull_request.base.sha }}":
			baseCheckout = s.With["path"] == "trusted"
			baseAt = i
		case strings.Contains(s.Run, "cp trusted/"):
			cp, cpAt = s.Run, i
		case strings.HasPrefix(s.Uses, "abd-ulbasit/upgradescope@"):
			gateAt = i
		case strings.HasPrefix(s.Run, "rm -rf -- trusted"):
			clearAt = i
		}
	}
	// A pull request can commit .upgradescope.yaml as a symlink to a file that
	// a later step writes (helm template's output, say). The copy must be the
	// step right before the gate, so nothing overwrites it, and must delete
	// the destination first, so cp does not write through the symlink.
	if cpAt < 0 || gateAt < 0 || cpAt != gateAt-1 {
		t.Errorf("the copy (step %d) must be the step right before the gate (step %d)", cpAt, gateAt)
	}
	// A pull request can also commit a "trusted" directory or symlink where
	// the base commit is checked out.
	if baseAt < 0 || clearAt < 0 || clearAt > baseAt {
		t.Errorf("the path the base commit goes to must be cleared (step %d) before it is checked out (step %d)", clearAt, baseAt)
	}
	var lines []string
	for _, l := range strings.Split(cp, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			lines = append(lines, l)
		}
	}
	if len(lines) != 2 || lines[0] != "rm -f -- .upgradescope.yaml upgradescope-baseline.json" ||
		lines[1] != "cp trusted/.upgradescope.yaml trusted/upgradescope-baseline.json ." {
		t.Errorf("the copy step must remove the destinations, then copy: %q", lines)
	}
	if cp != "" {
		runCopyAgainstSymlinks(t, cp)
	}
	for _, s := range steps {
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

// Runs the documented copy step in a workspace where the pull request
// committed both files as symlinks into a directory that a later step
// writes. The trusted content must end up in a regular file, and a later
// write to the symlink's old target must not reach it.
func runCopyAgainstSymlinks(t *testing.T, script string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the step is a POSIX shell script")
	}
	ws := t.TempDir()
	mk := func(name, content string) {
		p := filepath.Join(ws, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	mk("trusted/.upgradescope.yaml", "ignore: []\n")
	mk("trusted/upgradescope-baseline.json", "{}\n")
	mk("rendered/chart/cfg.yaml", "placeholder\n")
	mk("rendered/chart/base.json", "placeholder\n")
	for link, target := range map[string]string{
		".upgradescope.yaml":         "rendered/chart/cfg.yaml",
		"upgradescope-baseline.json": "rendered/chart/base.json",
	} {
		if err := os.Symlink(target, filepath.Join(ws, link)); err != nil {
			t.Skipf("no symlinks here: %v", err)
		}
	}
	cmd := exec.Command("sh", "-ec", script)
	cmd.Dir = ws
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("the copy step failed: %v\n%s", err, out)
	}
	// The step that ran before the gate in the attack: helm template rewrites
	// the placeholders with the pull request's own rules.
	mk("rendered/chart/cfg.yaml", "ignore: [{category: removed-api, reason: x}]\n")
	mk("rendered/chart/base.json", "forged\n")
	for name, want := range map[string]string{".upgradescope.yaml": "ignore: []\n", "upgradescope-baseline.json": "{}\n"} {
		got, err := os.ReadFile(filepath.Join(ws, name))
		if err != nil || string(got) != want {
			t.Errorf("%s = %q (%v), want the base commit's %q", name, got, err, want)
		}
		if fi, err := os.Lstat(filepath.Join(ws, name)); err != nil || fi.Mode()&os.ModeSymlink != 0 {
			t.Errorf("%s is still a symlink after the copy", name)
		}
	}
}
