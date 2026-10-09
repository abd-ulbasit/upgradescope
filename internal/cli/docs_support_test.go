package cli

import (
	"bytes"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/abd-ulbasit/upgradescope/internal/engine"
	"github.com/abd-ulbasit/upgradescope/internal/inventory"
	"github.com/abd-ulbasit/upgradescope/internal/kb"
)

const supportLifecycleDoc = "../../docs/concepts/support-lifecycle.md"

// The examples on docs/concepts/support-lifecycle.md are presented as scan
// output, so they are pinned to it: each is regenerated from the embedded
// knowledge base at the date the page names and compared with the page. A
// change to the wording, the dates or the fix line fails here until the page
// is regenerated, instead of showing a reader output the tool no longer
// prints. The page wraps the detail for width; whitespace is not compared.
//
// With -update (make kb-derived, which both weekly refresh jobs run before
// they commit) a stale example is rewritten from the scan, its detail
// wrapped at 80 columns; an example that matches is left as it is. The
// support-lifecycle examples are matched to the cases below in order.
func TestSupportLifecycleDocExamplesMatchScanOutput(t *testing.T) {
	raw, err := os.ReadFile(supportLifecycleDoc)
	if err != nil {
		t.Fatal(err)
	}
	page := string(raw)
	blocks := supportLifecycleBlocks(page)

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
	if len(blocks) != len(cases) {
		t.Fatalf("%s has %d support-lifecycle examples, want %d (one per case, in order)", supportLifecycleDoc, len(blocks), len(cases))
	}
	rewrite := page
	for i, c := range cases {
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
			title, detail, fix := supportLifecycleFinding(buf.String())
			if title == "" || detail == "" || fix == "" {
				t.Fatalf("no complete support-lifecycle finding in:\n%s", buf.String())
			}
			want := squash(title) + "|" + squash(detail) + "|" + squash(fix)
			got, err := exampleKey(blocks[i])
			if err != nil {
				t.Fatal(err)
			}
			if got == want {
				return
			}
			if *update {
				rewrite = strings.Replace(rewrite, "```text\n"+blocks[i]+"```", "```text\n"+formatExample(title, detail, fix)+"```", 1)
				return
			}
			t.Errorf("%s is stale for %s (go test ./internal/cli -run TestSupportLifecycleDocExamplesMatchScanOutput -update, or make kb-derived, rewrites it)\npage:  %s\nscan:  %s", supportLifecycleDoc, c.name, got, want)
		})
	}
	if *update && rewrite != page {
		if err := os.WriteFile(supportLifecycleDoc, []byte(rewrite), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// supportLifecycleBlocks returns the body of each ```text block of page
// that is a support-lifecycle finding, in order.
func supportLifecycleBlocks(page string) []string {
	var blocks []string
	for _, block := range strings.Split(page, "```text\n")[1:] {
		block, _, _ = strings.Cut(block, "```")
		if strings.HasPrefix(block, "[support-lifecycle] ") {
			blocks = append(blocks, block)
		}
	}
	return blocks
}

// exampleKey is a page example as "title|detail|fix", whitespace squashed.
func exampleKey(block string) (string, error) {
	title, rest, _ := strings.Cut(block, "\n")
	detail, fix, ok := strings.Cut(rest, "fix: ")
	if !ok {
		return "", fmt.Errorf("doc example %q has no fix line", title)
	}
	return squash(title) + "|" + squash(detail) + "|" + squash(fix), nil
}

// supportLifecycleFinding is the first support-lifecycle finding of a scan
// table: its title, its detail (the lines under it, joined) and its fix.
func supportLifecycleFinding(table string) (title, detail, fix string) {
	lines := strings.Split(table, "\n")
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
	return title, detail, fix
}

// formatExample is a finding as the page shows it: the title, the detail
// wrapped at 80 columns under a 4-space indent, and the fix on one line.
func formatExample(title, detail, fix string) string {
	const indent, width = "    ", 80
	var b strings.Builder
	b.WriteString(squash(title) + "\n")
	line := indent
	for _, w := range strings.Fields(detail) {
		if line != indent && len(line)+1+len(w) > width {
			b.WriteString(line + "\n")
			line = indent
		}
		if line != indent {
			line += " "
		}
		line += w
	}
	if line != indent {
		b.WriteString(line + "\n")
	}
	b.WriteString(indent + "fix: " + squash(fix) + "\n")
	return b.String()
}

func squash(s string) string { return strings.Join(strings.Fields(s), " ") }

// formatExample's output reads back as the finding it was made from, and
// wraps the detail at 80 columns: what -update writes, the test accepts.
func TestFormatExampleRoundTrips(t *testing.T) {
	title := "[support-lifecycle] Kubernetes 1.34 leaves Amazon EKS standard support on 2026-12-02"
	detail := strings.Repeat("Amazon EKS ends standard support for Kubernetes 1.34 on 2026-12-02; ", 4)
	fix := "Upgrade the control plane, one minor at a time. The nearest minor in standard support is 1.35; the newest known is 1.37."
	out := formatExample(title, detail, fix)
	blocks := supportLifecycleBlocks("```text\n" + out + "```\n")
	if len(blocks) != 1 {
		t.Fatalf("formatExample's output is not one example:\n%s", out)
	}
	got, err := exampleKey(blocks[0])
	if err != nil {
		t.Fatal(err)
	}
	if want := squash(title) + "|" + squash(detail) + "|" + squash(fix); got != want {
		t.Errorf("round trip:\n got %s\nwant %s", got, want)
	}
	for _, l := range strings.Split(strings.TrimSuffix(out, "\n"), "\n")[1:] {
		if strings.HasPrefix(l, "    fix: ") {
			continue
		}
		if len(l) > 80 || !strings.HasPrefix(l, "    ") {
			t.Errorf("detail line of %d columns, or not indented 4: %q", len(l), l)
		}
	}
}
