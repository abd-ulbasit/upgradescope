// Package codequality renders engine reports as GitLab Code Quality
// reports (artifacts:reports:codequality), which GitLab shows in the merge
// request widget and the pipeline's Code Quality tab. Shared by the CLI's
// --output gitlab-codequality and the server's
// /api/v1/gate?format=gitlab-codequality endpoint.
package codequality

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/abd-ulbasit/upgradescope/internal/engine"
	"github.com/abd-ulbasit/upgradescope/internal/inventory"
)

// issue is one report entry: the fields GitLab documents
// (https://docs.gitlab.com/ci/testing/code_quality/#code-quality-report-format).
type issue struct {
	Description string   `json:"description"`
	CheckName   string   `json:"check_name"`
	Fingerprint string   `json:"fingerprint"`
	Severity    string   `json:"severity"`
	Location    location `json:"location"`
}

type location struct {
	Path  string `json:"path"`
	Lines lines  `json:"lines"`
}

type lines struct {
	Begin int `json:"begin"`
}

const (
	// VirtualPrefix starts the location.path of an entry that has no file:
	// upgradescope/<finding key> or upgradescope/not-assessed/<capability>.
	// GitLab requires a path, and an entry it cannot place on a file still
	// shows in the merge request widget; its file link goes nowhere.
	VirtualPrefix = "upgradescope/"
	// fingerprintVersion is hashed into every fingerprint, so a change of
	// its inputs can be versioned.
	fingerprintVersion = "upgradescope/codequality/v1"
	// maxListed bounds the objects a description names.
	maxListed = 5
)

// Write renders the report as a GitLab Code Quality report: a JSON array
// with one entry per (finding, object located in a file) — check_name the
// finding's key, location the object's file and line, as in SARIF — and,
// since GitLab requires a location, one entry anchored to the virtual
// path upgradescope/<key>, line 1, per finding with no located object
// (live-cluster findings, add-ons, skew, a /gate stream posted without
// ?path=) and per located finding with affected objects that have no file
// or are not recorded. Severity: blocker → critical, warning → minor,
// info → info. A required assessment gap, which makes the verdict unknown,
// is a critical entry (check not-assessed/<capability>), a partial one is
// info, and an optional check that did not run is left out, as in SARIF.
// Suppressed findings are left out: GitLab has no dismissed state, and
// every other output lists them. Baseline-unchanged findings stay: GitLab
// compares the merge request's report with the target branch's itself.
//
// Fingerprints hash the key and the object's file, namespace and name
// (the key and virtual path for an unlocated entry), never its line or
// counts, so an entry survives edits that move it; objects sharing an
// identity are numbered after the first. GitLab keys entries by
// fingerprint, so every one is unique. An empty report is [], not null.
func Write(w io.Writer, r engine.Report) error {
	issues := []issue{}
	occurrences := map[string]int{}
	add := func(check, severity, desc, path string, line int, identity ...string) {
		base := fingerprint(append([]string{check, path}, identity...), 0)
		fp := base
		if n := occurrences[base]; n > 0 {
			fp = fingerprint(append([]string{check, path}, identity...), n)
		}
		occurrences[base]++
		issues = append(issues, issue{Description: desc, CheckName: check, Fingerprint: fp, Severity: severity,
			Location: location{Path: path, Lines: lines{Begin: line}}})
	}

	for _, f := range r.Findings {
		check := f.Key
		if check == "" {
			check = string(f.Category)
		}
		sev := severity(f.Severity)
		var loose []inventory.ObjectRef
		located := 0
		for _, o := range f.Objects {
			if o.File == "" || o.Line < 1 {
				loose = append(loose, o)
				continue
			}
			located++
			add(check, sev, objectMessage(f, o), o.File, o.Line, o.Namespace, o.Name)
		}
		switch {
		case located == 0:
			add(check, sev, findingMessage(f), VirtualPrefix+check, 1)
		case len(loose)+f.ObjectsOmitted > 0:
			msg := fmt.Sprintf("%s: %d more affected object(s) are not listed with a file and line", f.Title, len(loose)+f.ObjectsOmitted)
			if len(loose) > 0 {
				msg += ": " + objectList(loose, f.ObjectsOmitted)
			} else {
				msg += fmt.Sprintf(" (at most %d objects are recorded per finding)", inventory.MaxObjectRefs)
			}
			add(check, sev, msg+".", VirtualPrefix+check, 1)
		}
	}

	for _, g := range r.NotAssessed {
		sev := "info"
		switch {
		case g.Required:
			sev = "critical"
		case !g.Partial:
			continue
		}
		msg := fmt.Sprintf("Not assessed: %s: %s", g.Label(), sentence(g.Reason))
		if len(g.Skipped) > 0 {
			msg += " Skipped: " + strings.Join(g.Skipped, ", ") + "."
		}
		if g.Required {
			msg += " The verdict is unknown: a blocker may have been missed."
		}
		check := "not-assessed/" + string(g.Capability)
		add(check, sev, msg, VirtualPrefix+check, 1)
	}

	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(issues)
}

func severity(s engine.Severity) string {
	switch s {
	case engine.SevBlocker:
		return "critical"
	case engine.SevWarning:
		return "minor"
	default: // info
		return "info"
	}
}

// objectMessage describes one located object: the finding, which object,
// where it was rendered from, and the fix.
func objectMessage(f engine.Finding, o inventory.ObjectRef) string {
	msg := f.Title + ": " + objectName(o)
	if o.RenderedFrom != "" {
		msg += " (rendered from " + o.RenderedFrom + ")"
	}
	msg += "."
	if f.Remediation != "" {
		msg += " Fix: " + sentence(f.Remediation)
	}
	return msg
}

// findingMessage describes a finding without a located object: what it
// is, its evidence, the objects it names, and the fix.
func findingMessage(f engine.Finding) string {
	msg := sentence(f.Title)
	if f.Detail != "" {
		msg += " " + sentence(f.Detail)
	}
	if len(f.Objects) > 0 {
		msg += " Objects: " + objectList(f.Objects, f.ObjectsOmitted) + "."
	}
	if f.Remediation != "" {
		msg += " Fix: " + sentence(f.Remediation)
	}
	return msg
}

// objectList names up to maxListed objects, then counts the rest plus
// extra (objects never recorded).
func objectList(objs []inventory.ObjectRef, extra int) string {
	var names []string
	for i, o := range objs {
		if i == maxListed {
			break
		}
		name := objectName(o)
		if o.Line > 0 {
			name += fmt.Sprintf(" (line %d)", o.Line)
		}
		names = append(names, name)
	}
	if more := len(objs) - len(names) + extra; more > 0 {
		names = append(names, fmt.Sprintf("and %d more", more))
	}
	return strings.Join(names, ", ")
}

// objectName is "namespace/name", "name" or "(unnamed)".
func objectName(o inventory.ObjectRef) string {
	name := o.Name
	if name == "" {
		name = "(unnamed)"
	}
	if o.Namespace != "" {
		name = o.Namespace + "/" + name
	}
	return name
}

// sentence ends s with a full stop unless it already ends a sentence.
func sentence(s string) string {
	s = strings.TrimSpace(s)
	if strings.HasSuffix(s, ".") || strings.HasSuffix(s, "!") || strings.HasSuffix(s, "?") {
		return s
	}
	return s + "."
}

// fingerprint hashes parts (and the occurrence, after the first) into a
// hex SHA-256.
func fingerprint(parts []string, occurrence int) string {
	parts = append([]string{fingerprintVersion}, parts...)
	if occurrence > 0 {
		parts = append(parts, strconv.Itoa(occurrence))
	}
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return hex.EncodeToString(sum[:])
}
