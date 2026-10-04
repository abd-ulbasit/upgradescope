// Command eol-sync keeps registry entries that declare an
// endoflife_product slug in sync with the endoflife.date API.
//
// For every registry/data/*.yaml containing `endoflife_product: <slug>` it
// GETs https://endoflife.date/api/<slug>.json and rewrites ONLY the
// top-level `cycles:` block (appending one when missing); every other byte
// of the file (matchers, support, citations, comments, compat rows) is
// preserved.
//
// # Cycles, not a product date
//
// The engine maps an installed add-on version to its release cycle and
// judges that cycle's end of life, so every cycle the API publishes is
// written — date or boolean eol, plus the supported Kubernetes range where
// the product publishes one — each citing https://endoflife.date/<slug>.
// eol-sync never touches support.status: a product-level "eol" means the
// whole product was retired (ingress-nginx), which a human records. When
// every cycle has ended, eol-sync says so in its output for that review.
//
// # Managed Kubernetes providers
//
// registry/data/providers/*.yaml describe a managed service's Kubernetes
// support calendar (EKS, GKE, AKS). An entry that declares a slug has its
// top-level `versions:` block generated the same way: each minor's
// standard-support end ("eol") and extended-support end ("extendedSupport"),
// and nothing else changes, the hand-entered price included. Entries
// without a slug (GKE) are hand-curated and skipped.
//
// Usage:
//
//	go run . -dir ../../registry/data          # rewrite files in place
//	go run . -dir ../../registry/data -check   # exit 1 on drift, write nothing
//
// stdlib only, on purpose: this tool runs in CI cron with no module cache.
package main

import (
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

func main() {
	dir := flag.String("dir", "../../registry/data", "registry data directory")
	check := flag.Bool("check", false, "report drift and exit 1 instead of rewriting files")
	flag.Parse()

	fetch := func(slug string) ([]byte, error) {
		return fetchProduct("https://endoflife.date/api/"+slug+".json", 30*time.Second)
	}
	drift, err := run(*dir, *check, fetch, time.Now().UTC(), os.Stdout)
	if err != nil {
		fmt.Fprintln(os.Stderr, "eol-sync:", err)
		os.Exit(1)
	}
	if *check && drift > 0 {
		fmt.Fprintf(os.Stderr, "eol-sync: %d file(s) out of sync with endoflife.date — run `make eol-sync`\n", drift)
		os.Exit(1)
	}
}

// syncResult is what one entry's API response renders: the block that
// replaces the entry's generated key, a one-line summary for the output,
// and a note to print after the entry's line, if any.
type syncResult struct {
	block         []byte
	summary, note string
}

// run processes every *.yaml in dir and in dir/providers, and returns how
// many files drifted from the API-derived state. In check mode files are
// never written.
func run(dir string, check bool, fetch func(slug string) ([]byte, error), now time.Time, out io.Writer) (drift int, err error) {
	paths, err := filepath.Glob(filepath.Join(dir, "*.yaml"))
	if err != nil {
		return 0, err
	}
	if len(paths) == 0 {
		return 0, fmt.Errorf("no *.yaml files in %s", dir)
	}
	provPaths, err := filepath.Glob(filepath.Join(dir, "providers", "*.yaml"))
	if err != nil {
		return 0, err
	}
	for _, path := range append(paths, provPaths...) {
		raw, err := os.ReadFile(path)
		if err != nil {
			return drift, err
		}
		slug := extractSlug(raw)
		if slug == "" {
			continue // hand-curated entry
		}
		key, render := "cycles", renderEntryCycles
		if filepath.Base(filepath.Dir(path)) == "providers" {
			key, render = "versions", renderEntryWindows
		}
		body, err := fetch(slug)
		if err != nil {
			return drift, fmt.Errorf("%s: fetch %q: %w", filepath.Base(path), slug, err)
		}
		res, err := render(body, slug, now)
		if err != nil {
			return drift, fmt.Errorf("%s: %w", filepath.Base(path), err)
		}
		updated := rewriteBlock(raw, key, res.block)
		state := "in sync"
		if string(updated) != string(raw) {
			drift++
			if check {
				state = "DRIFT"
			} else {
				if err := os.WriteFile(path, updated, 0o644); err != nil {
					return drift, err
				}
				state = "updated"
			}
		}
		fmt.Fprintf(out, "eol-sync: %-28s slug=%-12s %s %s\n", filepath.Base(path), slug, res.summary, state)
		if res.note != "" {
			fmt.Fprintf(out, "eol-sync: NOTE %s: %s\n", filepath.Base(path), res.note)
		}
	}
	return drift, nil
}

// renderEntryCycles renders an add-on entry's cycles block.
func renderEntryCycles(body []byte, slug string, now time.Time) (syncResult, error) {
	rows, err := computeCycles(body)
	if err != nil {
		return syncResult{}, err
	}
	res := syncResult{
		block:   renderCycles(rows, "https://endoflife.date/"+slug),
		summary: fmt.Sprintf("cycles=%-3d newest=%-6s eol=%-12s", len(rows), rows[0].cycle, rows[0].eol),
	}
	if allEnded(rows, now) {
		res.note = "every cycle has ended; if upstream retired the product, set support.status: eol by hand"
	}
	return res, nil
}

// renderEntryWindows renders a provider entry's versions block.
func renderEntryWindows(body []byte, _ string, _ time.Time) (syncResult, error) {
	rows, err := computeWindows(body)
	if err != nil {
		return syncResult{}, err
	}
	return syncResult{
		block:   renderWindows(rows),
		summary: fmt.Sprintf("versions=%-3d newest=%-6s std-end=%-12s", len(rows), rows[0].minor, rows[0].standardEnd),
	}, nil
}

func fetchProduct(url string, timeout time.Duration) ([]byte, error) {
	client := &http.Client{Timeout: timeout}
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "upgradescope-eol-sync (+https://github.com/abd-ulbasit/upgradescope)")
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 1<<20))
}
