package server

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"slices"
	"time"

	"github.com/abd-ulbasit/upgradescope/internal/engine"
	"github.com/abd-ulbasit/upgradescope/internal/inventory"
	"github.com/abd-ulbasit/upgradescope/internal/server/notify"
	"github.com/abd-ulbasit/upgradescope/internal/server/store"
)

// Evaluation passes. A verdict depends on more than the inventory: on the
// KB, on the server's --targets and --team-map, and on the date (EOL
// windows). So a stored evaluation goes stale without any new snapshot,
// and is re-evaluated — on a duplicate push and by the hourly ticker —
// when its KB version or team-map hash differs from the server's, when it
// was evaluated before the current UTC day (EOL math is day-granular), or
// when a configured target has none.
//
// A re-evaluation with an unchanged result (verdict, score, finding keys)
// refreshes the stored row's evaluatedAt instead of adding history; a
// changed one inserts a row and notifies. Every pass commits atomically
// with its outbox messages (store.CommitEvaluations), and notifications
// are delivered after commit (outbox.go).

// reevaluateInterval is how often the background ticker re-evaluates every
// cluster's latest snapshot (plus one pass at startup), so an EOL date
// passing surfaces and alerts even when no agent pushes.
const reevaluateInterval = time.Hour

// evalTargets lists the targets a snapshot is evaluated against: the
// default (next minor above the inventory's server version; skipped when
// unparseable) plus every configured extra target, deduped. Targets at or
// below the cluster's current minor are not applicable and are skipped.
func (s *Server) evalTargets(inv inventory.Inventory) []inventory.Version {
	server, err := inventory.ParseVersion(inv.ServerVersion)
	known := err == nil
	targets := make([]inventory.Version, 0, len(s.extraTargets)+1)
	if known {
		targets = append(targets, server.Next())
	}
	for _, t := range s.extraTargets {
		if known && t.Compare(server) <= 0 {
			continue
		}
		if !slices.Contains(targets, t) {
			targets = append(targets, t)
		}
	}
	return targets
}

// notApplicable reports whether target is at or below the inventory's
// server version: the cluster already runs it.
func notApplicable(inv inventory.Inventory, target inventory.Version) bool {
	server, err := inventory.ParseVersion(inv.ServerVersion)
	return err == nil && target.Compare(server) <= 0
}

// hashTeamMap fingerprints the team map for staleness checks ("" = none).
func hashTeamMap(tm TeamMap) string {
	if len(tm) == 0 {
		return ""
	}
	b, _ := json.Marshal(tm) // a slice of string pairs cannot fail
	return fmt.Sprintf("%x", sha256.Sum256(b))[:16]
}

// verdictOf reads the verdict back from a stored row: Ready is
// verdict == ready, and blocked means at least one blocker.
func verdictOf(e store.Evaluation) engine.Verdict {
	switch {
	case e.Ready:
		return engine.VerdictReady
	case e.Blockers > 0:
		return engine.VerdictBlocked
	default:
		return engine.VerdictUnknown
	}
}

// evaluation runs the engine for one target and builds the row to store.
func (s *Server) evaluation(cluster store.Cluster, inv inventory.Inventory, target inventory.Version, now time.Time) (store.Evaluation, engine.Report, error) {
	rep := engine.Evaluate(inv, s.cfg.KB, target, now)
	repJSON, err := json.Marshal(rep)
	if err != nil {
		return store.Evaluation{}, engine.Report{}, fmt.Errorf("marshaling report (cluster %d, target %s): %w", cluster.ID, target, err)
	}
	var blockers, warnings int
	for _, f := range rep.Findings {
		switch f.Severity {
		case engine.SevBlocker:
			blockers++
		case engine.SevWarning:
			warnings++
		}
	}
	return store.Evaluation{
		ClusterID:   cluster.ID,
		Target:      target.String(),
		KBVersion:   s.cfg.KB.Version,
		Score:       rep.Score,
		Ready:       rep.Ready,
		Blockers:    blockers,
		Warnings:    warnings,
		Report:      repJSON,
		CreatedAt:   now,
		EvaluatedAt: now,
		TeamMapHash: s.teamMapHash,
	}, rep, nil
}

// futureTolerance is how far ahead of this server's clock a stored
// evaluation may be dated before it counts as future-dated: replicas'
// clocks differ by a little, a stepped-back clock by much more.
const futureTolerance = 5 * time.Minute

// stale reports whether a stored evaluation must be recomputed now. A row
// evaluated "in the future" (the clock stepped backwards since) is stale
// too: its EOL math used a date that has not come yet.
func (s *Server) stale(e store.Evaluation, now time.Time) bool {
	today := now.UTC().Truncate(24 * time.Hour)
	return e.KBVersion != s.cfg.KB.Version || e.TeamMapHash != s.teamMapHash ||
		e.EvaluatedAt.Before(today) || e.EvaluatedAt.After(now.Add(futureTolerance))
}

// sameResult reports whether rep matches the stored row on everything a
// history point or a notification depends on: verdict, score, and the
// set of (severity, finding key).
func sameResult(stored store.Evaluation, rep engine.Report) bool {
	if verdictOf(stored) != rep.Verdict || stored.Score != rep.Score {
		return false
	}
	var prev engine.Report
	if err := json.Unmarshal(stored.Report, &prev); err != nil {
		return false
	}
	return slices.Equal(findingSignature(prev), findingSignature(rep))
}

func findingSignature(rep engine.Report) []string {
	sig := make([]string, 0, len(rep.Findings))
	for _, f := range rep.Findings {
		sig = append(sig, string(f.Severity)+"\x00"+findingKey(f))
	}
	slices.Sort(sig)
	return sig
}

// deltaFor computes the notification delta of one new evaluation. The
// baseline is the last evaluation with a decided verdict, so an "unknown"
// pass (a collector failure) is neither a transition nor a reset; a pass
// whose own verdict is unknown notifies nothing — what it could not see is
// not news. Failures are logged and never fail the pass.
func (s *Server) deltaFor(ctx context.Context, cluster store.Cluster, e store.Evaluation, cur engine.Report) targetDelta {
	d := targetDelta{target: notify.Target{Target: cur.Target.String(), Verdict: string(cur.Verdict), Score: cur.Score, Blockers: e.Blockers}}
	if len(s.sinks) == 0 || cur.Verdict == engine.VerdictUnknown || cluster.ID == 0 {
		return d // a cluster's first push has no baseline either
	}
	target := cur.Target.String()
	prev, err := s.cfg.Store.LatestKnownEvaluation(ctx, cluster.ID, target)
	if errors.Is(err, store.ErrNotFound) {
		return d // first decided evaluation of this target: no delta
	}
	if err != nil {
		log.Printf("server: loading notification baseline (cluster %d, target %s): %v", cluster.ID, target, err)
		return d
	}
	var prevRep engine.Report
	if err := json.Unmarshal(prev.Report, &prevRep); err != nil {
		log.Printf("server: decoding previous report (cluster %d, target %s): %v", cluster.ID, target, err)
		return d
	}
	d.changes = ComputeDelta(&prevRep, cur)
	return d
}

// outboxFor turns one pass's deltas into one notification for the
// cluster, queued once per sink with the same delivery id.
func (s *Server) outboxFor(cluster store.Cluster, deltas []targetDelta, now time.Time) []store.OutboxMessage {
	n, ok := buildNotification(cluster, deltas, now, newDeliveryID())
	if !ok {
		return nil
	}
	payload, err := json.Marshal(n)
	if err != nil {
		log.Printf("server: encoding notification (cluster %s): %v", cluster.Name, err)
		return nil
	}
	msgs := make([]store.OutboxMessage, 0, len(s.sinks))
	for _, sk := range s.sinks {
		msgs = append(msgs, store.OutboxMessage{Sink: sk.name, Payload: payload, CreatedAt: now})
	}
	return msgs
}

// ingestSnapshot evaluates a pushed snapshot against every target, then
// commits the cluster (registered or touched: cluster.ID is 0 for a new
// name), the snapshot, its evaluations and their notifications in one
// transaction. A duplicate (same hash as the latest snapshot) commits only
// the touch and the push's envelope, then re-evaluates the stored
// snapshot where stale.
func (s *Server) ingestSnapshot(ctx context.Context, cluster store.Cluster, snap store.Snapshot, inv inventory.Inventory) (int64, bool, error) {
	// A duplicate (the agent's hourly force-sync) is the common push: go
	// straight to re-evaluating what is stale, instead of evaluating every
	// target for the commit to discard. The commit still checks the hash,
	// for a push racing this one.
	if cluster.ID != 0 {
		latest, err := s.cfg.Store.LatestSnapshot(ctx, cluster.ID)
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			return 0, false, fmt.Errorf("loading latest snapshot (cluster %d): %w", cluster.ID, err)
		}
		if err == nil && latest.Hash == snap.Hash {
			snapID, dup, err := s.cfg.Store.CommitEvaluations(ctx, store.EvaluationBatch{Cluster: &cluster, Snapshot: &snap})
			if err != nil {
				return 0, false, err
			}
			// Not a duplicate after all (another push moved the cluster on
			// meanwhile): the snapshot is stored without evaluations, and
			// reevaluate fills in every target.
			return snapID, dup, s.reevaluate(ctx, cluster, snapID, inv)
		}
	}

	// Server-side team override (spec: labels + server override) — rewrite
	// namespace→team attribution before evaluation; stored reports carry the
	// mapped teams. The stored snapshot keeps the original labels.
	evalInv := inv
	evalInv.Namespaces = s.cfg.TeamMap.Apply(inv.Namespaces)
	now := s.now()
	batch := store.EvaluationBatch{Cluster: &cluster, Snapshot: &snap}
	var deltas []targetDelta
	for _, target := range s.evalTargets(inv) {
		e, rep, err := s.evaluation(cluster, evalInv, target, now)
		if err != nil {
			return 0, false, err
		}
		batch.Insert = append(batch.Insert, e)
		deltas = append(deltas, s.deltaFor(ctx, cluster, e, rep))
	}
	batch.Outbox = s.outboxFor(cluster, deltas, now)
	snapID, duplicate, err := s.cfg.Store.CommitEvaluations(ctx, batch)
	if err != nil {
		return 0, false, err
	}
	if duplicate {
		if cluster.ID == 0 { // registered by a push racing this one
			c, err := s.cfg.Store.ClusterByName(ctx, cluster.Name)
			if err != nil {
				return 0, false, fmt.Errorf("loading cluster %q: %w", cluster.Name, err)
			}
			cluster.ID = c.ID
		}
		return snapID, true, s.reevaluate(ctx, cluster, snapID, inv)
	}
	if len(batch.Outbox) > 0 {
		s.kickOutbox()
	}
	return snapID, false, nil
}

// reevaluate brings a stored snapshot's evaluations up to date: targets
// with no evaluation, or a stale one, are recomputed; unchanged results
// are refreshed in place, changed ones inserted with their notifications.
// A concurrent writer that got there first (store.ErrConflict) has done
// the same work, so the pass is dropped.
func (s *Server) reevaluate(ctx context.Context, cluster store.Cluster, snapID int64, inv inventory.Inventory) error {
	evalInv := inv
	evalInv.Namespaces = s.cfg.TeamMap.Apply(inv.Namespaces)
	now := s.now()
	batch := store.EvaluationBatch{ClusterID: cluster.ID, SnapshotID: snapID, Current: map[string]int64{}}
	var deltas []targetDelta
	for _, target := range s.evalTargets(inv) {
		cur, err := s.cfg.Store.CurrentEvaluation(ctx, cluster.ID, target.String())
		found := err == nil
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			return fmt.Errorf("loading current evaluation (cluster %d, target %s): %w", cluster.ID, target, err)
		}
		if found && cur.SnapshotID != snapID {
			// A newer snapshot arrived meanwhile; its own ingest evaluated it.
			log.Printf("server: re-evaluation of cluster %d skipped: snapshot %d is no longer the latest", cluster.ID, snapID)
			return nil
		}
		if found && !s.stale(cur, now) {
			continue
		}
		batch.Current[target.String()] = cur.ID // 0 when not found
		e, rep, err := s.evaluation(cluster, evalInv, target, now)
		if err != nil {
			return err
		}
		if found && sameResult(cur, rep) {
			e.ID = cur.ID
			batch.Refresh = append(batch.Refresh, e)
			continue
		}
		batch.Insert = append(batch.Insert, e)
		deltas = append(deltas, s.deltaFor(ctx, cluster, e, rep))
	}
	if len(batch.Current) == 0 {
		return nil
	}
	batch.Outbox = s.outboxFor(cluster, deltas, now)
	_, _, err := s.cfg.Store.CommitEvaluations(ctx, batch)
	if errors.Is(err, store.ErrConflict) {
		log.Printf("server: re-evaluation of cluster %d skipped: %v", cluster.ID, err)
		return nil
	}
	if err != nil {
		return err
	}
	if len(batch.Outbox) > 0 {
		s.kickOutbox()
	}
	return nil
}

// reevaluateAll runs reevaluate over every cluster's latest snapshot. One
// cluster's failure is logged and never stops the others.
func (s *Server) reevaluateAll(ctx context.Context) {
	clusters, err := s.cfg.Store.ListClusters(ctx)
	if err != nil {
		log.Printf("server: re-evaluation: listing clusters: %v", err)
		return
	}
	for _, c := range clusters {
		if ctx.Err() != nil {
			return
		}
		snap, err := s.cfg.Store.LatestSnapshot(ctx, c.ID)
		if errors.Is(err, store.ErrNotFound) {
			continue
		}
		if err != nil {
			log.Printf("server: re-evaluation: latest snapshot of cluster %d: %v", c.ID, err)
			continue
		}
		var inv inventory.Inventory
		if err := json.Unmarshal(snap.Inventory, &inv); err != nil {
			log.Printf("server: re-evaluation: stored inventory of cluster %d (snapshot %d) is corrupt: %v", c.ID, snap.ID, err)
			continue
		}
		if err := s.reevaluate(ctx, c, snap.ID, inv); err != nil {
			log.Printf("server: re-evaluation of cluster %d: %v", c.ID, err)
		}
	}
}

// runReevaluation is the background ticker: one pass at startup (a new KB
// or config takes effect without waiting for pushes), then every
// reevaluateInterval, until ctx ends.
func (s *Server) runReevaluation(ctx context.Context) {
	tick := time.NewTicker(s.reevaluateInterval)
	defer tick.Stop()
	for {
		s.reevaluateAll(ctx)
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

// notificationOf decodes an outbox message's payload. A message queued by
// a server that predates the versioned payload holds one PascalCase event
// ({"Cluster", "Target", "Kind", "Title", "Detail"}); it becomes a
// one-change notification whose delivery id is derived from the message.
func notificationOf(m store.OutboxMessage) (notify.Notification, error) {
	var probe struct {
		SchemaVersion int `json:"schemaVersion"`
	}
	if err := json.Unmarshal(m.Payload, &probe); err != nil {
		return notify.Notification{}, err
	}
	if probe.SchemaVersion != 0 {
		var n notify.Notification
		err := json.Unmarshal(m.Payload, &n)
		return n, err
	}
	var ev struct{ Cluster, Target, Kind, Title, Detail string }
	if err := json.Unmarshal(m.Payload, &ev); err != nil {
		return notify.Notification{}, err
	}
	if ev.Kind == "" {
		return notify.Notification{}, errors.New("payload is neither a notification nor a legacy event")
	}
	return notify.Notification{
		SchemaVersion: notify.SchemaVersion,
		DeliveryID:    fmt.Sprintf("outbox-%d", m.ID),
		Type:          notify.TypeReadinessChanged,
		Timestamp:     m.CreatedAt.UTC(),
		Cluster:       notify.Cluster{ID: m.ClusterID, Name: ev.Cluster},
		Changes:       []notify.Change{{Kind: ev.Kind, Title: ev.Title, Detail: ev.Detail, Targets: []string{ev.Target}}},
	}, nil
}
