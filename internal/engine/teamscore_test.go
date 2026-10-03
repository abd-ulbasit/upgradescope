package engine

import (
	"reflect"
	"testing"

	"github.com/abd-ulbasit/upgradescope/internal/inventory"
)

func TestTeamScores(t *testing.T) {
	requiredGap := []CapabilityGap{{Capability: inventory.CapAPIUsage, Reason: "discovery: forbidden", Required: true}}
	optionalGap := []CapabilityGap{{Capability: inventory.CapHelm, Reason: "secrets list forbidden"}}
	cases := []struct {
		name     string
		findings []Finding
		gaps     []CapabilityGap
		want     map[string]TeamScore
	}{
		{
			name:     "no findings → empty map",
			findings: nil,
			want:     map[string]TeamScore{},
		},
		{
			name: "single team, one blocker",
			findings: []Finding{
				{Severity: SevBlocker, Teams: []string{"payments"}},
			},
			want: map[string]TeamScore{
				"payments": {Score: 75, Ready: false, Verdict: VerdictBlocked, Blockers: 1},
			},
		},
		{
			name: "finding with N teams counts for each",
			findings: []Finding{
				{Severity: SevBlocker, Teams: []string{"payments", "platform"}},
				{Severity: SevWarning, Teams: []string{"platform"}},
			},
			want: map[string]TeamScore{
				"payments": {Score: 75, Ready: false, Verdict: VerdictBlocked, Blockers: 1},
				"platform": {Score: 70, Ready: false, Verdict: VerdictBlocked, Blockers: 1, Warnings: 1},
			},
		},
		{
			name: "another team's blocker does not block a team",
			findings: []Finding{
				{Severity: SevBlocker, Teams: []string{"data"}},
				{Severity: SevWarning, Teams: []string{"shop"}},
			},
			want: map[string]TeamScore{
				"data": {Score: 75, Ready: false, Verdict: VerdictBlocked, Blockers: 1},
				"shop": {Score: 95, Ready: true, Verdict: VerdictReady, Warnings: 1},
			},
		},
		{
			name: "teamless findings → team \"\"",
			findings: []Finding{
				{Severity: SevWarning},
				{Severity: SevInfo},
				{Severity: SevBlocker, Teams: []string{"data"}},
			},
			want: map[string]TeamScore{
				"":     {Score: 95, Ready: true, Verdict: VerdictReady, Warnings: 1},
				"data": {Score: 75, Ready: false, Verdict: VerdictBlocked, Blockers: 1},
			},
		},
		{
			name: "info-only team scores 100 ready",
			findings: []Finding{
				{Severity: SevInfo, Teams: []string{"shop"}},
			},
			want: map[string]TeamScore{
				"shop": {Score: 100, Ready: true, Verdict: VerdictReady},
			},
		},
		// #196 VS-14: a team's verdict is only as ready as the report could
		// assess. A required gap may hide any team's blocker, so every team
		// is unknown (blocked if it has a blocker of its own).
		{
			name: "required gap makes every team unknown",
			findings: []Finding{
				{Severity: SevWarning, Teams: []string{"payments"}},
				{Severity: SevInfo},
				{Severity: SevBlocker, Teams: []string{"data"}},
			},
			gaps: requiredGap,
			want: map[string]TeamScore{
				"":         {Score: 100, Ready: false, Verdict: VerdictUnknown},
				"payments": {Score: 95, Ready: false, Verdict: VerdictUnknown, Warnings: 1},
				"data":     {Score: 75, Ready: false, Verdict: VerdictBlocked, Blockers: 1},
			},
		},
		{
			name: "an optional gap leaves teams ready",
			findings: []Finding{
				{Severity: SevWarning, Teams: []string{"payments"}},
			},
			gaps: optionalGap,
			want: map[string]TeamScore{
				"payments": {Score: 95, Ready: true, Verdict: VerdictReady, Warnings: 1},
			},
		},
		// A cluster-wide blocker attributed to no team (kubelet skew, an
		// object in an unlabelled namespace) cannot be ruled out as any
		// team's: every team is blocked, its own score and counts
		// unchanged. It beats a required gap, as in the report's verdict.
		{
			name: "unattributed blocker blocks every team",
			findings: []Finding{
				{Severity: SevBlocker},
				{Severity: SevWarning, Teams: []string{"payments"}},
				{Severity: SevInfo, Teams: []string{"frontend"}},
			},
			gaps: requiredGap,
			want: map[string]TeamScore{
				"":         {Score: 75, Ready: false, Verdict: VerdictBlocked, Blockers: 1},
				"payments": {Score: 95, Ready: false, Verdict: VerdictBlocked, Warnings: 1},
				"frontend": {Score: 100, Ready: false, Verdict: VerdictBlocked},
			},
		},
		{
			name: "score floors at 5 (same formula as Score)",
			findings: []Finding{
				{Severity: SevBlocker, Teams: []string{"x"}},
				{Severity: SevBlocker, Teams: []string{"x"}},
				{Severity: SevBlocker, Teams: []string{"x"}},
				{Severity: SevBlocker, Teams: []string{"x"}},
				{Severity: SevWarning, Teams: []string{"x"}},
				{Severity: SevWarning, Teams: []string{"x"}},
				{Severity: SevWarning, Teams: []string{"x"}},
				{Severity: SevWarning, Teams: []string{"x"}},
				{Severity: SevWarning, Teams: []string{"x"}},
			},
			want: map[string]TeamScore{
				"x": {Score: 5, Ready: false, Verdict: VerdictBlocked, Blockers: 4, Warnings: 5},
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := TeamScores(Report{Findings: tc.findings, NotAssessed: tc.gaps})
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("TeamScores = %+v, want %+v", got, tc.want)
			}
		})
	}
}
