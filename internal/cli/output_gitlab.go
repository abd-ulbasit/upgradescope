package cli

import (
	"io"

	"github.com/abd-ulbasit/upgradescope/internal/codequality"
	"github.com/abd-ulbasit/upgradescope/internal/engine"
)

// WriteGitLabCodeQuality renders the report as a GitLab Code Quality
// report (shared writer in internal/codequality).
func WriteGitLabCodeQuality(w io.Writer, r engine.Report) error {
	return codequality.Write(w, r)
}
