package server

import "os"

// heapRun is set when hack/test-heap.sh (make test-heap, CI's test-heap
// job) runs the heap-bound tests: they take about ten minutes in all, past
// go test's default timeout, so a plain `go test ./...` skips them, as
// `go test -race` does (raceEnabled). Every heap-bound test checks both:
// `if testing.Short() || raceEnabled || !heapRun`.
var heapRun = os.Getenv("UPGRADESCOPE_HEAP") == "1"
