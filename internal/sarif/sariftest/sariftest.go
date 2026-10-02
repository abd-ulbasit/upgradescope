// Package sariftest checks SARIF documents against what GitHub code
// scanning accepts, for every package that emits SARIF (the CLI's
// --output sarif and the server's /api/v1/gate?format=sarif).
package sariftest

import (
	"encoding/json"
	"net/url"
	"strings"
	"testing"
)

// The subset of SARIF 2.1.0 that GitHub validates, decoded independently
// of the writer's own model so a field the writer drops is caught.
type log struct {
	Runs []struct {
		Tool struct {
			Driver struct {
				Name  string `json:"name"`
				Rules []struct {
					ID               string `json:"id"`
					ShortDescription struct {
						Text string `json:"text"`
					} `json:"shortDescription"`
					FullDescription struct {
						Text string `json:"text"`
					} `json:"fullDescription"`
					Help struct {
						Text     string `json:"text"`
						Markdown string `json:"markdown"`
					} `json:"help"`
					HelpURI              string `json:"helpUri"`
					DefaultConfiguration struct {
						Level string `json:"level"`
					} `json:"defaultConfiguration"`
					Properties struct {
						Tags []string `json:"tags"`
					} `json:"properties"`
				} `json:"rules"`
			} `json:"driver"`
		} `json:"tool"`
		Invocations []struct {
			ExecutionSuccessful        *bool `json:"executionSuccessful"`
			ToolExecutionNotifications []struct {
				Level   string `json:"level"`
				Message struct {
					Text string `json:"text"`
				} `json:"message"`
			} `json:"toolExecutionNotifications"`
		} `json:"invocations"`
		Results []struct {
			RuleID    string `json:"ruleId"`
			RuleIndex int    `json:"ruleIndex"`
			Message   struct {
				Text string `json:"text"`
			} `json:"message"`
			Locations []struct {
				PhysicalLocation *struct {
					ArtifactLocation struct {
						URI string `json:"uri"`
					} `json:"artifactLocation"`
					Region *struct {
						StartLine int `json:"startLine"`
					} `json:"region"`
				} `json:"physicalLocation"`
			} `json:"locations"`
			PartialFingerprints map[string]string `json:"partialFingerprints"`
			BaselineState       *string           `json:"baselineState"`
			Suppressions        []struct {
				Kind string `json:"kind"`
			} `json:"suppressions"`
		} `json:"results"`
	} `json:"runs"`
}

// AssertGitHubAcceptable checks every property GitHub code scanning marks
// as required, and the limits it enforces, per
// https://docs.github.com/en/code-security/code-scanning/integrating-with-code-scanning/sarif-support-for-code-scanning
// (the official SARIF 2.1.0 JSON schema is not vendored: its OASIS IPR
// terms are not a clear redistribution license). Invocations, which the
// schema allows but GitHub does not require, must still be well formed.
func AssertGitHubAcceptable(t testing.TB, raw []byte) {
	t.Helper()
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	if doc["$schema"] == "" || doc["$schema"] == nil {
		t.Error("$schema missing")
	}
	if doc["version"] != "2.1.0" {
		t.Errorf("version = %v, want 2.1.0", doc["version"])
	}
	var l log
	if err := json.Unmarshal(raw, &l); err != nil {
		t.Fatal(err)
	}
	if n := len(l.Runs); n < 1 || n > 20 {
		t.Fatalf("runs = %d, want 1..20", n)
	}
	for _, run := range l.Runs {
		d := run.Tool.Driver
		if d.Name == "" || d.Rules == nil {
			t.Errorf("tool.driver name/rules missing: %+v", d)
		}
		if len(d.Rules) > 25000 || len(run.Results) > 25000 {
			t.Errorf("rules=%d results=%d exceed GitHub limits", len(d.Rules), len(run.Results))
		}
		ids := map[string]bool{}
		for _, rule := range d.Rules {
			if rule.ID == "" || ids[rule.ID] {
				t.Errorf("rule id %q empty or duplicated", rule.ID)
			}
			ids[rule.ID] = true
			if rule.ShortDescription.Text == "" || rule.ShortDescription.Text == rule.ID || len(rule.ShortDescription.Text) > 1024 {
				t.Errorf("rule %s shortDescription %q: want human text ≤1024 chars, not the id", rule.ID, rule.ShortDescription.Text)
			}
			if rule.FullDescription.Text == "" || len(rule.FullDescription.Text) > 1024 {
				t.Errorf("rule %s fullDescription %q: want text ≤1024 chars", rule.ID, rule.FullDescription.Text)
			}
			if rule.Help.Text == "" || rule.Help.Markdown == "" {
				t.Errorf("rule %s help = %+v, want text and markdown", rule.ID, rule.Help)
			}
			if u, err := url.Parse(rule.HelpURI); err != nil || !u.IsAbs() {
				t.Errorf("rule %s helpUri %q: want an absolute URL", rule.ID, rule.HelpURI)
			}
			if n := len(rule.Properties.Tags); n == 0 || n > 20 {
				t.Errorf("rule %s tags = %v, want 1..20", rule.ID, rule.Properties.Tags)
			}
			if !validLevel(rule.DefaultConfiguration.Level) {
				t.Errorf("rule %s defaultConfiguration.level = %q", rule.ID, rule.DefaultConfiguration.Level)
			}
		}
		for i, inv := range run.Invocations {
			if inv.ExecutionSuccessful == nil {
				t.Errorf("invocations[%d]: executionSuccessful is required", i)
			}
			for j, n := range inv.ToolExecutionNotifications {
				if n.Message.Text == "" || !validLevel(n.Level) {
					t.Errorf("invocations[%d] notification %d = %+v: want a message and a level", i, j, n)
				}
			}
		}
		for i, res := range run.Results {
			if !ids[res.RuleID] || res.RuleIndex < 0 || res.RuleIndex >= len(d.Rules) || d.Rules[res.RuleIndex].ID != res.RuleID {
				t.Errorf("results[%d] ruleId %q / ruleIndex %d do not match a rule", i, res.RuleID, res.RuleIndex)
			}
			if res.Message.Text == "" {
				t.Errorf("results[%d] message.text empty", i)
			}
			if len(res.PartialFingerprints) == 0 {
				t.Errorf("results[%d] partialFingerprints empty", i)
			}
			if s := res.BaselineState; s != nil && *s != "new" && *s != "unchanged" && *s != "updated" && *s != "absent" {
				t.Errorf("results[%d] baselineState %q: want new, unchanged, updated or absent", i, *s)
			}
			for _, sup := range res.Suppressions {
				if sup.Kind != "inSource" && sup.Kind != "external" {
					t.Errorf("results[%d] suppression kind %q: want inSource or external", i, sup.Kind)
				}
			}
			if n := len(res.Locations); n < 1 || n > 1000 {
				t.Fatalf("results[%d] has %d locations, GitHub requires 1..1000", i, n)
			}
			for _, loc := range res.Locations {
				pl := loc.PhysicalLocation
				if pl == nil {
					t.Errorf("results[%d]: location without physicalLocation", i)
					continue
				}
				u, err := url.Parse(pl.ArtifactLocation.URI)
				if err != nil || pl.ArtifactLocation.URI == "" || u.IsAbs() || strings.HasPrefix(u.Path, "/") {
					t.Errorf("results[%d] uri %q: want a repository-relative URI reference", i, pl.ArtifactLocation.URI)
				}
				if pl.Region == nil || pl.Region.StartLine < 1 {
					t.Errorf("results[%d] region %+v: want startLine ≥ 1", i, pl.Region)
				}
			}
		}
	}
}

func validLevel(l string) bool {
	switch l {
	case "none", "note", "warning", "error":
		return true
	}
	return false
}
