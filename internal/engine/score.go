package engine

// Score implements the spec §6 formula:
//
//	score = max(0, 100 − min(75, 25·#blockers) − min(20, 5·#warnings))
//	ready = #blockers == 0
//
// Info findings are listed but never scored.
func Score(findings []Finding) (score int, ready bool) {
	var blockers, warnings int
	for _, f := range findings {
		switch f.Severity {
		case SevBlocker:
			blockers++
		case SevWarning:
			warnings++
		}
	}
	score = 100 - min(75, 25*blockers) - min(20, 5*warnings)
	if score < 0 {
		score = 0
	}
	return score, blockers == 0
}
