package server

import (
	"net/http"

	"github.com/abd-ulbasit/upgradescope/internal/engine"
)

// renderTeamScores maps engine.TeamScores output for the wire: the
// empty-string team (findings no team owns) renders as
// engine.UnattributedTeam. No label value can take that name (a label value
// holds no parentheses) and ParseTeamMap refuses it, so a real team called
// "unattributed" stays its own row and nothing else is keyed the same.
// Should a team of that very name reach here anyway (a stored map from
// before the name was reserved), the two are never left to overwrite each
// other: they are one row that keeps every blocker and warning and takes the
// lower score and the worse verdict, so nothing disappears.
func renderTeamScores(m map[string]engine.TeamScore) map[string]engine.TeamScore {
	ts, ok := m[""]
	if !ok {
		return m
	}
	out := make(map[string]engine.TeamScore, len(m))
	for k, v := range m {
		out[k] = v
	}
	delete(out, "")
	if real, clash := out[engine.UnattributedTeam]; clash {
		ts = mergeTeamScores(real, ts)
	}
	out[engine.UnattributedTeam] = ts
	return out
}

// mergeTeamScores is two rows that must share one name: the counts add, the
// score is the lower and the verdict the worse, so neither hides the other.
func mergeTeamScores(a, b engine.TeamScore) engine.TeamScore {
	v := worseVerdict(a.Verdict, b.Verdict)
	return engine.TeamScore{
		Score:    min(a.Score, b.Score),
		Verdict:  v,
		Ready:    v == engine.VerdictReady,
		Blockers: a.Blockers + b.Blockers,
		Warnings: a.Warnings + b.Warnings,
	}
}

// reportWithTeams decorates an engine.Report with per-team scores at the
// presentation layer — Teams is computed, never stored, so the engine's
// Report contract stays pure. Envelope (schemaVersion, toolVersion) leads
// the JSON; withTeams leaves it zero and the handler sets it (versioned).
type reportWithTeams struct {
	engine.Envelope
	engine.Report
	Teams map[string]engine.TeamScore `json:"teams,omitempty"`
}

func withTeams(rep engine.Report) reportWithTeams {
	return withTeamsIn(rep, fleetScope, nil)
}

// withTeamsIn is withTeams as sc sees it: the team scores of its teams,
// computed from the whole report, and the findings it owns, cut to it by
// ns, the evaluated inventory's namespace teams (readScope.report).
func withTeamsIn(rep engine.Report, sc readScope, ns map[string]string) reportWithTeams {
	return reportWithTeams{Report: sc.report(rep, ns), Teams: sc.renderedTeams(rep)}
}

// handleTeams: GET /api/v1/clusters/{id}/teams?target= — per-team readiness
// scores computed from the same report the report endpoint serves (current
// stored evaluation, else what-if), with the same evaluatedAt/snapshotId/
// source metadata.
func (s *Server) handleTeams(w http.ResponseWriter, r *http.Request) {
	rep, meta, ok := s.reportForRequest(w, r)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, struct {
		Target string                      `json:"target"`
		Teams  map[string]engine.TeamScore `json:"teams"`
		reportMeta
	}{rep.Target.String(), scopeOf(r).renderedTeams(rep), meta})
}
