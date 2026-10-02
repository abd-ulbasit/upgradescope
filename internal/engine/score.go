package engine

// Score implements the spec §6 formula:
//
//	score = 100 − min(75, 25·#blockers) − min(20, 5·#warnings)
//	ready = #blockers == 0
//
// The caps bound the deductions, so the score never drops below 5.
// Info findings are listed but never scored. This ready is findings-only
// (per-team scores use it); a Report's readiness is its Verdict, which is
// also unknown when a required capability was not assessed.
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
	return 100 - min(75, 25*blockers) - min(20, 5*warnings), blockers == 0
}
