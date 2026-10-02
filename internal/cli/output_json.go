package cli

import (
	"encoding/json"
	"io"

	"github.com/abd-ulbasit/upgradescope/internal/engine"
)

// reportSchemaVersion versions the shape of the JSON report, shared with the
// server's report-shaped responses (engine.ReportSchemaVersion, which states
// the stability promise). toolVersion is informational (the binary that
// produced the report) and carries no compatibility meaning.
const reportSchemaVersion = engine.ReportSchemaVersion

// WriteJSON renders the report as canonical two-space-indented JSON with a
// trailing newline. This is the machine-readable contract; field names come
// from the engine.Report struct tags and must stay stable (see
// reportSchemaVersion). schemaVersion and toolVersion lead the object. A
// `teams` field (per-team scores, teamless bucket keyed "unattributed") is
// added at presentation time when any finding exists — it is computed here,
// never stored in the engine report.
func WriteJSON(w io.Writer, r engine.Report) error {
	return writeJSON(w, r, nil)
}

// writeJSON is WriteJSON for a --files scan: filesBase (when non-nil) is
// emitted as `filesBase`, the scanned directory relative to the working
// directory ("" = the working directory itself; absolute when outside it)
// that the findings' object file paths are relative to.
func writeJSON(w io.Writer, r engine.Report, filesBase *string) error {
	out := struct {
		engine.Envelope
		FilesBase *string `json:"filesBase,omitempty"`
		engine.Report
		Teams map[string]engine.TeamScore `json:"teams,omitempty"`
	}{Envelope: engine.NewEnvelope(version), FilesBase: filesBase, Report: r, Teams: teamScoresForOutput(r)}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(out)
}

// teamScoresForOutput computes presentation-time team scores, renaming the
// engine's teamless "" bucket to "unattributed" (unless a real team already
// claims that name — then the "" key is kept rather than merging scores).
func teamScoresForOutput(r engine.Report) map[string]engine.TeamScore {
	scores := engine.TeamScores(r)
	ts, ok := scores[""]
	if !ok {
		return scores
	}
	if _, taken := scores["unattributed"]; !taken {
		delete(scores, "")
		scores["unattributed"] = ts
	}
	return scores
}
