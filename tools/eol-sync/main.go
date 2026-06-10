// Command eol-sync keeps registry entries that declare an
// endoflife_product slug in sync with the endoflife.date API.
//
// For every registry/data/*.yaml containing `endoflife_product: <slug>` it
// GETs https://endoflife.date/api/<slug>.json and rewrites ONLY the
// support.status and support.eol_date lines; every other byte of the file
// (matchers, citations, comments, compat rows) is preserved.
//
// # The EOL rule
//
// The endoflife.date API returns release cycles newest-first; each cycle's
// "eol" field is either a boolean or a "YYYY-MM-DD" date. eol-sync looks at
// the NEWEST cycle only:
//
//   - eol == false          → status=supported, eol_date cleared
//   - eol == true           → status=eol,       eol_date cleared (date unknown)
//   - eol == "YYYY-MM-DD"   → eol_date recorded; status=eol when the date is
//     today or earlier (UTC), else supported
//
// Rationale: older cycles going EOL means "upgrade the add-on", not "the
// add-on is dead" — an add-on is flagged EOL only when its newest release
// line is EOL (that is product-level end of life, e.g. ingress-nginx).
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

// run processes every *.yaml in dir and returns how many files drifted
// from the API-derived state. In check mode files are never written.
func run(dir string, check bool, fetch func(slug string) ([]byte, error), now time.Time, out io.Writer) (drift int, err error) {
	paths, err := filepath.Glob(filepath.Join(dir, "*.yaml"))
	if err != nil {
		return 0, err
	}
	if len(paths) == 0 {
		return 0, fmt.Errorf("no *.yaml files in %s", dir)
	}
	for _, path := range paths {
		raw, err := os.ReadFile(path)
		if err != nil {
			return drift, err
		}
		slug := extractSlug(raw)
		if slug == "" {
			continue // hand-curated entry
		}
		body, err := fetch(slug)
		if err != nil {
			return drift, fmt.Errorf("%s: fetch %q: %w", filepath.Base(path), slug, err)
		}
		status, date, err := computeSupport(body, now)
		if err != nil {
			return drift, fmt.Errorf("%s: %w", filepath.Base(path), err)
		}
		updated, err := rewriteSupport(raw, status, date)
		if err != nil {
			return drift, fmt.Errorf("%s: %w", filepath.Base(path), err)
		}
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
		fmt.Fprintf(out, "eol-sync: %-28s slug=%-12s status=%-9s eol_date=%-10s %s\n",
			filepath.Base(path), slug, status, orDash(date), state)
	}
	return drift, nil
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
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
