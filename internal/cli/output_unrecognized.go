package cli

import (
	"fmt"
	"io"

	"github.com/abd-ulbasit/upgradescope/internal/engine"
	"github.com/abd-ulbasit/upgradescope/internal/inventory"
)

// unrecognizedLimit caps the unrecognized image repositories the table and
// Markdown list; JSON carries the full (capped) list.
const unrecognizedLimit = 10

// unrecognizedNote says what an unrecognized image is (#18): a gap in
// add-on detection, not a finding.
const unrecognizedNote = "No add-on image matcher claims these repositories, so an add-on running one is found only through its labels or Helm release"

// unrecognizedShown returns the repositories to list and how many more
// there are, the cap's omissions included.
func unrecognizedShown(r engine.Report) (shown []string, more int) {
	shown = r.UnrecognizedImages
	if len(shown) > unrecognizedLimit {
		shown = shown[:unrecognizedLimit]
	}
	return shown, len(r.UnrecognizedImages) - len(shown) + r.UnrecognizedImagesOmitted
}

// writeUnrecognizedImages lists the image repositories no image matcher
// claims, with their total count, after the rest of the table report.
func writeUnrecognizedImages(w io.Writer, r engine.Report) {
	total := len(r.UnrecognizedImages) + r.UnrecognizedImagesOmitted
	if total == 0 {
		return
	}
	shown, more := unrecognizedShown(r)
	fmt.Fprintf(w, "\nUNRECOGNIZED IMAGES (%d)\n  %s:\n", total, unrecognizedNote)
	for _, repo := range shown {
		fmt.Fprintf(w, "  - %s\n", repo)
	}
	if more > 0 {
		fmt.Fprintf(w, "  …and %d more (--output json lists up to %d)\n", more, inventory.MaxUnrecognizedImages)
	}
}

// mdUnrecognizedImages is writeUnrecognizedImages for Markdown, folded
// into a <details> block: the repositories come from the scanned cluster
// or manifests, so each is a code span.
func mdUnrecognizedImages(w io.Writer, r engine.Report) {
	total := len(r.UnrecognizedImages) + r.UnrecognizedImagesOmitted
	if total == 0 {
		return
	}
	shown, more := unrecognizedShown(r)
	fmt.Fprintf(w, "\n<details><summary>Unrecognized images (%d)</summary>\n\n%s.\n\n", total, unrecognizedNote)
	for _, repo := range shown {
		fmt.Fprintf(w, "- %s\n", mdCode(repo))
	}
	if more > 0 {
		fmt.Fprintf(w, "- …and %d more (`--output json` lists up to %d)\n", more, inventory.MaxUnrecognizedImages)
	}
	fmt.Fprintf(w, "\n</details>\n")
}
