//go:build !unix

package agent

import "time"

// maxRSSBytes and cpuTime are not measured off Unix: the benchmark reports 0.
func maxRSSBytes() int64 { return 0 }

func cpuTime() time.Duration { return 0 }
