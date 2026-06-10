package cli

import (
	"fmt"
	"io"
	"strings"

	"github.com/abd-ulbasit/upgradescope/internal/engine"
)

// WriteTable renders a human-readable plain-text report. No ANSI escape
// codes are emitted (NO_COLOR-safe by construction). Findings arrive
// pre-sorted from the engine (severity desc, category, title); we only
// group them under severity headers.
func WriteTable(w io.Writer, r engine.Report) {
	fmt.Fprintln(w, "upgradescope upgrade readiness report")
	fmt.Fprintln(w)
	fmt.Fprintf(w, "Cluster:  %s\n", r.ClusterID)
	fmt.Fprintf(w, "Target:   %s\n", r.Target)
	fmt.Fprintf(w, "KB:       %s\n", r.KBVersion)
	fmt.Fprintln(w)
	fmt.Fprintf(w, "SCORE  %d/100\n", r.Score)
	ready := "no"
	if r.Ready {
		ready = "yes"
	}
	fmt.Fprintf(w, "READY  %s\n", ready)

	for _, sev := range []engine.Severity{engine.SevBlocker, engine.SevWarning, engine.SevInfo} {
		var group []engine.Finding
		for _, f := range r.Findings {
			if f.Severity == sev {
				group = append(group, f)
			}
		}
		if len(group) == 0 {
			continue
		}
		fmt.Fprintf(w, "\n%s (%d)\n", strings.ToUpper(string(sev)), len(group))
		for _, f := range group {
			fmt.Fprintf(w, "  [%s] %s\n", f.Category, f.Title)
			if f.Detail != "" {
				fmt.Fprintf(w, "      %s\n", f.Detail)
			}
			if len(f.Teams) > 0 {
				fmt.Fprintf(w, "      teams: %s\n", strings.Join(f.Teams, ", "))
			}
		}
	}

	if len(r.Findings) == 0 {
		fmt.Fprintf(w, "\nNo findings.\n")
	}

	if len(r.NotAssessed) > 0 {
		fmt.Fprintf(w, "\nNOT ASSESSED\n")
		for _, g := range r.NotAssessed {
			fmt.Fprintf(w, "  %s: %s\n", g.Capability, g.Reason)
		}
	}
}
