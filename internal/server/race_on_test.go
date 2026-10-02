//go:build race

package server

// raceEnabled: the race detector instruments every allocation, which makes
// the heap-bound tests slow (minutes) and their heap figures meaningless,
// so they skip under it. make test-heap (CI's test-heap job) runs every
// test that reads raceEnabled without -race, and with heapRun set.
const raceEnabled = true
