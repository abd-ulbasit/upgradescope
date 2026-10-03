// Package examples holds no code: it is the offline half of the checks on the
// example policies and the Renovate preset. The other half (the policies
// evaluated by the Kyverno CLI and gator, the preset validated by Renovate's
// own validator) runs in hack/examples-test.sh and needs those tools. What
// these tests prove is that the examples still describe the object the agent
// writes: the fixtures decode as the real status type, the policies name the
// real group, version, resource and default object name, and the static
// Renovate preset has not drifted from the add-on registry.
package examples

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"

	"github.com/abd-ulbasit/upgradescope/internal/crd"
	"github.com/abd-ulbasit/upgradescope/registry"
)

func read(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.FromSlash(path))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// docs splits a multi-document YAML file.
func docs(t *testing.T, path string) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, part := range strings.Split(string(read(t, path)), "\n---\n") {
		var m map[string]any
		if err := yaml.Unmarshal([]byte(part), &m); err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		if len(m) > 0 {
			out = append(out, m)
		}
	}
	return out
}

// strictStatus decodes a ClusterReadiness fixture's status as the agent's
// own type, refusing a field it does not have, and returns it.
func strictStatus(t *testing.T, where string, obj map[string]any) crd.Status {
	t.Helper()
	if obj["apiVersion"] != crd.Group+"/"+crd.Version || obj["kind"] != crd.Kind {
		t.Fatalf("%s: apiVersion/kind = %v/%v, want %s/%s %s", where, obj["apiVersion"], obj["kind"], crd.Group, crd.Version, crd.Kind)
	}
	if name := obj["metadata"].(map[string]any)["name"]; name != crd.DefaultName {
		t.Fatalf("%s: name %v, want %s", where, name, crd.DefaultName)
	}
	raw, err := json.Marshal(obj["status"])
	if err != nil {
		t.Fatal(err)
	}
	var st crd.Status
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&st); err != nil {
		t.Fatalf("%s: status is not the agent's status: %v", where, err)
	}
	if st.LastEvaluated.IsZero() {
		t.Errorf("%s: no lastEvaluated, which the policies read", where)
	}
	for _, tg := range st.Targets {
		if tg.Ready != (tg.Verdict == "ready") {
			t.Errorf("%s: target %s ready=%v with verdict %q: the agent writes ready == (verdict == ready)", where, tg.Target, tg.Ready, tg.Verdict)
		}
	}
	return st
}

// The ClusterReadiness objects the policy tests feed the policies must be
// ones the agent could write, or the tests prove nothing about a real cluster.
func TestFixturesAreWhatTheAgentWrites(t *testing.T) {
	var n int
	for _, f := range []string{
		"policies/gatekeeper/tests/inventory/fresh.yaml",
		"policies/gatekeeper/tests/inventory/stale.yaml",
	} {
		for _, d := range docs(t, f) {
			strictStatus(t, f, d)
			n++
		}
	}
	tests, err := filepath.Glob("policies/kyverno/tests/*/kyverno-test.yaml")
	if err != nil || len(tests) == 0 {
		t.Fatalf("no kyverno test cases found: %v", err)
	}
	for _, f := range tests {
		var tc struct {
			APICallResponses []struct {
				URLPath  string `json:"urlPath"`
				Response struct {
					Body map[string]any `json:"body"`
				} `json:"response"`
			} `json:"apiCallResponses"`
		}
		if err := yaml.Unmarshal(read(t, f), &tc); err != nil {
			t.Fatalf("%s: %v", f, err)
		}
		if len(tc.APICallResponses) != 1 {
			t.Fatalf("%s: %d apiCallResponses, want 1", f, len(tc.APICallResponses))
		}
		r := tc.APICallResponses[0]
		if want := "/apis/" + crd.Group + "/" + crd.Version + "/" + crd.Plural + "/" + crd.DefaultName; r.URLPath != want {
			t.Errorf("%s: mocks %s, want %s", f, r.URLPath, want)
		}
		// "missing" feeds the empty object the policy's default stands for.
		if len(r.Response.Body) == 0 {
			if !strings.HasSuffix(filepath.Dir(f), "missing") {
				t.Errorf("%s: empty response body outside the missing case", f)
			}
			continue
		}
		strictStatus(t, f, r.Response.Body)
		n++
	}
	if n == 0 {
		t.Fatal("no fixture checked")
	}
}

// The policies must read the object the chart's agent writes.
func TestPoliciesNameTheObjectTheAgentWrites(t *testing.T) {
	kyverno := string(read(t, "policies/kyverno/upgradescope-ready-target.yaml"))
	path := "/apis/" + crd.Group + "/" + crd.Version + "/" + crd.Plural + "/" + crd.DefaultName
	for _, want := range []string{
		"urlPath: " + path,
		"readiness.status.targets[?target==",
		"readiness.status.lastEvaluated",
		"failureAction: Audit", // the default must stay non-blocking
	} {
		if !strings.Contains(kyverno, want) {
			t.Errorf("kyverno policy lacks %q", want)
		}
	}

	rego := string(read(t, "policies/gatekeeper/template.yaml"))
	for _, want := range []string{
		`data.inventory.cluster["` + crd.Group + "/" + crd.Version + `"].` + crd.Kind + "[name]",
		`object.get(input.parameters, "crName", "` + crd.DefaultName + `")`,
		"cr.status.lastEvaluated",
		"cr.status.targets",
	} {
		if !strings.Contains(rego, want) {
			t.Errorf("gatekeeper template lacks %q", want)
		}
	}

	var constraint struct {
		Spec struct {
			EnforcementAction string `json:"enforcementAction"`
			Parameters        struct {
				CRName string `json:"crName"`
			} `json:"parameters"`
		} `json:"spec"`
	}
	if err := yaml.Unmarshal(read(t, "policies/gatekeeper/constraint.yaml"), &constraint); err != nil {
		t.Fatal(err)
	}
	if constraint.Spec.EnforcementAction != "warn" {
		t.Errorf("gatekeeper constraint enforcementAction = %q, want warn (the default must stay non-blocking)", constraint.Spec.EnforcementAction)
	}
	if constraint.Spec.Parameters.CRName != crd.DefaultName {
		t.Errorf("gatekeeper constraint crName = %q, want %s", constraint.Spec.Parameters.CRName, crd.DefaultName)
	}

	for _, d := range docs(t, "policies/gatekeeper/config.yaml") {
		sync := d["spec"].(map[string]any)["sync"].(map[string]any)["syncOnly"].([]any)[0].(map[string]any)
		if sync["group"] != crd.Group || sync["version"] != crd.Version || sync["kind"] != crd.Kind {
			t.Errorf("sync config replicates %v, want %s/%s %s", sync, crd.Group, crd.Version, crd.Kind)
		}
	}
}

// Both readers get exactly read access to the one resource, no more.
func TestReaderRolesAreReadOnly(t *testing.T) {
	for _, f := range []string{"policies/kyverno/rbac.yaml", "policies/gatekeeper/rbac.yaml"} {
		var roles int
		for _, d := range docs(t, f) {
			if d["kind"] != "ClusterRole" {
				continue
			}
			roles++
			rules := d["rules"].([]any)
			if len(rules) != 1 {
				t.Fatalf("%s: %d rules, want 1", f, len(rules))
			}
			r := rules[0].(map[string]any)
			if got := r["apiGroups"].([]any); len(got) != 1 || got[0] != crd.Group {
				t.Errorf("%s: apiGroups %v", f, got)
			}
			if got := r["resources"].([]any); len(got) != 1 || got[0] != crd.Plural {
				t.Errorf("%s: resources %v", f, got)
			}
			var verbs []string
			for _, v := range r["verbs"].([]any) {
				verbs = append(verbs, v.(string))
			}
			sort.Strings(verbs)
			if strings.Join(verbs, ",") != "get,list,watch" {
				t.Errorf("%s: verbs %v, want get, list, watch", f, verbs)
			}
		}
		if roles != 1 {
			t.Errorf("%s: %d ClusterRoles, want 1", f, roles)
		}
	}
}

type rule struct {
	Description string   `json:"description"`
	Datasources []string `json:"matchDatasources"`
	Packages    []string `json:"matchPackageNames"`
	UpdateTypes []string `json:"matchUpdateTypes"`
	Automerge   *bool    `json:"automerge"`
	GroupName   string   `json:"groupName"`
	Approval    bool     `json:"dependencyDashboardApproval"`
	Labels      []string `json:"addLabels"`
	Priority    *int     `json:"prPriority"`
	Notes       []string `json:"prBodyNotes"`
}

type preset struct {
	Schema      string `json:"$schema"`
	Description string `json:"description"`
	Rules       []rule `json:"packageRules"`
}

func loadPreset(t *testing.T) preset {
	t.Helper()
	var p preset
	dec := json.NewDecoder(bytes.NewReader(read(t, "renovate/upgradescope.json")))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&p); err != nil {
		t.Fatalf("renovate/upgradescope.json: %v", err)
	}
	if len(p.Rules) == 0 {
		t.Fatal("no packageRules")
	}
	for i, r := range p.Rules {
		if r.Description == "" {
			t.Errorf("packageRules[%d] has no description", i)
		}
		if len(r.Packages) == 0 || len(r.Datasources) == 0 {
			t.Errorf("packageRules[%d] matches nothing (datasources %v, packages %v)", i, r.Datasources, r.Packages)
		}
	}
	return p
}

// The preset is static, so the one thing that can rot unnoticed is its list of
// charts. It must be the registry's: a chart the registry learns of and the
// preset does not is an add-on whose bumps are not grouped, and an add-on the
// registry lists as end of life must be in the EOL rule.
func TestRenovatePresetMatchesTheRegistry(t *testing.T) {
	p := loadPreset(t)
	addons, err := registry.Load()
	if err != nil {
		t.Fatal(err)
	}
	all, eol := map[string]bool{}, map[string]bool{}
	for _, a := range addons {
		for _, c := range a.Matchers.Charts {
			all[c] = true
			if a.Support.Status == "eol" {
				eol[c] = true
			}
		}
	}

	set := func(r rule) map[string]bool {
		m := map[string]bool{}
		for _, n := range r.Packages {
			m[n] = true
		}
		return m
	}
	same := func(name string, got, want map[string]bool) {
		for n := range want {
			if !got[n] {
				t.Errorf("%s lacks chart %q (run `grep -h -A3 charts: registry/data/*.yaml`)", name, n)
			}
		}
		for n := range got {
			if !want[n] {
				t.Errorf("%s names chart %q, which no registry entry matches", name, n)
			}
		}
	}

	var grouped, major, eolRule *rule
	for i := range p.Rules {
		r := &p.Rules[i]
		switch {
		case len(r.UpdateTypes) > 0 && r.UpdateTypes[0] == "minor":
			grouped = r
		case len(r.UpdateTypes) > 0 && r.UpdateTypes[0] == "major":
			major = r
		case r.Priority != nil:
			eolRule = r
		}
	}
	if grouped == nil || major == nil || eolRule == nil {
		t.Fatalf("want a minor/patch group rule, a major rule and an EOL rule, got %+v", p.Rules)
	}
	same("the minor/patch group", set(*grouped), all)
	same("the major rule", set(*major), all)
	same("the EOL rule", set(*eolRule), eol)

	if grouped.GroupName == "" {
		t.Error("the minor/patch rule must group (groupName)")
	}
	if !major.Approval {
		t.Error("the major rule must wait for Dependency Dashboard approval")
	}
	if major.Automerge == nil || *major.Automerge || eolRule.Automerge == nil || *eolRule.Automerge {
		t.Error("major and EOL bumps must say automerge: false")
	}
	if len(eolRule.Notes) == 0 || !strings.Contains(eolRule.Notes[0], "end of life") {
		t.Error("the EOL rule must say in the PR that the bump does not make the add-on supported")
	}
	if !strings.Contains(p.Description, "STATIC") {
		t.Error("the preset must say it is static")
	}
}
