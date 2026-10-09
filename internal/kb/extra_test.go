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

// An extra entry may not claim an image an embedded entry (of another id)
// already claims: that image would be judged twice, and by the operator's
// entry wrongly. A one-segment matcher claims only that exact repository, so
// a bare "operator" is accepted and leaves cilium/operator to Cilium.
func TestLoadWithRegistryRejectsDoubleClaims(t *testing.T) {
	load := func(matcher string) error {
		dir := t.TempDir()
		entry := "schema_version: 2\nid: my-operator\ndisplay_name: My Operator\nmatchers:\n  images:\n    - " + matcher +
			"\nsupport:\n  status: eol\n  citations:\n    - https://acme.dev/lifecycle\n"
		if err := os.WriteFile(filepath.Join(dir, "my-operator.yaml"), []byte(entry), 0o644); err != nil {
			t.Fatal(err)
		}
		_, err := LoadWithRegistry(dir)
		return err
	}
	if err := load("cilium/operator"); err == nil || !strings.Contains(err.Error(), "cilium") || !strings.Contains(err.Error(), "my-operator") {
		t.Errorf("claiming cilium/operator: want an error naming both entries, got %v", err)
	}
	if err := load("operator"); err != nil {
		t.Errorf("an exact one-segment matcher claims no mirror path and no other product: %v", err)
	}

	// A replacement copied from an earlier release, whose claim has since
	// moved to another entry (#265), is told to copy the file again.
	dir := t.TempDir()
	old := "schema_version: 2\nid: rke2-ingress-nginx\ndisplay_name: RKE2 Ingress NGINX\nmatchers:\n  images:\n    - rancher/nginx-ingress-controller" +
		"\nsupport:\n  status: supported\n  citations:\n    - https://docs.rke2.io/reference/ingress_migration\n"
	if err := os.WriteFile(filepath.Join(dir, "rke2-ingress-nginx.yaml"), []byte(old), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadWithRegistry(dir); err == nil || !strings.Contains(err.Error(), "copy of registry/data/rke2-ingress-nginx.yaml from an earlier release") {
		t.Errorf("an outdated copy of an embedded entry: want the hint to copy it again, got %v", err)
	}
}
