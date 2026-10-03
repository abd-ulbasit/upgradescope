//go:build !unix

package agent

// maxRSSBytes is not measured off Unix: the benchmark reports 0.
func maxRSSBytes() int64 { return 0 }
