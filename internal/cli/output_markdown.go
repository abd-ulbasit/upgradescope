package cli

import (
	"fmt"
	"io"
	"strings"

	"github.com/abd-ulbasit/upgradescope/internal/engine"
)

// WriteMarkdown renders the report as GitHub-flavoured Markdown: a header
// line (verdict, target, score, finding counts, KB version), one table row
// per finding (severity, title, objects as file:line, remediation), then
// the NOT ASSESSED gaps. The GitHub Action writes it to the job's step
// summary. Object names, file paths and titles can come from the scanned
// manifests, so every cell is escaped: nothing in them can end a cell,
// a row or a code span, or add markup.
func WriteMarkdown(w io.Writer, r engine.Report) {
	verdict := string(r.Verdict)
	if r.Verdict == engine.VerdictUnknown {
		verdict += " (required checks were not assessed)"
	}
	fmt.Fprintf(w, "### upgradescope: %s\n\n", verdict)

	counts := map[engine.Severity]int{}
	for _, f := range r.Findings {
		counts[f.Severity]++
	}
	fmt.Fprintf(w, "Target **%s** · score **%d/100** · %s, %s, %d info · KB %s\n",
		r.Target, r.Score,
		plural(counts[engine.SevBlocker], "blocker"), plural(counts[engine.SevWarning], "warning"),
		counts[engine.SevInfo], mdCode(r.KBVersion))

	if len(r.Findings) == 0 {
		fmt.Fprintf(w, "\nNo findings.\n")
	} else {
		fmt.Fprintf(w, "\n| Severity | Finding | Objects | Remediation |\n|---|---|---|---|\n")
		for _, f := range r.Findings {
			fmt.Fprintf(w, "| %s | %s | %s | %s |\n",
				f.Severity, mdText(f.Title), mdObjects(f), mdText(f.Remediation))
		}
	}

	if len(r.NotAssessed) > 0 {
		fmt.Fprintf(w, "\n**Not assessed**\n\n")
		for _, g := range r.NotAssessed {
			req := ""
			if g.Required {
				req = " (required)"
			}
			fmt.Fprintf(w, "- %s%s: %s\n", g.Capability, req, mdText(g.Reason))
		}
	}
}

// mdObjects is a finding's Objects cell: "`file:line` [ns/]name (rendered
// from `template`)" per object, the first tableObjectLimit of them, then
// "…and N more", separated by <br>.
func mdObjects(f engine.Finding) string {
	shown := f.Objects
	if len(shown) > tableObjectLimit {
		shown = shown[:tableObjectLimit]
	}
	var lines []string
	for _, o := range shown {
		var parts []string
		switch {
		case o.File != "":
			parts = append(parts, mdCode(fmt.Sprintf("%s:%d", o.File, o.Line)))
		case o.Line > 0:
			parts = append(parts, fmt.Sprintf("line %d", o.Line))
		}
		name := o.Name
		if name == "" {
			name = "(unnamed)"
		}
		if o.Namespace != "" {
			name = o.Namespace + "/" + name
		}
		parts = append(parts, mdText(name))
		if o.RenderedFrom != "" {
			parts = append(parts, "(rendered from "+mdCode(o.RenderedFrom)+")")
		}
		lines = append(lines, strings.Join(parts, " "))
	}
	if more := len(f.Objects) - len(shown) + f.ObjectsOmitted; more > 0 {
		lines = append(lines, fmt.Sprintf("…and %d more", more))
	}
	return strings.Join(lines, "<br>")
}

// mdEscaper backslash-escapes the punctuation that is markup inside a
// table cell (CommonMark allows escaping any ASCII punctuation; GFM reads
// \| as a literal pipe), so a cell renders as the text it holds.
var mdEscaper = strings.NewReplacer(
	`\`, `\\`, "`", "\\`", "*", `\*`, "_", `\_`, "[", `\[`, "]", `\]`,
	"<", `\<`, ">", `\>`, "|", `\|`, "~", `\~`, "&", `\&`,
)

// mdText escapes s for a table cell and folds it onto one line.
func mdText(s string) string {
	return mdEscaper.Replace(oneLine(s))
}

// mdCode renders s as a code span in a table cell: the fence is one
// backtick longer than the longest backtick run in s, and | is escaped
// (GFM splits cells before parsing code spans).
func mdCode(s string) string {
	s = strings.ReplaceAll(oneLine(s), "|", `\|`)
	longest, run := 0, 0
	for _, c := range s {
		if c == '`' {
			run++
			longest = max(longest, run)
		} else {
			run = 0
		}
	}
	fence := strings.Repeat("`", longest+1)
	if strings.HasPrefix(s, "`") || strings.HasSuffix(s, "`") {
		s = " " + s + " "
	}
	return fence + s + fence
}

// oneLine joins s's lines with spaces: a newline would end the table row.
func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

func plural(n int, noun string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, noun)
	}
	return fmt.Sprintf("%d %ss", n, noun)
}
