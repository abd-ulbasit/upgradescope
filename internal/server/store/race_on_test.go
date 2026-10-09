//go:build race

package store

// raceEnabled: the race detector slows every store call several-fold, so
// the wall-clock bounds of the timing tests do not apply under it.
const raceEnabled = true
