package cli

import (
	"bytes"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/abd-ulbasit/upgradescope/internal/engine"
	"github.com/abd-ulbasit/upgradescope/internal/inventory"
	"github.com/abd-ulbasit/upgradescope/internal/kb"
)

// The examples on docs/concepts/support-lifecycle.md are presented as scan
// output, so they are pinned to it: each is regenerated from the embedded
// knowledge base at the date the page names and compared with the page. A
// change to the wording, the dates or the fix line fails here until the page
// is regenerated, instead of showing a reader output the tool no longer
// prints. The page wraps the detail for width; whitespace is not compared.
func TestSupportLifecycleDocExamplesMatchScanOutput(t *testing.T) {
	raw, err := os.ReadFile("../../docs/concepts/support-lifecycle.md")
	if err != nil {
		t.Fatal(err)
	}
	examples := map[string]string{} // finding title -> "title|detail|fix"
	for _, block := range strings.Split(string(raw), "```text\n")[1:] {
		block, _, _ = strings.Cut(block, "```")
		if !strings.HasPrefix(block, "[support-lifecycle] ") {
			continue
		}
		title, rest, _ := strings.Cut(block, "\n")
		detail, fix, ok := strings.Cut(rest, "fix: ")
		if !ok {
			t.Fatalf("doc example %q has no fix line", title)
		}
		examples[title] = squash(title) + "|" + squash(detail) + "|" + squash(fix)
	}

	k, err := kb.Load()
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name     string
		provider inventory.Provider
		server   string
		date     string
	}{
		{"EKS 1.34 on 2026-10-15", inventory.ProviderEKS, "v1.34.2-eks-3abc123", "2026-10-15"},
		{"AKS 1.33 on 2026-10-04", inventory.ProviderAKS, "v1.33.4", "2026-10-04"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			now, err := time.Parse("2006-01-02", c.date)
			if err != nil {
				t.Fatal(err)
			}
			inv := inventory.Inventory{SchemaVersion: 1, ClusterID: "c", Provider: c.provider, ServerVersion: c.server}
			r := engine.Evaluate(inv, k, inventory.Version{Major: 1, Minor: 36}, now)
			var buf bytes.Buffer
			if err := WriteTable(&buf, r); err != nil {
				t.Fatal(err)
			}
			var title, detail, fix string
			lines := strings.Split(buf.String(), "\n")
			for i, l := range lines {
				if !strings.HasPrefix(l, "  [support-lifecycle] ") {
					continue
				}
				title = strings.TrimPrefix(l, "  ")
				for _, n := range lines[i+1:] {
					if f, ok := strings.CutPrefix(n, "      fix: "); ok {
						fix = f
						break
					}
					if !strings.HasPrefix(n, "      ") {
						break
					}
					detail += n + " "
				}
				break
			}
			if title == "" || detail == "" || fix == "" {
				t.Fatalf("no complete support-lifecycle finding in:\n%s", buf.String())
			}
			want := squash(title) + "|" + squash(detail) + "|" + squash(fix)
			got, ok := examples[title]
			if !ok {
				t.Fatalf("docs/concepts/support-lifecycle.md has no example titled %q", title)
			}
			if got != want {
				t.Errorf("docs/concepts/support-lifecycle.md is stale for %s\npage:  %s\nscan:  %s", c.name, got, want)
			}
		})
	}
}

func squash(s string) string { return strings.Join(strings.Fields(s), " ") }
