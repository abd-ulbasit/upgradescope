package engine

import (
	"testing"

	"github.com/abd-ulbasit/upgradescope/internal/inventory"
)

func TestScore(t *testing.T) {
	b := Finding{Severity: SevBlocker}
	w := Finding{Severity: SevWarning}
	i := Finding{Severity: SevInfo}
	cases := []struct {
		name     string
		findings []Finding
		score    int
		ready    bool
	}{
		{"no findings", nil, 100, true},
		{"one blocker", []Finding{b}, 75, false},
		{"three blockers", []Finding{b, b, b}, 25, false},
		{"four blockers hits 75 cap", []Finding{b, b, b, b}, 25, false},
		{"four warnings", []Finding{w, w, w, w}, 80, true},
		{"five warnings hits 20 cap", []Finding{w, w, w, w, w}, 80, true},
		{"one blocker two warnings", []Finding{b, w, w}, 65, false},
		{"both caps mixed", []Finding{b, b, b, b, w, w, w, w, w}, 5, false},
		{"infos never scored", []Finding{i, i, i}, 100, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			score, ready := Score(tc.findings)
			if score != tc.score || ready != tc.ready {
				t.Fatalf("Score() = (%d, %v), want (%d, %v)", score, ready, tc.score, tc.ready)
			}
		})
	}
}

// Rescore recomputes a report's score and verdict from its current
// findings: a caller that removes findings after Evaluate (suppression)
// must not leave the score and verdict of the findings it removed.
func TestRescore(t *testing.T) {
	required := []CapabilityGap{{Capability: inventory.CapAPIUsage, Reason: "forbidden", Required: true}}
	cases := []struct {
		name     string
		findings []Finding
		gaps     []CapabilityGap
		score    int
		verdict  Verdict
	}{
		{"no findings", nil, nil, 100, VerdictReady},
		{"blocker", []Finding{{Severity: SevBlocker}}, nil, 75, VerdictBlocked},
		{"warning only", []Finding{{Severity: SevWarning}}, nil, 95, VerdictReady},
		{"required gap", nil, required, 100, VerdictUnknown},
		{"blocker and required gap", []Finding{{Severity: SevBlocker}}, required, 75, VerdictBlocked},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := Report{Score: 1, Verdict: VerdictBlocked, Findings: tc.findings, NotAssessed: tc.gaps}
			r.Rescore()
			if r.Score != tc.score || r.Verdict != tc.verdict || r.Ready != (tc.verdict == VerdictReady) {
				t.Fatalf("Rescore() = score %d, verdict %s, ready %v; want %d, %s", r.Score, r.Verdict, r.Ready, tc.score, tc.verdict)
			}
		})
	}
}
