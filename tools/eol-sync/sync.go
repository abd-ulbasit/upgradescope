package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"time"
)

var slugRe = regexp.MustCompile(`(?m)^endoflife_product:[ \t]*([^\s#]+)`)

// extractSlug returns the endoflife_product slug declared at the top level
// of a registry YAML entry, or "" for hand-curated entries.
func extractSlug(raw []byte) string {
	m := slugRe.FindSubmatch(raw)
	if m == nil {
		return ""
	}
	return string(m[1])
}

// computeSupport applies the newest-cycle EOL rule (see package doc) to an
// endoflife.date API response and returns the desired support.status and
// support.eol_date values.
func computeSupport(apiJSON []byte, now time.Time) (status, eolDate string, err error) {
	var cycles []struct {
		Cycle json.RawMessage `json:"cycle"`
		EOL   json.RawMessage `json:"eol"`
	}
	if err := json.Unmarshal(apiJSON, &cycles); err != nil {
		return "", "", fmt.Errorf("parse endoflife.date response: %w", err)
	}
	if len(cycles) == 0 {
		return "", "", fmt.Errorf("endoflife.date response has no cycles")
	}
	newest := cycles[0] // API returns cycles newest-first

	var b bool
	if err := json.Unmarshal(newest.EOL, &b); err == nil {
		if b {
			return "eol", "", nil
		}
		return "supported", "", nil
	}
	var s string
	if err := json.Unmarshal(newest.EOL, &s); err != nil {
		return "", "", fmt.Errorf("newest cycle eol field %s is neither bool nor string", newest.EOL)
	}
	d, err := time.Parse("2006-01-02", s)
	if err != nil {
		return "", "", fmt.Errorf("newest cycle eol date %q: not YYYY-MM-DD", s)
	}
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	if !d.After(today) {
		return "eol", s, nil
	}
	return "supported", s, nil
}

var (
	statusLineRe  = regexp.MustCompile(`^[ \t]*status:`)
	eolDateLineRe = regexp.MustCompile(`^[ \t]*eol_date:`)
)

// rewriteSupport returns raw with the support block's status and eol_date
// lines set to the given values; every other byte is preserved. An empty
// date removes the eol_date line; a missing one is inserted right after
// status. Only lines inside the top-level `support:` block are touched.
func rewriteSupport(raw []byte, status, eolDate string) ([]byte, error) {
	lines := bytes.SplitAfter(raw, []byte("\n"))
	var out [][]byte
	inSupport, statusSeen := false, false
	for _, line := range lines {
		trimmed := bytes.TrimRight(line, "\n")
		switch {
		case bytes.Equal(trimmed, []byte("support:")):
			inSupport = true
			out = append(out, line)
			continue
		case inSupport && len(trimmed) > 0 && trimmed[0] != ' ' && trimmed[0] != '\t':
			inSupport = false // left the block (next top-level key)
		}
		if !inSupport {
			out = append(out, line)
			continue
		}
		switch {
		case statusLineRe.Match(trimmed):
			statusSeen = true
			out = append(out, []byte("  status: "+status+"\n"))
			if eolDate != "" {
				out = append(out, []byte("  eol_date: \""+eolDate+"\"\n"))
			}
		case eolDateLineRe.Match(trimmed):
			// dropped: re-emitted (with the desired value) right after status
		default:
			out = append(out, line)
		}
	}
	if !statusSeen {
		return nil, fmt.Errorf("no status line found inside a top-level support: block")
	}
	return bytes.Join(out, nil), nil
}
