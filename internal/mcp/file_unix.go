//go:build unix

package mcp

import "syscall"

// openNonblock keeps an open from blocking on a FIFO that ReadFile's stat
// did not see (one swapped in between the stat and the open).
const openNonblock = syscall.O_NONBLOCK
