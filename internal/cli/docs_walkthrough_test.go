package cli

// Docs that drifted from the code in the October 2026 round-3 walk-through
// (#306, #307, #308): the contributor map, the Renovate example's flag and
// the clean JUnit report. TestDocsFlagsExist is the guard behind #307: a
// documented command line is checked against the flags the command defines.

import (
	"io/fs"
	"os"
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

// markdownFiles lists every Markdown file of the repository except the
// changelogs (history, naming flags that were removed), design notes and
// vendored or generated trees.
func markdownFiles(t *testing.T) []string {
	t.Helper()
	var files []string
	err := filepath.WalkDir(repoRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", ".claude", "node_modules", "bin", "dist", "superpowers":
				return filepath.SkipDir
			}
			return nil
		}
		rel, err := filepath.Rel(repoRoot, path)
		if err != nil {
			return err
		}
		if strings.HasSuffix(path, ".md") && rel != "CHANGELOG.md" && rel != "docs/changelog.md" {
			files = append(files, filepath.ToSlash(rel))
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(files)
	return files
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
// The first word must be a subcommand: a line that starts with a flag or
// with anything else is not an invocation (prose that names the program, or
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
