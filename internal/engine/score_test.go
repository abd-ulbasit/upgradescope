package engine

import "testing"

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
