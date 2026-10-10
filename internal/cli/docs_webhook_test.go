package cli

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/abd-ulbasit/upgradescope/internal/engine"
	"github.com/abd-ulbasit/upgradescope/internal/inventory"
	"github.com/abd-ulbasit/upgradescope/internal/kb"
)

// The add-on changes of the payload example on docs/reference/webhook.md
// are presented as what the server sends, so they are regenerated from the
// embedded registry at the example's own timestamp: a finding the engine
// can never produce (Istio 1.24 reaching end of life on 2026-11-30, when
// the registry has it ended on 2025-06-24) or a title the engine words
// differently fails here instead of misleading a reader (#269).
func TestWebhookDocExampleAddOnChangesMatchTheEngine(t *testing.T) {
	raw, err := os.ReadFile("../../docs/reference/webhook.md")
	if err != nil {
		t.Fatal(err)
	}
	_, section, ok := strings.Cut(string(raw), "## Payload (schemaVersion 1)")
	if !ok {
		t.Fatal("webhook.md has no \"Payload (schemaVersion 1)\" section")
	}
	_, block, _ := strings.Cut(section, "```json\n")
	block, _, _ = strings.Cut(block, "```")
	var payload struct {
		Timestamp string `json:"timestamp"`
		Changes   []struct {
			Kind, Key, Severity, Title, Detail string
		} `json:"changes"`
	}
	if err := json.Unmarshal([]byte(block), &payload); err != nil {
		t.Fatalf("the example payload is not JSON: %v", err)
	}
	at, err := time.Parse(time.RFC3339, payload.Timestamp)
	if err != nil {
		t.Fatalf("timestamp %q: %v", payload.Timestamp, err)
	}
	k, err := kb.Load()
	if err != nil {
		t.Fatal(err)
	}
	// The installs the example's changes are about, one release per change.
	inv := inventory.Inventory{SchemaVersion: 1, ClusterID: "prod-eu-1", Source: inventory.SourceCluster, ServerVersion: "v1.34.2",
		AddOns: []inventory.AddOnInstance{
			{ID: "ingress-nginx", Version: "1.11.3", Namespaces: []string{"ingress-nginx"}, Source: "image"},
			{ID: "istio", Version: "1.30.1", Namespaces: []string{"istio-system"}, Source: "image"},
		}}
	r := engine.Evaluate(inv, k, inventory.Version{Major: 1, Minor: 35}, at)
	byKey := map[string]engine.Finding{}
	for _, f := range r.Findings {
		byKey[f.Key] = f
	}
	checked := 0
	for _, c := range payload.Changes {
		if !strings.HasPrefix(c.Key, "eol-") {
			continue
		}
		checked++
		f, ok := byKey[c.Key]
		switch {
		case !ok:
			t.Errorf("change %s: the engine produces no such finding at %s (it has %v)", c.Key, payload.Timestamp, keysOf(byKey))
		case string(f.Severity) != c.Severity || f.Title != c.Title || f.Detail != c.Detail:
			t.Errorf("change %s differs from the engine's finding:\n doc    %s %q\n        %q\n engine %s %q\n        %q", c.Key, c.Severity, c.Title, c.Detail, f.Severity, f.Title, f.Detail)
		}
	}
	if checked != 2 {
		t.Errorf("checked %d add-on changes, want the example's 2", checked)
	}
}

func keysOf(m map[string]engine.Finding) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}
