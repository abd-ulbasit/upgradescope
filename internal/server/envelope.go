package server

import (
	"fmt"
	"strconv"
)

// The push envelope's agentVersion and kbVersion are labels the agent
// writes about itself (its release, the datasets it judged by) and the
// server stores beside the snapshot; the server quotes neither in anything
// it builds (judgedView names digests it parsed itself), but they are
// agent-controlled text, so they are bounded and printable at ingest like
// every other identifier a push carries. A genuine one is short: a semantic
// version, "dev" or "(devel)"; kb.Version is a Go module and version, two
// 8-character digests and their labels.
const (
	maxAgentVersionBytes = 128
	maxKBVersionBytes    = 512
)

// printableASCII reports whether s is only the printable ASCII characters
// (space to tilde): no control character, newline or escape sequence, no
// bidirectional override.
func printableASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < 0x20 || s[i] > 0x7e {
			return false
		}
	}
	return true
}

// validateEnvelopeLabel checks a push's agentVersion or kbVersion (empty is
// valid: a v0.1.x agent sent no kbVersion). The error does not echo the
// value.
func validateEnvelopeLabel(field, s string, max int) error {
	if len(s) > max {
		return fmt.Errorf("%s is %d bytes, over the %d a genuine one takes", field, len(s), max)
	}
	if !printableASCII(s) {
		return fmt.Errorf("%s has a character that is not printable ASCII (a control character, newline or non-ASCII one)", field)
	}
	return nil
}

// quoteLabel quotes an agent's version label for a reason when it is a
// genuine one, and says that it is not otherwise: a snapshot stored before
// ingest checked labels carries whatever its agent sent, and every read
// path builds reasons from it.
func quoteLabel(s string) string {
	if len(s) > maxAgentVersionBytes || !printableASCII(s) {
		return "(a version label that is not a plain, short one)"
	}
	return strconv.Quote(s)
}
