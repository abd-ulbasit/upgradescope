//go:build race

package collect

// raceEnabled: the race detector slows the Helm memory test about tenfold.
const raceEnabled = true
