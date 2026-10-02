package server

import "github.com/abd-ulbasit/upgradescope/internal/engine"

// versioned stamps r with the report envelope (#60): the report-shaped
// /api/v1 responses (a cluster's report, stored or what-if, and the gate's
// JSON answer) lead with schemaVersion and toolVersion as `scan --output
// json` does. toolVersion is this server's build (Config.Version), also
// for a stored evaluation an older server wrote: the envelope versions the
// response's shape, which is this server's.
func (s *Server) versioned(r reportWithTeams) reportWithTeams {
	r.Envelope = engine.NewEnvelope(s.cfg.Version)
	return r
}
