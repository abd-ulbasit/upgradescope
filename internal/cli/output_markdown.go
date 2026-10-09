package cli

import (
	"fmt"
	"io"
	"strings"

	"github.com/abd-ulbasit/upgradescope/internal/collect"
	"github.com/abd-ulbasit/upgradescope/internal/engine"
	"github.com/abd-ulbasit/upgradescope/internal/suppress"
	"github.com/abd-ulbasit/upgradescope/internal/textsafe"
)

// WriteMarkdown renders the report as GitHub-flavoured Markdown: a header
// line (verdict, target, score, finding and suppressed counts, KB
// version); for a live scan, the kubeconfig context and API server it
// read; against a baseline, how many findings are new; one table row
// per finding (severity, baseline state when compared, title, objects as
// file:line, remediation); the suppressed findings with the reason each
// was accepted and what accepted it; then the NOT ASSESSED gaps and the
// image repositories no add-on registry entry matches, folded. The
// GitHub Action writes it to the job's step summary, where it has to say
// why a gate passed: blockers suppressed, or all in the baseline. Object
// names, file paths, titles and reasons can come from the scanned
// manifests or the config file, so every cell is escaped: nothing in them
// can end a cell, a row or a code span, or add markup, and a control
// character or bidi override in them is shown as an escape (textsafe), not
// written to the step summary or a log as itself.
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
	suppressed := ""
	if n := len(r.Suppressed); n > 0 {
		suppressed = fmt.Sprintf(" · %d suppressed", n)
	}
	fmt.Fprintf(w, "Target **%s** · score **%d/100** · %s, %s, %d info%s · KB %s\n",
		r.Target, r.Score,
		plural(counts[engine.SevBlocker], "blocker"), plural(counts[engine.SevWarning], "warning"),
		counts[engine.SevInfo], suppressed, mdCode(r.KBVersion))
	var cluster []string // live scans only: which cluster this was
	if r.KubeContext != "" {
		cluster = append(cluster, "Context "+mdCode(r.KubeContext))
	}
	if r.APIServer != "" {
		cluster = append(cluster, "API server "+mdCode(r.APIServer))
	}
	if len(cluster) > 0 {
		fmt.Fprintf(w, "\n%s\n", strings.Join(cluster, " · "))
	}

	added, unchanged := baselineCounts(r)
	compared := added+unchanged > 0
	if compared {
		fmt.Fprintf(w, "\n**Baseline:** %d new, %d unchanged. Only new findings fail the gate; score and verdict count both.\n", added, unchanged)
	}
	mdPlan(w, r)

	switch {
	case len(r.Findings) == 0 && len(r.Suppressed) > 0:
		fmt.Fprintf(w, "\nNo findings left after suppression.\n")
	case len(r.Findings) == 0:
		fmt.Fprintf(w, "\nNo findings.\n")
	case compared:
		fmt.Fprintf(w, "\n| Severity | Baseline | Finding | Objects | Remediation |\n|---|---|---|---|---|\n")
		for _, f := range r.Findings {
			state := "**new**"
			if f.BaselineState == engine.BaselineUnchanged {
				state = "unchanged"
			}
			fmt.Fprintf(w, "| %s | %s | %s | %s | %s |\n",
				f.Severity, state, mdText(f.Title), mdObjects(f), mdText(f.Remediation))
		}
	default:
		fmt.Fprintf(w, "\n| Severity | Finding | Objects | Remediation |\n|---|---|---|---|\n")
		for _, f := range r.Findings {
			fmt.Fprintf(w, "| %s | %s | %s | %s |\n",
				f.Severity, mdText(f.Title), mdObjects(f), mdText(f.Remediation))
		}
	}

	if len(r.Suppressed) > 0 {
		fmt.Fprintf(w, "\n**Suppressed (%d).** Accepted by an ignore rule or annotation, so they are not scored and do not fail the gate.\n", len(r.Suppressed))
		fmt.Fprintf(w, "\n| Severity | Finding | Objects | Reason | Suppressed by |\n|---|---|---|---|---|\n")
		for _, s := range r.Suppressed {
			fmt.Fprintf(w, "| %s | %s | %s | %s | %s |\n",
				s.Severity, mdText(s.Title), mdObjects(s.Finding), mdText(s.Reason), mdSuppressedBy(s))
		}
	}

	if len(r.NotAssessed) > 0 {
		fmt.Fprintf(w, "\n**Not assessed**\n\n")
		for _, g := range r.NotAssessed {
			skipped := ""
			if len(g.Skipped) > 0 {
				skipped = ". Skipped: " + mdText(strings.Join(g.Skipped, ", "))
			}
			fmt.Fprintf(w, "- %s: %s%s\n", esc(g.Label()), mdText(g.Reason), skipped)
		}
	}
	mdUnrecognizedImages(w, r)
}

// baselineCounts counts the findings marked new and unchanged; both are
// zero when the report was not compared with a baseline.
func baselineCounts(r engine.Report) (added, unchanged int) {
	for _, f := range r.Findings {
		switch f.BaselineState {
		case engine.BaselineNew:
			added++
		case engine.BaselineUnchanged:
			unchanged++
		}
	}
	return added, unchanged
}

// mdSuppressedBy is a suppressed finding's "Suppressed by" cell: the
// object annotation, or the rule's source (the config file path, or the
// agent's spec.ignore) as a code span, with the rule's expiry date.
func mdSuppressedBy(s engine.SuppressedFinding) string {
	var by string
	switch s.Source {
	case "":
	case suppress.AnnotationSource:
		by = mdCode(collect.IgnoreAnnotation) + " annotation"
	default:
		by = mdCode(s.Source)
	}
	if s.Expires != "" {
		by = strings.TrimSpace(by + " until " + mdText(s.Expires))
	}
	return by
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

// mdText escapes s for a table cell and folds it onto one line. The
// control characters are escaped before the markup is, so the backslash of
// an escape is escaped too and the cell shows \x1b, not the character.
func mdText(s string) string {
	return mdEscaper.Replace(oneLine(textsafe.Escape(s)))
}

// mdCode renders s as a code span in a table cell: the fence is one
// backtick longer than the longest backtick run in s, and | is escaped
// (GFM splits cells before parsing code spans). A backslash is left alone:
// GFM's cell splitter only treats \| as an escape and code spans take
// backslashes literally, so a\|b, written as a\\|b, stays in its cell and
// shows as a\|b (checked against GitHub's /markdown API). Escaping it as
// mdText does would show the extra backslash.
func mdCode(s string) string {
	s = strings.ReplaceAll(oneLine(textsafe.Escape(s)), "|", `\|`)
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

// oneLine collapses the runs of spaces that remain once the controls are
// escaped (the escape of a newline is two printable characters, so no line
// break is left that could end the table row).
func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

func plural(n int, noun string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, noun)
	}
	return fmt.Sprintf("%d %ss", n, noun)
}
