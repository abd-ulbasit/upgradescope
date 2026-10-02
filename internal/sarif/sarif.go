// Package sarif renders engine reports as SARIF 2.1.0 for GitHub code
// scanning. Shared by the CLI's --output sarif and the server's
// /api/v1/gate?format=sarif endpoint.
package sarif

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"strconv"
	"strings"

	"github.com/abd-ulbasit/upgradescope/internal/engine"
	"github.com/abd-ulbasit/upgradescope/internal/inventory"
)

// SARIF 2.1.0 model — only the fields we emit.
type sarifLog struct {
	Schema  string     `json:"$schema"`
	Version string     `json:"version"`
	Runs    []sarifRun `json:"runs"`
}

type sarifRun struct {
	Tool        sarifTool           `json:"tool"`
	Invocations []sarifInvocation   `json:"invocations"`
	Results     []sarifResult       `json:"results"`
	Properties  *sarifRunProperties `json:"properties"`
}

// sarifInvocation reports what did not fit in results: one notification
// per finding without a file location, and per finding whose affected
// objects are not all listed.
type sarifInvocation struct {
	ExecutionSuccessful        bool                `json:"executionSuccessful"`
	ToolExecutionNotifications []sarifNotification `json:"toolExecutionNotifications,omitempty"`
}

type sarifNotification struct {
	Level      string            `json:"level"`
	Message    sarifText         `json:"message"`
	Properties map[string]string `json:"properties,omitempty"`
}

// sarifRunProperties carries the verdict, so a consumer that cannot see
// the omitted findings as results can still gate on the document.
type sarifRunProperties struct {
	Ready           bool `json:"ready"`
	Score           int  `json:"score"`
	Findings        int  `json:"findings"`
	OmittedFindings int  `json:"omittedFindings"`
	Suppressed      int  `json:"suppressed,omitempty"` // len(Report.Suppressed)
}

type sarifTool struct {
	Driver sarifDriver `json:"driver"`
}

type sarifDriver struct {
	Name           string      `json:"name"`
	InformationURI string      `json:"informationUri,omitempty"`
	Version        string      `json:"version,omitempty"`
	Rules          []sarifRule `json:"rules"`
}

type sarifRule struct {
	ID                   string              `json:"id"`
	ShortDescription     sarifText           `json:"shortDescription"`
	FullDescription      sarifText           `json:"fullDescription"`
	Help                 sarifMarkdown       `json:"help"`
	HelpURI              string              `json:"helpUri,omitempty"`
	DefaultConfiguration sarifConfiguration  `json:"defaultConfiguration"`
	Properties           sarifRuleProperties `json:"properties"`
}

type sarifText struct {
	Text string `json:"text"`
}

type sarifMarkdown struct {
	Text     string `json:"text"`
	Markdown string `json:"markdown,omitempty"`
}

type sarifConfiguration struct {
	Level string `json:"level"`
}

type sarifRuleProperties struct {
	Tags []string `json:"tags"`
}

type sarifResult struct {
	RuleID              string            `json:"ruleId"`
	RuleIndex           int               `json:"ruleIndex"`
	Level               string            `json:"level"`
	Message             sarifText         `json:"message"`
	Locations           []sarifLocation   `json:"locations"`
	PartialFingerprints map[string]string `json:"partialFingerprints"`
	// BaselineState is "new" or "unchanged" when the report was compared
	// with a baseline (engine.Finding.BaselineState), else omitted.
	BaselineState string             `json:"baselineState,omitempty"`
	Suppressions  []sarifSuppression `json:"suppressions,omitempty"`
}

// sarifSuppression marks a result accepted by an ignore rule or object
// annotation; kind is always "external" (the reason lives outside the
// scanned file, or in an annotation SARIF cannot point at).
type sarifSuppression struct {
	Kind          string `json:"kind"`
	Justification string `json:"justification,omitempty"`
}

type sarifLocation struct {
	PhysicalLocation *sarifPhysicalLocation `json:"physicalLocation"`
}

type sarifPhysicalLocation struct {
	ArtifactLocation sarifArtifactLocation `json:"artifactLocation"`
	Region           *sarifRegion          `json:"region"`
}

type sarifArtifactLocation struct {
	URI string `json:"uri"`
}

type sarifRegion struct {
	StartLine int `json:"startLine"`
}

const (
	informationURI = "https://github.com/abd-ulbasit/upgradescope"
	docsURI        = informationURI + "#readme"
	// fingerprintKey names our partialFingerprints entry. GitHub tracks
	// alerts by primaryLocationLineHash, which upload-sarif computes when
	// it is absent, so ours is for other consumers (and is versioned in
	// case its inputs change).
	fingerprintKey = "upgradescope/v1"
)

// categoryText is the human rule text per category: a short title and a
// full description. Only API-usage findings carry file locations today;
// the rest are listed so a future anchored category still reads well.
var categoryText = map[engine.Category][2]string{
	engine.CatRemovedAPI: {"Removed Kubernetes API",
		"The object uses an API version that the target Kubernetes version (or the minor after it) no longer serves; applying it after the upgrade fails."},
	engine.CatDeprecatedAPI: {"Deprecated Kubernetes API",
		"The object uses a deprecated API version that a later Kubernetes release removes."},
	engine.CatDeprecatedAPIInUse: {"Deprecated Kubernetes API in use",
		"Clients still request a deprecated API version from the apiserver."},
	engine.CatEOLAddon:       {"End-of-life add-on", "The add-on version has reached end of life upstream."},
	engine.CatEOLApproaching: {"Add-on approaching end of life", "The add-on version reaches end of life upstream within 90 days."},
	engine.CatVersionSkew:    {"Kubernetes version skew", "Component versions violate the Kubernetes version-skew policy."},
	engine.CatChartIncompat:  {"Incompatible add-on version", "The add-on version does not support the target Kubernetes version."},
	engine.CatKBStale:        {"Knowledge base out of date", "The knowledge base does not cover the Kubernetes version being assessed."},
}

// Unanchored counts the findings Write leaves out because none of their
// objects has a file location (live-cluster findings, add-ons, skew,
// kb-stale, a manifest stream posted to /gate without ?path=). Callers can
// tell users the SARIF is not the whole report.
func Unanchored(r engine.Report) int {
	n := 0
	for _, f := range r.Findings {
		if len(anchored(f)) == 0 {
			n++
		}
	}
	return n
}

// anchored returns the finding's objects that have a file location.
func anchored(f engine.Finding) []inventory.ObjectRef {
	var out []inventory.ObjectRef
	for _, o := range f.Objects {
		if o.File != "" && o.Line > 0 {
			out = append(out, o)
		}
	}
	return out
}

// Write renders the report as SARIF 2.1.0 that GitHub code scanning
// accepts: GitHub rejects the whole file if any result lacks a physical
// location, so every result carries one (artifactLocation.uri = the
// object's file, region.startLine = its apiVersion line) and findings
// without any file location are not results (see Unanchored). One result
// per (finding, located object), in report order; one rule per finding Key
// (falling back to the category), listed only when it has results, with
// the finding's remediation as help and its first citation as helpUri.
// toolVersion stamps tool.driver.version ("" omits it).
//
// Nothing is dropped silently: every unanchored finding is a tool
// execution notification at its severity's level (a /gate stream without
// ?path= has no file names, so all of its findings land there), objects
// not listed as results are counted in a note, and run.properties records
// ready, score and the counts, so the document never reads as a clean pass
// that the report is not.
//
// Suppressed findings (Report.Suppressed) follow: their located objects
// are results carrying an external suppression whose justification is the
// reason, the others are notes. A report compared with a baseline sets
// each result's baselineState.
func Write(w io.Writer, r engine.Report, toolVersion string) error {
	rules := []sarifRule{}
	ruleIndex := map[string]int{}
	results := []sarifResult{}
	notes := []sarifNotification{}
	occurrences := map[string]int{} // results per identity fingerprint
	omitted := 0

	// addResults adds one result per located object of f, with f's rule.
	addResults := func(f engine.Finding, objs []inventory.ObjectRef, suppressions []sarifSuppression) {
		id := f.Key
		if id == "" {
			id = string(f.Category)
		}
		idx, seen := ruleIndex[id]
		if !seen { // rule ids must be unique; keys are, but stay safe
			idx = len(rules)
			ruleIndex[id] = idx
			rules = append(rules, rule(id, f))
		}
		for _, o := range objs {
			base := fingerprint(id, o, 0)
			fp := base
			if n := occurrences[base]; n > 0 {
				fp = fingerprint(id, o, n)
			}
			occurrences[base]++
			results = append(results, sarifResult{
				RuleID:    id,
				RuleIndex: idx,
				Level:     level(f.Severity),
				Message:   sarifText{Text: message(f, o)},
				Locations: []sarifLocation{{PhysicalLocation: &sarifPhysicalLocation{
					ArtifactLocation: sarifArtifactLocation{URI: fileURI(o.File)},
					Region:           &sarifRegion{StartLine: o.Line},
				}}},
				PartialFingerprints: map[string]string{fingerprintKey: fp},
				BaselineState:       string(f.BaselineState),
				Suppressions:        suppressions,
			})
		}
	}

	for _, f := range r.Findings {
		objs := anchored(f)
		if len(objs) == 0 {
			omitted++
			notes = append(notes, notification(f, level(f.Severity), unanchoredMessage(f)))
			continue
		}
		if n := f.ObjectsOmitted + len(f.Objects) - len(objs); n > 0 {
			notes = append(notes, notification(f, "note", unlistedMessage(f, n)))
		}
		addResults(f, objs, nil)
	}
	// Suppressed findings stay visible: located objects are results with
	// an external suppression (code scanning shows them as dismissed),
	// the rest are notes.
	for _, s := range r.Suppressed {
		if objs := anchored(s.Finding); len(objs) > 0 {
			addResults(s.Finding, objs, []sarifSuppression{{Kind: "external", Justification: s.Reason}})
		} else {
			notes = append(notes, notification(s.Finding, "note", suppressedMessage(s)))
		}
	}

	log := sarifLog{
		Schema:  "https://json.schemastore.org/sarif-2.1.0.json",
		Version: "2.1.0",
		Runs: []sarifRun{{
			Tool: sarifTool{Driver: sarifDriver{
				Name:           "upgradescope",
				InformationURI: informationURI,
				Version:        toolVersion,
				Rules:          rules,
			}},
			Invocations: []sarifInvocation{{ExecutionSuccessful: true, ToolExecutionNotifications: notes}},
			Results:     results,
			Properties: &sarifRunProperties{
				Ready: r.Ready, Score: r.Score, Findings: len(r.Findings), OmittedFindings: omitted, Suppressed: len(r.Suppressed),
			},
		}},
	}

	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(log)
}

// rule builds the reporting descriptor for one finding.
func rule(id string, f engine.Finding) sarifRule {
	text, ok := categoryText[f.Category]
	if !ok {
		text = [2]string{string(f.Category), f.Title}
	}
	short := text[0]
	if subject, ok := strings.CutPrefix(id, string(f.Category)+"/"); ok {
		short += ": " + subject
	}

	fix := f.Remediation
	if fix == "" {
		fix = "see the references"
	}
	helpText := fmt.Sprintf("%s.\n\nFix: %s.", f.Title, fix)
	helpMD := fmt.Sprintf("%s.\n\n**Fix:** %s.", f.Title, fix)
	if len(f.Citations) > 0 {
		helpText += "\n\nReferences:"
		helpMD += "\n\n**References:**\n"
		for _, c := range f.Citations {
			helpText += "\n- " + c
			helpMD += fmt.Sprintf("\n- <%s>", c)
		}
	}
	helpURI := docsURI
	if len(f.Citations) > 0 {
		helpURI = f.Citations[0]
	}

	return sarifRule{
		ID:                   id,
		ShortDescription:     sarifText{Text: short},
		FullDescription:      sarifText{Text: text[1]},
		Help:                 sarifMarkdown{Text: helpText, Markdown: helpMD},
		HelpURI:              helpURI,
		DefaultConfiguration: sarifConfiguration{Level: level(f.Severity)},
		Properties:           sarifRuleProperties{Tags: []string{"kubernetes", "upgrade", string(f.Category)}},
	}
}

// message describes one located object: the finding, which object, where
// it was rendered from, and the fix.
func message(f engine.Finding, o inventory.ObjectRef) string {
	msg := f.Title + ": " + objectName(o)
	if o.RenderedFrom != "" {
		msg += " (rendered from " + o.RenderedFrom + ")"
	}
	msg += "."
	if f.Remediation != "" {
		msg += " Fix: " + f.Remediation + "."
	}
	return msg
}

// maxListed bounds the objects a notification names.
const maxListed = 5

// notification wraps a message about finding f; its properties identify
// the finding for consumers that match on keys.
func notification(f engine.Finding, lvl, msg string) sarifNotification {
	key := f.Key
	if key == "" {
		key = string(f.Category)
	}
	return sarifNotification{Level: lvl, Message: sarifText{Text: msg}, Properties: map[string]string{
		"findingKey": key, "category": string(f.Category), "severity": string(f.Severity),
	}}
}

// unanchoredMessage describes a finding that is not a result: what it is,
// its evidence, the objects it names (a /gate stream's carry lines), and
// the fix.
func unanchoredMessage(f engine.Finding) string {
	msg := "Not reported as a result (no file location): " + sentence(f.Title)
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

// suppressedMessage describes a suppressed finding without a file
// location: why it was accepted, then what it is.
func suppressedMessage(s engine.SuppressedFinding) string {
	why := s.Reason
	if s.Expires != "" {
		why += "; until " + s.Expires
	}
	msg := "Suppressed (" + why + "): " + sentence(s.Title)
	if len(s.Objects) > 0 {
		msg += " Objects: " + objectList(s.Objects, s.ObjectsOmitted) + "."
	}
	return msg
}

// unlistedMessage counts the n affected objects of an anchored finding
// that are not results: refs without a file, and refs beyond the
// inventory.MaxObjectRefs that are recorded per finding.
func unlistedMessage(f engine.Finding, n int) string {
	var loose []inventory.ObjectRef
	for _, o := range f.Objects {
		if o.File == "" || o.Line < 1 {
			loose = append(loose, o)
		}
	}
	msg := fmt.Sprintf("%s: %d more affected object(s) are not listed as results", f.Title, n)
	if len(loose) > 0 {
		msg += ": " + objectList(loose, f.ObjectsOmitted)
	} else {
		msg += fmt.Sprintf(" (at most %d objects are recorded per finding)", inventory.MaxObjectRefs)
	}
	return msg + "."
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
		switch {
		case o.File != "" && o.Line > 0:
			name += fmt.Sprintf(" (%s:%d)", o.File, o.Line)
		case o.Line > 0:
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

// fileURI turns a slash-separated path into a SARIF URI reference:
// relative paths stay relative (GitHub resolves them against the
// repository root), absolute paths become file:// URIs.
func fileURI(p string) string {
	u := url.URL{Path: p}
	if strings.HasPrefix(p, "/") {
		u.Scheme = "file"
	}
	return u.String()
}

// fingerprint identifies a result by rule and object, not line, so an
// alert survives unrelated edits that move the object within its file.
// occurrence (0-based, in file order) separates objects that share that
// identity — unnamed objects, or one name in two documents of a file —
// and leaves the first one's fingerprint unchanged.
func fingerprint(id string, o inventory.ObjectRef, occurrence int) string {
	parts := []string{id, o.File, o.Namespace, o.Name}
	if occurrence > 0 {
		parts = append(parts, strconv.Itoa(occurrence))
	}
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return hex.EncodeToString(sum[:])
}

func level(s engine.Severity) string {
	switch s {
	case engine.SevBlocker:
		return "error"
	case engine.SevWarning:
		return "warning"
	default: // info
		return "note"
	}
}
