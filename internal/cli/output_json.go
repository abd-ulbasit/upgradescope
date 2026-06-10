package cli

import (
	"encoding/json"
	"io"

	"github.com/abd-ulbasit/upgradescope/internal/engine"
)

// WriteJSON renders the report as canonical two-space-indented JSON with a
// trailing newline. This is the machine-readable contract; field names come
// from the engine.Report struct tags and must stay stable.
func WriteJSON(w io.Writer, r engine.Report) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(r)
}
