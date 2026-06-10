package engine

import (
	"reflect"
	"testing"
)

func TestTeamScores(t *testing.T) {
	cases := []struct {
		name     string
		findings []Finding
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
				"payments": {Score: 75, Ready: false, Blockers: 1},
			},
		},
		{
			name: "finding with N teams counts for each",
			findings: []Finding{
				{Severity: SevBlocker, Teams: []string{"payments", "platform"}},
				{Severity: SevWarning, Teams: []string{"platform"}},
			},
			want: map[string]TeamScore{
				"payments": {Score: 75, Ready: false, Blockers: 1},
				"platform": {Score: 70, Ready: false, Blockers: 1, Warnings: 1},
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
				"":     {Score: 95, Ready: true, Warnings: 1},
				"data": {Score: 75, Ready: false, Blockers: 1},
			},
		},
		{
			name: "info-only team scores 100 ready",
			findings: []Finding{
				{Severity: SevInfo, Teams: []string{"shop"}},
			},
			want: map[string]TeamScore{
				"shop": {Score: 100, Ready: true},
			},
		},
		{
			name: "score floors at 0 (same formula as Score)",
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
				"x": {Score: 5, Ready: false, Blockers: 4, Warnings: 5},
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := TeamScores(Report{Findings: tc.findings})
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("TeamScores = %+v, want %+v", got, tc.want)
			}
		})
	}
}
