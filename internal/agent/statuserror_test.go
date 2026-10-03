package agent

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/abd-ulbasit/upgradescope/internal/crd"
	"github.com/abd-ulbasit/upgradescope/internal/engine"
	"github.com/abd-ulbasit/upgradescope/internal/inventory"
)

func statusErrorAnnotation(t *testing.T, dyn *dynamicfake.FakeDynamicClient) (string, bool) {
	t.Helper()
	obj, err := dyn.Resource(crd.GVR()).Get(context.Background(), crd.DefaultName, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	v, ok := obj.GetAnnotations()[crd.StatusErrorAnnotation]
	return v, ok
}

// Issue #199 (RB-07): an agent that loses write access to its status must
// not leave the CR showing a stale verdict unmarked. It annotates the CR
// (which only needs patch on the object, not on its status) with the time
// and reason of the failure, and removes the annotation with the next
// status write that succeeds.
func TestTickMarksTheCRWhenTheStatusWriteFails(t *testing.T) {
	ctx := context.Background()
	dyn := fakeDyn().(*dynamicfake.FakeDynamicClient)
	failing, patchDenied := false, false
	forbidden := func(sub string) error {
		return apierrors.NewForbidden(schema.GroupResource{Group: crd.Group, Resource: crd.Plural + sub}, crd.DefaultName, errors.New("RBAC: cannot update status"))
	}
	dyn.PrependReactor("update", crd.Plural, func(a k8stesting.Action) (bool, runtime.Object, error) {
		if failing && a.GetSubresource() == "status" {
			return true, nil, forbidden("/status")
		}
		return false, nil, nil
	})
	dyn.PrependReactor("patch", crd.Plural, func(k8stesting.Action) (bool, runtime.Object, error) {
		if patchDenied {
			return true, nil, forbidden("")
		}
		return false, nil, nil
	})
	r := testRunner(t, dyn, "")
	at := time.Date(2026, 10, 3, 10, 0, 0, 0, time.UTC)
	r.now = func() time.Time { return at }

	if err := r.tick(ctx); err != nil {
		t.Fatalf("first tick: %v", err)
	}
	if v, ok := statusErrorAnnotation(t, dyn); ok {
		t.Fatalf("annotation %q on a healthy CR", v)
	}

	failing = true
	at = at.Add(time.Minute)
	if err := r.tick(ctx); err == nil || !strings.Contains(err.Error(), "update clusterreadiness") {
		t.Fatalf("tick with the status write forbidden: err = %v, want the write error", err)
	}
	v, ok := statusErrorAnnotation(t, dyn)
	if !ok || !strings.HasPrefix(v, "2026-10-03T10:01:00Z ") || !strings.Contains(v, "forbidden") {
		t.Fatalf("annotation = %q (%v), want the failure's time and reason", v, ok)
	}

	// Patch revoked as well: nothing can mark the CR, the tick says so
	// (the earlier marker stays, it is the CR's, not ours to guess about).
	patchDenied = true
	at = at.Add(time.Minute)
	if err := r.tick(ctx); err == nil || !strings.Contains(err.Error(), crd.StatusErrorAnnotation) {
		t.Errorf("tick with status and patch forbidden: err = %v, want it to name the annotation it could not set", err)
	}
	patchDenied = false

	failing = false
	at = at.Add(time.Minute)
	if err := r.tick(ctx); err != nil {
		t.Fatalf("tick after access returned: %v", err)
	}
	if v, ok := statusErrorAnnotation(t, dyn); ok {
		t.Errorf("annotation %q survived a successful status write", v)
	}
}

// The gauges describe the last successful tick. A verdict that old is no
// longer current once staleAfterFailedTicks ticks in a row have failed:
// the verdict, score, findings and capability series then drop, so an
// alert on "blocked" or "ready" does not keep reading a verdict the CR no
// longer carries. The timestamp, interval and KB series stay, so the age of
// the last good verdict can still be alerted on; one success brings it back.
func TestObserverDropsStaleVerdictGaugesAfterFailedTicks(t *testing.T) {
	now := time.Date(2026, 10, 3, 10, 0, 0, 0, time.UTC)
	o := newTestObserver(t, &now)
	good := tickReport{
		duration: time.Second,
		caps:     map[inventory.Capability]inventory.CapabilityStatus{inventory.CapAPIUsage: {Available: true}},
		reports: []engine.Report{{
			Target: inventory.Version{Major: 1, Minor: 38}, Verdict: engine.VerdictBlocked, Score: 70,
			Findings: []engine.Finding{{Category: engine.CatRemovedAPI, Severity: engine.SevBlocker}},
		}},
	}
	failed := tickReport{err: errors.New("update status: forbidden"), duration: time.Second}
	families := func() map[string]bool {
		fs, err := o.reg.Gather()
		if err != nil {
			t.Fatal(err)
		}
		out := map[string]bool{}
		for _, f := range fs {
			out[f.GetName()] = true
		}
		return out
	}
	verdictSeries := []string{"upgradescope_readiness_verdict", "upgradescope_readiness_score", "upgradescope_findings", "upgradescope_capability_available"}

	o.record(good)
	for i := 1; i < staleAfterFailedTicks; i++ {
		o.record(failed)
		for _, name := range verdictSeries {
			if !families()[name] {
				t.Fatalf("%s gone after %d failed ticks, want it kept until %d", name, i, staleAfterFailedTicks)
			}
		}
	}
	o.record(failed)
	got := families()
	for _, name := range verdictSeries {
		if got[name] {
			t.Errorf("%s still exported after %d failed ticks in a row", name, staleAfterFailedTicks)
		}
	}
	for _, name := range []string{"upgradescope_agent_last_success_timestamp_seconds", "upgradescope_agent_interval_seconds", "upgradescope_kb_info"} {
		if !got[name] {
			t.Errorf("%s dropped with the verdict, want it kept", name)
		}
	}

	o.record(good)
	for _, name := range verdictSeries {
		if !families()[name] {
			t.Errorf("%s missing after a successful tick", name)
		}
	}
}
