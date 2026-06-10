package engine

// TeamScore is one team's slice of a report, scored with the same spec §6
// formula as the cluster score, over only that team's findings.
type TeamScore struct {
	Score    int  `json:"score"`
	Ready    bool `json:"ready"`
	Blockers int  `json:"blockers"`
	Warnings int  `json:"warnings"`
}

// TeamScores groups a report's findings by team and scores each subset with
// Score. A finding attributed to N teams counts for each of them; a finding
// with no teams is attributed to the empty-string team "" (callers render it
// as "unattributed"). Pure: no I/O, no clock — same contract as Evaluate.
// The result is deliberately NOT part of engine.Report; servers and CLIs
// compute it at presentation time.
func TeamScores(r Report) map[string]TeamScore {
	byTeam := map[string][]Finding{}
	for _, f := range r.Findings {
		teams := f.Teams
		if len(teams) == 0 {
			teams = []string{""}
		}
		for _, team := range teams {
			byTeam[team] = append(byTeam[team], f)
		}
	}
	out := make(map[string]TeamScore, len(byTeam))
	for team, fs := range byTeam {
		score, ready := Score(fs)
		ts := TeamScore{Score: score, Ready: ready}
		for _, f := range fs {
			switch f.Severity {
			case SevBlocker:
				ts.Blockers++
			case SevWarning:
				ts.Warnings++
			}
		}
		out[team] = ts
	}
	return out
}
