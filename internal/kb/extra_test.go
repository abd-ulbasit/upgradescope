package kb

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/abd-ulbasit/upgradescope/registry"
)

// TestLoadWithRegistry: operator-supplied entries (--registry-dir) are
// validated like embedded ones, add to the registry, replace an embedded
// entry with the same id outright, and change the KB version, so a report
// says it was judged against a different registry (#49).
func TestLoadWithRegistry(t *testing.T) {
	base, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if k, err := LoadWithRegistry(""); err != nil || k.Version != base.Version {
		t.Fatalf(`LoadWithRegistry("") = %q, %v; want Load()'s %q`, k.Version, err, base.Version)
	}

	dir := t.TempDir()
	entry := func(id, display, citation string) string {
		return "schema_version: 2\nid: " + id + "\ndisplay_name: " + display + "\nmatchers:\n  images:\n    - acme/" + id +
			"\nsupport:\n  status: eol\n  citations:\n    - " + citation + "\n"
	}
	write := func(name, content string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("acme-proxy.yaml", entry("acme-proxy", "Acme Proxy", "https://acme.dev/lifecycle"))
	write("coredns.yaml", entry("coredns", "Our CoreDNS", "https://acme.dev/lifecycle"))
	k, err := LoadWithRegistry(dir)
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]registry.AddOn{}
	for _, a := range k.AddOns {
		byID[a.ID] = a
	}
	if got := byID["acme-proxy"].DisplayName; got != "Acme Proxy" {
		t.Errorf("acme-proxy = %q, want the added entry", got)
	}
	if c := byID["coredns"]; c.DisplayName != "Our CoreDNS" || len(c.Cycles) != 0 || len(c.Matchers.Charts) != 0 {
		t.Errorf("coredns = %+v, want the extra entry alone: it replaces the embedded one, no field is inherited", c)
	}
	if len(k.AddOns) != len(base.AddOns)+1 {
		t.Errorf("%d add-ons, want %d: one added, one replaced", len(k.AddOns), len(base.AddOns)+1)
	}
	if k.Version == base.Version {
		t.Errorf("KB version %q does not tell the extra registry from the embedded one", k.Version)
	}
	if len(k.APILifecycle) != len(base.APILifecycle) {
		t.Error("the extra registry changed the API lifecycle data")
	}

	write("broken.yaml", entry("broken", "Broken", "https://example.com/x"))
	if _, err := LoadWithRegistry(dir); err == nil || !strings.Contains(err.Error(), "broken.yaml") || !strings.Contains(err.Error(), dir) {
		t.Errorf("an invalid extra entry must fail the load naming its file and the path, got %v", err)
	}
}
