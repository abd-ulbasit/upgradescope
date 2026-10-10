package cli

// Docs that drifted from the code in the October 2026 round-3 walk-through
// (#306, #307, #308): the contributor map, the Renovate example's flag and
// the clean JUnit report. TestDocsFlagsExist is the guard behind #307: a
// documented command line is checked against the flags the command defines.

import (
	"go/build"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// TestDocsOtherCIJUnitCleanReport: a clean live scan is one passing
// `readiness/no findings`; a clean --files scan is the three skipped
// optional checks and no passed test (TestScanFilesCleanJUnit and
// TestWriteCleanReport pin the output the page describes, #308).
func TestDocsOtherCIJUnitCleanReport(t *testing.T) {
	const page = "docs/guides/other-ci.md"
	doc := strings.Join(strings.Fields(readDoc(t, page)), " ")
	for _, want := range []string{
		"clean live-cluster scan", "`readiness/no findings`", "clean `--files` scan never is",
		"`deprecated-calls`, `helm` or `versions`", "no passed test", "skipped tests are tests",
	} {
		if !strings.Contains(doc, want) {
			t.Errorf("%s does not say %q about the clean JUnit report", page, want)
		}
	}
	if strings.Contains(doc, "A clean report is one passing test") {
		t.Errorf("%s says a clean report is one passing test; only a live scan with every check assessed is (a files-mode scan holds three skipped tests)", page)
	}
	if strings.Contains(doc, "(Helm in files mode)") {
		t.Errorf("%s names only Helm as the optional check files mode skips; it skips deprecated-calls, helm and versions", page)
	}
}

// markdownFiles lists the tracked Markdown files of the repository except
// the changelogs (history, naming flags that were removed), design notes
// and generated trees. Only tracked files count: an untracked file (a docs
// virtualenv's site-packages, a scratch note) is not part of the docs and
// must not fail the guard on one contributor's machine while CI passes.
func markdownFiles(t *testing.T) []string {
	t.Helper()
	files, err := listFiles(repoRoot, ".md")
	if err != nil {
		t.Fatal(err)
	}
	files = slices.DeleteFunc(files, func(rel string) bool {
		return rel == "CHANGELOG.md" || rel == "docs/changelog.md" || strings.HasPrefix(rel, "docs/superpowers/")
	})
	return files
}

// listFiles asks git for the tracked files with the extension ext under
// root, sorted and slash-separated. Without
// git, or outside a work tree (a source tarball), it walks the tree and
// skips what is never documentation: dot-directories, virtualenvs, the
// built site, node_modules and build output.
func listFiles(root, ext string) ([]string, error) {
	var files []string
	cmd := exec.Command("git", "ls-files", "-z", "--", "*"+ext)
	cmd.Dir = root
	if out, err := cmd.Output(); err == nil {
		for _, rel := range strings.Split(string(out), "\x00") {
			// A file deleted from the work tree is still listed until the
			// deletion is staged.
			if _, err := os.Stat(filepath.Join(root, rel)); rel != "" && err == nil {
				files = append(files, rel)
			}
		}
	} else {
		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				switch name := d.Name(); {
				case path != root && strings.HasPrefix(name, "."), name == "venv", name == "site", name == "node_modules", name == "bin", name == "dist":
					return filepath.SkipDir
				}
				return nil
			}
			if rel, err := filepath.Rel(root, path); err == nil && strings.HasSuffix(rel, ext) {
				files = append(files, filepath.ToSlash(rel))
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	sort.Strings(files)
	return files, nil
}

// TestListMarkdownIgnoresUntracked: the file list is the tracked files, so a
// virtualenv or a scratch note in the work tree cannot fail the guard; and
// the walk used without git skips the trees that are never documentation.
func TestListMarkdownIgnoresUntracked(t *testing.T) {
	write := func(root, rel string) {
		t.Helper()
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("`upgradescope scan --bogus`\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	layout := []string{"README.md", "docs/a.md", "CHANGELOG.md", "docs/changelog.md", "scratch.md",
		".venv/lib/site-packages/x/README.md", "site/index.md", "node_modules/p/README.md", "bin/n.md", "docs/.hidden/h.md"}

	t.Run("git", func(t *testing.T) {
		if _, err := exec.LookPath("git"); err != nil {
			t.Skip("git is not installed")
		}
		root := t.TempDir()
		for _, args := range [][]string{{"init", "-q"}, {"config", "user.email", "t@example.com"}, {"config", "user.name", "t"}} {
			c := exec.Command("git", args...)
			c.Dir = root
			if out, err := c.CombinedOutput(); err != nil {
				t.Fatalf("git %v: %v\n%s", args, err, out)
			}
		}
		for _, rel := range layout {
			write(root, rel)
		}
		c := exec.Command("git", "add", "README.md", "docs/a.md", "CHANGELOG.md", "docs/changelog.md")
		c.Dir = root
		if out, err := c.CombinedOutput(); err != nil {
			t.Fatalf("git add: %v\n%s", err, out)
		}
		got, err := listFiles(root, ".md")
		if err != nil {
			t.Fatal(err)
		}
		if want := []string{"CHANGELOG.md", "README.md", "docs/a.md", "docs/changelog.md"}; !slices.Equal(got, want) {
			t.Errorf("with git: %v, want only the tracked pages %v", got, want)
		}
	})

	t.Run("walk", func(t *testing.T) {
		// A temp dir sits in no work tree, so git fails and the walk runs.
		root := t.TempDir()
		for _, rel := range layout {
			write(root, rel)
		}
		got, err := listFiles(root, ".md")
		if err != nil {
			t.Fatal(err)
		}
		if want := []string{"CHANGELOG.md", "README.md", "docs/a.md", "docs/changelog.md", "scratch.md"}; !slices.Equal(got, want) {
			t.Errorf("without git: %v, want %v", got, want)
		}
	})
}

var (
	inlineCode = regexp.MustCompile("`([^`\n]+)`")
	// invocation: `upgradescope` as a word of its own (a path ending in it
	// counts, an image name with a tag does not). Every occurrence on a line
	// is one invocation: `cd upgradescope && upgradescope scan ...` has two.
	invocation = regexp.MustCompile(`(?:^|[\s"'(=/])upgradescope(?:\s+|$)`)
	flagToken  = regexp.MustCompile(`^--([a-zA-Z][a-zA-Z0-9-]*)`)
)

// docInvocation is one `upgradescope ...` command line a page shows.
type docInvocation struct {
	page  string
	line  int
	words []string // after `upgradescope`, up to the end of the command
}

// shellWords splits s like a shell for our purpose: quoted strings are one
// word, and the first pipe, redirect, `;`, `&`, `&&`, `||`, `--` or comment
// ends the command (what follows belongs to another program).
func shellWords(s string) []string {
	var words []string
	var cur strings.Builder
	var quote rune
	started := false
	flush := func() {
		if started {
			words = append(words, cur.String())
		}
		cur.Reset()
		started = false
	}
	for _, r := range s {
		switch {
		case quote != 0:
			if r == quote {
				quote = 0
			} else {
				cur.WriteRune(r)
			}
		case r == '"' || r == '\'':
			quote, started = r, true
			// A quoted word is a value, never a flag: mark it.
			if cur.Len() == 0 {
				cur.WriteString("\x00")
			}
		case r == ' ' || r == '\t':
			flush()
		default:
			cur.WriteRune(r)
			started = true
		}
	}
	flush()
	var out []string
	for _, w := range words {
		switch {
		case w == "|" || w == "||" || w == "&&" || w == ";" || w == "&" || w == "--" || strings.HasPrefix(w, "#"),
			strings.HasPrefix(w, ">") || strings.HasPrefix(w, "<") || strings.HasPrefix(w, "2>"):
			return out
		case strings.HasSuffix(w, ";"):
			return append(out, strings.TrimSuffix(w, ";"))
		}
		out = append(out, w)
	}
	return out
}

// docInvocations finds the `upgradescope ...` command lines in a page's
// fenced code blocks (backslash continuations joined) and inline code.
func docInvocations(page, text string) []docInvocation {
	var found []docInvocation
	add := func(line int, s string) {
		for _, loc := range invocation.FindAllStringIndex(s, -1) {
			found = append(found, docInvocation{page: page, line: line, words: shellWords(s[loc[1]:])})
		}
	}
	lines := strings.Split(text, "\n")
	fence := ""
	var pending strings.Builder
	pendingLine := 0
	for i, l := range lines {
		trim := strings.TrimSpace(l)
		if marker := strings.TrimLeft(trim, "`~"); strings.HasPrefix(trim, "```") || strings.HasPrefix(trim, "~~~") {
			mark := trim[:len(trim)-len(marker)]
			switch {
			case fence == "":
				fence = mark[:3]
			case strings.HasPrefix(mark, fence) && marker == "":
				fence = ""
				if pending.Len() > 0 {
					add(pendingLine, pending.String())
					pending.Reset()
				}
			}
			continue
		}
		if fence == "" {
			for _, m := range inlineCode.FindAllStringSubmatch(l, -1) {
				add(i+1, m[1])
			}
			continue
		}
		if pending.Len() == 0 {
			pendingLine = i + 1
		}
		if strings.HasSuffix(trim, "\\") {
			pending.WriteString(strings.TrimSuffix(trim, "\\") + " ")
			continue
		}
		pending.WriteString(trim)
		add(pendingLine, pending.String())
		pending.Reset()
	}
	return found
}

// resolve finds the command a documented line runs and the words after it.
// Leading flags are skipped (`upgradescope --kubeconfig x scan --bogus` is a
// `scan` line), but the first word that is not a flag must be a subcommand:
// anything else is not an invocation (prose that names the program, or
// `gh ... --repo abd-ulbasit/upgradescope --some-flag`). Subcommands are
// followed down (`tokens create`). A line that stops at a command with
// subcommands of its own and shows an alternation or placeholder in place
// of one (`clusters delete|rename --server`) is ambiguous: not checked.
func resolve(root *cobra.Command, words []string) (*cobra.Command, []string, bool) {
	cmd := root
	for i, w := range words {
		if strings.HasPrefix(w, "-") {
			continue
		}
		var next *cobra.Command
		for _, c := range cmd.Commands() {
			if c.Name() == w || slices.Contains(c.Aliases, w) {
				next = c
			}
		}
		if next == nil {
			if i == 0 {
				return nil, nil, false
			}
			continue
		}
		cmd = next
	}
	if cmd == root {
		return nil, nil, false
	}
	if cmd.HasSubCommands() && slices.ContainsFunc(words, func(w string) bool { return strings.ContainsAny(w, "|<[") }) {
		return nil, nil, false
	}
	return cmd, words, true
}

// TestDocsFlagsExist: a command line in the docs names only flags the
// command defines (#307: `scan --format json` when the flag is `--output`).
// It checks every Markdown file's code blocks and inline code for
// `upgradescope <command> ... --flag`; what follows a pipe, a redirect or
// `--` is another program's, and a quoted word is a value.
func TestDocsFlagsExist(t *testing.T) {
	root := Root()
	checked := 0
	for _, page := range markdownFiles(t) {
		for _, inv := range docInvocations(page, readDoc(t, page)) {
			cmd, rest, ok := resolve(root, inv.words)
			if !ok {
				continue
			}
			for _, w := range rest {
				m := flagToken.FindStringSubmatch(w)
				if m == nil || strings.HasPrefix(w, "\x00") {
					continue
				}
				checked++
				name := m[1]
				if name == "help" || (name == "version" && cmd == root) || cmd.Flag(name) != nil {
					continue
				}
				t.Errorf("%s:%d: `%s ... --%s`: %s defines no such flag (see `%s --help`)",
					inv.page, inv.line, cmd.CommandPath(), name, cmd.CommandPath(), cmd.CommandPath())
			}
		}
	}
	t.Logf("checked %d documented flags", checked)
	if checked < 100 {
		t.Errorf("checked only %d documented flags: the scan lost its way through the docs", checked)
	}
}

// TestDocsFlagScannerFindsDrift pins the scanner itself, so the guard above
// cannot go quiet unnoticed: it flags the #307 line, reads continuations,
// and leaves another program's flags alone.
func TestDocsFlagScannerFindsDrift(t *testing.T) {
	root := Root()
	flagsOf := func(text string) []string {
		var bad []string
		for _, inv := range docInvocations("x.md", text) {
			cmd, rest, ok := resolve(root, inv.words)
			if !ok {
				continue
			}
			for _, w := range rest {
				if m := flagToken.FindStringSubmatch(w); m != nil && !strings.HasPrefix(w, "\x00") && cmd.Flag(m[1]) == nil && m[1] != "help" {
					bad = append(bad, cmd.Name()+" --"+m[1])
				}
			}
		}
		return bad
	}
	for _, tc := range []struct {
		name, text string
		want       []string
	}{
		{"inline, the #307 line", "from `upgradescope scan --format json` is", []string{"scan --format"}},
		{"fenced, continued", "```sh\nupgradescope scan \\\n  --files rendered \\\n  --bogus x\n```", []string{"scan --bogus"}},
		{"subcommand of a subcommand", "`upgradescope tokens create prod --nope`", []string{"create --nope"}},
		{"real flags", "`upgradescope scan --files rendered --target 1.37 --output json`", nil},
		{"after a pipe", "`upgradescope scan --output json | jq --raw-output .score`", nil},
		{"after a pipe, fenced", "```sh\nupgradescope scan --output json | jq --raw-output .score\n```", nil},
		{"after a redirect", "```\nupgradescope scan --output json > r.json --whatever\n```", nil},
		{"another program before it", "```sh\nhelm template x --output-dir rendered && upgradescope scan --files rendered\n```", nil},
		{"after &&", "```sh\nupgradescope scan --files rendered && kubectl apply --server-side\n```", nil},
		{"after a semicolon", "`upgradescope scan --files rendered; ls --all`", nil},
		{"after a spaced semicolon", "`upgradescope scan --files rendered ; ls --all`", nil},
		{"the second invocation on a line", "`cd upgradescope && upgradescope scan --bogus`", []string{"scan --bogus"}},
		{"a quoted value", "`upgradescope scan --files \"a --format\"`", nil},
		{"a path to the binary", "```\n./bin/upgradescope scan --nope\n```", []string{"scan --nope"}},
		{"a tagged image is not a command line", "`ghcr.io/abd-ulbasit/upgradescope:1.0 --nope`", nil},
		{"a tagged image in a fence", "```sh\ndocker run ghcr.io/abd-ulbasit/upgradescope:1.0 scan --nope\n```", nil},
		{"prose", "`upgradescope` is a scanner with `--nope`", nil},
	} {
		if got := flagsOf(tc.text); !slices.Equal(got, tc.want) {
			t.Errorf("%s: drift = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestContributingRepositoryMap: CONTRIBUTING.md's map names every
// directory under internal/ and every command the binary registers, and the
// architecture page lists every command in its table (#306).
func TestContributingRepositoryMap(t *testing.T) {
	const page = "CONTRIBUTING.md"
	doc := readDoc(t, page)
	_, afterHeading, ok := strings.Cut(doc, "## Repository map")
	if !ok {
		t.Fatalf("%s has no `## Repository map`", page)
	}
	repoMap, _, _ := strings.Cut(afterHeading, "\n## ")
	entries, err := os.ReadDir(filepath.Join(repoRoot, "internal"))
	if err != nil {
		t.Fatal(err)
	}
	mapped := map[string]string{} // package name → its map line
	inInternal := false
	for _, l := range strings.Split(repoMap, "\n") {
		switch {
		case strings.HasPrefix(l, "internal/"):
			inInternal = true
		case inInternal && strings.HasPrefix(l, "  ") && !strings.HasPrefix(l, "    "):
			if name, _, _ := strings.Cut(strings.TrimSpace(l), "/"); name != "" {
				mapped[name] = l
			}
		case inInternal && !strings.HasPrefix(l, " "):
			inInternal = false
		}
	}
	for _, e := range entries {
		if e.IsDir() && mapped[e.Name()] == "" {
			t.Errorf("%s: the repository map has no line for internal/%s", page, e.Name())
		}
	}
	for name := range mapped {
		if _, err := os.Stat(filepath.Join(repoRoot, "internal", name)); err != nil {
			t.Errorf("%s: the repository map lists internal/%s, which does not exist", page, name)
		}
	}

	cliLine := mapped["cli"]
	archTable := tableRows(readDoc(t, "docs/architecture.md"), "| Command | Runs | Produces |")
	for _, c := range Root().Commands() {
		if !slices.Contains(wordsOf(cliLine), c.Name()) {
			t.Errorf("%s: the map's cli line does not name the `%s` command: %q", page, c.Name(), cliLine)
		}
		if !strings.Contains(archTable, "`upgradescope "+c.Name()+"`") {
			t.Errorf("docs/architecture.md: the command table has no row for `upgradescope %s`", c.Name())
		}
	}

	for _, stale := range []string{"dashboard not built", "holds only `.gitkeep`", "no dashboard"} {
		if strings.Contains(doc, stale) {
			t.Errorf("%s says %q; internal/server/webdist is committed, so make build embeds the dashboard", page, stale)
		}
	}
	flat := strings.Join(strings.Fields(doc), " ")
	for _, want := range []string{"`make build` embeds the dashboard", "committed", "`make web`", "`web/`"} {
		if !strings.Contains(flat, want) {
			t.Errorf("%s does not say %q about the dashboard build", page, want)
		}
	}

	arch := strings.Join(strings.Fields(readDoc(t, "docs/architecture.md")), " ")
	for _, want := range []string{"junit", "gitlab-codequality", "`upgradescope mcp`"} {
		if !strings.Contains(arch, want) {
			t.Errorf("docs/architecture.md does not mention %s", want)
		}
	}
}

// goPackageDirs lists the directories under root (slash-separated, relative
// to the repository) that hold tracked non-test Go files, skipping testdata
// and the test-helper packages (named *test).
func goPackageDirs(t *testing.T, root string) []string {
	t.Helper()
	files, err := listFiles(filepath.Join(repoRoot, root), ".go")
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	var dirs []string
	for _, f := range files {
		dir := path.Join(root, path.Dir(f))
		if strings.HasSuffix(f, "_test.go") || seen[dir] {
			continue
		}
		skip := false
		for _, part := range strings.Split(path.Dir(f), "/") {
			if part == "testdata" || strings.HasSuffix(part, "test") {
				skip = true
			}
		}
		if !skip {
			seen[dir] = true
			dirs = append(dirs, dir)
		}
	}
	return dirs
}

// TestArchitectureComponentTable: every row of the component table in
// docs/architecture.md lists exactly the packages of this module that the
// package imports (the imports of its non-test files, as `go list` shows
// them), and every package under internal/ has a row. "Depends on" is a
// claim about the code; it drifted when agent, crd and server grew imports
// the table did not name.
func TestArchitectureComponentTable(t *testing.T) {
	const page = "docs/architecture.md"
	table := tableRows(readDoc(t, page), "| Package | Responsibility | Depends on |")
	if table == "" {
		t.Fatalf("%s has no component table", page)
	}
	const module = "github.com/abd-ulbasit/upgradescope/"
	rows := map[string][]string{} // package directory → the packages its row names
	for _, l := range strings.Split(table, "\n") {
		cells := strings.Split(l, "|")
		if len(cells) < 5 || !strings.HasPrefix(strings.TrimSpace(cells[1]), "`") {
			continue
		}
		pkg := strings.Trim(strings.TrimSpace(cells[1]), "`")
		var deps []string
		for _, m := range inlineCode.FindAllStringSubmatch(cells[3], -1) {
			if !strings.Contains(m[1], "-") { // client-go and the like are not ours
				deps = append(deps, m[1])
			}
		}
		rows[pkg] = deps
	}
	for pkg, named := range rows {
		if strings.HasSuffix(pkg, "/") || strings.HasPrefix(pkg, "tools/") {
			continue // web/ and the separate modules under tools/
		}
		p, err := build.ImportDir(filepath.Join(repoRoot, pkg), 0)
		if err != nil {
			t.Errorf("%s: the component table lists %s, which is not a Go package: %v", page, pkg, err)
			continue
		}
		var want []string
		for _, imp := range p.Imports {
			if rel, ok := strings.CutPrefix(imp, module); ok {
				want = append(want, strings.TrimPrefix(rel, "internal/"))
			}
		}
		slices.Sort(want)
		got := slices.Clone(named)
		slices.Sort(got)
		if !slices.Equal(got, want) {
			t.Errorf("%s: %s depends on %v (go list), but its row names %v", page, pkg, want, got)
		}
	}
	for _, dir := range goPackageDirs(t, "internal") {
		if _, ok := rows[dir]; !ok {
			t.Errorf("%s: the component table has no row for %s", page, dir)
		}
	}
}

// TestContributingMapNamesTopLevelPackages: CONTRIBUTING.md's map has a line
// for every top-level directory that holds a Go package (api/ was missing
// while internal/mcp imported it).
func TestContributingMapNamesTopLevelPackages(t *testing.T) {
	doc := readDoc(t, "CONTRIBUTING.md")
	_, afterHeading, ok := strings.Cut(doc, "## Repository map")
	if !ok {
		t.Fatal("CONTRIBUTING.md has no `## Repository map`")
	}
	repoMap, _, _ := strings.Cut(afterHeading, "\n## ")
	top := map[string]bool{}
	entries, err := os.ReadDir(repoRoot)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.IsDir() && e.Name() != "internal" {
			top[e.Name()] = len(goPackageDirs(t, e.Name())) > 0
		}
	}
	if !top["api"] || !top["cmd"] || !top["registry"] {
		t.Fatalf("goPackageDirs lost its way: top-level Go dirs = %v", top)
	}
	for name, hasGo := range top {
		if hasGo && !strings.Contains("\n"+repoMap, "\n"+name+"/") {
			t.Errorf("CONTRIBUTING.md: the repository map has no line for %s/, a top-level directory with Go packages", name)
		}
	}
}

// tableRows returns the Markdown table that starts at header, as one string.
func tableRows(doc, header string) string {
	_, after, ok := strings.Cut(doc, header)
	if !ok {
		return ""
	}
	var b strings.Builder
	for _, l := range strings.Split(after, "\n") {
		if !strings.HasPrefix(l, "|") {
			if b.Len() == 0 && strings.TrimSpace(l) == "" {
				continue
			}
			break
		}
		b.WriteString(l + "\n")
	}
	return b.String()
}

// wordsOf splits a map line into its words, so a command name matches only
// as a whole word (`scan` is not found in `scanner`).
func wordsOf(s string) []string {
	return strings.FieldsFunc(s, func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_')
	})
}
