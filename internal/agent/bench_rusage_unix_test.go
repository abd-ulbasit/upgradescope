//go:build unix

package agent

import (
	"runtime"
	"syscall"
)

// maxRSSBytes is this process's peak resident set size so far. Linux
// reports ru_maxrss in KiB, the BSDs and macOS in bytes.
func maxRSSBytes() int64 {
	var ru syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &ru); err != nil {
		return 0
	}
	if runtime.GOOS == "linux" {
		return int64(ru.Maxrss) << 10
	}
	return int64(ru.Maxrss)
}
