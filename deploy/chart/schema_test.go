package chart

import (
	"encoding/json"
	"os"
	"regexp"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/api/validate/content"

	"github.com/abd-ulbasit/upgradescope/internal/crd"
)

// agentSchemaProp returns the values.schema.json definition of agent.<name>.
func agentSchemaProp(t *testing.T, name string) map[string]any {
	t.Helper()
	raw, err := os.ReadFile("values.schema.json")
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	props, _ := doc["properties"].(map[string]any)
	agent, _ := props["agent"].(map[string]any)
	agentProps, _ := agent["properties"].(map[string]any)
	prop, ok := agentProps[name].(map[string]any)
	if !ok {
		t.Fatalf("values.schema.json has no properties.agent.properties.%s", name)
	}
	return prop
}

// agent.crName is passed to the agent as --cr-name and put in the RBAC
// resourceNames. The agent refuses a name that is not an RFC 1123 subdomain
// at start (validAgentNames), and the apiserver would 422 the object on
// every tick, so the schema holds the same rule: a pattern that only
// checked the character set let a..b and a.-b through to helm install.
// This part needs no helm: it holds the schema's pattern against the
// validator the agent uses, over every string of a small alphabet.
func TestSchemaCRNameAgreesWithTheAgent(t *testing.T) {
	prop := agentSchemaProp(t, "crName")
	pattern, _ := prop["pattern"].(string)
	maxLen, _ := prop["maxLength"].(float64)
	if pattern == "" || maxLen == 0 {
		t.Fatalf("agent.crName needs a pattern and a maxLength, has %v", prop)
	}
	re, err := regexp.Compile(pattern)
	if err != nil {
		t.Fatalf("agent.crName pattern %q: %v", pattern, err)
	}
	schemaAccepts := func(s string) bool { return re.MatchString(s) && float64(len(s)) <= maxLen }
	agentAccepts := func(s string) bool { return len(content.IsDNS1123Subdomain(s)) == 0 }

	check := func(s string) {
		t.Helper()
		if got, want := schemaAccepts(s), agentAccepts(s); got != want {
			t.Errorf("agent.crName=%.40q (%d bytes): schema accepts %v, the agent accepts %v", s, len(s), got, want)
		}
	}
	// The empty string is the one deliberate difference: the agent reads it
	// as "use the default", while a chart value of "" would render an empty
	// --cr-name and an empty RBAC resourceName.
	if schemaAccepts("") {
		t.Error(`agent.crName="" accepted by the schema`)
	}

	// Every string up to six characters over letters, a digit, both
	// separators, an uppercase letter and an underscore: the dot and dash
	// placements the pattern has to get right, and the characters it must
	// refuse.
	alphabet := []string{"a", "0", "-", ".", "A", "_"}
	var gen func(prefix string, depth int)
	gen = func(prefix string, depth int) {
		if prefix != "" {
			check(prefix)
		}
		if depth == 0 {
			return
		}
		for _, c := range alphabet {
			gen(prefix+c, depth-1)
		}
	}
	gen("", 6)

	// The length limit, on one label and on a dotted name.
	label63 := strings.Repeat("a", 63)
	for _, s := range []string{
		strings.Repeat("a", 253), strings.Repeat("a", 254), // one long label (the validator allows 253 here; it is not a label limit)
		strings.Join([]string{label63, label63, label63, strings.Repeat("a", 61)}, "."),       // 253
		strings.Join([]string{label63, label63, label63, strings.Repeat("a", 62)}, "."),       // 254
		strings.Join([]string{label63, label63, label63, strings.Repeat("a", 61)}, ".") + ".", // 254, trailing dot
	} {
		check(s)
	}
}

// The same rule, end to end: what helm install accepts and refuses.
func TestSchemaCRName(t *testing.T) {
	label63 := strings.Repeat("a", 63)
	name253 := strings.Join([]string{label63, label63, label63, strings.Repeat("a", 61)}, ".")
	if len(name253) != 253 {
		t.Fatalf("test setup: name253 is %d bytes", len(name253))
	}
	for _, good := range []string{"cluster", "a.b", "prod.eu-west-1", name253} {
		if out := renderErr(t, "agent.crName="+good); out != "" {
			t.Errorf("agent.crName=%.40q rejected: %s", good, out)
		}
	}
	for _, bad := range []string{"a..b", "a.-b", "Prod", name253 + "a" /* 254 bytes */} {
		if renderErr(t, "agent.crName="+bad) == "" {
			t.Errorf("agent.crName=%.40q (%d bytes) rendered, want a schema error", bad, len(bad))
		}
	}
}

// The ClusterReadiness CRD caps spec.targets at 8 (crd.MaxTargets) and the
// agent refuses a longer --targets at start, so the pod would crash-loop;
// the schema refuses it at helm install instead. The cap is read from the
// schema and held against the CRD's, so the two cannot drift apart.
func TestSchemaAgentTargetsCapIsTheCRDs(t *testing.T) {
	maxItems, ok := agentSchemaProp(t, "targets")["maxItems"].(float64)
	if !ok || int(maxItems) != crd.MaxTargets {
		t.Errorf("agent.targets maxItems = %v, want crd.MaxTargets (%d)", maxItems, crd.MaxTargets)
	}
}

func TestSchemaAgentTargetsAtMostEight(t *testing.T) {
	eight := "agent.targets={1.30,1.31,1.32,1.33,1.34,1.35,1.36,1.37}"
	if out := renderErr(t, eight); out != "" {
		t.Errorf("8 agent.targets rejected: %s", out)
	}
	nine := "agent.targets={1.30,1.31,1.32,1.33,1.34,1.35,1.36,1.37,1.38}"
	if renderErr(t, nine) == "" {
		t.Error("9 agent.targets rendered, want a schema error")
	}
}
