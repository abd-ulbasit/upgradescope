package crd

import (
	"context"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	dynamicfake "k8s.io/client-go/dynamic/fake"
)

func TestReadyCondition(t *testing.T) {
	cases := []struct {
		name       string
		st         Status
		wantStatus metav1.ConditionStatus
		wantReason string
		wantMsg    string
	}{
		{
			name:       "ready",
			st:         Status{Targets: []TargetStatus{{Target: "1.36", Score: 100, Ready: true, Verdict: "ready"}}},
			wantStatus: metav1.ConditionTrue, wantReason: ReasonReady, wantMsg: "1.36",
		},
		{
			name:       "blocked",
			st:         Status{Targets: []TargetStatus{{Target: "1.36", Score: 40, Verdict: "blocked", Blockers: 3}}},
			wantStatus: metav1.ConditionFalse, wantReason: ReasonBlocked, wantMsg: "3 blocker",
		},
		{
			name:       "unknown",
			st:         Status{Targets: []TargetStatus{{Target: "1.36", Score: 90, Verdict: "unknown"}}},
			wantStatus: metav1.ConditionUnknown, wantReason: ReasonNotAssessed, wantMsg: "notAssessed",
		},
		{
			// Only the first target decides; the second is blocked.
			name: "first target wins",
			st: Status{Targets: []TargetStatus{
				{Target: "1.36", Score: 100, Ready: true, Verdict: "ready"},
				{Target: "1.37", Score: 10, Verdict: "blocked", Blockers: 9},
			}},
			wantStatus: metav1.ConditionTrue, wantReason: ReasonReady, wantMsg: "1.36",
		},
		{
			name:       "no target resolved",
			st:         Status{NotAssessed: []string{"targets: no spec targets and server version unknown"}},
			wantStatus: metav1.ConditionUnknown, wantReason: ReasonNotAssessed, wantMsg: "server version unknown",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := ReadyCondition(tc.st)
			if c.Type != ConditionReady {
				t.Errorf("type = %q, want %q", c.Type, ConditionReady)
			}
			if c.Status != tc.wantStatus || c.Reason != tc.wantReason {
				t.Errorf("status/reason = %s/%s, want %s/%s", c.Status, c.Reason, tc.wantStatus, tc.wantReason)
			}
			if !strings.Contains(c.Message, tc.wantMsg) {
				t.Errorf("message = %q, want it to contain %q", c.Message, tc.wantMsg)
			}
		})
	}
}

// readStatus decodes the stored status of the named CR.
func readStatus(t *testing.T, dyn *dynamicfake.FakeDynamicClient, name string) Status {
	t.Helper()
	obj, err := dyn.Resource(GVR()).Get(context.Background(), name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	raw, found, err := unstructured.NestedMap(obj.Object, "status")
	if err != nil || !found {
		t.Fatalf("status not written: found=%v err=%v", found, err)
	}
	var st Status
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(raw, &st); err != nil {
		t.Fatalf("decode status: %v", err)
	}
	return st
}

// WriteStatus sets the Ready condition and observedGeneration. The
// condition's lastTransitionTime moves only when its status changes, so
// "blocked since" survives every tick that re-confirms it.
func TestWriteStatusReadyConditionTransitions(t *testing.T) {
	ctx := context.Background()
	cr := newCRObject(DefaultName, "1.36")
	cr.SetGeneration(3)
	dyn := newDynFake(cr)
	t1 := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	blocked := func(at time.Time) Status {
		return Status{
			LastEvaluated: metav1.NewTime(at),
			Targets:       []TargetStatus{{Target: "1.36", Score: 40, Verdict: "blocked", Blockers: 2}},
		}
	}

	if err := WriteStatus(ctx, dyn, DefaultName, blocked(t1)); err != nil {
		t.Fatal(err)
	}
	st := readStatus(t, dyn, DefaultName)
	if st.ObservedGeneration != 3 {
		t.Errorf("observedGeneration = %d, want 3", st.ObservedGeneration)
	}
	c := meta.FindStatusCondition(st.Conditions, ConditionReady)
	if c == nil || c.Status != metav1.ConditionFalse || c.Reason != ReasonBlocked {
		t.Fatalf("Ready condition = %+v, want False/Blocked", c)
	}
	if !c.LastTransitionTime.Time.Equal(t1) || c.ObservedGeneration != 3 {
		t.Errorf("condition ltt/gen = %v/%d, want %v/3", c.LastTransitionTime, c.ObservedGeneration, t1)
	}

	// Same verdict ten minutes later: the transition time stays.
	if err := WriteStatus(ctx, dyn, DefaultName, blocked(t1.Add(10*time.Minute))); err != nil {
		t.Fatal(err)
	}
	c = meta.FindStatusCondition(readStatus(t, dyn, DefaultName).Conditions, ConditionReady)
	if !c.LastTransitionTime.Time.Equal(t1) {
		t.Errorf("lastTransitionTime moved to %v on an unchanged verdict, want %v", c.LastTransitionTime, t1)
	}

	// Verdict flips to ready: the transition time is that evaluation's.
	t3 := t1.Add(20 * time.Minute)
	ready := Status{
		LastEvaluated: metav1.NewTime(t3),
		Targets:       []TargetStatus{{Target: "1.36", Score: 100, Ready: true, Verdict: "ready"}},
	}
	if err := WriteStatus(ctx, dyn, DefaultName, ready); err != nil {
		t.Fatal(err)
	}
	st = readStatus(t, dyn, DefaultName)
	if len(st.Conditions) != 1 {
		t.Fatalf("conditions = %+v, want exactly one (Ready)", st.Conditions)
	}
	c = &st.Conditions[0]
	if c.Status != metav1.ConditionTrue || !c.LastTransitionTime.Time.Equal(t3) {
		t.Errorf("Ready condition = %+v, want True since %v", c, t3)
	}
}

// A spec edited after the agent read it bumps the generation before the
// status write. The status must claim the generation that was evaluated,
// not the newer one, or Argo CD takes it as current.
func TestWriteStatusStampsTheEvaluatedGeneration(t *testing.T) {
	cr := newCRObject(DefaultName, "1.37")
	cr.SetGeneration(4) // edited since the agent read generation 3
	dyn := newDynFake(cr)
	if err := WriteStatus(context.Background(), dyn, DefaultName, Status{ObservedGeneration: 3}); err != nil {
		t.Fatal(err)
	}
	st := readStatus(t, dyn, DefaultName)
	c := meta.FindStatusCondition(st.Conditions, ConditionReady)
	if st.ObservedGeneration != 3 || c == nil || c.ObservedGeneration != 3 {
		t.Errorf("observedGeneration = %d, condition %+v, want both 3", st.ObservedGeneration, c)
	}
}

func TestManifestStatusHasConditionsAndLastEvaluatedColumn(t *testing.T) {
	v := parseManifest(t).Spec.Versions[0]
	status := v.Schema.OpenAPIV3Schema.Properties["status"]
	conds, ok := status.Properties["conditions"]
	if !ok || conds.Type != "array" || conds.Items == nil {
		t.Fatalf("status.conditions schema = %+v, want an array", conds)
	}
	for _, f := range []string{"type", "status", "reason", "message", "lastTransitionTime", "observedGeneration"} {
		if _, ok := conds.Items.Schema.Properties[f]; !ok {
			t.Errorf("status.conditions[] has no %q property", f)
		}
	}
	if og, ok := status.Properties["observedGeneration"]; !ok || og.Type != "integer" {
		t.Errorf("status.observedGeneration schema = %+v, want integer", og)
	}
	found := false
	for _, pc := range v.AdditionalPrinterColumns {
		if pc.Name == "LastEvaluated" {
			found = pc.JSONPath == ".status.lastEvaluated" && pc.Type == "date"
		}
	}
	if !found {
		t.Errorf("printer columns %+v: want LastEvaluated (date, .status.lastEvaluated)", v.AdditionalPrinterColumns)
	}
}
