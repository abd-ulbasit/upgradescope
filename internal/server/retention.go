package server

import (
	"context"
	"log"
	"time"

	"github.com/abd-ulbasit/upgradescope/internal/inventory"
	"github.com/abd-ulbasit/upgradescope/internal/server/store"
)

// Retention. Every changed snapshot and one evaluation per target used to
// be kept forever, so the database grew with the number of inventory
// changes (node, image and namespace churn included) until the volume
// filled. With Config.Retention set, the server prunes rows older than the
// window — never a cluster's latest snapshot or its evaluations — once at
// startup and then every retentionInterval. History coarser than the
// window is gone: score history and exports reach back Retention at most.
//
// One older evaluation, and its snapshot, is kept past the window for the
// targets the server can still compare against: a cluster's newest
// decided evaluation of each target it evaluates, and of the
// upgradeLookback minors below its default target (the baselines of an
// upgrade, upgradeBaseline). A target the server no longer evaluates for
// the cluster (dropped from --targets, or the previous defaults of a
// cluster that upgraded further) ages out like any evaluation.

// retentionInterval is how often the pruner runs after the startup pass.
const retentionInterval = 24 * time.Hour

// Retention is observable (#263): a prune that fails is counted
// (upgradescope_retention_prune_failures_total), and the time of the last
// complete one is exported (upgradescope_retention_last_success_timestamp_seconds),
// which the chart's UpgradescopeRetentionStale alert watches. A failed
// prune is not a readiness failure: /readyz pings the store and nothing
// else, because a server that cannot prune still ingests and serves, and
// restarting it would not help.
//
// The store deletes in bounded batches (store.Prune), so a failure partway
// leaves the batches it committed deleted and the next run (the next day,
// or the next start) resumes where it stopped.

// pruneOnce deletes what has aged out of the retention window and records
// the outcome. A failure is logged and counted; the next run retries and
// resumes where this one stopped. A prune cut short by shutdown is neither
// a failure nor a success.
func (s *Server) pruneOnce(ctx context.Context) {
	rm := s.metrics.retention
	cutoff := s.now().Add(-s.cfg.Retention)
	res, err := s.cfg.Store.Prune(ctx, cutoff, s.retainedBaselines(ctx))
	rm.deleted.WithLabelValues("snapshots").Add(float64(res.Snapshots))
	rm.deleted.WithLabelValues("evaluations").Add(float64(res.Evaluations))
	if err != nil {
		if ctx.Err() != nil {
			log.Printf("server: retention: pruning before %s stopped by shutdown after %d snapshots and %d evaluations; the next run resumes: %v",
				cutoff.UTC().Format(time.RFC3339), res.Snapshots, res.Evaluations, err)
			return
		}
		rm.failures.WithLabelValues(storeKind(s.cfg.Store)).Inc()
		log.Printf("server: retention: pruning before %s failed after %d snapshots and %d evaluations; the next run resumes: %v",
			cutoff.UTC().Format(time.RFC3339), res.Snapshots, res.Evaluations, err)
		return
	}
	rm.succeeded(s.now())
	if res.Snapshots > 0 || res.Evaluations > 0 {
		log.Printf("server: retention: pruned %d snapshots and %d evaluations older than %s",
			res.Snapshots, res.Evaluations, cutoff.UTC().Format(time.RFC3339))
	}
}

// retainedBaselines is, per cluster, the targets whose notification
// baselines retention keeps: the ones the server evaluates at the
// cluster's server version (evalTargets) and the lookbackTargets of its
// default target. A cluster whose version is not known is left out, and
// keeps every target's baseline; so is every cluster when the heads
// cannot be read.
//
// Prune applies this set in its own transactions, after it was computed:
// a version change landing between the two can leave out a baseline only
// the new version needs. That is harmless for an ordinary upgrade, since
// consecutive lookback sets overlap (a one-minor upgrade keeps all but the
// oldest of the previous set, and the baseline the new default compares
// with is the previous default's, in both sets). A jump of several minors
// in that window can lose a baseline; the cost is that the first push
// after it has none and notifies nothing for that target, as for a first
// evaluation.
func (s *Server) retainedBaselines(ctx context.Context) store.PruneBaselines {
	heads, err := s.cfg.Store.LatestSnapshotHeads(ctx)
	if err != nil {
		log.Printf("server: retention: latest snapshots, keeping every baseline: %v", err)
		return nil
	}
	keep := make(store.PruneBaselines, len(heads))
	for id, head := range heads {
		version, err := s.versionOf(ctx, head)
		if err != nil {
			log.Printf("server: retention: cluster %d, keeping every baseline: %v", id, err)
			continue
		}
		server, err := inventory.ParseVersion(version)
		if err != nil {
			continue
		}
		targets := s.evalTargets(version)
		targets = append(targets, lookbackTargets(server.Next())...)
		names := make([]string, 0, len(targets))
		for _, t := range targets {
			names = append(names, t.String())
		}
		keep[id] = names
	}
	return keep
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
