//go:build !race

package engine

// raceDetector: whether the tests run under the race detector, whose
// slowdown is not uniform and so invalidates a timing ratio.
const raceDetector = false
