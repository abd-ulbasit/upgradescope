package cli

// Docs that drifted from the code in the October 2026 round-2 audit (#200).
// Each test names the page to fix when it fails.

import (
	"io/fs"
	"path/filepath"
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
