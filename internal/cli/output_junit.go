package cli

import (
	"io"

	"github.com/abd-ulbasit/upgradescope/internal/engine"
	"github.com/abd-ulbasit/upgradescope/internal/junit"
)

// WriteJUnit renders the report as JUnit XML (shared writer in
// internal/junit) whose test outcomes follow the gate: failOn and
// allowIncomplete as `scan --fail-on` and `--allow-incomplete` take them.
func WriteJUnit(w io.Writer, r engine.Report, failOn string, allowIncomplete bool) error {
	return junit.Write(w, r, junit.Options{FailOn: failOn, AllowIncomplete: allowIncomplete})
}
