package crd

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestAllTargetsReadyCondition(t *testing.T) {
	ready := func(v string) TargetStatus { return TargetStatus{Target: v, Score: 100, Ready: true, Verdict: "ready"} }
	blocked := func(v string, n int) TargetStatus {
		return TargetStatus{Target: v, Score: 40, Verdict: "blocked", Blockers: n}
	}
	unknown := func(v string) TargetStatus { return TargetStatus{Target: v, Score: 90, Verdict: "unknown"} }
	cases := []struct {
		name       string
		st         Status
		wantStatus metav1.ConditionStatus
		wantReason string
		wantMsg    []string
	}{
		{
			name:       "first ready, second blocked: False and names the second",
			st:         Status{Targets: []TargetStatus{ready("1.37"), blocked("1.38", 3)}},
			wantStatus: metav1.ConditionFalse, wantReason: ReasonBlocked, wantMsg: []string{"1.38 blocked (3 blockers)"},
		},
		{
			name:       "one blocker is singular",
			st:         Status{Targets: []TargetStatus{ready("1.37"), blocked("1.38", 1)}},
			wantStatus: metav1.ConditionFalse, wantReason: ReasonBlocked, wantMsg: []string{"1.38 blocked (1 blocker)"},
		},
		{
			name:       "blocked wins over unknown, and both are named",
			st:         Status{Targets: []TargetStatus{unknown("1.37"), blocked("1.38", 2)}},
			wantStatus: metav1.ConditionFalse, wantReason: ReasonBlocked, wantMsg: []string{"1.38 blocked (2 blockers)", "1.37 not assessed"},
		},
		{
			name:       "a verdict-less target with blockers is blocked, as Ready reads it",
			st:         Status{Targets: []TargetStatus{ready("1.37"), {Target: "1.38", Blockers: 2}}},
			wantStatus: metav1.ConditionFalse, wantReason: ReasonBlocked, wantMsg: []string{"1.38 blocked (2 blockers)"},
		},
		{
			name:       "one unknown, none blocked: Unknown",
			st:         Status{Targets: []TargetStatus{ready("1.37"), unknown("1.38")}},
			wantStatus: metav1.ConditionUnknown, wantReason: ReasonNotAssessed, wantMsg: []string{"1.38 not assessed"},
		},
		{
			name:       "all ready: True",
			st:         Status{Targets: []TargetStatus{ready("1.37"), ready("1.38")}},
			wantStatus: metav1.ConditionTrue, wantReason: ReasonReady, wantMsg: []string{"2 targets ready", "1.37", "1.38"},
		},
		{
			name:       "v0.1 status without verdicts: Ready bool counts",
			st:         Status{Targets: []TargetStatus{{Target: "1.37", Ready: true}}},
			wantStatus: metav1.ConditionTrue, wantReason: ReasonReady, wantMsg: []string{"1.37"},
		},
		{
			name:       "no targets: Unknown",
			st:         Status{NotAssessed: []string{"targets: no spec targets and server version unknown"}},
			wantStatus: metav1.ConditionUnknown, wantReason: ReasonNotAssessed, wantMsg: []string{"no target evaluated", "server version unknown"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tc.st.LastEvaluated = metav1.NewTime(time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC))
			c := AllTargetsReadyCondition(tc.st)
			if c.Type != "AllTargetsReady" {
				t.Errorf("type = %q", c.Type)
			}
			if c.Status != tc.wantStatus || c.Reason != tc.wantReason {
				t.Errorf("status/reason = %s/%s, want %s/%s", c.Status, c.Reason, tc.wantStatus, tc.wantReason)
			}
			for _, w := range tc.wantMsg {
				if !strings.Contains(c.Message, w) {
					t.Errorf("message = %q, want it to contain %q", c.Message, w)
				}
			}
			if !c.LastTransitionTime.Equal(&tc.st.LastEvaluated) {
				t.Errorf("lastTransitionTime = %v, want the evaluation time", c.LastTransitionTime)
			}
		})
	}
}

// Ready keeps its first-target meaning whatever the later targets say.
func TestAllTargetsReadyLeavesReadyAlone(t *testing.T) {
	st := Status{Targets: []TargetStatus{
		{Target: "1.37", Score: 100, Ready: true, Verdict: "ready"},
		{Target: "1.38", Score: 10, Verdict: "blocked", Blockers: 3},
	}}
	if c := ReadyCondition(st); c.Status != metav1.ConditionTrue {
		t.Errorf("Ready = %s, want True (first target)", c.Status)
	}
}

// Written to the object, a plan with the first target ready and the second
// blocked reads Ready=True and AllTargetsReady=False naming 1.38, and the
// new condition's transition time moves only when its status changes.
func TestWriteStatusSetsAllTargetsReady(t *testing.T) {
	ctx := context.Background()
	cr := newCRObject(DefaultName, "1.37")
	cr.SetGeneration(2)
	dyn := newDynFake(cr)
	t1 := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	plan := func(at time.Time, secondBlockers int) Status {
		second := TargetStatus{Target: "1.38", Score: 100, Ready: true, Verdict: "ready"}
		if secondBlockers > 0 {
			second = TargetStatus{Target: "1.38", Score: 30, Verdict: "blocked", Blockers: secondBlockers}
		}
		return Status{
			LastEvaluated: metav1.NewTime(at),
			Targets:       []TargetStatus{{Target: "1.37", Score: 100, Ready: true, Verdict: "ready"}, second},
		}
	}
	if err := WriteStatus(ctx, dyn, DefaultName, plan(t1, 3)); err != nil {
		t.Fatal(err)
	}
	st := readStatus(t, dyn, DefaultName)
	r := meta.FindStatusCondition(st.Conditions, ConditionReady)
	a := meta.FindStatusCondition(st.Conditions, ConditionAllTargetsReady)
	if r == nil || r.Status != metav1.ConditionTrue {
		t.Fatalf("Ready = %+v, want True", r)
	}
	if a == nil || a.Status != metav1.ConditionFalse || !strings.Contains(a.Message, "1.38 blocked (3 blockers)") {
		t.Fatalf("AllTargetsReady = %+v, want False naming 1.38", a)
	}
	if a.ObservedGeneration != 2 || !a.LastTransitionTime.Time.Equal(t1) {
		t.Errorf("AllTargetsReady generation/ltt = %d/%v, want 2/%v", a.ObservedGeneration, a.LastTransitionTime, t1)
	}

	// Still blocked ten minutes later (fewer blockers): the time stays.
	if err := WriteStatus(ctx, dyn, DefaultName, plan(t1.Add(10*time.Minute), 1)); err != nil {
		t.Fatal(err)
	}
	a = meta.FindStatusCondition(readStatus(t, dyn, DefaultName).Conditions, ConditionAllTargetsReady)
	if !a.LastTransitionTime.Time.Equal(t1) || !strings.Contains(a.Message, "(1 blocker)") {
		t.Errorf("AllTargetsReady = %+v, want ltt %v and the new count", a, t1)
	}

	// Cleared: True, transition time is that evaluation's.
	t3 := t1.Add(20 * time.Minute)
	if err := WriteStatus(ctx, dyn, DefaultName, plan(t3, 0)); err != nil {
		t.Fatal(err)
	}
	st = readStatus(t, dyn, DefaultName)
	a = meta.FindStatusCondition(st.Conditions, ConditionAllTargetsReady)
	if a.Status != metav1.ConditionTrue || !a.LastTransitionTime.Time.Equal(t3) || len(st.Conditions) != 2 {
		t.Errorf("AllTargetsReady = %+v of %d conditions, want True since %v, 2 conditions", a, len(st.Conditions), t3)
	}
}

// The message names every offending target and still fits any bound: eight
// targets, all blocked, with the largest counts.
func TestAllTargetsReadyMessageStaysBoundedAtMaxTargets(t *testing.T) {
	var st Status
	for i := 0; i < MaxTargets; i++ {
		st.Targets = append(st.Targets, TargetStatus{Target: fmt.Sprintf("1.%d", 30+i), Verdict: "blocked", Blockers: 1 << 30})
	}
	c := AllTargetsReadyCondition(st)
	if len(c.Message) > 1024 {
		t.Errorf("message is %d bytes, want at most 1024", len(c.Message))
	}
	for _, tg := range st.Targets {
		if !strings.Contains(c.Message, tg.Target+" blocked") {
			t.Errorf("message %q does not name %s", c.Message, tg.Target)
		}
	}
	// Hostile target names (status is written by the agent, but the type
	// is open) are clipped, not passed through whole.
	st.Targets[0].Target = strings.Repeat("<", 5000)
	if c := AllTargetsReadyCondition(st); len(c.Message) > 1100 {
		t.Errorf("message with a 5000-byte target name is %d bytes", len(c.Message))
	}
}

func TestManifestHasBlockersColumn(t *testing.T) {
	v := parseManifest(t).Spec.Versions[0]
	var names []string
	for _, pc := range v.AdditionalPrinterColumns {
		names = append(names, pc.Name)
		switch pc.Name {
		case "Blockers":
			if pc.Type != "integer" || pc.JSONPath != ".status.targets[0].blockers" {
				t.Errorf("Blockers column = %+v, want integer on .status.targets[0].blockers", pc)
			}
			if !strings.Contains(pc.Description, "first target") || !strings.Contains(pc.Description, "AllTargetsReady") {
				t.Errorf("Blockers description %q must say it is the first target's count and point to AllTargetsReady", pc.Description)
			}
		case "Ready":
			if !strings.Contains(pc.Description, "AllTargetsReady") {
				t.Errorf("Ready description %q must point to AllTargetsReady", pc.Description)
			}
		case "Server", "Support":
			t.Errorf("unexpected column %s", pc.Name)
		}
	}
	if strings.Join(names, ",") != "Target,Score,Ready,Blockers,LastEvaluated,Age" {
		t.Errorf("columns = %v", names)
	}
}

// The chart ships a copy of the manifest in crds/; Helm installs it on
// first install only, so a drifted copy installs a stale schema.
func TestChartCRDCopyMatchesManifest(t *testing.T) {
	copyPath := filepath.Join("..", "..", "deploy", "chart", "crds", ManifestFile)
	got, err := os.ReadFile(copyPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(Manifest) {
		t.Errorf("%s differs from internal/crd/manifest.yaml; copy it over", copyPath)
	}
}
