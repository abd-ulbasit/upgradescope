package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"time"
)

// windowRow is one generated managed-provider row, registry.SupportWindow:
// a Kubernetes minor's standard-support end and, where the provider offered
// extended support for it, that support's end.
type windowRow struct {
	minor, standardEnd, extendedEnd string
}

var minorRe = regexp.MustCompile(`^[0-9]+\.[0-9]+$`)

// computeWindows turns an endoflife.date response for a managed Kubernetes
// service (amazon-eks, azure-kubernetes-service) into provider rows, newest
// first as the API returns them. "eol" is the end of standard support and
// "extendedSupport" the end of extended support; a minor the provider never
// offered extended support for publishes false (or nothing) there, and its
// row carries no extended_end. A standard end that is not a date is an error:
// the engine cannot tell when such a minor leaves support.
func computeWindows(apiJSON []byte) ([]windowRow, error) {
	var cycles []struct {
		Cycle           json.RawMessage `json:"cycle"`
		EOL             json.RawMessage `json:"eol"`
		ExtendedSupport json.RawMessage `json:"extendedSupport"`
	}
	if err := json.Unmarshal(apiJSON, &cycles); err != nil {
		return nil, fmt.Errorf("parse endoflife.date response: %w", err)
	}
	if len(cycles) == 0 {
		return nil, fmt.Errorf("endoflife.date response has no cycles")
	}
	rows := make([]windowRow, 0, len(cycles))
	for _, c := range cycles {
		var row windowRow
		if err := json.Unmarshal(c.Cycle, &row.minor); err != nil || !minorRe.MatchString(row.minor) {
			return nil, fmt.Errorf("cycle %s is not a Kubernetes MAJOR.MINOR version", c.Cycle)
		}
		if err := json.Unmarshal(c.EOL, &row.standardEnd); err != nil {
			return nil, fmt.Errorf("cycle %s: eol %s is not a standard-support end date", row.minor, c.EOL)
		}
		if _, err := time.Parse("2006-01-02", row.standardEnd); err != nil {
			return nil, fmt.Errorf("cycle %s: eol date %q: not YYYY-MM-DD", row.minor, row.standardEnd)
		}
		if len(c.ExtendedSupport) > 0 && string(c.ExtendedSupport) != "false" && string(c.ExtendedSupport) != "null" {
			if err := json.Unmarshal(c.ExtendedSupport, &row.extendedEnd); err != nil {
				return nil, fmt.Errorf("cycle %s: extendedSupport %s is neither a date nor false", row.minor, c.ExtendedSupport)
			}
			if _, err := time.Parse("2006-01-02", row.extendedEnd); err != nil {
				return nil, fmt.Errorf("cycle %s: extendedSupport date %q: not YYYY-MM-DD", row.minor, row.extendedEnd)
			}
		}
		rows = append(rows, row)
	}
	return rows, nil
}

// renderWindows renders the provider versions block: one flow mapping per
// minor, so a refresh diff shows exactly which minors changed.
func renderWindows(rows []windowRow) []byte {
	var b bytes.Buffer
	b.WriteString("versions:\n")
	for _, r := range rows {
		fmt.Fprintf(&b, "  - {minor: %q, standard_end: %q", r.minor, r.standardEnd)
		if r.extendedEnd != "" {
			fmt.Fprintf(&b, ", extended_end: %q", r.extendedEnd)
		}
		b.WriteString("}\n")
	}
	return b.Bytes()
}
