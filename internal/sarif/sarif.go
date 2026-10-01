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
	Tool    sarifTool     `json:"tool"`
	Results []sarifResult `json:"results"`
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
// kb-stale, a posted manifest stream). Callers can tell users the SARIF is
// not the whole report.
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
// without any file location are omitted (see Unanchored). One result per
// (finding, located object), in report order; one rule per finding Key
// (falling back to the category), listed only when it has results, with
// the finding's remediation as help and its first citation as helpUri.
// toolVersion stamps tool.driver.version ("" omits it).
func Write(w io.Writer, r engine.Report, toolVersion string) error {
	rules := []sarifRule{}
	ruleIndex := map[string]int{}
	results := []sarifResult{}

	for _, f := range r.Findings {
		objs := anchored(f)
		if len(objs) == 0 {
			continue
		}
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
			results = append(results, sarifResult{
				RuleID:    id,
				RuleIndex: idx,
				Level:     level(f.Severity),
				Message:   sarifText{Text: message(f, o)},
				Locations: []sarifLocation{{PhysicalLocation: &sarifPhysicalLocation{
					ArtifactLocation: sarifArtifactLocation{URI: fileURI(o.File)},
					Region:           &sarifRegion{StartLine: o.Line},
				}}},
				PartialFingerprints: map[string]string{fingerprintKey: fingerprint(id, o)},
			})
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
			Results: results,
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
	name := o.Name
	if name == "" {
		name = "(unnamed)"
	}
	if o.Namespace != "" {
		name = o.Namespace + "/" + name
	}
	msg := f.Title + ": " + name
	if o.RenderedFrom != "" {
		msg += " (rendered from " + o.RenderedFrom + ")"
	}
	msg += "."
	if f.Remediation != "" {
		msg += " Fix: " + f.Remediation + "."
	}
	return msg
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
func fingerprint(id string, o inventory.ObjectRef) string {
	sum := sha256.Sum256([]byte(strings.Join([]string{id, o.File, o.Namespace, o.Name}, "\x00")))
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
