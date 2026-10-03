package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
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

// cycleRow is one generated registry cycle. eol is already a YAML scalar:
// a quoted date, true or false.
type cycleRow struct {
	cycle, eol, k8sMin, k8sMax string
}

var (
	// registry.Validate's cycle grammar: the leading components of a version.
	cycleNameRe = regexp.MustCompile(`^[0-9]+(\.[0-9]+)*$`)
	// "1.32 - 1.36", "1.23.3 - 1.25" (bounds cut to MAJOR.MINOR), "1.17+".
	k8sRangeRe = regexp.MustCompile(`^(\d+\.\d+)(?:\.\d+)?\s*(?:(\+)|-\s*(\d+\.\d+)(?:\.\d+)?)$`)
)

// computeCycles turns an endoflife.date API response into registry cycle
// rows, newest first as the API returns them. Each cycle's "eol" is a date
// or a boolean; the supported Kubernetes range is copied where the product
// publishes one (Istio and KEDA as supportedKubernetesVersions, Kyverno as
// supportedK8sVersions).
func computeCycles(apiJSON []byte) ([]cycleRow, error) {
	var cycles []struct {
		Cycle         json.RawMessage `json:"cycle"`
		EOL           json.RawMessage `json:"eol"`
		K8sVersions   string          `json:"supportedKubernetesVersions"`
		K8sVersionsV2 string          `json:"supportedK8sVersions"`
	}
	if err := json.Unmarshal(apiJSON, &cycles); err != nil {
		return nil, fmt.Errorf("parse endoflife.date response: %w", err)
	}
	if len(cycles) == 0 {
		return nil, fmt.Errorf("endoflife.date response has no cycles")
	}
	rows := make([]cycleRow, 0, len(cycles))
	for _, c := range cycles {
		var row cycleRow
		if err := json.Unmarshal(c.Cycle, &row.cycle); err != nil {
			row.cycle = string(c.Cycle) // some products publish numeric cycles
		}
		if !cycleNameRe.MatchString(row.cycle) {
			return nil, fmt.Errorf("cycle %s is not a dotted version; the registry cannot map installed versions to it", c.Cycle)
		}
		var ended bool
		var date string
		switch {
		case json.Unmarshal(c.EOL, &ended) == nil:
			row.eol = fmt.Sprint(ended)
		case json.Unmarshal(c.EOL, &date) == nil:
			if _, err := time.Parse("2006-01-02", date); err != nil {
				return nil, fmt.Errorf("cycle %s: eol date %q: not YYYY-MM-DD", row.cycle, date)
			}
			row.eol = `"` + date + `"`
		default:
			return nil, fmt.Errorf("cycle %s: eol field %s is neither bool nor string", row.cycle, c.EOL)
		}
		if k8s := strings.TrimSpace(c.K8sVersions + c.K8sVersionsV2); k8s != "" {
			m := k8sRangeRe.FindStringSubmatch(k8s)
			if m == nil {
				return nil, fmt.Errorf("cycle %s: supported Kubernetes versions %q: want \"1.32 - 1.36\" or \"1.17+\"", row.cycle, k8s)
			}
			row.k8sMin, row.k8sMax = m[1], m[3]
		}
		rows = append(rows, row)
	}
	return rows, nil
}

// allEnded reports whether every cycle has ended by now: the product may
// be retired as a whole, which only a human records (support.status).
func allEnded(rows []cycleRow, now time.Time) bool {
	today := now.UTC().Format("2006-01-02")
	for _, r := range rows {
		switch {
		case r.eol == "false":
			return false
		case r.eol != "true" && strings.Trim(r.eol, `"`) > today:
			return false
		}
	}
	return true
}

// renderCycles renders the registry cycles block: one flow mapping per
// cycle, so a refresh diff shows exactly which release lines changed.
func renderCycles(rows []cycleRow, citation string) []byte {
	var b bytes.Buffer
	b.WriteString("cycles:\n")
	for _, r := range rows {
		fmt.Fprintf(&b, "  - {cycle: %q, eol: %s", r.cycle, r.eol)
		if r.k8sMin != "" {
			fmt.Fprintf(&b, ", k8s_min: %q", r.k8sMin)
		}
		if r.k8sMax != "" {
			fmt.Fprintf(&b, ", k8s_max: %q", r.k8sMax)
		}
		fmt.Fprintf(&b, ", citations: [%q]}\n", citation)
	}
	return b.Bytes()
}

// rewriteCycles returns raw with its top-level cycles block replaced by
// block, or block appended when there is none. The block runs from the
// "cycles:" key (whatever follows it on the line: "[]", a comment) to the
// next top-level key; blank lines and column-0 comments just before that key
// stay with it. Every other byte is preserved, CRLF line endings included.
func rewriteCycles(raw, block []byte) []byte { return rewriteBlock(raw, "cycles", block) }

// rewriteBlock is rewriteCycles for any top-level key: a provider entry
// has its "versions:" block replaced the same way.
func rewriteBlock(raw []byte, key string, block []byte) []byte {
	prefix := []byte(key + ":")
	lines := bytes.SplitAfter(raw, []byte("\n"))
	var out, held [][]byte // held: blank/comment lines whose owner is not yet known
	inBlock, replaced := false, false
	for _, line := range lines {
		trimmed := bytes.TrimRight(line, "\r\n")
		if rest, ok := bytes.CutPrefix(trimmed, prefix); ok && (len(rest) == 0 || rest[0] == ' ' || rest[0] == '\t') {
			inBlock, replaced = true, true
			out = append(out, block)
			continue
		}
		if inBlock {
			switch {
			case len(bytes.TrimSpace(trimmed)) == 0 || trimmed[0] == '#':
				held = append(held, line)
				continue
			case trimmed[0] == ' ' || trimmed[0] == '\t':
				held = nil // more of the block: what was held belonged to it
				continue
			}
			inBlock = false // next top-level key
			out = append(out, held...)
			held = nil
		}
		out = append(out, line)
	}
	out = append(out, held...)
	if !replaced {
		if len(raw) > 0 && raw[len(raw)-1] != '\n' {
			out = append(out, []byte("\n"))
		}
		out = append(out, block)
	}
	return bytes.Join(out, nil)
}
