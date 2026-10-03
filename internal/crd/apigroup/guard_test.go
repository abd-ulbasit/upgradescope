package apigroup

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// legacyAllowed are the tracked files that may still name the pre-v0.2.0
// group: the compat read path and its tests, the migration guide that
// tells users what to rename and delete, and history, which is not
// rewritten (the design docs under docs/superpowers are not tracked).
var legacyAllowed = []string{
	// The compat read path, and the tests that show old keys honoured
	// with a deprecation note.
	"internal/crd/apigroup/legacy.go",
	"internal/crd/apigroup/legacy_test.go",
	"internal/collect/ignore_test.go",
	"internal/cli/scan_suppress_test.go",
	"internal/server/gate_suppress_test.go",
	"internal/agent/legacycrd_test.go",
	"internal/crd/apigroup/guard_test.go", // this test's own spellings of it
	// The migration: which annotations to rewrite, which CRD to delete.
	"docs/operations/upgrade.md",
	// History.
	"CHANGELOG.md",
	"docs/research.md",
}

// legacyName matches LegacyGroup as written plainly or escaped for a
// regular expression, a JSONPath or a Go string (upgradescope\.dev,
// upgradescope\\.dev, upgradescope[.]dev), in any case: an escaped
// spelling in a kubectl jsonpath or a grep still names the old key.
var legacyName = regexp.MustCompile(`(?i)upgradescope(\\*\.|\[\.\])dev`)

func TestLegacyNameMatchesEscapedSpellings(t *testing.T) {
	for _, s := range []string{
		LegacyGroup, LegacyIgnoreAnnotation,
		`{.metadata.annotations.upgradescope\.dev/status-error}`,
		`"upgradescope\\.dev/ignore"`,
		`grep 'upgradescope[.]dev'`,
		"UpgradeScope.Dev",
	} {
		if !legacyName.MatchString(s) {
			t.Errorf("legacyName does not match %q", s)
		}
	}
	for _, s := range []string{Group, IgnoreAnnotation, "upgradescope-dev", "upgradescope dev", "upgradescopeXdev"} {
		if legacyName.MatchString(s) {
			t.Errorf("legacyName matches %q", s)
		}
	}
}

func allowed(path string) bool {
	for _, a := range legacyAllowed {
		if path == a {
			return true
		}
	}
	return false
}

// The old group, LegacyGroup, is on a domain the project never owned
// (#68). Nothing may name it again, as an API group, annotation key, URL
// or example, except the files above: a stray one would be a resource,
// key or link on someone else's domain.
func TestNoLegacyGroupOutsideTheCompatPath(t *testing.T) {
	git, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git not installed")
	}
	top, err := exec.Command(git, "rev-parse", "--show-toplevel").Output()
	if err != nil {
		t.Skip("not in a git checkout")
	}
	root := strings.TrimSpace(string(top))
	out, err := exec.Command(git, "-C", root, "ls-files", "-z").Output()
	if err != nil {
		t.Fatalf("git ls-files: %v", err)
	}
	var files int
	for _, path := range strings.Split(strings.TrimRight(string(out), "\x00"), "\x00") {
		if path == "" || allowed(path) {
			continue
		}
		b, err := os.ReadFile(filepath.Join(root, path))
		if os.IsNotExist(err) {
			continue // deleted in the working tree, not yet staged
		}
		if err != nil {
			t.Fatal(err)
		}
		files++
		if legacyName.Match(b) {
			for i, line := range strings.Split(string(b), "\n") {
				if legacyName.MatchString(line) {
					t.Errorf("%s:%d names the pre-v0.2.0 group %s; use %s (internal/crd/apigroup), or, for a compat read or its test, list the file in legacyAllowed", path, i+1, LegacyGroup, Group)
				}
			}
		}
	}
	if files < 100 {
		t.Fatalf("checked %d tracked files; want the whole repository", files)
	}
}

// Every allowed file exists, so the list cannot hide a typo or outlive the
// file it excuses.
func TestLegacyAllowedFilesExist(t *testing.T) {
	top, err := exec.Command("git", "rev-parse", "--show-toplevel").Output()
	if err != nil {
		t.Skip("not in a git checkout")
	}
	root := strings.TrimSpace(string(top))
	for _, a := range legacyAllowed {
		if _, err := os.Stat(filepath.Join(root, a)); err != nil {
			t.Errorf("legacyAllowed names %s: %v", a, err)
		}
	}
}
