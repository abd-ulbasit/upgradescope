//go:build !unix

package collect

import "time"

// processCPU is not measured off Unix: the benchmark reports 0.
func processCPU() time.Duration { return 0 }
