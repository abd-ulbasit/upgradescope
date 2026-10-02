package server

import (
	"time"

	"github.com/abd-ulbasit/upgradescope/internal/server/store"
)

// DefaultStaleAfter is how long a cluster may go without a push before the
// API and /metrics call it stale. An agent pushes on every change and at
// least every --force-sync-every (1h), on its next tick (10m by default),
// so an unchanged, healthy cluster pushes about every 70 minutes; 2h
// leaves room for that and one missed push. It matches the chart's
// UpgradescopeClusterStale alert (7200s).
const DefaultStaleAfter = 2 * time.Hour

func (s *Server) staleAfter() time.Duration {
	if s.cfg.StaleAfter > 0 {
		return s.cfg.StaleAfter
	}
	return DefaultStaleAfter
}

// clusterStale reports whether c's agent has not pushed within the stale
// window: its scores describe the cluster as it was at LastSeen.
func (s *Server) clusterStale(c store.Cluster, now time.Time) bool {
	return now.Sub(c.LastSeen) > s.staleAfter()
}
