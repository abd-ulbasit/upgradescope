package cli

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// execScanFiles runs the real scan pipeline (no stub) at --target 1.36 and
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
	} {
		if got := manifestBase(in); got != want {
			t.Errorf("manifestBase(%q) = %q, want %q", in, got, want)
		}
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
