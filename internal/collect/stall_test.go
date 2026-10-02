package collect

import (
	"context"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"k8s.io/client-go/rest"

	"github.com/abd-ulbasit/upgradescope/internal/engine"
	"github.com/abd-ulbasit/upgradescope/internal/inventory"
	"github.com/abd-ulbasit/upgradescope/internal/kb"
)

// stallingAPIServer is an API server that accepts every request and never
// answers the ones stall picks, until the client gives up or the test
// ends; the others go to h (404 when nil).
func stallingAPIServer(t *testing.T, stall func(*http.Request) bool, h http.Handler) *httptest.Server {
	t.Helper()
	if h == nil {
		h = http.NotFoundHandler()
	}
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if stall(r) {
			select {
			case <-r.Context().Done():
			case <-release:
			}
			return
		}
		h.ServeHTTP(w, r)
	}))
	t.Cleanup(func() { close(release); srv.Close() })
	return srv
}

func stallPaths(paths ...string) func(*http.Request) bool {
	return func(r *http.Request) bool {
		for _, p := range paths {
			if r.URL.Path == p {
				return true
			}
		}
		return false
	}
}

// within runs f and fails the test if it has not returned after d. A
// regression that ignores the context then fails here instead of hanging
// the test binary.
func within(t *testing.T, d time.Duration, f func()) {
	t.Helper()
	done := make(chan struct{})
	go func() { defer close(done); f() }()
	select {
	case <-done:
	case <-time.After(d):
		t.Fatalf("still running after %v: the call does not honour its context", d)
	}
}

func stallClients(t *testing.T, srv *httptest.Server) Clients {
	t.Helper()
	c, err := NewClients(&rest.Config{Host: srv.URL}) // no per-request timeout
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// #94: a stalled /version held a scan for over seven minutes, because
// ServerVersion ignored the scan's context.
func TestCollectVersionsHonoursContextOnStalledVersion(t *testing.T) {
	c := stallClients(t, stallingAPIServer(t, stallPaths("/version"), nil))
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	var err error
	within(t, 5*time.Second, func() {
		err = collectVersions(ctx, c.Discovery, c.Kube, "team", &inventory.Inventory{})
	})
	if err == nil || !strings.Contains(err.Error(), "server version") {
		t.Errorf("err = %v, want a server version error", err)
	}
}

// Discovery of groups and resources must give up with the context too.
func TestCollectAPIUsageHonoursContextOnStalledDiscovery(t *testing.T) {
	c := stallClients(t, stallingAPIServer(t, stallPaths("/api", "/apis"), nil))
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	var err error
	within(t, 5*time.Second, func() {
		_, err = collectAPIUsage(ctx, c.Discovery, c.Metadata, flaggedLifecycle(), &inventory.Inventory{})
	})
	if err == nil || !strings.Contains(err.Error(), "discovery") {
		t.Errorf("err = %v, want a discovery error", err)
	}
}

// fakeAPIServer answers what the live sub-collectors ask with an empty
// cluster: a version, the kube-system namespace, no other objects, an
// API surface with nothing flagged, and an apiserver /metrics.
func fakeAPIServer() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		body := `{"metadata":{},"items":[]}` // a typed list; the client knows its kind
		switch {
		case r.URL.Path == "/version":
			body = `{"major":"1","minor":"34","gitVersion":"v1.34.2"}`
		case r.URL.Path == "/api":
			body = `{"kind":"APIVersions","versions":["v1"]}`
		case r.URL.Path == "/apis":
			body = `{"kind":"APIGroupList","apiVersion":"v1","groups":[]}`
		case r.URL.Path == "/api/v1":
			body = `{"kind":"APIResourceList","groupVersion":"v1","resources":[]}`
		case r.URL.Path == "/api/v1/namespaces/kube-system":
			body = `{"metadata":{"name":"kube-system","uid":"uid-1"}}`
		case r.URL.Path == "/metrics":
			w.Header().Set("Content-Type", "text/plain")
			body = "# TYPE apiserver_request_total counter\napiserver_request_total{code=\"200\",verb=\"LIST\"} 1\n" // no deprecated API requested
		case strings.Contains(r.Header.Get("Accept"), "as=PartialObjectMetadataList"):
			body = `{"kind":"PartialObjectMetadataList","apiVersion":"meta.k8s.io/v1","metadata":{},"items":[]}`
		}
		_, _ = w.Write([]byte(body))
	})
}

// The live steps run in this order: helm before addons (which consumes
// the releases), api-usage before deprecated-calls (which consumes its
// own deprecated LISTs), crds (which lists no deprecated version), and
// the /metrics scrape last.
func TestStepOrder(t *testing.T) {
	var got []inventory.Capability
	for _, s := range steps(Clients{}, kb.KB{}, Options{}) {
		got = append(got, s.cap)
	}
	want := []inventory.Capability{inventory.CapVersions, inventory.CapHelm, inventory.CapAddOns, inventory.CapAPIUsage, inventory.CapCRDs, inventory.CapDeprecatedCalls}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("step order = %v, want %v", got, want)
	}
}

// With nothing stalled, everything is assessed: the fixture is sound.
func TestCollectFakeAPIServerAssessesEverything(t *testing.T) {
	c := stallClients(t, stallingAPIServer(t, func(*http.Request) bool { return false }, fakeAPIServer()))
	inv := Collect(context.Background(), c, loadKB(t), Options{})
	for _, cp := range []inventory.Capability{inventory.CapVersions, inventory.CapHelm, inventory.CapAddOns, inventory.CapAPIUsage, inventory.CapCRDs, inventory.CapDeprecatedCalls} {
		if st := inv.Capabilities[cp]; !st.Available || st.Partial {
			t.Errorf("%s = %+v, want available", cp, st)
		}
	}
}

// One stalled step gets its share of the time left and no more: it is not
// assessed, naming the deadline, and every step after it still runs. With
// no per-request timeout, only the step deadline stops it. The stalled
// add-on LIST leaves a required check unassessed, so the verdict is
// unknown, never ready.
func TestCollectStalledStepDegradesOnlyItsCapability(t *testing.T) {
	c := stallClients(t, stallingAPIServer(t, stallPaths("/api/v1/pods"), fakeAPIServer()))
	k := loadKB(t)
	const budget = 2 * time.Second
	ctx, cancel := context.WithTimeout(context.Background(), budget)
	defer cancel()
	start := time.Now()
	var inv inventory.Inventory
	within(t, budget+3*time.Second, func() { inv = Collect(ctx, c, k, Options{}) })
	if took := time.Since(start); took > budget+500*time.Millisecond {
		t.Errorf("Collect took %v, want at most the %v budget", took, budget)
	}
	addons := inv.Capabilities[inventory.CapAddOns]
	if addons.Available || !strings.Contains(addons.Reason, "step deadline") {
		t.Errorf("addons = %+v, want not available, the reason naming the step deadline", addons)
	}
	for _, cp := range []inventory.Capability{inventory.CapVersions, inventory.CapHelm, inventory.CapAPIUsage, inventory.CapCRDs, inventory.CapDeprecatedCalls} {
		if st := inv.Capabilities[cp]; !st.Available {
			t.Errorf("%s = %+v, want available: only the stalled step degrades", cp, st)
		}
	}
	rep := engine.Evaluate(inv, k, inventory.Version{Major: 1, Minor: 35}, time.Now())
	if rep.Verdict != engine.VerdictUnknown {
		t.Errorf("verdict = %s (findings %+v), want unknown (add-ons not assessed)", rep.Verdict, rep.Findings)
	}
}

// The crds step follows the same per-step deadline: a CRD LIST that never
// answers costs crds its assessment and nothing else. crds is not a
// required check, so the scan of the otherwise empty cluster stays ready,
// with crds named as not assessed.
func TestCollectStalledCRDListDegradesOnlyCRDs(t *testing.T) {
	c := stallClients(t, stallingAPIServer(t, stallPaths("/apis/apiextensions.k8s.io/v1/customresourcedefinitions"), fakeAPIServer()))
	k := loadKB(t)
	const budget = 2 * time.Second
	ctx, cancel := context.WithTimeout(context.Background(), budget)
	defer cancel()
	var inv inventory.Inventory
	within(t, budget+3*time.Second, func() { inv = Collect(ctx, c, k, Options{}) })
	crds := inv.Capabilities[inventory.CapCRDs]
	if crds.Available || !strings.Contains(crds.Reason, "step deadline") {
		t.Errorf("crds = %+v, want not available, the reason naming the step deadline", crds)
	}
	for _, cp := range []inventory.Capability{inventory.CapVersions, inventory.CapHelm, inventory.CapAddOns, inventory.CapAPIUsage, inventory.CapDeprecatedCalls} {
		if st := inv.Capabilities[cp]; !st.Available {
			t.Errorf("%s = %+v, want available: only the stalled step degrades", cp, st)
		}
	}
	rep := engine.Evaluate(inv, k, inventory.Version{Major: 1, Minor: 35}, time.Now())
	if rep.Verdict != engine.VerdictReady {
		t.Errorf("verdict = %s (findings %+v, gaps %+v), want ready: crds is not required", rep.Verdict, rep.Findings, rep.NotAssessed)
	}
	if i := slices.IndexFunc(rep.NotAssessed, func(g engine.CapabilityGap) bool { return g.Capability == inventory.CapCRDs }); i < 0 || rep.NotAssessed[i].Required {
		t.Errorf("notAssessed = %+v, want a crds gap that is not required", rep.NotAssessed)
	}
}

// #94/DC-02: a /metrics that never answers held the scan for its whole
// five-minute budget. The per-request timeout ends that request, and the
// scan, long before the budget.
func TestCollectStalledMetricsEndsAtRequestTimeout(t *testing.T) {
	srv := stallingAPIServer(t, stallPaths("/metrics"), fakeAPIServer())
	c, err := NewClients(&rest.Config{Host: srv.URL, Timeout: 300 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	k := loadKB(t)
	start := time.Now()
	var inv inventory.Inventory
	within(t, 10*time.Second, func() { inv = Collect(ctx, c, k, Options{}) })
	if took := time.Since(start); took > 5*time.Second {
		t.Errorf("Collect took %v, want about the 300ms request timeout", took)
	}
	if st := inv.Capabilities[inventory.CapDeprecatedCalls]; st.Available || !strings.Contains(st.Reason, "/metrics") {
		t.Errorf("deprecated-calls = %+v, want not available, naming /metrics", st)
	}
	for _, cp := range []inventory.Capability{inventory.CapVersions, inventory.CapHelm, inventory.CapAddOns, inventory.CapAPIUsage} {
		if st := inv.Capabilities[cp]; !st.Available {
			t.Errorf("%s = %+v, want available", cp, st)
		}
	}
}

// Each step gets an equal share of the time left before the deadline, so
// a stalled step leaves every later one at least budget/steps; the last
// step gets whatever is left. Without a deadline there is no step limit.
func TestRunStepsSharesTheDeadline(t *testing.T) {
	inv := inventory.Inventory{Capabilities: map[inventory.Capability]inventory.CapabilityStatus{}}
	var left []time.Duration
	record := func(ctx context.Context, _ *inventory.Inventory) error {
		d, ok := ctx.Deadline()
		if !ok {
			left = append(left, -1)
			return nil
		}
		left = append(left, time.Until(d))
		return nil
	}
	stall := func(ctx context.Context, inv *inventory.Inventory) error {
		_ = record(ctx, inv)
		<-ctx.Done()
		return ctx.Err()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 800*time.Millisecond)
	defer cancel()
	runSteps(ctx, &inv, []step{
		{cap: inventory.CapVersions, run: stall},
		{cap: inventory.CapHelm, run: record},
		{cap: inventory.CapAddOns, run: stall},
		{cap: inventory.CapAPIUsage, run: record},
	})
	// 800ms over 4 steps: the first stall takes its 200ms; the next step
	// gets a third of the 600ms left and returns at once, the second stall
	// half of it, and the last step all that remains.
	want := []time.Duration{200 * time.Millisecond, 200 * time.Millisecond, 300 * time.Millisecond, 300 * time.Millisecond}
	if len(left) != len(want) {
		t.Fatalf("steps saw %v, want 4 deadlines", left)
	}
	for i := range want {
		if d := left[i] - want[i]; d > 0 || d < -150*time.Millisecond {
			t.Errorf("step %d: deadline in %v, want about %v", i, left[i], want[i])
		}
	}
	for _, cp := range []inventory.Capability{inventory.CapVersions, inventory.CapAddOns} {
		if st := inv.Capabilities[cp]; st.Available || !strings.Contains(st.Reason, "step deadline") {
			t.Errorf("%s = %+v, want not available, naming the step deadline", cp, st)
		}
	}
	if st := inv.Capabilities[inventory.CapAPIUsage]; !st.Available {
		t.Errorf("api-usage = %+v, want available after two stalled steps", st)
	}

	left = nil
	runSteps(context.Background(), &inv, []step{{cap: inventory.CapVersions, run: record}})
	if len(left) != 1 || left[0] != -1 {
		t.Errorf("no scan deadline: step saw %v, want no deadline", left)
	}
}

// A stalled /metrics degrades deprecated-calls only, which is not a
// required capability (managed control planes forbid /metrics), so an
// otherwise clean cluster is still ready: the gap is reported, not
// required, and the verdict rests on the stored objects.
func TestCollectStalledMetricsVerdictCanBeReady(t *testing.T) {
	srv := stallingAPIServer(t, stallPaths("/metrics"), fakeAPIServer())
	c, err := NewClients(&rest.Config{Host: srv.URL, Timeout: 300 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	k := loadKB(t)
	var inv inventory.Inventory
	within(t, 10*time.Second, func() { inv = Collect(context.Background(), c, k, Options{}) })
	rep := engine.Evaluate(inv, k, inventory.Version{Major: 1, Minor: 35}, time.Now())
	if rep.Verdict != engine.VerdictReady {
		t.Errorf("verdict = %s (findings %+v, gaps %+v), want ready", rep.Verdict, rep.Findings, rep.NotAssessed)
	}
	var gap bool
	for _, g := range rep.NotAssessed {
		if g.Capability == inventory.CapDeprecatedCalls {
			gap = true
			if g.Required {
				t.Errorf("deprecated-calls gap = %+v, want not required", g)
			}
		}
	}
	if !gap {
		t.Errorf("notAssessed = %+v, want the deprecated-calls gap reported", rep.NotAssessed)
	}
}
