package agent

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/rest"

	"github.com/abd-ulbasit/upgradescope/internal/collect"
	"github.com/abd-ulbasit/upgradescope/internal/crd"
)

// #238's acceptance case end to end, through the real collector: an
// apiserver whose /metrics never answers holds the last step (the
// deprecated-calls scrape) to its step deadline, which is the collection's
// deadline. Before the reserve that was the tick deadline, so the status
// write that followed ran on an expired context and the ClusterReadiness
// stayed stale and unmarked. Now the status is written, current, and says
// the scrape gave up at its step deadline; the push is sent too. The tick
// budget is scaled down from the 30s of --interval 1m (the reserve is half
// of it either way).
func TestTickWithAStalledMetricsScrapeWritesTheStatus(t *testing.T) {
	release := make(chan struct{})
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		switch req.URL.Path {
		case "/metrics":
			select { // an apiserver that never answers
			case <-req.Context().Done():
			case <-release:
			}
		case "/version":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"major":"1","minor":"35","gitVersion":"v1.35.2"}`))
		default:
			http.NotFound(w, req)
		}
	}))
	t.Cleanup(func() { close(release); api.Close() })
	clients, err := collect.NewClients(&rest.Config{Host: api.URL}) // no --request-timeout
	if err != nil {
		t.Fatal(err)
	}

	fake := fakeDyn().(*dynamicfake.FakeDynamicClient)
	srv := newSnapServer(t)
	cfg := Config{ServerURL: srv.srv.URL, ServerToken: "tok", ClusterName: "lab", Interval: time.Minute}
	if err := cfg.applyDefaults(); err != nil {
		t.Fatal(err)
	}
	r := newRunner(clients, ctxDyn{Interface: fake}, mustKB(t), cfg)
	r.pusher.wait = func(context.Context, time.Duration) error { return nil }
	if want := 30 * time.Second; r.tickBudget != want {
		t.Fatalf("tick budget at --interval 1m = %v, want %v", r.tickBudget, want)
	}
	const budget = 4 * time.Second
	r.tickBudget = budget

	start := time.Now()
	rep := r.runTick(context.Background())
	if rep.err != nil || rep.pushErr != nil {
		t.Fatalf("tick err = %v, push err = %v; want both nil when only the scrape stalls", rep.err, rep.pushErr)
	}
	if took := time.Since(start); took >= budget {
		t.Errorf("tick took %v, its whole budget %v: the reserve was not kept", took, budget)
	}
	st := readCRStatus(t, fake, crd.DefaultName)
	if st.ObservedServerVersion != "v1.35.2" || len(st.Targets) != 1 {
		t.Errorf("status server version %q, targets %+v; want this tick's", st.ObservedServerVersion, st.Targets)
	}
	var scrape string
	for _, n := range st.NotAssessed {
		if strings.Contains(n, "deprecated-calls") {
			scrape = n
		}
	}
	if !strings.Contains(scrape, "step deadline") {
		t.Errorf("notAssessed = %q, want the deprecated-calls entry to name the step deadline", st.NotAssessed)
	}
	if v, ok := statusErrorAnnotation(t, fake); ok {
		t.Errorf("status-error annotation %q on a tick whose status was written", v)
	}
	if srv.count() != 1 {
		t.Errorf("%d pushes, want 1", srv.count())
	}
}
