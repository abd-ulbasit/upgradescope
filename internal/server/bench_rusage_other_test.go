//go:build !unix

package server

import "time"

// cpuTime is not measured off Unix: the benchmark reports 0.
func cpuTime() time.Duration { return 0 }
