package cli

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/abd-ulbasit/upgradescope/internal/codequality/codequalitytest"
)

// The GitLab recipe: render next to the chart, scan from the repository
// root, publish artifacts:reports:codequality. Every entry GitLab can place
// is on a repository-relative file and line; a finding with no file
// (kb-stale at an uncovered target) is on the virtual upgradescope/ path.
func TestScanFilesGitLabCodeQuality(t *testing.T) {
	dir := writeFiles(t, map[string]string{"rendered/all.yaml": "---\n# Source: demo/templates/cron.yaml\n" + removedAPIs})
	t.Chdir(dir)
	out, stderr, err := execScanFiles(t, "--files", "rendered", "--output", "gitlab-codequality", "--target", targetAboveKBHorizon(t))
	if ExitCode(err) != 2 {
		t.Fatalf("ExitCode = %d (err %v), want 2", ExitCode(err), err)
	}
	type loc struct {
		check, severity, path string
		line                  int
	}
	var got []loc
	for _, e := range codequalitytest.AssertGitLabAcceptable(t, []byte(out)) {
		got = append(got, loc{e.CheckName, e.Severity, e.Location.Path, e.Location.Lines.Begin})
	}
	want := map[loc]bool{
		{"removed-api/batch/v1beta1/CronJob", "critical", "rendered/all.yaml", 3}:             true,
		{"removed-api/networking.k8s.io/v1beta1/Ingress", "critical", "rendered/all.yaml", 8}: true,
		{"kb-stale", "minor", "upgradescope/kb-stale", 1}:                                     true,
		{"not-assessed/kb-coverage", "critical", "upgradescope/not-assessed/kb-coverage", 1}:  true,
	}
	for _, g := range got {
		if !want[g] {
			t.Errorf("unexpected entry %+v", g)
		}
		delete(want, g)
	}
	for w := range want {
		t.Errorf("missing entry %+v (got %+v)", w, got)
	}
	if !strings.Contains(out, "rendered from demo/templates/cron.yaml") {
		t.Errorf("Code Quality lacks the helm source:\n%s", out)
	}
	if stderr != "" {
		t.Errorf("stderr = %q, want none inside the working directory", stderr)
	}
}

// Manifests outside the working directory cannot be placed in the
// repository: the paths are absolute and stderr says why GitLab will not
// show them on the diff.
func TestScanFilesGitLabOutsideWorkingDir(t *testing.T) {
	dir := writeFiles(t, map[string]string{"rendered/all.yaml": removedAPIs, "repo/README": ""})
	t.Chdir(filepath.Join(dir, "repo"))
	out, stderr, err := execScanFiles(t, "--files", "../rendered", "--output", "gitlab-codequality")
	if ExitCode(err) != 2 {
		t.Fatalf("ExitCode = %d (err %v), want 2", ExitCode(err), err)
	}
	if want := `"path": "` + filepath.ToSlash(filepath.Join(dir, "rendered", "all.yaml")); !strings.Contains(out, want) {
		t.Errorf("Code Quality lacks %s:\n%s", want, out)
	}
	if !strings.Contains(stderr, "outside the working directory") || !strings.Contains(stderr, "GitLab") {
		t.Errorf("stderr = %q, want a note that GitLab cannot place the locations", stderr)
	}
}
