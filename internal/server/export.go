package server

import (
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/abd-ulbasit/upgradescope/internal/engine"
	"github.com/abd-ulbasit/upgradescope/internal/server/store"
)

// handleExport: GET /api/v1/clusters/{id}/export?target=&format=csv|html —
// auditor-facing report export from the current STORED evaluation (of the
// latest snapshot; no recompute: an audit artifact must reflect what the
// system actually recorded, evaluatedAt included). 404 when no such
// evaluation exists for the target, or when the cluster already runs it.
func (s *Server) handleExport(w http.ResponseWriter, r *http.Request) {
	c, ok := s.requireCluster(w, r)
	if !ok {
		return
	}
	target, ok := s.resolveTarget(w, r, c.ID)
	if !ok {
		return
	}
	format := r.URL.Query().Get("format")
	switch format {
	case "csv", "html":
	default:
		errJSON(w, http.StatusUnprocessableEntity, fmt.Sprintf("invalid format %q (want csv or html)", format))
		return
	}

	ctx := r.Context()
	snap, inv, err := s.latestInventory(ctx, c.ID)
	if errors.Is(err, store.ErrNotFound) {
		errJSON(w, http.StatusNotFound, "no snapshots for cluster")
		return
	}
	if err != nil {
		internalErr(w, "loading latest snapshot", err)
		return
	}
	if version := judgedAt(snap, inv); notApplicable(version, target) {
		errJSON(w, http.StatusNotFound, fmt.Sprintf("cluster %s already runs %s: target %s is not applicable", c.Name, version, target))
		return
	}
	eval, err := s.cfg.Store.CurrentEvaluation(ctx, c.ID, target.String())
	if errors.Is(err, store.ErrNotFound) {
		errJSON(w, http.StatusNotFound, "no stored evaluation of the latest snapshot for target "+target.String()+
			" (exports cover the default target and --targets; a newly added target is evaluated on the next push or hourly pass)")
		return
	}
	if err != nil {
		internalErr(w, "loading evaluation", err)
		return
	}
	var rep engine.Report
	if err := json.Unmarshal(eval.Report, &rep); err != nil {
		internalErr(w, "decoding stored report", fmt.Errorf("evaluation %d: %w", eval.ID, err))
		return
	}

	if format == "csv" {
		w.Header().Set("Content-Type", "text/csv; charset=utf-8")
		w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", exportFilename(c.Name, target.String(), "csv")))
		w.WriteHeader(http.StatusOK)
		_ = writeExportCSV(w, c.Name, eval, rep)
		return
	}

	history, err := s.cfg.Store.ScoreHistory(ctx, c.ID, target.String(), 100)
	if err != nil {
		internalErr(w, "loading history", err)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_ = writeExportHTML(w, exportData{
		Cluster:     c.Name,
		Target:      target.String(),
		Eval:        eval,
		Report:      rep,
		History:     history,
		GeneratedAt: s.now(),
	})
}

func exportFilename(cluster, target, ext string) string {
	return fmt.Sprintf("upgradescope-%s-%s.%s", cluster, target, ext)
}

// csvSafe guards against spreadsheet formula injection: cluster names,
// namespaces, and team labels are attacker-influenceable, and a cell
// starting with = + - @ (or a tab/CR remnant) executes as a formula when
// the CSV is opened in Excel/Sheets. Prefixing with ' forces text.
func csvSafe(s string) string {
	if s == "" {
		return s
	}
	switch s[0] {
	case '=', '+', '-', '@', '\t', '\r':
		return "'" + s
	}
	return s
}

// CSV row types beyond the finding severities (blocker, warning, info), in
// the severity column so consumers filter on one column.
const (
	csvSummary     = "summary"      // first row: the verdict, always present
	csvNotAssessed = "not-assessed" // one per capability the evaluation could not assess
)

// writeExportCSV emits a summary row (verdict and score in the title; the
// KB version and evaluation time are columns of every row), one row per
// finding, then one not-assessed row per capability gap — so a clean
// cluster's export is not a bare header, and a partly assessed one does
// not look clean. Multi-valued columns (teams, namespaces, citations) are
// ";"-joined inside a single CSV field. All non-numeric fields pass
// through csvSafe.
func writeExportCSV(w io.Writer, cluster string, eval store.Evaluation, rep engine.Report) error {
	cw := csv.NewWriter(w)
	if err := cw.Write([]string{
		"cluster", "target", "evaluatedAt", "kbVersion", "verdict", "score", "severity", "category", "key",
		"title", "detail", "remediation", "teams", "namespaces", "citations",
	}); err != nil {
		return err
	}
	row := func(severity, category, key, title, detail, remediation string, teams, namespaces, citations []string) error {
		return cw.Write([]string{
			csvSafe(cluster),
			rep.Target.String(),
			eval.EvaluatedAt.UTC().Format(time.RFC3339),
			csvSafe(rep.KBVersion),
			string(rep.Verdict),
			strconv.Itoa(rep.Score),
			severity,
			category,
			csvSafe(key),
			csvSafe(title),
			csvSafe(detail),
			csvSafe(remediation),
			csvSafe(strings.Join(teams, ";")),
			csvSafe(strings.Join(namespaces, ";")),
			csvSafe(strings.Join(citations, ";")),
		})
	}
	var blockers, warnings int
	for _, f := range rep.Findings {
		switch f.Severity {
		case engine.SevBlocker:
			blockers++
		case engine.SevWarning:
			warnings++
		}
	}
	summary := fmt.Sprintf("%s for %s: score %d/100", rep.Verdict, rep.Target, rep.Score)
	detail := fmt.Sprintf("%d blockers, %d warnings, %d capabilities not assessed", blockers, warnings, len(rep.NotAssessed))
	if err := row(csvSummary, "", "", summary, detail, "", nil, nil, nil); err != nil {
		return err
	}
	for _, f := range rep.Findings {
		if err := row(string(f.Severity), string(f.Category), f.Key, f.Title, f.Detail, f.Remediation, f.Teams, f.Namespaces, f.Citations); err != nil {
			return err
		}
	}
	for _, g := range rep.NotAssessed {
		title := string(g.Capability) + " not assessed"
		if g.Required {
			title += " (required: the verdict cannot be ready)"
		}
		if err := row(csvNotAssessed, "capability", string(g.Capability), title, g.Reason, "", nil, nil, nil); err != nil {
			return err
		}
	}
	cw.Flush()
	return cw.Error()
}

// exportData feeds the self-contained HTML report template.
type exportData struct {
	Cluster     string
	Target      string
	Eval        store.Evaluation
	Report      engine.Report
	History     []store.ScorePoint
	GeneratedAt time.Time
}

type severityGroup struct {
	Severity string
	Findings []engine.Finding
}

// Groups returns the report's findings bucketed by severity, blockers first,
// skipping empty buckets (findings arrive pre-sorted from the engine).
func (d exportData) Groups() []severityGroup {
	var out []severityGroup
	for _, sev := range []engine.Severity{engine.SevBlocker, engine.SevWarning, engine.SevInfo} {
		var g []engine.Finding
		for _, f := range d.Report.Findings {
			if f.Severity == sev {
				g = append(g, f)
			}
		}
		if len(g) > 0 {
			out = append(out, severityGroup{Severity: string(sev), Findings: g})
		}
	}
	return out
}

// ScoreClass picks the badge color bucket: ok ≥90, warn ≥70, else bad.
func (d exportData) ScoreClass() string {
	switch {
	case d.Report.Score >= 90:
		return "ok"
	case d.Report.Score >= 70:
		return "warn"
	default:
		return "bad"
	}
}

// Sparkline renders the score history as an inline SVG polyline (no JS, no
// CDN — the export must be a single self-contained file). X is time, from
// the first point to the last (points spaced by index made a burst of
// changes look like months), Y maps score 0–100 onto the viewbox, and the
// label names the dates spanned; a single point renders as just the dot.
func (d exportData) Sparkline() template.HTML {
	const width, height, pad = 260.0, 48.0, 4.0
	n := len(d.History)
	if n == 0 {
		return ""
	}
	first, last := d.History[0].At, d.History[n-1].At
	span := last.Sub(first)
	x := func(i int) float64 {
		if n == 1 || span <= 0 {
			if n == 1 {
				return width / 2
			}
			return pad + (width-2*pad)*float64(i)/float64(n-1) // no time spread: fall back to index
		}
		return pad + (width-2*pad)*float64(d.History[i].At.Sub(first))/float64(span)
	}
	y := func(score int) float64 {
		return pad + (height-2*pad)*(1-float64(score)/100)
	}
	var b strings.Builder
	fmt.Fprintf(&b, `<svg class="spark" width="%.0f" height="%.0f" viewBox="0 0 %.0f %.0f" role="img" aria-label="score history, %s to %s">`,
		width, height, width, height, first.UTC().Format("2006-01-02"), last.UTC().Format("2006-01-02"))
	if n > 1 {
		b.WriteString(`<polyline fill="none" stroke="#2563eb" stroke-width="2" points="`)
		for i, p := range d.History {
			if i > 0 {
				b.WriteByte(' ')
			}
			fmt.Fprintf(&b, "%.1f,%.1f", x(i), y(p.Score))
		}
		b.WriteString(`"/>`)
	}
	fmt.Fprintf(&b, `<circle cx="%.1f" cy="%.1f" r="3" fill="#2563eb"/>`, x(n-1), y(d.History[n-1].Score))
	b.WriteString(`</svg>`)
	return template.HTML(b.String()) // #nosec G203 — built from numbers and formatted dates only
}

var exportTemplate = template.Must(template.New("export").Parse(`<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<title>upgradescope report — {{.Cluster}} → {{.Target}}</title>
<style>
  :root { color-scheme: light; }
  body { font: 14px/1.5 -apple-system, "Segoe UI", Roboto, Helvetica, Arial, sans-serif; color: #111827; margin: 2rem auto; max-width: 60rem; padding: 0 1rem; }
  h1 { font-size: 1.3rem; margin: 0 0 .25rem; }
  h2 { font-size: 1rem; text-transform: uppercase; letter-spacing: .05em; margin: 1.5rem 0 .5rem; }
  .meta { color: #6b7280; margin: 0 0 1rem; }
  .meta span { margin-right: 1.25rem; }
  .badge { display: inline-block; font-weight: 700; padding: .15rem .6rem; border-radius: .375rem; color: #fff; }
  .badge.ok { background: #16a34a; } .badge.warn { background: #d97706; } .badge.bad { background: #dc2626; }
  table { border-collapse: collapse; width: 100%; }
  th, td { text-align: left; vertical-align: top; padding: .4rem .6rem; border-bottom: 1px solid #e5e7eb; }
  th { background: #f9fafb; font-weight: 600; }
  .sev-blocker td:first-child { color: #dc2626; font-weight: 700; }
  .sev-warning td:first-child { color: #d97706; font-weight: 700; }
  .sev-info td:first-child { color: #6b7280; }
  .detail { color: #4b5563; }
  .cites a { color: #2563eb; word-break: break-all; }
  .spark { vertical-align: middle; }
  .gaps { color: #6b7280; }
  @media print {
    body { margin: 0; max-width: none; }
    a { color: inherit; text-decoration: none; }
    .badge { -webkit-print-color-adjust: exact; print-color-adjust: exact; }
  }
</style>
</head>
<body>
<h1>upgradescope upgrade-readiness report</h1>
<p class="meta">
  <span>Cluster: <strong>{{.Cluster}}</strong></span>
  <span>Target: <strong>{{.Target}}</strong></span>
  <span>KB: {{.Report.KBVersion}}</span>
  <span>Evaluated: {{.Eval.EvaluatedAt.UTC.Format "2006-01-02 15:04 UTC"}}</span>
  <span>Generated: {{.GeneratedAt.UTC.Format "2006-01-02 15:04 UTC"}}</span>
</p>
<p>
  <span class="badge {{.ScoreClass}}">score {{.Report.Score}}/100</span>
  {{if .Report.Ready}}<span class="badge ok">ready</span>{{else}}<span class="badge bad">not ready</span>{{end}}
  {{.Sparkline}}
</p>
{{range .Groups}}
<h2>{{.Severity}} ({{len .Findings}})</h2>
<table>
  <tr><th>severity</th><th>category</th><th>finding</th><th>teams</th><th>namespaces</th></tr>
  {{range .Findings}}
  <tr class="sev-{{.Severity}}">
    <td>{{.Severity}}</td>
    <td>{{.Category}}</td>
    <td>
      <strong>{{.Title}}</strong>
      {{if .Detail}}<div class="detail">{{.Detail}}</div>{{end}}
      {{if .Remediation}}<div class="detail">Remediation: {{.Remediation}}</div>{{end}}
      {{if .Citations}}<div class="cites">{{range .Citations}}<a href="{{.}}">{{.}}</a> {{end}}</div>{{end}}
    </td>
    <td>{{range $i, $t := .Teams}}{{if $i}}, {{end}}{{$t}}{{end}}</td>
    <td>{{range $i, $n := .Namespaces}}{{if $i}}, {{end}}{{$n}}{{end}}</td>
  </tr>
  {{end}}
</table>
{{else}}
<p>No findings.</p>
{{end}}
{{if .Report.NotAssessed}}
<h2>not assessed</h2>
<ul class="gaps">
{{range .Report.NotAssessed}}<li>{{.Capability}}: {{.Reason}}</li>
{{end}}</ul>
{{end}}
</body>
</html>
`))

func writeExportHTML(w io.Writer, d exportData) error {
	return exportTemplate.Execute(w, d)
}
