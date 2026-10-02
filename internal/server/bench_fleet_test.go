package server

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"runtime"
	"runtime/metrics"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/abd-ulbasit/upgradescope/internal/inventory"
)

// Fleet-scale gate (#125 SV-14, `make bench-server`): 500 clusters with
// realistic inventories on SQLite, 10 concurrent /fleet readers. Fails when
// /fleet p95 is over 1s or the live heap peaks over 512MiB (inside the
// chart's 1Gi default memory limit). Off by default: it seeds 500 pushes and runs for
// tens of seconds.
const (
	benchClusters = 500
	benchReaders  = 10
	benchRequests = 10 // per reader
	benchP95      = time.Second
	benchHeap     = 512 << 20
)

// benchInventory is a mid-sized production cluster: 40 nodes, 150
// namespaces, 25 Helm releases, 12 add-ons and 8 flagged APIs with their
// object refs — about 35 KiB of JSON.
func benchInventory(i int) inventory.Inventory {
	inv := testInventoryWithPSP()
	inv.ClusterID = fmt.Sprintf("uid-%d", i)
	for n := range 40 {
		inv.Nodes = append(inv.Nodes, inventory.NodeInfo{Name: fmt.Sprintf("node-%d-%d", i, n), KubeletVersion: "v1.34.2", ContainerRuntime: "containerd://1.7.27"})
	}
	for n := range 150 {
		inv.Namespaces = append(inv.Namespaces, inventory.NamespaceInfo{Name: fmt.Sprintf("team-%d-ns-%d", n%12, n), Team: fmt.Sprintf("team-%d", n%12)})
	}
	for n := range 25 {
		inv.HelmReleases = append(inv.HelmReleases, inventory.HelmRelease{Name: fmt.Sprintf("release-%d", n), Namespace: fmt.Sprintf("team-%d-ns-%d", n%12, n), ChartName: fmt.Sprintf("chart-%d", n), ChartVersion: "1.2.3", AppVersion: "4.5.6", Status: "deployed", Revision: 3})
	}
	for n := range 12 {
		inv.AddOns = append(inv.AddOns, inventory.AddOnInstance{ID: fmt.Sprintf("addon-%d", n), Version: "1.0.0", Namespaces: []string{"kube-system"}, Source: "image"})
	}
	for k := range 8 {
		u := inventory.APIUsage{Group: "example.com", Version: "v1beta1", Kind: fmt.Sprintf("Kind%d", k), Count: 30, Namespaces: map[string]int{}}
		for o := range 30 {
			ns := fmt.Sprintf("team-%d-ns-%d", o%12, o)
			u.Namespaces[ns]++
			u.Objects = append(u.Objects, inventory.ObjectRef{Namespace: ns, Name: fmt.Sprintf("object-%d", o), Manager: "helm"})
		}
		inv.APIUsage = append(inv.APIUsage, u)
	}
	return inv
}

// heapSampler records the peak of live heap object bytes while running.
type heapSampler struct {
	peak atomic.Uint64
	stop chan struct{}
	done sync.WaitGroup
}

func startHeapSampler() *heapSampler {
	h := &heapSampler{stop: make(chan struct{})}
	h.done.Add(1)
	go func() {
		defer h.done.Done()
		sample := []metrics.Sample{{Name: "/memory/classes/heap/objects:bytes"}}
		tick := time.NewTicker(5 * time.Millisecond)
		defer tick.Stop()
		for {
			metrics.Read(sample)
			if v := sample[0].Value.Uint64(); v > h.peak.Load() {
				h.peak.Store(v)
			}
			select {
			case <-h.stop:
				return
			case <-tick.C:
			}
		}
	}()
	return h
}

func (h *heapSampler) finish() uint64 { close(h.stop); h.done.Wait(); return h.peak.Load() }

func TestBenchServerFleet(t *testing.T) {
	if os.Getenv("UPGRADESCOPE_BENCH") != "1" {
		t.Skip("set UPGRADESCOPE_BENCH=1 (make bench-server) to run the fleet-scale gate")
	}
	h := newHarness(t, Config{KB: testKB(), ExtraTargets: []string{"1.36"}}, aug1)
	start := time.Now()
	for i := range benchClusters {
		if code, out := h.push(fmt.Sprintf("cluster-%03d", i), benchInventory(i)); code != http.StatusAccepted {
			t.Fatalf("seed push %d = %d %v", i, code, out)
		}
	}
	t.Logf("seeded %d clusters in %v", benchClusters, time.Since(start).Round(time.Millisecond))

	runtime.GC()
	sampler := startHeapSampler()
	var mu sync.Mutex
	var latencies []time.Duration
	var wg sync.WaitGroup
	errs := make(chan error, benchReaders)
	for range benchReaders {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range benchRequests {
				began := time.Now()
				resp, err := http.Get(h.ts.URL + "/api/v1/fleet")
				if err != nil {
					errs <- err
					return
				}
				_, _ = io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
				if resp.StatusCode != http.StatusOK {
					errs <- fmt.Errorf("GET /fleet = %d", resp.StatusCode)
					return
				}
				mu.Lock()
				latencies = append(latencies, time.Since(began))
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	peak := sampler.finish()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}

	slices.Sort(latencies)
	p50 := latencies[len(latencies)/2]
	p95 := latencies[(len(latencies)*95+99)/100-1]
	t.Logf("/fleet over %d clusters, %d readers x %d requests: p50 %v, p95 %v, max %v; peak live heap %d MiB",
		benchClusters, benchReaders, benchRequests, p50.Round(time.Millisecond), p95.Round(time.Millisecond),
		latencies[len(latencies)-1].Round(time.Millisecond), peak>>20)
	if p95 > benchP95 {
		t.Errorf("/fleet p95 = %v, want under %v", p95, benchP95)
	}
	if peak > benchHeap {
		t.Errorf("peak live heap = %d MiB, want under %d MiB", peak>>20, benchHeap>>20)
	}
}
