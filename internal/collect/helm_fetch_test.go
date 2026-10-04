package collect

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"net/http/httptest"
	"os"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/rest"
	clienttesting "k8s.io/client-go/testing"

	"github.com/abd-ulbasit/upgradescope/internal/inventory"
	"github.com/abd-ulbasit/upgradescope/internal/kb"
)

// startFetching hands the payloads over in index order however the fetches
// finish, and never holds more than workers of them: the ones being
// fetched, fetched and waiting, and the one the caller is decoding.
func TestStartFetchingHandsOverInOrderWithinItsBound(t *testing.T) {
	const n, workers = 200, 8
	var held, maxHeld atomic.Int64
	fetch := func(_ context.Context, i int) ([]byte, error) {
		if h := held.Add(1); h > maxHeld.Load() {
			maxHeld.Store(h) // only an upper bound is checked, so a lost race is harmless: it under-reports
		}
		time.Sleep(time.Duration(rand.IntN(400)) * time.Microsecond)
		if i%7 == 3 {
			return nil, fmt.Errorf("fetch %d failed", i)
		}
		return []byte(strconv.Itoa(i)), nil
	}
	f := startFetching(context.Background(), n, workers, fetch)
	defer f.stop()
	for i := range n {
		data, err := f.next()
		if i%7 == 3 {
			if err == nil || err.Error() != fmt.Sprintf("fetch %d failed", i) {
				t.Fatalf("result %d: err = %v, want fetch %d's error", i, err, i)
			}
		} else if err != nil || string(data) != strconv.Itoa(i) {
			t.Fatalf("result %d = %q, %v; want fetch %d's payload", i, data, err, i)
		}
		time.Sleep(time.Duration(rand.IntN(200)) * time.Microsecond) // decoding
		held.Add(-1)                                                 // done with it before the next call
	}
	if m := maxHeld.Load(); m > workers {
		t.Errorf("up to %d payloads held at once, want at most %d", m, workers)
	}
}

// With one worker, or one payload, nothing runs on another goroutine: each
// next call fetches, as the loop did before #226.
func TestStartFetchingWithOneWorkerFetchesOnNext(t *testing.T) {
	for _, tc := range []struct{ n, workers int }{{5, 1}, {1, 8}} {
		calls := 0
		f := startFetching(context.Background(), tc.n, tc.workers, func(_ context.Context, i int) ([]byte, error) {
			calls++
			return []byte{byte(i)}, nil
		})
		for i := range tc.n {
			if calls != i {
				t.Fatalf("n=%d workers=%d: %d fetches before next call %d, want %d", tc.n, tc.workers, calls, i, i)
			}
			if data, _ := f.next(); data[0] != byte(i) {
				t.Fatalf("next %d = %v", i, data)
			}
		}
		f.stop()
	}
}

// A canceled context ends the fetches in flight and fails every later one
// at once, and stop leaves no goroutine behind, even when the caller stops
// before taking every payload.
func TestStartFetchingStopsPromptlyOnCancel(t *testing.T) {
	before := runtime.NumGoroutine()
	ctx, cancel := context.WithCancel(context.Background())
	var started atomic.Int64
	fetch := func(ctx context.Context, i int) ([]byte, error) {
		if ctx.Err() != nil {
			return nil, ctx.Err() // a client checks its context before it sends
		}
		started.Add(1)
		<-ctx.Done() // a request that never answers
		return nil, ctx.Err()
	}
	f := startFetching(ctx, 1000, 8, fetch)
	time.AfterFunc(20*time.Millisecond, cancel)
	start := time.Now()
	for i := range 500 { // half of them, then stop
		if _, err := f.next(); !errors.Is(err, context.Canceled) {
			t.Fatalf("result %d: err = %v, want context.Canceled", i, err)
		}
	}
	f.stop()
	if d := time.Since(start); d > 5*time.Second {
		t.Errorf("took %v after the cancel to fail the fetches", d)
	}
	if s := started.Load(); s > 8 {
		t.Errorf("%d fetches reached the network, want at most the 8 in flight when the context was canceled", s)
	}
	deadline := time.Now().Add(5 * time.Second)
	for runtime.NumGoroutine() > before && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if g := runtime.NumGoroutine(); g > before {
		t.Errorf("%d goroutines after stop, %d before: the fetcher leaked", g, before)
	}
}

// helmFetchFixture is a cluster of Helm releases with every outcome a
// release can have: read, from either driver; deleted since the list; GET
// refused; payload not decodable; manifest not fully parsed; uninstalled;
// failed upgrade over a deployed revision; upgraded rewrites two of them.
// GETs answer after a random
// delay, so concurrent fetches finish out of order.
func helmFetchFixture(t *testing.T, upgraded bool) []k8sruntime.Object {
	t.Helper()
	var objs []k8sruntime.Object
	big := "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: big\ndata:\n" + strings.Repeat(" k: v\n", maxManifestNodes/2+10)
	for i := range 60 {
		ns := fmt.Sprintf("team-%d", i%7)
		r := helmRev{ns: ns, release: fmt.Sprintf("rel-%02d", i), rev: 1, status: "deployed",
			chart: []string{"cert-manager", "ingress-nginx", "app"}[i%3], chartVersion: fmt.Sprintf("1.%d.0", i), appVersion: fmt.Sprintf("v1.%d.0", i),
			manifest: "apiVersion: policy/v1beta1\nkind: PodSecurityPolicy\nmetadata:\n  name: p\n",
			uid:      fmt.Sprintf("uid-%d", i), rv: fmt.Sprintf("%d", 100+i)}
		if upgraded && (i == 5 || i == 21) {
			r.chartVersion, r.rv = "2.0.0", r.rv+"-upgraded"
		}
		switch i % 10 {
		case 0: // configmaps driver
			objs = append(objs, helmConfigMap(t, r))
			continue
		case 1: // not decodable
			s := helmSecret(t, r)
			s.Data["release"] = []byte("not base64 at all!")
			objs = append(objs, s)
			continue
		case 2: // manifest with a document over the node bound
			r.manifest = big
		case 3: // uninstalled with history kept
			r.status = "uninstalled"
		case 4: // a failed upgrade over a deployed revision
			prev := r
			prev.status = "deployed"
			objs = append(objs, helmSecret(t, prev))
			r.rev, r.status, r.uid, r.rv = 2, "failed", r.uid+"-2", r.rv+"0"
		}
		objs = append(objs, helmSecret(t, r))
	}
	return objs
}

// helmFetchReactors make the fixture's GETs answer late and out of order,
// refuse some, and report others deleted since the list.
func helmFetchReactors(kube interface {
	PrependReactor(verb, resource string, reaction clienttesting.ReactionFunc)
}) {
	var mu sync.Mutex
	rng := rand.New(rand.NewPCG(7, 8))
	react := func(a clienttesting.Action) (bool, k8sruntime.Object, error) {
		g := a.(clienttesting.GetAction)
		mu.Lock()
		d := time.Duration(rng.IntN(3000)) * time.Microsecond
		mu.Unlock()
		time.Sleep(d)
		switch g.GetName() {
		case "sh.helm.release.v1.rel-15.v1", "sh.helm.release.v1.rel-27.v1", "sh.helm.release.v1.rel-48.v1":
			return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: g.GetResource().Resource}, g.GetName(), errors.New("no get"))
		case "sh.helm.release.v1.rel-38.v1", "sh.helm.release.v1.rel-55.v1":
			return true, nil, apierrors.NewNotFound(schema.GroupResource{Resource: g.GetResource().Resource}, g.GetName())
		}
		return false, nil, nil
	}
	kube.PrependReactor("get", "secrets", react)
	kube.PrependReactor("get", "configmaps", react)
}

// The concurrent fetch changes nothing but the time a cold step takes
// (#226): against the same API, with GETs finishing out of order, the
// inventory, the error and its reason, the gaps and their order, and what
// the cache keeps are those of fetching one release at a time, on a cold
// cache and on a warm one after releases changed.
func TestCollectHelmConcurrentMatchesSequential(t *testing.T) {
	lifecycle := []kb.APILifecycleEntry{{Group: "policy", Version: "v1beta1", Kind: "PodSecurityPolicy", Removed: &inventory.Version{Major: 1, Minor: 25}}}
	type outcome struct {
		inv     inventory.Inventory
		err     string
		partial partialError
		entries map[helmCacheKey]helmCacheEntry
	}
	run := func(workers int) []outcome {
		cache := NewHelmCache()
		var outs []outcome
		for pass := range 2 {
			// The second pass is a warm cache with misses among hits: two
			// releases were upgraded in place (a new resourceVersion).
			kube, meta := helmClients(t, helmFetchFixture(t, pass == 1)...)
			helmFetchReactors(kube)
			var o outcome
			err := collectHelmFetching(context.Background(), kube, meta, lifecycle, cache, workers, &o.inv)
			if err != nil {
				o.err = err.Error()
				errors.As(err, &o.partial)
			}
			o.entries = map[helmCacheKey]helmCacheEntry{}
			for k, e := range cache.entries {
				o.entries[k] = e
			}
			outs = append(outs, o)
		}
		return outs
	}
	want := run(1)
	t.Logf("one at a time: %s; skipped %q", want[0].err, want[0].partial.skipped)
	if !want[0].partial.incomplete || len(want[0].inv.HelmReleases) == 0 || len(want[0].partial.skipped) < 6 {
		t.Fatalf("fixture does not exercise the outcomes: partial %+v, %d releases", want[0].partial, len(want[0].inv.HelmReleases))
	}
	for attempt := range 5 {
		got := run(helmFetchWorkers)
		for pass := range want {
			if !reflect.DeepEqual(got[pass], want[pass]) {
				t.Fatalf("attempt %d, pass %d: %d workers gave\n%+v\nerr %q\none at a time gave\n%+v\nerr %q",
					attempt, pass, helmFetchWorkers, got[pass].inv.HelmReleases, got[pass].err, want[pass].inv.HelmReleases, want[pass].err)
			}
		}
	}
}

// benchReleasePayload is a release payload shaped like the scale lab's
// (hack/bench/seed/helm.go): a manifest of objects rendered with
// per-release values, and bundled CRD-like chart files, as Helm's
// base64(gzip(JSON)).
func benchReleasePayload(t testing.TB, seed uint64, objects, files int) string {
	t.Helper()
	rng := rand.New(rand.NewPCG(seed, 1))
	var manifest strings.Builder
	for o := range objects {
		fmt.Fprintf(&manifest, "---\n# Source: app/templates/obj-%d.yaml\napiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: app-%d\n  labels:\n    checksum: %016x%016x\ndata:\n  config.yaml: |\n    port: %d\n    upstream: https://svc-%d.internal:%d\n", o, o, rng.Uint64(), rng.Uint64(), 8000+rng.IntN(1000), rng.IntN(500), 8000+rng.IntN(1000))
	}
	var chartFiles []map[string]string
	for f := range files {
		var crd strings.Builder
		fmt.Fprintf(&crd, "apiVersion: apiextensions.k8s.io/v1\nkind: CustomResourceDefinition\nmetadata:\n  name: kind%d.example.com\nspec:\n  versions:\n  - name: v1\n    schema:\n      openAPIV3Schema:\n        properties:\n", f)
		for p := range 120 {
			fmt.Fprintf(&crd, "          field%d:\n            type: string\n            description: %016x%016x controls item %d\n            maxLength: %d\n", p, rng.Uint64(), rng.Uint64(), p, 16+rng.IntN(240))
		}
		chartFiles = append(chartFiles, map[string]string{"name": fmt.Sprintf("crds/crd-%d.yaml", f), "data": base64.StdEncoding.EncodeToString([]byte(crd.String()))})
	}
	doc, err := json.Marshal(map[string]any{
		"info":     map[string]any{"status": "deployed"},
		"chart":    map[string]any{"metadata": map[string]any{"name": "app", "version": "1.2.3", "appVersion": "v1.2.3", "kubeVersion": ">=1.21.0-0"}, "files": chartFiles},
		"manifest": manifest.String(),
	})
	if err != nil {
		t.Fatal(err)
	}
	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	zw.Write(doc)
	zw.Close()
	return base64.StdEncoding.EncodeToString(gz.Bytes())
}

// benchReleaseMix is 100 payloads in the scale lab's mix: 80 small (12
// objects, 1 file), 15 medium (60, 4) and 5 large (300, 20) releases.
func benchReleaseMix(t testing.TB) []string {
	t.Helper()
	var out []string
	for i := range 100 {
		switch {
		case i < 80:
			out = append(out, benchReleasePayload(t, uint64(i), 12, 1))
		case i < 95:
			out = append(out, benchReleasePayload(t, uint64(i), 60, 4))
		default:
			out = append(out, benchReleasePayload(t, uint64(i), 300, 20))
		}
	}
	return out
}

// coldHelmStep runs a cold Helm step (no cache) with the given workers
// against srv, through the clients the agent and scan build (NewClients:
// client-go's own rate limit of clientQPS, burst clientBurst).
func coldHelmStep(t testing.TB, srv *httptest.Server, workers int) (inventory.Inventory, time.Duration) {
	t.Helper()
	c, err := NewClients(&rest.Config{Host: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	lifecycle := []kb.APILifecycleEntry{{Version: "v1", Kind: "ConfigMap", Deprecated: &inventory.Version{Major: 1, Minor: 99}}}
	var inv inventory.Inventory
	start := time.Now()
	err = collectHelmFetching(context.Background(), c.Kube, c.Metadata, lifecycle, nil, workers, &inv)
	d := time.Since(start)
	if pe := (partialError{}); !errors.As(err, &pe) || pe.incomplete {
		t.Fatalf("%d workers: err = %v, want every release read", workers, err)
	}
	return inv, d
}

// A cold Helm step against an apiserver 25 ms away is at least twice as
// fast with the concurrent fetch, reads the same releases, and never has
// more than helmFetchWorkers GETs in flight (#226). The numbers in
// docs/operations/scale.md are TestCollectHelmColdFetchAtRoundTrip's.
func TestCollectHelmFetchesConcurrentlyWithinItsBound(t *testing.T) {
	if testing.Short() {
		t.Skip("waits on simulated round trips")
	}
	const releases, rtt = 64, 25 * time.Millisecond
	srv, gets, maxInflight := helmAPIServerWith(t, releases, 1, []string{benchReleasePayload(t, 1, 12, 1)}, rtt)
	seqInv, seq := coldHelmStep(t, srv, 1)
	if m := maxInflight.Load(); m != 1 {
		t.Errorf("one worker had %d GETs in flight, want 1", m)
	}
	maxInflight.Store(0)
	gets.Store(0)
	conInv, con := coldHelmStep(t, srv, helmFetchWorkers)
	t.Logf("%d releases at %v: one at a time %v, %d workers %v (%.1fx)", releases, rtt, seq.Round(time.Millisecond), helmFetchWorkers, con.Round(time.Millisecond), seq.Seconds()/con.Seconds())
	if !reflect.DeepEqual(conInv, seqInv) {
		t.Errorf("the concurrent fetch read other releases than the sequential one")
	}
	if g := gets.Load(); g != releases {
		t.Errorf("%d GETs, want one per release (%d)", g, releases)
	}
	if m := maxInflight.Load(); m > helmFetchWorkers || m < 2 {
		t.Errorf("%d GETs in flight at most, want 2 to %d", m, helmFetchWorkers)
	}
	if con*2 > seq {
		t.Errorf("concurrent %v, sequential %v: want at least 2x faster", con, seq)
	}
}

// TestCollectHelmColdFetchAtRoundTrip measures a cold Helm step at 1,000
// releases in the scale lab's mix against an apiserver a round trip away,
// for each number of workers (docs/operations/scale.md, #226). Manual:
// UPGRADESCOPE_BENCH_HELM_RTT=60ms go test -run TestCollectHelmColdFetchAtRoundTrip -v ./internal/collect
// (UPGRADESCOPE_BENCH_HELM_RELEASES and UPGRADESCOPE_BENCH_HELM_WORKERS,
// a comma-separated list, change the shape). The round trip is a delay
// before every answer of a local server, so it is all wait and no
// bandwidth limit; the payloads are generated, so decoding them costs about
// what the lab's do.
func TestCollectHelmColdFetchAtRoundTrip(t *testing.T) {
	rttEnv := os.Getenv("UPGRADESCOPE_BENCH_HELM_RTT")
	if rttEnv == "" {
		t.Skip("set UPGRADESCOPE_BENCH_HELM_RTT (e.g. 60ms) to measure a cold Helm step")
	}
	rtt, err := time.ParseDuration(rttEnv)
	if err != nil {
		t.Fatal(err)
	}
	releases := 1000
	if v := os.Getenv("UPGRADESCOPE_BENCH_HELM_RELEASES"); v != "" {
		if releases, err = strconv.Atoi(v); err != nil {
			t.Fatal(err)
		}
	}
	workers := []int{1, 2, 4, 8, 16}
	if v := os.Getenv("UPGRADESCOPE_BENCH_HELM_WORKERS"); v != "" {
		workers = nil
		for _, f := range strings.Split(v, ",") {
			n, err := strconv.Atoi(f)
			if err != nil {
				t.Fatal(err)
			}
			workers = append(workers, n)
		}
	}
	mix := benchReleaseMix(t)
	stored := 0
	for _, p := range mix {
		stored += len(p)
	}
	srv, _, maxInflight := helmAPIServerWith(t, releases, 1, mix, rtt)
	// The decoding alone, with no wait: what no number of workers removes.
	var decode time.Duration
	{
		flagged := map[gvk]bool{{"", "v1", "ConfigMap"}: true} // as coldHelmStep's lifecycle flags
		start := time.Now()
		for i := range releases {
			decodeHelmEntry([]byte(mix[i%len(mix)]), flagged)
		}
		decode = time.Since(start)
	}
	t.Logf("%d releases, %.1f KiB stored each on average, round trip %v; decoding them alone took %v (GOMAXPROCS %d)",
		releases, float64(stored)/float64(len(mix))/1024, rtt, decode.Round(time.Millisecond), runtime.GOMAXPROCS(0))
	var base time.Duration
	var want inventory.Inventory
	for _, w := range workers {
		maxInflight.Store(0)
		runtime.GC()
		cpu0 := processCPU()
		inv, d := coldHelmStep(t, srv, w)
		cpu := processCPU() - cpu0
		if base == 0 {
			base, want = d, inv
		} else if !reflect.DeepEqual(inv, want) {
			t.Errorf("%d workers read other releases than %d", w, workers[0])
		}
		t.Logf("workers %2d: %7.2f s wall, %6.2f s CPU, %d GETs in flight at most, %.2fx the first", w, d.Seconds(), cpu.Seconds(), maxInflight.Load(), base.Seconds()/d.Seconds())
	}
}
