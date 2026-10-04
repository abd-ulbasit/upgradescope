package collect

import (
	"context"
	"sync"
)

// helmFetchWorkers is how many Helm release payloads are fetched at once
// (#226). The releases the cache does not hold (all of them on the agent's
// first tick and in every one-shot scan) were fetched one GET at a time, so
// over a link of round trip R a cold Helm step waited R per release on top
// of decoding it: 1,000 releases at 60 ms are a minute of waiting. Eight
// GETs in flight cut that wait eightfold, which takes it under the client's
// own rate limit (clientQPS after a burst of clientBurst: 14 s for 1,000
// releases) and under the decoding, so more workers would gain nothing
// there; docs/operations/scale.md has the measurements the number was
// picked from. It is also what a cold step may hold beside the release
// being decoded: at most helmFetchWorkers payloads, each at most the 1 MiB
// of data Kubernetes allows a Secret or ConfigMap (see startFetching).
const helmFetchWorkers = 8

// fetcher hands out the results of fetch(0), fetch(1), … in that order,
// fetched by up to workers goroutines ahead of the caller.
type fetcher struct {
	next func() ([]byte, error)
	stop func()
}

// startFetching starts fetching the n payloads fetch(ctx, i) returns, on up
// to workers goroutines, and returns a fetcher whose next hands them over in
// index order, waiting for the one due when it has not arrived. At most
// workers payloads are held at once, counting those being fetched, those
// fetched and not yet handed over, and the last one handed over until next
// is called again: a fetch starts only when there is room, so a caller that
// is slower than the network (decoding) never has the cluster's releases
// pile up in memory. Decoding stays on the caller's goroutine, one release
// at a time.
//
// fetch gets ctx as it is: when ctx is done, each fetch not yet started
// fails at once with ctx's error, as the one-at-a-time loop's did, so the
// caller sees the same failures in the same order. stop, which the caller
// must call (deferred) however it leaves, stops starting fetches and waits
// for the ones in flight to return, so no goroutine outlives the step.
//
// workers ≤ 1 fetches on the caller's goroutine, one payload per next call:
// the loop as it was before #226.
func startFetching(ctx context.Context, n, workers int, fetch func(ctx context.Context, i int) ([]byte, error)) fetcher {
	if workers <= 1 || n <= 1 {
		i := 0
		return fetcher{
			next: func() ([]byte, error) {
				defer func() { i++ }()
				return fetch(ctx, i)
			},
			stop: func() {},
		}
	}
	workers = min(workers, n)
	type result struct {
		data []byte
		err  error
	}
	results := make([]chan result, n) // results[i] carries fetch(i), buffered so a worker never waits on the caller
	for i := range results {
		results[i] = make(chan result, 1)
	}
	room := make(chan struct{}, workers) // one token per payload held
	jobs := make(chan int)
	quit := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { // hands out indexes in order, each once there is room for its payload
		defer wg.Done()
		defer close(jobs)
		for i := range n {
			select {
			case room <- struct{}{}:
			case <-quit:
				return
			}
			select {
			case jobs <- i:
			case <-quit:
				return
			}
		}
	}()
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range jobs {
				data, err := fetch(ctx, i)
				results[i] <- result{data, err}
			}
		}()
	}
	next, held := 0, false
	var once sync.Once
	return fetcher{
		next: func() ([]byte, error) {
			if held { // the payload handed over last is done with
				<-room
			}
			r := <-results[next]
			results[next] = nil
			next, held = next+1, true
			return r.data, r.err
		},
		stop: func() {
			once.Do(func() {
				close(quit)
				wg.Wait()
			})
		},
	}
}
