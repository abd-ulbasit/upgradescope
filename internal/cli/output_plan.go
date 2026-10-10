package cli

import (
	"fmt"
	"io"
	"strings"

	"github.com/abd-ulbasit/upgradescope/internal/engine"
)

// writePlan renders scan --plan's hops for the table: per upgrade, its
// verdict and counts, the findings it adds ("+"), the ones whose severity
// changes ("~", naming the upgrade that listed them), and how many it
// carries from earlier upgrades. Nothing without hops.
func writePlan(w io.Writer, r engine.Report) {
	if len(r.Hops) == 0 {
		return
	}
	first, last := r.Hops[0], r.Hops[len(r.Hops)-1]
	fmt.Fprintf(w, "\nUPGRADE PLAN  %s → %s in %s; each finding is listed at the first upgrade it affects\n",
		first.From, last.To, plural(len(r.Hops), "upgrade"))
	for _, h := range r.Hops {
		fmt.Fprintf(w, "  %s → %s  %-7s  %s, %s, %d info\n", h.From, h.To, h.Verdict,
			plural(h.Count(engine.SevBlocker), "blocker"), plural(h.Count(engine.SevWarning), "warning"), h.Count(engine.SevInfo))
		for _, f := range h.Findings {
			fmt.Fprintf(w, "      + %-7s  [%s] %s\n", f.Severity, esc(string(f.Category)), esc(f.Title))
		}
		for _, c := range h.Changed {
			fmt.Fprintf(w, "      ~ %s → %s  %s (listed at %s)\n", c.Was, c.Severity, esc(c.Title), c.Since)
		}
		switch carried := len(h.Carried); {
		case carried > 0 && len(h.Findings)+len(h.Changed) == 0:
			fmt.Fprintf(w, "      nothing new; %d carried from earlier upgrades\n", carried)
		case carried > 0:
			fmt.Fprintf(w, "      %d carried from earlier upgrades\n", carried)
		case len(h.Findings)+len(h.Changed) == 0:
			fmt.Fprintf(w, "      no findings\n")
		}
	}
}

// mdPlan renders scan --plan's hops for Markdown: one table row per
// upgrade with its verdict, counts, the findings it adds and the severity
// changes. Nothing without hops.
func mdPlan(w io.Writer, r engine.Report) {
	if len(r.Hops) == 0 {
		return
	}
	first, last := r.Hops[0], r.Hops[len(r.Hops)-1]
	fmt.Fprintf(w, "\n**Upgrade plan:** %s → %s in %s. Each finding is listed at the first upgrade it affects.\n",
		first.From, last.To, plural(len(r.Hops), "upgrade"))
	fmt.Fprintf(w, "\n| Upgrade | Verdict | Blockers | Warnings | New findings | Severity changes |\n|---|---|---|---|---|---|\n")
	for _, h := range r.Hops {
		added := make([]string, 0, len(h.Findings))
		for _, f := range h.Findings {
			added = append(added, fmt.Sprintf("%s: %s", f.Severity, mdText(f.Title)))
		}
		changed := make([]string, 0, len(h.Changed))
		for _, c := range h.Changed {
			changed = append(changed, fmt.Sprintf("%s → %s: %s", c.Was, c.Severity, mdText(c.Title)))
		}
		fmt.Fprintf(w, "| %s → %s | %s | %d | %d | %s | %s |\n", h.From, h.To, h.Verdict,
			h.Count(engine.SevBlocker), h.Count(engine.SevWarning), strings.Join(added, "<br>"), strings.Join(changed, "<br>"))
	}
}
