package agent

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
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
	// What the loop does with each tick's outcome, on its own clock so
	// /readyz can be asked after the allowed age has passed.
	obsNow := at
	o := newTestObserver(t, &obsNow)

	if err := r.tick(ctx); err != nil {
		t.Fatalf("first tick: %v", err)
	}
	o.record(r.last)
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
	o.record(r.last)

	// Patch revoked as well: nothing can mark the CR, the tick says so
	// (the earlier marker stays, it is the CR's, not ours to guess about).
	patchDenied = true
	at = at.Add(time.Minute)
	if err := r.tick(ctx); err == nil || !strings.Contains(err.Error(), crd.StatusErrorAnnotation) {
		t.Errorf("tick with status and patch forbidden: err = %v, want it to name the annotation it could not set", err)
	}
	patchDenied = false
	// The failing agent stops being ready once its last success is older
	// than the allowed window, and says why.
	obsNow = obsNow.Add(o.readyWindow() + time.Minute)
	o.record(r.last)
	if code, body := serve(t, o.handler(), "/readyz"); code != http.StatusServiceUnavailable || !strings.Contains(body, "forbidden") {
		t.Errorf("/readyz = %d %q, want 503 naming the status failure", code, body)
	}

	failing = false
	at = at.Add(time.Minute)
	if err := r.tick(ctx); err != nil {
		t.Fatalf("tick after access returned: %v", err)
	}
	o.record(r.last)
	if code, body := serve(t, o.handler(), "/readyz"); code != http.StatusOK {
		t.Errorf("/readyz = %d %q after a successful status write, want 200", code, body)
	}
	if v, ok := statusErrorAnnotation(t, dyn); ok {
		t.Errorf("annotation %q survived a successful status write", v)
	}
}

// A status write that succeeded is a current status, even when the marker
// of an earlier failure could not be cleared afterwards: the tick is not
// failed (the gauges stay, /readyz stays up), the CR is not marked as
// failing again, and the stuck marker is reported on its own for the next
// tick to retry.
func TestTickWithAFailedMarkerClearIsNotAFailedTick(t *testing.T) {
	ctx := context.Background()
	dyn := fakeDyn().(*dynamicfake.FakeDynamicClient)
	failing, patchDenied := false, false
	var patches []string
	dyn.PrependReactor("update", crd.Plural, func(a k8stesting.Action) (bool, runtime.Object, error) {
		if failing && a.GetSubresource() == "status" {
			return true, nil, apierrors.NewForbidden(schema.GroupResource{Group: crd.Group, Resource: crd.Plural + "/status"}, crd.DefaultName, errors.New("RBAC"))
		}
		return false, nil, nil
	})
	dyn.PrependReactor("patch", crd.Plural, func(a k8stesting.Action) (bool, runtime.Object, error) {
		if patchDenied {
			patches = append(patches, string(a.(k8stesting.PatchAction).GetPatch()))
			return true, nil, apierrors.NewForbidden(schema.GroupResource{Group: crd.Group, Resource: crd.Plural}, crd.DefaultName, errors.New("RBAC"))
		}
		return false, nil, nil
	})
	r := testRunner(t, dyn, "")

	failing = true
	if err := r.tick(ctx); err == nil {
		t.Fatal("tick with the status write forbidden succeeded")
	}
	if _, ok := statusErrorAnnotation(t, dyn); !ok {
		t.Fatal("CR not marked after the failed status write")
	}

	failing, patchDenied = false, true
	if err := r.tick(ctx); err != nil {
		t.Fatalf("tick whose status write succeeded: %v, want it not failed over the marker", err)
	}
	if r.last.err != nil || r.last.markerErr == nil || !strings.Contains(r.last.markerErr.Error(), crd.StatusErrorAnnotation) {
		t.Errorf("last.err = %v, last.markerErr = %v; want no failure and the marker error reported separately", r.last.err, r.last.markerErr)
	}
	if len(patches) != 1 || !strings.Contains(patches[0], "null") {
		t.Errorf("patches = %q, want the one attempt to clear the marker and no new marking", patches)
	}
}

// The gauges describe the last successful tick. A verdict that old is no
// longer current once staleAfterFailedTicks ticks in a row have failed and
// the last success is older than the NotTicking alert's threshold: the
// verdict, score, findings and capability series then drop, so an alert on
// "blocked" or "ready" does not keep reading a verdict the CR no longer
// carries. The timestamp, interval and KB series stay, so the age of the
// last good verdict can still be alerted on; one success brings it back.
// The series must never drop before UpgradescopeAgentNotTicking fires
// (2*interval+10m after the last success), or a firing blocked alert would
// resolve with nothing else firing: at a 1m interval three failed ticks are
// only 3m, so the age bound decides there.
func TestObserverDropsStaleVerdictGaugesAfterFailedTicks(t *testing.T) {
	verdictSeries := []string{"upgradescope_readiness_verdict", "upgradescope_readiness_score", "upgradescope_findings", "upgradescope_capability_available"}
	for _, interval := range []time.Duration{time.Minute, 10 * time.Minute, 30 * time.Minute} {
		t.Run(interval.String(), func(t *testing.T) {
			now := time.Date(2026, 10, 3, 10, 0, 0, 0, time.UTC)
			o := newObserver(slog.New(slog.NewTextHandler(io.Discard, nil)), mustKB(t), interval)
			o.now = func() time.Time { return now }
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

			o.record(good)
			start := now
			// The NotTicking alert fires 2*interval+10m after the last
			// success; the verdict series must outlive that instant.
			alertFires := 2*interval + 10*time.Minute
			var withdrawnAt time.Duration
			for i := 1; i <= 40 && withdrawnAt == 0; i++ {
				now = now.Add(interval)
				o.record(failed)
				age := now.Sub(start)
				if families()[verdictSeries[0]] {
					continue
				}
				if i < staleAfterFailedTicks {
					t.Fatalf("verdict series gone after %d failed ticks, want them kept until %d", i, staleAfterFailedTicks)
				}
				if age <= alertFires {
					t.Fatalf("verdict series gone %s after the last success, before the NotTicking alert fires at %s", age, alertFires)
				}
				withdrawnAt = age
			}
			if withdrawnAt == 0 {
				t.Fatalf("verdict series still exported %s after the last success", now.Sub(start))
			}
			got := families()
			for _, name := range verdictSeries {
				if got[name] {
					t.Errorf("%s exported while the verdict was withdrawn", name)
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
		})
	}
}
