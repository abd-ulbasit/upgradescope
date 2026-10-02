//go:build race

package server

// raceEnabled: the race detector instruments every allocation, which makes
// the heap-bound tests slow (minutes) and their heap figures meaningless.
const raceEnabled = true
