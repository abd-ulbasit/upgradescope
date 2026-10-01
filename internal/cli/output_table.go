package cli

import (
	"fmt"
	"io"
	"sort"
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
			writeObjects(w, f)
			if len(f.Teams) > 0 {
				fmt.Fprintf(w, "      teams: %s\n", strings.Join(f.Teams, ", "))
			}
			if f.Remediation != "" {
				fmt.Fprintf(w, "      fix: %s\n", f.Remediation)
			}
			for _, c := range f.Citations {
				fmt.Fprintf(w, "      see: %s\n", c)
			}
		}
	}

	if len(r.Findings) == 0 {
		fmt.Fprintf(w, "\nNo findings.\n")
	}

	writeTeamsSection(w, r)

	if len(r.NotAssessed) > 0 {
		fmt.Fprintf(w, "\nNOT ASSESSED\n")
		for _, g := range r.NotAssessed {
			fmt.Fprintf(w, "  %s: %s\n", g.Capability, g.Reason)
		}
	}
}

// tableObjectLimit caps the objects listed under one finding so a large
// render stays readable; JSON and SARIF carry the full (capped) list.
const tableObjectLimit = 5

// writeObjects lists a finding's affected objects, "[ns/]name  file:line",
// with the Helm template each was rendered from, then "…and N more".
func writeObjects(w io.Writer, f engine.Finding) {
	shown := f.Objects
	if len(shown) > tableObjectLimit {
		shown = shown[:tableObjectLimit]
	}
	for _, o := range shown {
		name := o.Name
		if name == "" {
			name = "(unnamed)"
		}
		if o.Namespace != "" {
			name = o.Namespace + "/" + name
		}
		switch {
		case o.File != "":
			name += fmt.Sprintf("  %s:%d", o.File, o.Line)
		case o.Line > 0:
			name += fmt.Sprintf("  line %d", o.Line)
		}
		if o.RenderedFrom != "" {
			name += " (rendered from " + o.RenderedFrom + ")"
		}
		fmt.Fprintf(w, "      - %s\n", name)
	}
	if more := len(f.Objects) - len(shown) + f.ObjectsOmitted; more > 0 {
		fmt.Fprintf(w, "      …and %d more\n", more)
	}
}

// writeTeamsSection renders per-team scores, only when at least one finding
// is attributed to a named team. Teams sort alphabetically; the teamless
// bucket renders as "unattributed", last.
func writeTeamsSection(w io.Writer, r engine.Report) {
	scores := engine.TeamScores(r)
	names := make([]string, 0, len(scores))
	hasNamed := false
	for name := range scores {
		if name != "" {
			hasNamed = true
		}
		names = append(names, name)
	}
	if !hasNamed {
		return
	}
	sort.Slice(names, func(i, j int) bool {
		if (names[i] == "") != (names[j] == "") {
			return names[j] == "" // teamless bucket sorts last
		}
		return names[i] < names[j]
	})
	width := 0
	for _, name := range names {
		if name == "" {
			name = "unattributed"
		}
		if len(name) > width {
			width = len(name)
		}
	}
	fmt.Fprintf(w, "\nTEAMS\n")
	for _, name := range names {
		ts := scores[name]
		label := name
		if label == "" {
			label = "unattributed"
		}
		ready := "no"
		if ts.Ready {
			ready = "yes"
		}
		fmt.Fprintf(w, "  %-*s  %3d/100  ready %-3s  blockers %d  warnings %d\n",
			width, label, ts.Score, ready, ts.Blockers, ts.Warnings)
	}
}
