// Package codequalitytest checks GitLab Code Quality reports against what
// GitLab accepts, for every package that emits one (the CLI's --output
// gitlab-codequality and the server's /api/v1/gate?format=gitlab-codequality).
package codequalitytest

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"strings"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

// schemaJSON is GitLab's schema for one report entry, vendored verbatim;
// testdata/README.md has its source, revision and license.
//
//go:embed testdata/gitlab-codeclimate.schema.json
var schemaJSON []byte

// schemaURL is where GitLab publishes it.
const schemaURL = "https://gitlab.com/gitlab-org/gitlab/-/raw/master/app/validators/json_schemas/codeclimate.json"

// Entry is one report entry as GitLab documents it, decoded independently
// of the writer's model so a field the writer drops is caught.
type Entry struct {
	Description string `json:"description"`
	CheckName   string `json:"check_name"`
	Fingerprint string `json:"fingerprint"`
	Severity    string `json:"severity"`
	Location    struct {
		Path  string `json:"path"`
		Lines struct {
			Begin int `json:"begin"`
		} `json:"lines"`
	} `json:"location"`
}

// severities are the values GitLab documents.
var severities = map[string]bool{"info": true, "minor": true, "major": true, "critical": true, "blocker": true}

// AssertGitLabAcceptable fails t unless raw is a report GitLab reads in
// full: a JSON array whose every entry validates against GitLab's schema
// (its parser stops at the first entry that does not) and meets the
// documented requirements the schema leaves out — a check_name, a
// severity GitLab knows, a repository-relative path without "./", a line
// — with no fingerprint repeated (GitLab keys entries by it, so a repeat
// replaces an entry). It returns the decoded entries.
func AssertGitLabAcceptable(t *testing.T, raw []byte) []Entry {
	t.Helper()
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(schemaJSON))
	if err != nil {
		t.Fatal(err)
	}
	c := jsonschema.NewCompiler()
	if err := c.AddResource(schemaURL, doc); err != nil {
		t.Fatal(err)
	}
	sch, err := c.Compile(schemaURL)
	if err != nil {
		t.Fatal(err)
	}

	var items []json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil || items == nil {
		t.Fatalf("not a JSON array (err %v):\n%s", err, raw)
	}
	entries := make([]Entry, 0, len(items))
	seen := map[string]int{}
	for i, item := range items {
		inst, err := jsonschema.UnmarshalJSON(bytes.NewReader(item))
		if err != nil {
			t.Fatal(err)
		}
		if err := sch.Validate(inst); err != nil {
			t.Errorf("entry %d does not match GitLab's schema: %v\n%s", i, err, item)
		}
		var e Entry
		if err := json.Unmarshal(item, &e); err != nil {
			t.Fatalf("entry %d: %v", i, err)
		}
		if e.Description == "" || e.CheckName == "" || e.Fingerprint == "" {
			t.Errorf("entry %d lacks a description, check_name or fingerprint: %s", i, item)
		}
		if !severities[e.Severity] {
			t.Errorf("entry %d: severity %q is not one GitLab knows", i, e.Severity)
		}
		p := e.Location.Path
		if p == "" || strings.HasPrefix(p, "/") || strings.HasPrefix(p, "./") || strings.Contains(p, `\`) {
			t.Errorf("entry %d: location.path %q is not a repository-relative path without ./", i, p)
		}
		if e.Location.Lines.Begin < 1 {
			t.Errorf("entry %d: location.lines.begin = %d, want a line", i, e.Location.Lines.Begin)
		}
		if j, dup := seen[e.Fingerprint]; dup {
			t.Errorf("entries %d and %d share fingerprint %s; GitLab would keep only one", j, i, e.Fingerprint)
		}
		seen[e.Fingerprint] = i
		entries = append(entries, e)
	}
	return entries
}
