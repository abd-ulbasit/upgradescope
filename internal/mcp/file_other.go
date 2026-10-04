//go:build !unix

package mcp

// openNonblock is 0 where there is no O_NONBLOCK: ReadFile's stat is the
// check there.
const openNonblock = 0
