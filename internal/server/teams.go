package server

import (
	"net/http"

	"github.com/abd-ulbasit/upgradescope/internal/engine"
)

// unattributedTeam is the rendered key for findings carrying no team.
const unattributedTeam = "unattributed"

// renderTeamScores maps engine.TeamScores output for the wire: the
// empty-string team renders as "unattributed". If a real team is literally
// named "unattributed" (pathological), the "" entry keeps its empty key
// rather than silently merging two different scores under one name.
func renderTeamScores(m map[string]engine.TeamScore) map[string]engine.TeamScore {
	ts, ok := m[""]
	if !ok {
		return m
	}
	out := make(map[string]engine.TeamScore, len(m))
	for k, v := range m {
		out[k] = v
	}
	if _, taken := out[unattributedTeam]; !taken {
		delete(out, "")
		out[unattributedTeam] = ts
	}
	return out
}

// reportWithTeams decorates an engine.Report with per-team scores at the
// presentation layer — Teams is computed, never stored, so the engine's
// Report contract stays pure.
type reportWithTeams struct {
	engine.Report
	Teams map[string]engine.TeamScore `json:"teams,omitempty"`
}

func withTeams(rep engine.Report) reportWithTeams {
	return reportWithTeams{Report: rep, Teams: renderTeamScores(engine.TeamScores(rep))}
}

// handleTeams: GET /api/v1/clusters/{id}/teams?target= — per-team readiness
// scores computed from the same report the report endpoint serves (stored
// evaluation, else what-if).
func (s *Server) handleTeams(w http.ResponseWriter, r *http.Request) {
	rep, ok := s.reportForRequest(w, r)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"target": rep.Target.String(),
		"teams":  renderTeamScores(engine.TeamScores(rep)),
	})
}
