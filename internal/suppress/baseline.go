package suppress

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"

	"github.com/abd-ulbasit/upgradescope/internal/engine"
)

// Baseline is what a previous JSON report found, for gating only on what
// is new since. Findings are matched by Key, which is count-free and
// stable across runs (engine.Finding.Key), and by severity, because a key
// can escalate (an API removed one minor past the target is a warning; at
// the target, the same key is a blocker); objects by namespace, name and
// file, not line, so editing around an object does not make it new.
type Baseline struct {
	findings map[string]baselineEntry
}

type baselineEntry struct {
	objects  map[objectID]bool
	total    int // listed plus omitted objects, summed over findings with the key
	severity int // highest severityRank among findings with the key
}

// severityRank orders severities for escalation. A missing or unknown
// severity ranks lowest, so a baseline entry without one matches nothing.
var severityRank = map[engine.Severity]int{engine.SevInfo: 1, engine.SevWarning: 2, engine.SevBlocker: 3}

type objectID struct{ namespace, name, file string }

// ReadBaseline parses a JSON report (scan --output json, or
// --write-baseline) whose schemaVersion is at most schemaVersion. Only
// its findings count: suppressed entries are not baseline, so a finding
// whose rule expires is new again.
func ReadBaseline(r io.Reader, schemaVersion int) (Baseline, error) {
	var doc struct {
		SchemaVersion *int              `json:"schemaVersion"`
		Findings      *[]engine.Finding `json:"findings"`
	}
	if err := json.NewDecoder(r).Decode(&doc); err != nil {
		return Baseline{}, fmt.Errorf("baseline is not an upgradescope JSON report: %w", err)
	}
	if doc.SchemaVersion == nil || doc.Findings == nil {
		return Baseline{}, errors.New("baseline is not an upgradescope JSON report (want the output of scan --output json or --write-baseline)")
	}
	if *doc.SchemaVersion > schemaVersion {
		return Baseline{}, fmt.Errorf("baseline schemaVersion %d is newer than this upgradescope reads (%d)", *doc.SchemaVersion, schemaVersion)
	}
	b := Baseline{findings: map[string]baselineEntry{}}
	for _, f := range *doc.Findings {
		if legacyUnservedFinding(f) {
			continue
		}
		e, ok := b.findings[findingID(f)]
		if !ok {
			e = baselineEntry{objects: map[objectID]bool{}}
		}
		for _, o := range f.Objects {
			e.objects[objectID{o.Namespace, o.Name, o.File}] = true
		}
		e.total += len(f.Objects) + f.ObjectsOmitted
		e.severity = max(e.severity, severityRank[f.Severity])
		b.findings[findingID(f)] = e
	}
	return b, nil
}

// legacyUnservedFinding reports whether f is a "not served until X"
// blocker a release before #300 wrote, under the key of the API's removal
// (removed-api/<group>/<version>/<kind>, without /unserved). Recorded as
// is, it would hold the removal blocker of the same API at a later target
// (the same key, severity and objects); it is dropped instead, so that
// the blocker resurfaces once under its new key and the removal is new.
func legacyUnservedFinding(f engine.Finding) bool {
	if f.Category != engine.CatRemovedAPI || !strings.Contains(f.Title, " is not served until ") {
		return false
	}
	_, unservedKey := engine.BaseOfUnservedKey(f.Key)
	return !unservedKey && !strings.HasPrefix(f.Key, string(engine.CatRemovedAPI)+"/helm-release/")
}

// Mark returns r with every finding's BaselineState set: unchanged when
// the baseline had its key at the same or a higher severity, every object
// it lists, and at least as many objects in all (new ones could hide
// among the unlisted); new otherwise.
// r itself is not modified.
func (b Baseline) Mark(r engine.Report) engine.Report {
	r.Findings = slices.Clone(r.Findings)
	for i, f := range r.Findings {
		state := engine.BaselineNew
		if e, ok := b.findings[findingID(f)]; ok && severityRank[f.Severity] <= e.severity && len(f.Objects)+f.ObjectsOmitted <= e.total {
			state = engine.BaselineUnchanged
			for _, o := range f.Objects {
				if !e.objects[objectID{o.Namespace, o.Name, o.File}] {
					state = engine.BaselineNew
					break
				}
			}
		}
		r.Findings[i].BaselineState = state
	}
	return r
}

// findingID is the Key, or for a keyless finding (none today) its
// category and title.
func findingID(f engine.Finding) string {
	if f.Key != "" {
		return f.Key
	}
	return string(f.Category) + "\x00" + f.Title
}
