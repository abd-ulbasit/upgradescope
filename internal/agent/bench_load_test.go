package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/url"
	"os"
	"runtime"
	"runtime/metrics"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	apiextensionsclient "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/abd-ulbasit/upgradescope/internal/collect"
	"github.com/abd-ulbasit/upgradescope/internal/crd"
	"github.com/abd-ulbasit/upgradescope/internal/inventory"
	"github.com/abd-ulbasit/upgradescope/internal/kb"
)

// TestBenchAgentTick measures what one agent tick costs a real apiserver
// (#71, hack/bench/agent.sh): the real tick (collect, evaluate, write the
// ClusterReadiness status) against the cluster in the kubeconfig named by
// UPGRADESCOPE_BENCH_KUBECONFIG, with the clients the agent builds. Per tick
// it records the requests by verb and resource, the response bytes client-go
// read and the bytes on the wire, the wall time, and the process's live
// heap and peak RSS. It reads only that kubeconfig, never the default one.
//
// Knobs: UPGRADESCOPE_BENCH_TICKS (default 5), UPGRADESCOPE_BENCH_LABEL (a
// name for the cluster's fill level), UPGRADESCOPE_BENCH_OUT (a file that
// gets one JSON line per tick), UPGRADESCOPE_BENCH_EXPECT_NODES and
// UPGRADESCOPE_BENCH_EXPECT_HELM (fail unless the inventory holds that many:
// a benchmark of a cluster that was not seeded measures nothing), and
// UPGRADESCOPE_BENCH_NO_HELM_CACHE=1 (collect with no Helm release cache, as
// every tick did before #71: the "before" rows of docs/operations/scale.md).
// With the GitOps fill (#233, hack/bench/agent.sh BENCH_GITOPS=1) the lab
// has the Argo CD and Flux CRDs: UPGRADESCOPE_BENCH_GITOPS=1 accepts the
// one gap that brings (a partial helm capability that skips only Argo CD:
// it renders charts and leaves no release), and
// UPGRADESCOPE_BENCH_EXPECT_GITOPS fails unless the inventory holds that
// many GitOps charts, which is what shows the collector read the objects.
func TestBenchAgentTick(t *testing.T) {
	kubeconfig := os.Getenv("UPGRADESCOPE_BENCH_KUBECONFIG")
	if kubeconfig == "" {
		t.Skip("set UPGRADESCOPE_BENCH_KUBECONFIG (hack/bench/agent.sh does) to measure an agent tick against a lab cluster")
	}
	ticks := envInt(t, "UPGRADESCOPE_BENCH_TICKS", 5)
	label := os.Getenv("UPGRADESCOPE_BENCH_LABEL")
	gitopsLab := os.Getenv("UPGRADESCOPE_BENCH_GITOPS") == "1"

	cfg, proxy, rec, err := benchRESTConfig(kubeconfig)
	if err != nil {
		t.Fatal(err)
	}
	defer proxy.close()

	clients, err := collect.NewClients(cfg)
	if err != nil {
		t.Fatal(err)
	}
	dyn, err := dynamic.NewForConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	apiext, err := apiextensionsclient.NewForConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	k, err := kb.Load()
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := crd.EnsureCRD(ctx, apiext); err != nil { // as Run does at startup
		t.Fatalf("install the ClusterReadiness CRD: %v", err)
	}

	acfg := Config{CRName: "bench-agent", Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	if err := acfg.applyDefaults(); err != nil {
		t.Fatal(err)
	}
	r := newRunner(clients, dyn, k, acfg)
	var (
		collectDur time.Duration
		last       inventory.Inventory
	)
	realCollect := r.collectFn
	switch v := os.Getenv("UPGRADESCOPE_BENCH_NO_HELM_CACHE"); v {
	case "":
	case "1":
		// What every tick cost before #71: the agent's collection with the
		// Helm step given no cache, so every release is fetched again.
		realCollect = func(ctx context.Context) inventory.Inventory {
			return collect.Collect(ctx, clients, k, collect.Options{TeamLabel: acfg.TeamLabel})
		}
	default:
		t.Fatalf("UPGRADESCOPE_BENCH_NO_HELM_CACHE=%q: set it to 1 or leave it unset", v)
	}
	r.collectFn = func(ctx context.Context) inventory.Inventory {
		start := time.Now()
		last = realCollect(ctx)
		collectDur = time.Since(start)
		return last
	}

	var out *os.File
	if p := os.Getenv("UPGRADESCOPE_BENCH_OUT"); p != "" {
		if out, err = os.OpenFile(p, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644); err != nil {
			t.Fatal(err)
		}
		defer out.Close()
	}
	for n := 1; n <= ticks; n++ {
		rec.reset()
		proxy.resetCounts()
		runtime.GC()
		sampler := startBenchSampler()
		cpu0 := cpuTime()
		rep := r.runTick(ctx)
		cpu := cpuTime() - cpu0
		heap, sys := sampler.finish()
		stats, total := rec.stats()
		res := benchTick{
			Label: label, Tick: n, WallMS: rep.duration.Milliseconds(), CollectMS: collectDur.Milliseconds(), CPUMS: cpu.Milliseconds(),
			Requests: total, ByVerbResource: stats, BodyBytes: rec.bodyBytes.Load(),
			WireDownBytes: proxy.down.Load(), WireUpBytes: proxy.up.Load(), Connections: proxy.conns.Load(),
			PeakHeapBytes: heap, PeakRuntimeBytes: sys, MaxRSSBytes: maxRSSBytes(),
			Nodes: len(last.Nodes), Namespaces: len(last.Namespaces), HelmReleases: len(last.HelmReleases),
			GitOpsCharts: len(last.GitOpsCharts), AddOns: len(last.AddOns), APIUsage: len(last.APIUsage), Targets: len(rep.reports),
			Capabilities: map[string]string{},
		}
		if rep.err != nil {
			res.Error = rep.err.Error()
		}
		for c, s := range last.Capabilities {
			state := "available"
			switch {
			case !s.Available:
				state = "unavailable: " + s.Reason
			case s.Partial:
				state = "partial: " + s.Reason
			}
			res.Capabilities[string(c)] = state
		}
		t.Logf("tick %d: %d requests, %d MiB body, %d MiB wire, %v wall (%v collecting, %v CPU), peak heap %d MiB, max RSS %d MiB",
			n, res.Requests, res.BodyBytes>>20, res.WireDownBytes>>20, rep.duration.Round(time.Millisecond),
			collectDur.Round(time.Millisecond), cpu.Round(time.Millisecond), res.PeakHeapBytes>>20, res.MaxRSSBytes>>20)
		if out != nil {
			line, _ := json.Marshal(res)
			fmt.Fprintf(out, "%s\n", line)
		}
		if rep.err != nil {
			t.Errorf("tick %d failed: %v", n, rep.err)
		}
		for c, s := range last.Capabilities {
			if !benchCapabilityOK(c, s, gitopsLab) {
				t.Errorf("tick %d: capability %s is not fully available (%s), so the tick did not do the work being measured", n, c, s.Reason)
			}
		}
		if want := envInt(t, "UPGRADESCOPE_BENCH_EXPECT_GITOPS", -1); want >= 0 && res.GitOpsCharts != want {
			t.Errorf("tick %d: inventory has %d GitOps charts, want %d: the collector did not read what was seeded (are rbac and the CRDs there?)", n, res.GitOpsCharts, want)
		}
		if want := envInt(t, "UPGRADESCOPE_BENCH_EXPECT_NODES", -1); want >= 0 && res.Nodes != want {
			t.Errorf("tick %d: inventory has %d nodes, want %d: the cluster is not seeded as expected", n, res.Nodes, want)
		}
		if want := envInt(t, "UPGRADESCOPE_BENCH_EXPECT_HELM", -1); want >= 0 && res.HelmReleases != want {
			t.Errorf("tick %d: inventory has %d Helm releases, want %d", n, res.HelmReleases, want)
		}
	}
}

// benchTick is one tick's measurements, one JSON line each.
type benchTick struct {
	Label            string            `json:"label"`
	Tick             int               `json:"tick"`
	WallMS           int64             `json:"wallMs"`
	CollectMS        int64             `json:"collectMs"`
	CPUMS            int64             `json:"cpuMs"` // the process's CPU time (user and system) over the tick
	Requests         int               `json:"requests"`
	ByVerbResource   []requestStat     `json:"byVerbResource"`
	BodyBytes        int64             `json:"bodyBytes"`        // response bodies as client-go read them (decompressed)
	WireDownBytes    int64             `json:"wireDownBytes"`    // apiserver to agent on the wire, TLS included
	WireUpBytes      int64             `json:"wireUpBytes"`      // agent to apiserver
	Connections      int64             `json:"connections"`      // TCP connections opened during this tick (a kept-alive connection from an earlier tick is not counted again)
	PeakHeapBytes    uint64            `json:"peakHeapBytes"`    // live heap objects, peak during the tick
	PeakRuntimeBytes uint64            `json:"peakRuntimeBytes"` // all memory the Go runtime held, peak during the tick
	MaxRSSBytes      int64             `json:"maxRssBytes"`      // process peak RSS so far (cumulative over ticks)
	Nodes            int               `json:"nodes"`
	Namespaces       int               `json:"namespaces"`
	HelmReleases     int               `json:"helmReleases"`
	GitOpsCharts     int               `json:"gitopsCharts"` // charts read from Argo CD Applications and Flux HelmReleases
	AddOns           int               `json:"addOns"`
	APIUsage         int               `json:"apiUsage"`
	Targets          int               `json:"targets"`
	Capabilities     map[string]string `json:"capabilities"`
	Error            string            `json:"error,omitempty"`
}

// benchCapabilityOK reports whether a capability read as fully as the
// benchmark needs. With the GitOps fill, the helm capability may also be
// partial with only Argo CD skipped (see TestBenchAgentTick).
func benchCapabilityOK(c inventory.Capability, s inventory.CapabilityStatus, gitopsLab bool) bool {
	if !s.Available {
		return false
	}
	if !s.Partial {
		return true
	}
	return gitopsLab && c == inventory.CapHelm && len(s.Skipped) == 1 && s.Skipped[0] == inventory.GitOpsArgoCD
}

func envInt(t *testing.T, name string, def int) int {
	t.Helper()
	s := os.Getenv(name)
	if s == "" {
		return def
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		t.Fatalf("%s=%q is not an integer", name, s)
	}
	return n
}

// benchSampler records the peak of the live heap objects and of all memory
// the Go runtime holds while a tick runs.
type benchSampler struct {
	heap, total atomic.Uint64
	stop        chan struct{}
	done        sync.WaitGroup
}

func startBenchSampler() *benchSampler {
	s := &benchSampler{stop: make(chan struct{})}
	s.done.Add(1)
	go func() {
		defer s.done.Done()
		sample := []metrics.Sample{{Name: "/memory/classes/heap/objects:bytes"}, {Name: "/memory/classes/total:bytes"}}
		tick := time.NewTicker(5 * time.Millisecond)
		defer tick.Stop()
		for {
			metrics.Read(sample)
			if v := sample[0].Value.Uint64(); v > s.heap.Load() {
				s.heap.Store(v)
			}
			if v := sample[1].Value.Uint64(); v > s.total.Load() {
				s.total.Store(v)
			}
			select {
			case <-s.stop:
				return
			case <-tick.C:
			}
		}
	}()
	return s
}

func (s *benchSampler) finish() (heap, total uint64) {
	close(s.stop)
	s.done.Wait()
	return s.heap.Load(), s.total.Load()
}

// benchRESTConfig loads the named kubeconfig (only that file) as the agent
// would, routed through a counting forwarder to the apiserver and with a
// request recorder wrapped around the transport. The TLS session is still
// the client's own with the apiserver: the certificate is checked against
// the kubeconfig's host, not the forwarder's address.
func benchRESTConfig(kubeconfig string) (*rest.Config, *wireProxy, *requestRecorder, error) {
	cfg, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("load %s: %w", kubeconfig, err)
	}
	u, err := url.Parse(cfg.Host)
	if err != nil || u.Host == "" {
		return nil, nil, nil, fmt.Errorf("kubeconfig server %q is not a URL", cfg.Host)
	}
	port := u.Port()
	if port == "" {
		port = "443"
		if u.Scheme == "http" {
			port = "80"
		}
	}
	proxy, err := newWireProxy(net.JoinHostPort(u.Hostname(), port))
	if err != nil {
		return nil, nil, nil, err
	}
	if cfg.TLSClientConfig.ServerName == "" {
		cfg.TLSClientConfig.ServerName = u.Hostname()
	}
	cfg.Host = u.Scheme + "://" + proxy.addr()
	rec := newRequestRecorder()
	cfg.WrapTransport = rec.transport
	cfg.UserAgent = "upgradescope-bench-agent"
	cfg.Timeout = 30 * time.Second // the agent's --request-timeout default
	return cfg, proxy, rec, nil
}
