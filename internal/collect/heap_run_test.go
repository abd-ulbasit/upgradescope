package collect

import "os"

// heapRun is set when hack/test-heap.sh (make test-heap, CI's test-heap
// job) runs the heap-bound tests. TestCollectHelmManifestParsingIsBounded
// reads a heap figure that a machine busy with other packages' tests
// inflates, so a plain `go test ./...` skips it, as the server's heap
// tests are skipped (internal/server's heapRun); the parsing bounds it
// relies on are pinned without heap figures by
// TestSplitManifestRunsHoldTheBounds, which always runs.
var heapRun = os.Getenv("UPGRADESCOPE_HEAP") == "1"
