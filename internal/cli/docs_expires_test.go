package cli

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

// expiresRE finds an ignore rule's expiry in a YAML or JSON snippet:
// `expires: 2026-12-31`, `expires: "2026-12-31"`, `"expires": "2026-12-31"`.
var expiresRE = regexp.MustCompile(`"?\bexpires"?\s*:\s*"?(\d{4}-\d{2}-\d{2})"?`)

// expiredExamples returns "path:line date" for each `expires:` date in the
// docs, README, Action, chart and example files under the roots of root that
// is earlier than horizon. The changelogs are history, not examples to copy,
// and testdata is not documentation.
func expiredExamples(t *testing.T, root string, roots []string, horizon time.Time) []string {
	t.Helper()
	var out []string
	for _, r := range roots {
		err := filepath.WalkDir(filepath.Join(root, r), func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				if n := d.Name(); n == "node_modules" || n == ".git" || n == "testdata" {
					return fs.SkipDir
				}
				return nil
			}
			switch strings.ToLower(filepath.Ext(p)) {
			case ".md", ".yaml", ".yml", ".json", ".txt", ".tpl":
			default:
				return nil
			}
			if strings.EqualFold(filepath.Base(p), "changelog.md") {
				return nil
			}
			raw, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			rel, _ := filepath.Rel(root, p)
			for i, line := range strings.Split(string(raw), "\n") {
				for _, m := range expiresRE.FindAllStringSubmatch(line, -1) {
					if d, err := time.Parse("2006-01-02", m[1]); err != nil || d.Before(horizon) {
						out = append(out, filepath.ToSlash(rel)+":"+strconv.Itoa(i+1)+" "+m[1])
					}
				}
			}
			return nil
		})
		if err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
	}
	return out
}

// docsClock is the test's "today": UPGRADESCOPE_TEST_NOW (a date, to try the
// suite under a shifted clock) or the wall clock.
func docsClock(t *testing.T) time.Time {
	t.Helper()
	if v := os.Getenv("UPGRADESCOPE_TEST_NOW"); v != "" {
		d, err := time.Parse("2006-01-02", v)
		if err != nil {
			t.Fatalf("UPGRADESCOPE_TEST_NOW=%q: want YYYY-MM-DD", v)
		}
		return d
	}
	return time.Now()
}

// An example `expires:` date in the docs is copied into a user's config,
// where a date in the past makes the rule expire at once and the finding it
// was meant to accept gate again (#269: `expires: 2026-12-31` was the
// canonical example on three pages). So every date in a copy-paste block is
// at least a year after the test clock; the examples use 2099-12-31 and say
// to write a date inside your own migration window.
func TestDocsExpiresExamplesAreFarInTheFuture(t *testing.T) {
	horizon := docsClock(t).AddDate(1, 0, 0)
	roots := []string{"docs", "README.md", "action", "examples", "deploy", "hack/demo"}
	// The scan must reach the examples: with a horizon past every date it
	// lists them all, so a wrong path cannot pass for "none expired".
	if all := expiredExamples(t, "../..", roots, time.Date(3000, 1, 1, 0, 0, 0, 0, time.UTC)); len(all) < 4 {
		t.Fatalf("found %d `expires:` examples under the docs, want at least the 4 in the config and suppression guides: %v", len(all), all)
	}
	got := expiredExamples(t, "../..", roots, horizon)
	if len(got) > 0 {
		t.Errorf("`expires:` examples earlier than %s (the test clock plus a year): %s\nuse a far-future date such as 2099-12-31, and say it is an example", horizon.Format("2006-01-02"), strings.Join(got, ", "))
	}
}

// The scanner itself, with an injected clock: an expired example is found
// in YAML (bare or quoted) and JSON, a far-future one is not, and the
// changelog and testdata are left out.
func TestExpiredExamplesFindsPastDates(t *testing.T) {
	root := t.TempDir()
	write := func(rel, body string) {
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("docs/a.md", "```yaml\nignore:\n  - key: x\n    expires: 2026-12-31\n```\n")
	write("docs/b.md", "spec:\n  ignore:\n    - expires: \"2027-01-01\"\n")
	write("docs/c.md", "{\"expires\": \"2026-06-30\"}\n")
	write("docs/ok.md", "    expires: 2099-12-31   # a date inside your migration window\n")
	write("docs/changelog.md", "expires: 2020-01-01\n")
	write("docs/testdata/old.yaml", "expires: 2020-01-01\n")
	write("README.md", "no dates here\n")

	clock := func(day string) time.Time {
		d, err := time.Parse("2006-01-02", day)
		if err != nil {
			t.Fatal(err)
		}
		return d.AddDate(1, 0, 0)
	}
	roots := []string{"docs", "README.md", "missing"}
	got := strings.Join(expiredExamples(t, root, roots, clock("2026-10-10")), ", ")
	if want := "docs/a.md:4 2026-12-31, docs/b.md:3 2027-01-01, docs/c.md:1 2026-06-30"; got != want {
		t.Errorf("found %q, want %q", got, want)
	}
	// Under a shifted clock the same files fail for a later date, not for 2099.
	if got := expiredExamples(t, root, roots, clock("2090-01-01")); len(got) != 3 {
		t.Errorf("with the clock at 2090: %v, want the same three", got)
	}
	if got := expiredExamples(t, root, roots, clock("2098-12-31").AddDate(1, 0, 0)); len(got) != 4 {
		t.Errorf("with the clock past 2099: %v, want 2099-12-31 flagged too", got)
	}
}
