package server

import (
	"context"
	"log"
	"time"
)

// Retention. Every changed snapshot and one evaluation per target used to
// be kept forever, so the database grew with the number of inventory
// changes (node, image and namespace churn included) until the volume
// filled. With Config.Retention set, the server prunes rows older than the
// window — never a cluster's latest snapshot or its evaluations — once at
// startup and then every retentionInterval. History coarser than the
// window is gone: score history and exports reach back Retention at most.

// retentionInterval is how often the pruner runs after the startup pass.
const retentionInterval = 24 * time.Hour

// pruneOnce deletes what has aged out of the retention window. A failure
// is logged; the next run retries.
func (s *Server) pruneOnce(ctx context.Context) {
	cutoff := s.now().Add(-s.cfg.Retention)
	res, err := s.cfg.Store.Prune(ctx, cutoff)
	if err != nil {
		log.Printf("server: retention: pruning before %s: %v", cutoff.UTC().Format(time.RFC3339), err)
		return
	}
	if res.Snapshots > 0 || res.Evaluations > 0 {
		log.Printf("server: retention: pruned %d snapshots and %d evaluations older than %s",
			res.Snapshots, res.Evaluations, cutoff.UTC().Format(time.RFC3339))
	}
}

// runRetention prunes at startup and then every retentionInterval until
// ctx ends. Start runs it only when Config.Retention is set.
func (s *Server) runRetention(ctx context.Context) {
	tick := time.NewTicker(s.retentionInterval)
	defer tick.Stop()
	for {
		s.pruneOnce(ctx)
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}
