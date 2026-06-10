package cli

import (
	"io"

	"github.com/abd-ulbasit/upgradescope/internal/engine"
	"github.com/abd-ulbasit/upgradescope/internal/sarif"
)

// WriteSARIF renders the report as SARIF 2.1.0 (shared writer in
// internal/sarif), stamped with the CLI build version.
func WriteSARIF(w io.Writer, r engine.Report) error {
	return sarif.Write(w, r, version)
}
