package cli

// Docs that drifted from the code in the October 2026 round-2 audit (#200).
// Each test names the page to fix when it fails.

import (
	"io/fs"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// docPages lists the hand-written pages and chart files a claim could hide
// in: the README, SECURITY.md, CONTRIBUTING.md, docs/ (without the design
// notes under docs/superpowers and the generated reference), the Action's
// and the chart's READMEs and templates, and helm-docs' template.
func docPages(t *testing.T) []string {
	t.Helper()
	pages := []string{"README.md", "SECURITY.md", "CONTRIBUTING.md", "action/README.md", "deploy/chart/README.md"}
	for _, dir := range []string{"docs", "deploy/chart/templates", "hack/docs"} {
		err := filepath.WalkDir(filepath.Join(repoRoot, dir), func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() && (d.Name() == "superpowers" || d.Name() == "reference") {
				return filepath.SkipDir
			}
			if !d.IsDir() && (strings.HasSuffix(path, ".md") || strings.HasSuffix(path, ".yaml") || strings.HasSuffix(path, ".gotmpl")) {
				rel, err := filepath.Rel(repoRoot, path)
				if err != nil {
					return err
				}
				pages = append(pages, rel)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	return pages
}

// TestDocsTokenStorage: the server keeps a token's sha256 hash and its
// first 8 characters (SE-03; #126 fixed the runtime message only). The
// help of every tokens command says so, nothing under README.md, docs/ or
// deploy/ still says "only its hash", and docs/getting-started/fleet.md
// shows the line the binary prints.
func TestDocsTokenStorage(t *testing.T) {
	root := newTokensCmd()
	create, _, err := root.Find([]string{"create"})
	if err != nil {
		t.Fatal(err)
	}
	for _, cmd := range append([]*cobra.Command{root}, root.Commands()...) {
		if help := cmd.Short + "\n" + cmd.Long; strings.Contains(help, "only its hash") {
			t.Errorf("`%s --help` says only the hash is stored; the first 8 characters are stored too", cmd.CommandPath())
		}
	}
	for _, cmd := range []*cobra.Command{root, create} {
		if !strings.Contains(strings.Join(strings.Fields(cmd.Long), " "), "first 8 characters") {
			t.Errorf("`%s --help` does not say the first 8 characters are stored", cmd.CommandPath())
		}
	}
	for _, page := range docPages(t) {
		if strings.Contains(readDoc(t, page), "only its hash") {
			t.Errorf("%s says only a token's hash is stored; it keeps the sha256 hash and the first 8 characters", page)
		}
	}
	for _, page := range []string{"docs/reference/cli/upgradescope_tokens_create.md", "docs/reference/cli/upgradescope_tokens.md"} {
		if strings.Contains(readDoc(t, page), "only its hash") {
			t.Errorf("%s still says only the hash is stored: run `make docs-gen`", page)
		}
	}

	_, stderr, err := execTokens(t, "create", "prod-eu-1", "--db", filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	_, printed, ok := strings.Cut(strings.TrimSpace(stderr), " created — ")
	if !ok {
		t.Fatalf("tokens create printed %q, want `... created — <what is stored>`", stderr)
	}
	if fleet := readDoc(t, "docs/getting-started/fleet.md"); !strings.Contains(fleet, " created — "+printed) {
		t.Errorf("docs/getting-started/fleet.md does not show the line `tokens create` prints: want it to end %q", " created — "+printed)
	}
}

// TestDocsSecurityHeadersScope: the headers are on every response the
// server's handler writes, not on the ones net/http writes before any
// handler runs (SE-06; server.go's securityHeaders comment is exact).
func TestDocsSecurityHeadersScope(t *testing.T) {
	const page = "docs/operations/security-model-and-rbac.md"
	doc := strings.Join(strings.Fields(readDoc(t, page)), " ")
	lower := strings.ToLower(doc)
	if strings.Contains(doc, "Every response carries") {
		t.Errorf("%s claims every response carries the security headers; net/http's own 400, 431 and 501 carry none", page)
	}
	if !strings.Contains(lower, "every response the server's handler writes") || !strings.Contains(lower, "before routing") {
		t.Errorf("%s must say the headers are on every response the server's handler writes, and that responses net/http writes before routing carry none", page)
	}
}

// TestDocsDeprecatedCallsDoNotSayWho: apiserver_requested_deprecated_apis
// says that a client asked, not which client (DC-01). Only the GKE and AKS
// comparison, about those services, may say who calls.
func TestDocsDeprecatedCallsDoNotSayWho(t *testing.T) {
	for _, page := range docPages(t) {
		if page == "docs/comparison.md" || page == "docs/claims.md" {
			continue
		}
		doc := strings.ToLower(readDoc(t, page))
		for _, bad := range []string{"who still calls", "who calls"} {
			if strings.Contains(doc, bad) {
				t.Errorf("%s says %q; the metric shows whether any client still calls deprecated APIs (not which)", page, bad)
			}
		}
	}
}

// TestDocsRound2Gaps: the small documentation gaps of #200 stay filled.
func TestDocsRound2Gaps(t *testing.T) {
	for _, tc := range []struct {
		page, claim string
		want        []string // lower case, after white space is collapsed
	}{
		{"CONTRIBUTING.md", "a third dispatch of one commit cancels the pending second (IR-05)",
			[]string{"third dispatch", "pending"}},
		{"docs/getting-started/ci-gate.md", "an alert reaches a pull request's diff only for a file committed to the repository (FS-03)",
			[]string{"committed", "pull request's diff", "rendered/"}},
		{"action/README.md", "an alert reaches a pull request's diff only for a file committed to the repository (FS-03)",
			[]string{"committed", "pull request's diff", "rendered/"}},
		{"docs/operations/retention-and-backup.md", "/history is a change log: what adds a row, what refreshes one in place, what `at` is (AG-12)",
			[]string{"change log", "refreshed in place", "server's clock", "team attribution"}},
	} {
		doc := strings.Join(strings.Fields(strings.ToLower(readDoc(t, tc.page))), " ")
		for _, w := range tc.want {
			if !strings.Contains(doc, w) {
				t.Errorf("%s does not say that %s (missing %q)", tc.page, tc.claim, w)
			}
		}
	}
}

// authorshipPassage returns the sentence of doc that says who wrote the code,
// from "Most of the code was written by" to the end of its sentence, link
// targets and line breaks removed so the README and the FAQ can be compared.
func authorshipPassage(t *testing.T, page string) string {
	t.Helper()
	doc := strings.Join(strings.Fields(readDoc(t, page)), " ")
	doc = regexp.MustCompile(`\]\([^)]*\)`).ReplaceAllString(doc, "]")
	const start = "Most of the code was written by"
	i := strings.Index(doc, start)
	if i < 0 {
		t.Fatalf("%s does not say that most of the code was written by coding agents (DO-05)", page)
	}
	rest := doc[i:]
	// The sentence ends at the first period that is followed by a space or the end.
	end := regexp.MustCompile(`\.( |$)`).FindStringIndex(rest)
	if end == nil {
		return rest
	}
	return rest[:end[0]+1]
}

// TestAuthorshipNoteConsistent (DO-05, #366): the README and the FAQ say the
// same thing about who wrote the code, and say only what the public history
// can show: coding agents wrote most of it (the Co-Authored-By trailers),
// from plans the author approved but did not write, and those planning notes
// are not published. "Specs and plans the author wrote" cannot be checked from
// a clone, and the notes are agent-co-authored themselves.
func TestAuthorshipNoteConsistent(t *testing.T) {
	readme, faq := authorshipPassage(t, "README.md"), authorshipPassage(t, "docs/faq.md")
	if readme != faq {
		t.Errorf("README.md and docs/faq.md differ on who wrote the code:\n  README.md:   %s\n  docs/faq.md: %s", readme, faq)
	}
	for _, page := range []string{"README.md", "docs/faq.md"} {
		doc := strings.ToLower(strings.Join(strings.Fields(readDoc(t, page)), " "))
		for _, old := range []string{"specs and plans the author wrote", "specs and plans he wrote"} {
			if strings.Contains(doc, old) {
				t.Errorf("%s still says %q: the notes are not published, so \"wrote\" cannot be checked", page, old)
			}
		}
	}
	for _, want := range []string{"Co-Authored-By", "not published", "approved"} {
		if !strings.Contains(readme, want) {
			t.Errorf("the authorship note does not say %q: %s", want, readme)
		}
	}

	// The trailers the note points at exist in the history (not asserted in a
	// shallow clone or an export, which may hold none).
	if out, err := exec.Command("git", "-C", repoRoot, "rev-parse", "--is-shallow-repository").Output(); err != nil || strings.TrimSpace(string(out)) != "false" {
		return
	}
	out, err := exec.Command("git", "-C", repoRoot, "log", "-i", "--grep=Co-Authored-By: Claude", "--oneline", "-1").Output()
	if err != nil {
		t.Skipf("git log: %v", err)
	}
	if strings.TrimSpace(string(out)) == "" {
		t.Error("the note points at Co-Authored-By trailers, and no commit in the history has one")
	}
}

// TestReadmeVerifyBlockIsSelfContained (#366): the README's "Verifying a
// download" block works copied verbatim. It sets every variable it uses,
// downloads the three files it reads, and checks the signature before the
// archive against the checksums. Run with $VERSION unset it used to fail with
// a certificate identity ending in "refs/tags/".
func TestReadmeVerifyBlockIsSelfContained(t *testing.T) {
	doc := readDoc(t, "README.md")
	i := strings.Index(doc, "Verifying a download")
	if i < 0 {
		t.Fatal("README.md has no \"Verifying a download\"")
	}
	rest := doc[i:]
	open := strings.Index(rest, "```sh\n")
	if open < 0 {
		t.Fatal("README.md: no sh block after \"Verifying a download\"")
	}
	block := rest[open+len("```sh\n"):]
	block = block[:strings.Index(block, "```")]

	assigned := map[string]bool{}
	for _, m := range regexp.MustCompile(`(?m)^([A-Za-z_][A-Za-z0-9_]*)=`).FindAllStringSubmatch(block, -1) {
		assigned[m[1]] = true
	}
	for _, m := range regexp.MustCompile(`\$\{?([A-Za-z_][A-Za-z0-9_]*)`).FindAllStringSubmatch(block, -1) {
		if !assigned[m[1]] {
			t.Errorf("README.md's verify block uses $%s and never sets it", m[1])
		}
	}
	for _, want := range []string{"checksums.txt\"", "checksums.txt.sigstore.json\"", "tar.gz\""} {
		if !strings.Contains(block, "curl") || !strings.Contains(block, want) {
			t.Errorf("README.md's verify block does not download %s", want)
		}
	}
	sig, sum := strings.Index(block, "cosign verify-blob"), strings.Index(block, "sha256sum")
	if sig < 0 || sum < 0 || sig > sum {
		t.Error("README.md's verify block must check the signature (cosign verify-blob) before the checksums (sha256sum)")
	}
}
