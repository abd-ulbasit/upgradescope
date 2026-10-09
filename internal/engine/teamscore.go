package engine

// UnattributedTeam is the name the presentation layers (CLI table and JSON,
// server API, dashboard) give the bucket of findings no team owns, which
// TeamScores keys by "". It carries parentheses, which a Kubernetes label
// value cannot, so no real team can be named it: a team called
// "unattributed" is a different row (#243).
const UnattributedTeam = "(unattributed)"

// TeamScore is one team's slice of a report, scored with the same spec §6
// formula as the cluster score, over only that team's findings.
type TeamScore struct {
	Score int `json:"score"`
	// Ready is Verdict == VerdictReady.
	Ready bool `json:"ready"`
	// Verdict is the team's readiness (see TeamScores): blocked by a
	// blocker of its own or one no team is attributed, otherwise unknown
	// when the report has a required gap, otherwise ready.
	Verdict  Verdict `json:"verdict"`
	Blockers int     `json:"blockers"`
	Warnings int     `json:"warnings"`
}

// TeamScores groups a report's findings by team and scores each subset with
// Score. A finding attributed to N teams counts for each of them; a finding
// with no teams is attributed to the empty-string team "" (callers render it
// as UnattributedTeam). Pure: no I/O, no clock — same contract as Evaluate.
// The result is deliberately NOT part of engine.Report; servers and CLIs
// compute it at presentation time.
//
// A team's verdict follows the report's rules (verdictFor) over what can
// concern it (#196): it is blocked by a blocker of its own, or by an
// unattributed one, which cannot be ruled out as the team's and blocks the
// cluster's upgrade either way; otherwise unknown on any required gap, which
// may have hidden a blocker of any team; otherwise ready. Score, Blockers and
// Warnings stay the team's own. Another team's blocker leaves it ready.
func TeamScores(r Report) map[string]TeamScore {
	byTeam := map[string][]Finding{}
	unattributedBlocker := false
	for _, f := range r.Findings {
		teams := f.Teams
		if len(teams) == 0 {
			teams = []string{""}
			unattributedBlocker = unattributedBlocker || f.Severity == SevBlocker
		}
		for _, team := range teams {
			byTeam[team] = append(byTeam[team], f)
		}
	}
	out := make(map[string]TeamScore, len(byTeam))
	for team, fs := range byTeam {
		score, _ := Score(fs)
		ts := TeamScore{Score: score}
		for _, f := range fs {
			switch f.Severity {
			case SevBlocker:
				ts.Blockers++
			case SevWarning:
				ts.Warnings++
			}
		}
		ts.Verdict = verdictFor(fs, r.NotAssessed)
		if unattributedBlocker {
			ts.Verdict = VerdictBlocked
		}
		ts.Ready = ts.Verdict == VerdictReady
		out[team] = ts
	}
	return out
}
