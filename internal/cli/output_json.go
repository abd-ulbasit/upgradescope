package cli

import (
	"encoding/json"
	"io"

	"github.com/abd-ulbasit/upgradescope/internal/engine"
)

// WriteJSON renders the report as canonical two-space-indented JSON with a
// trailing newline. This is the machine-readable contract; field names come
// from the engine.Report struct tags and must stay stable. A `teams` field
// (per-team scores, teamless bucket keyed "unattributed") is added at
// presentation time when any finding exists — it is computed here, never
// stored in the engine report.
func WriteJSON(w io.Writer, r engine.Report) error {
	out := struct {
		engine.Report
		Teams map[string]engine.TeamScore `json:"teams,omitempty"`
	}{Report: r, Teams: teamScoresForOutput(r)}
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
