package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/abd-ulbasit/upgradescope/internal/engine"
	"github.com/abd-ulbasit/upgradescope/internal/kb"
	"github.com/abd-ulbasit/upgradescope/internal/sarif/sariftest"
)

// execScanFiles runs the real scan pipeline (no stub) at --target 1.36 (a
// later --target wins) and
// returns stdout, stderr and the error.
func execScanFiles(t *testing.T, args ...string) (string, string, error) {
	t.Helper()
	cmd := newScanCmd()
	var out, errOut bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	cmd.SetArgs(append([]string{"--target", "1.36"}, args...))
	err := cmd.Execute()
	return out.String(), errOut.String(), err
}

// writeFiles creates files (slash paths → content) under a temp dir.
func writeFiles(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, content := range files {
		p := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// removedAPIs holds two APIs removed long before 1.36, without
// metadata.namespace (typical helm template output).
const removedAPIs = `apiVersion: batch/v1beta1
kind: CronJob
metadata:
  name: nightly
---
apiVersion: networking.k8s.io/v1beta1
kind: Ingress
metadata:
  name: web
`

// Zero Kubernetes objects is an operational error (exit 1), never a
// 100/100 pass: an empty render or a wrong path must not go green.
func TestScanFilesNoManifestsIsExitOne(t *testing.T) {
	for name, files := range map[string]map[string]string{
		"empty dir":         {},
		"no manifest files": {"notes.txt": "hi", "rendered": removedAPIs},
		"only non-manifest": {"values.yaml": "replicaCount: 1\n", "chart/templates/cm.yaml": "{{- if .Values.x }}\napiVersion: v1\n"},
	} {
		t.Run(name, func(t *testing.T) {
			dir := writeFiles(t, files)
			_, _, err := execScanFiles(t, "--files", dir)
			want := fmt.Sprintf("no Kubernetes manifests found under %s (%d files skipped)", dir, len(files))
			if err == nil || err.Error() != want {
				t.Fatalf("err = %v, want %q", err, want)
			}
			if ExitCode(err) != 1 {
				t.Fatalf("ExitCode = %d, want 1", ExitCode(err))
			}
		})
	}
}

// A file that does not parse (an unrendered chart template next to the
// render) is a warning on stderr, not a failed scan; the rendered
// manifests are still gated.
func TestScanFilesWarnsOnInvalidFiles(t *testing.T) {
	dir := writeFiles(t, map[string]string{
		"rendered.yaml":           removedAPIs,
		"chart/templates/cm.yaml": "{{- if .Values.x }}\napiVersion: v1\nkind: ConfigMap\n",
	})
	_, stderr, err := execScanFiles(t, "--files", dir, "--output", "json")
	if ExitCode(err) != 2 {
		t.Fatalf("ExitCode = %d (err %v), want 2: the removed APIs must still gate", ExitCode(err), err)
	}
	wantPrefix := "warning: skipped " + filepath.ToSlash(filepath.Join(dir, "chart/templates/cm.yaml")) + ":1: "
	if !strings.HasPrefix(stderr, wantPrefix) {
		t.Errorf("stderr = %q, want prefix %q", stderr, wantPrefix)
	}
}

// JSON keeps object paths relative to the scanned root and records that
// root (relative to the working directory) as filesBase, so a consumer can
// resolve them without knowing the --files argument.
func TestScanFilesJSONRecordsBase(t *testing.T) {
	dir := writeFiles(t, map[string]string{"rendered/all.yaml": removedAPIs})
	t.Chdir(dir)
	for args, wantBase := range map[string]string{"./rendered/": "rendered", "rendered/all.yaml": "rendered", ".": ""} {
		out, _, err := execScanFiles(t, "--files", args, "--output", "json")
		if ExitCode(err) != 2 {
			t.Fatalf("--files %s: ExitCode = %d (err %v), want 2", args, ExitCode(err), err)
		}
		var rep struct {
			FilesBase *string `json:"filesBase"`
			Findings  []struct {
				Objects []struct{ File string }
			}
		}
		if err := json.Unmarshal([]byte(out), &rep); err != nil {
			t.Fatal(err)
		}
		if rep.FilesBase == nil || *rep.FilesBase != wantBase {
			t.Errorf("--files %s: filesBase = %v, want %q", args, rep.FilesBase, wantBase)
		}
		file := rep.Findings[0].Objects[0].File
		if got := path.Join(wantBase, file); got != "rendered/all.yaml" {
			t.Errorf("--files %s: filesBase + file = %q, want rendered/all.yaml", args, got)
		}
	}
	// A live scan has no filesBase.
	var buf bytes.Buffer
	if err := WriteJSON(&buf, engine.Report{}); err != nil || strings.Contains(buf.String(), "filesBase") {
		t.Errorf("WriteJSON = %s (err %v), want no filesBase outside files mode", buf.String(), err)
	}
}

// Object paths are re-anchored to the working directory (the repo root in
// CI) so SARIF URIs resolve; paths outside it stay absolute.
func TestManifestBase(t *testing.T) {
	dir := writeFiles(t, map[string]string{"rendered/a.yaml": "", "one.yaml": ""})
	t.Chdir(dir)
	outside := t.TempDir()
	for in, want := range map[string]string{
		"rendered":                        "rendered",
		"./rendered/":                     "rendered",
		"one.yaml":                        "",
		".":                               "",
		"rendered/a.yaml":                 "rendered",
		filepath.Join(dir, "rendered"):    "rendered",
		filepath.Join(dir, "one.yaml"):    "",
		outside:                           filepath.ToSlash(outside),
		filepath.Join(outside, "missing"): filepath.ToSlash(filepath.Join(outside, "missing")),
		// relative but outside the working directory: absolute, so SARIF
		// says file:// rather than a "../" path GitHub cannot place
		"../sibling":                filepath.ToSlash(filepath.Join(filepath.Dir(dir), "sibling")),
		"rendered/../../sibling/x/": filepath.ToSlash(filepath.Join(filepath.Dir(dir), "sibling", "x")),
	} {
		if got := manifestBase(in); got != want {
			t.Errorf("manifestBase(%q) = %q, want %q", in, got, want)
		}
	}
}

// The CI recipe: render next to the chart, scan the repo, upload SARIF.
// Every result must point at a repository-relative file and line; findings
// with no file (kb-stale at an uncovered target) are left out with a note.
func TestScanFilesSARIFLocations(t *testing.T) {
	dir := writeFiles(t, map[string]string{
		"rendered/all.yaml":       "---\n# Source: demo/templates/cron.yaml\n" + removedAPIs,
		"chart/templates/cm.yaml": "{{- if .Values.x }}\napiVersion: v1\n",
		"chart/values.yaml":       "replicaCount: 1\n",
	})
	t.Chdir(dir)
	out, stderr, err := execScanFiles(t, "--files", "rendered", "--output", "sarif", "--target", targetAboveKBHorizon(t))
	if ExitCode(err) != 2 {
		t.Fatalf("ExitCode = %d (err %v), want 2", ExitCode(err), err)
	}
	var log struct {
		Runs []struct {
			Results []struct {
				RuleID    string `json:"ruleId"`
				Message   struct{ Text string }
				Locations []struct {
					PhysicalLocation struct {
						ArtifactLocation struct{ URI string }
						Region           struct{ StartLine int }
					}
				}
			}
		}
	}
	if err := json.Unmarshal([]byte(out), &log); err != nil {
		t.Fatalf("not SARIF: %v\n%s", err, out)
	}
	type loc struct {
		rule, uri string
		line      int
	}
	var got []loc
	for _, r := range log.Runs[0].Results {
		pl := r.Locations[0].PhysicalLocation
		got = append(got, loc{r.RuleID, pl.ArtifactLocation.URI, pl.Region.StartLine})
	}
	want := []loc{
		{"removed-api/batch/v1beta1/CronJob", "rendered/all.yaml", 3},
		{"removed-api/networking.k8s.io/v1beta1/Ingress", "rendered/all.yaml", 8},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("results = %+v, want %+v", got, want)
	}
	if msg := log.Runs[0].Results[0].Message.Text; !strings.Contains(msg, "rendered from demo/templates/cron.yaml") {
		t.Errorf("message %q lacks the helm source", msg)
	}
	if !strings.Contains(stderr, "note: 1 finding(s) have no file location, so they are not SARIF results") ||
		!strings.Contains(stderr, "tool execution notifications") {
		t.Errorf("stderr = %q, want the omitted-findings note", stderr)
	}
	sariftest.AssertGitHubAcceptable(t, []byte(out))
	if !strings.Contains(out, `"omittedFindings": 1`) || !strings.Contains(out, "knowledge base") {
		t.Errorf("SARIF does not record the omitted kb-stale finding:\n%s", out)
	}
}

// Manifests outside the working directory cannot be placed in the
// repository: SARIF says where they are (file://) and stderr says why
// GitHub will not annotate them.
func TestScanFilesSARIFOutsideWorkingDir(t *testing.T) {
	dir := writeFiles(t, map[string]string{"rendered/all.yaml": removedAPIs, "repo/README": ""})
	t.Chdir(filepath.Join(dir, "repo"))
	out, stderr, err := execScanFiles(t, "--files", "../rendered", "--output", "sarif")
	if ExitCode(err) != 2 {
		t.Fatalf("ExitCode = %d (err %v), want 2", ExitCode(err), err)
	}
	if want := `"uri": "file://` + filepath.ToSlash(filepath.Join(dir, "rendered", "all.yaml")); !strings.Contains(out, want) {
		t.Errorf("SARIF lacks %s:\n%s", want, out)
	}
	if !strings.Contains(stderr, "outside the working directory") {
		t.Errorf("stderr = %q, want a note that GitHub cannot place the locations", stderr)
	}
}

// A file named explicitly is scanned whatever its extension.
func TestScanFilesExtensionlessFile(t *testing.T) {
	dir := writeFiles(t, map[string]string{"rendered": removedAPIs})
	_, _, err := execScanFiles(t, "--files", filepath.Join(dir, "rendered"))
	if ExitCode(err) != 2 {
		t.Fatalf("ExitCode = %d (err %v), want 2", ExitCode(err), err)
	}
}

// targetAboveKBHorizon is one minor past the embedded knowledge base, so the
// scan always yields a kb-stale finding (which has no file location) no
// matter how far the weekly KB refresh has moved the horizon.
func targetAboveKBHorizon(t *testing.T) string {
	t.Helper()
	k, err := kb.Load()
	if err != nil {
		t.Fatal(err)
	}
	return k.MaxKnownK8s.Next().String()
}

// The Action's step summary: object paths resolve from the working
// directory (like table and SARIF), and --fail-on still sets the exit code.
func TestScanFilesMarkdown(t *testing.T) {
	dir := writeFiles(t, map[string]string{"rendered/all.yaml": removedAPIs})
	t.Chdir(dir)
	out, _, err := execScanFiles(t, "--files", "rendered", "--output", "markdown")
	if ExitCode(err) != 2 {
		t.Fatalf("ExitCode = %d (err %v), want 2", ExitCode(err), err)
	}
	for _, want := range []string{"### upgradescope: blocked\n", "`rendered/all.yaml:1` nightly", "`rendered/all.yaml:6` web"} {
		if !strings.Contains(out, want) {
			t.Errorf("markdown lacks %q:\n%s", want, out)
		}
	}
}
