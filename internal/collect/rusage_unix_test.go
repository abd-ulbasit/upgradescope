//go:build unix

package collect

import (
	"syscall"
	"time"
)

// processCPU is the CPU time (user and system) this process has used.
func processCPU() time.Duration {
	var ru syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &ru); err != nil {
		return 0
	}
	return time.Duration(ru.Utime.Nano() + ru.Stime.Nano())
}
