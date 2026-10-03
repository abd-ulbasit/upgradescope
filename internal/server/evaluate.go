package server

import (
	"cmp"
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
// and is re-evaluated — on a duplicate push and by the background pass
// (hourly and at UTC midnight) — when its KB version or team-map hash
// differs from the server's, when it was evaluated before the current UTC
// day (EOL math is day-granular), or when a configured target has none.
// Until then, reads serve the stored row marked outdated.
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
// default (next minor above serverVersion, the version the snapshot is
// judged at; skipped when unparseable) plus every configured extra
// target, deduped. Targets at or below the cluster's current minor are
// not applicable and are skipped.
func (s *Server) evalTargets(serverVersion string) []inventory.Version {
	server, err := inventory.ParseVersion(serverVersion)
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

// notApplicable reports whether target is at or below serverVersion: the
// cluster already runs it.
func notApplicable(serverVersion string, target inventory.Version) bool {
	server, err := inventory.ParseVersion(serverVersion)
	return err == nil && target.Compare(server) <= 0
}

// judgedVersion is the server version a stored snapshot is judged at
// (store.Snapshot.ServerVersion): the column, or for a row stored before
// it existed, the inventory's own serverVersion — decoding only that.
func judgedVersion(snap store.Snapshot) string {
	if snap.ServerVersion != "" {
		return snap.ServerVersion
	}
	var head struct {
		ServerVersion string `json:"serverVersion"`
	}
	_ = json.Unmarshal(snap.Inventory, &head) // corrupt: no version, as for a degraded push
	return head.ServerVersion
}

// judgedAt is judgedVersion for a snapshot whose inventory is decoded.
func judgedAt(snap store.Snapshot, inv inventory.Inventory) string {
	return cmp.Or(snap.ServerVersion, inv.ServerVersion)
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

// maxReportBytes caps a report the server evaluates, stores or serves:
// --max-snapshot-bytes, the most a push may take. No genuine inventory's
// report comes near it (reports name only what is flagged); the engine
// repeats what an inventory names, so without it a 5 MB push could build
// 41 MB reports.
func (s *Server) maxReportBytes() int64 { return s.maxSnapshotBytes() }

// reportTooLargeError is an evaluation whose report would be over
// maxReportBytes.
type reportTooLargeError struct {
	target inventory.Version
	limit  int64
}

func (e *reportTooLargeError) Error() string {
	return fmt.Sprintf("the report for target %s would be over the %s limit for a report (--max-snapshot-bytes)", e.target, sizeString(e.limit))
}

// evaluateWithin runs the engine for one target within maxReportBytes, or
// returns a *reportTooLargeError.
func (s *Server) evaluateWithin(inv inventory.Inventory, target inventory.Version, now time.Time) (engine.Report, error) {
	limit := s.maxReportBytes()
	rep, err := engine.EvaluateWithin(inv, s.cfg.KB, target, now, int(limit))
	if errors.Is(err, engine.ErrReportTooLarge) {
		return engine.Report{}, &reportTooLargeError{target: target, limit: limit}
	}
	return rep, err
}

// evaluation runs the engine for one target and builds the row to store:
// a report of at most maxReportBytes, encoded, or a *reportTooLargeError.
func (s *Server) evaluation(cluster store.Cluster, inv inventory.Inventory, target inventory.Version, now time.Time) (store.Evaluation, engine.Report, error) {
	rep, err := s.evaluateWithin(inv, target, now)
	if err != nil {
		return store.Evaluation{}, engine.Report{}, err
	}
	repJSON, err := marshalJSON(rep)
	if err != nil {
		return store.Evaluation{}, engine.Report{}, fmt.Errorf("marshaling report (cluster %d, target %s): %w", cluster.ID, target, err)
	}
	// EvaluateWithin charges about what each finding takes; the stored
	// bytes are what is capped.
	if limit := s.maxReportBytes(); int64(len(repJSON)) > limit {
		return store.Evaluation{}, engine.Report{}, &reportTooLargeError{target: target, limit: limit}
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
// set of (severity, finding key). stored is the row's findings
// (the Findings of its storedFindingHeads).
func sameResult(row store.Evaluation, stored []findingHead, rep engine.Report) bool {
	if verdictOf(row) != rep.Verdict || row.Score != rep.Score || len(stored) != len(rep.Findings) {
		return false
	}
	sig := make([]string, 0, len(rep.Findings))
	for _, f := range rep.Findings {
		sig = append(sig, findingSignature(headOf(f)))
	}
	prev := make([]string, 0, len(stored))
	for _, h := range stored {
		prev = append(prev, findingSignature(h))
	}
	slices.Sort(sig)
	slices.Sort(prev)
	return slices.Equal(prev, sig)
}

func findingSignature(h findingHead) string { return string(h.Severity) + "\x00" + h.key() }

// baseline is a notification baseline the caller already holds: the
// heads of the target's latest decided evaluation (storedHeads.baseline),
// when known.
type baseline struct {
	findings []findingHead
	ok       bool
}

// deltaFor computes the notification delta of one new evaluation. The
// baseline is the last evaluation with a decided verdict, so an "unknown"
// pass (a collector failure) is neither a transition nor a reset; a pass
// whose own verdict is unknown notifies nothing — what it could not see is
// not news. A decided pass that did not assess a capability carries the
// baseline's findings from it forward, when the baseline saw them with it
// (computeDelta, unassessed). known is that baseline when the caller
// holds it; otherwise deltaFor loads it, and decodes only its findings'
// heads and its gaps. Failures are logged and never fail the pass.
func (s *Server) deltaFor(ctx context.Context, cluster store.Cluster, e store.Evaluation, cur engine.Report, known baseline, unassessed func(findingHead) bool) targetDelta {
	d := targetDelta{target: notify.Target{Target: cur.Target.String(), Verdict: string(cur.Verdict), Score: cur.Score, Blockers: e.Blockers}}
	if len(s.sinks) == 0 || cur.Verdict == engine.VerdictUnknown || cluster.ID == 0 {
		return d // a cluster's first push has no baseline either
	}
	if known.ok {
		d.changes, d.carried = computeDelta(known.findings, cur, unassessed)
		return d
	}
	target := cur.Target.String()
	prev, err := s.cfg.Store.LatestKnownEvaluation(ctx, cluster.ID, target)
	if errors.Is(err, store.ErrNotFound) {
		// First decided evaluation of this target: after an upgrade, the
		// previous default target's is the baseline (upgradeBaseline).
		prev, err = s.upgradeBaseline(ctx, cluster.ID, cur)
	}
	if errors.Is(err, store.ErrNotFound) {
		return d // first decided evaluation of this target: no delta
	}
	if err != nil {
		log.Printf("server: loading notification baseline (cluster %d, target %s): %v", cluster.ID, target, err)
		return d
	}
	heads, err := storedFindingHeads(prev.Report)
	if err != nil {
		log.Printf("server: decoding previous report (cluster %d, target %s): %v", cluster.ID, target, err)
		return d
	}
	d.changes, d.carried = computeDelta(heads.baseline(), cur, unassessed)
	return d
}

// unassessedIn reports which baseline findings rep, evaluated from inv,
// could not have seen: those a capability rep did not assess hides
// (engine.HiddenBy of its gaps) that the pass that last saw them did
// assess (findingHead.SeenWithout), and deprecated callers while inv's
// apiserver was warming up (callsWarmingUp).
//
// A capability the finding was seen without cannot hide it: then the
// finding came from the others, and its absence from them is as much
// evidence as its presence was. That is what tells a capability that
// went away since the finding was seen (#189) from one a supported
// install never has, such as helm with rbac.helmSecrets=false, whose
// add-on findings are then resolved and announced again as usual.
func unassessedIn(rep engine.Report, inv inventory.Inventory) func(findingHead) bool {
	warming := callsWarmingUp(inv)
	return func(h findingHead) bool {
		if warming && h.Category == engine.CatDeprecatedAPIInUse {
			return true
		}
		return slices.ContainsFunc(engine.HiddenBy(rep.NotAssessed, h.Category, h.key()), func(c inventory.Capability) bool {
			return !slices.Contains(h.SeenWithout, c)
		})
	}
}

// deprecatedCallsWarmup is how long an apiserver must have been up when
// its /metrics were scraped before a deprecated caller missing from the
// scrape counts as gone. apiserver_requested_deprecated_apis starts empty
// at every apiserver start, and a client is counted again at its next
// request: within seconds for one that holds a watch (it reconnects), at
// its schedule for one that calls periodically. A day covers the common
// schedules (an hourly sync, a nightly CronJob, a daily CI deploy) and
// fits a readiness signal, which is not urgent; its cost is that after an
// apiserver restart a caller that really went away is resolved, and the
// cluster announced ready, up to a day late. A client that calls less
// often than daily is the documented limit (docs/operations.md,
// Notifications).
const deprecatedCallsWarmup = 24 * time.Hour

// callsWarmingUp reports whether inv's deprecated calls were scraped from
// an apiserver up for less than deprecatedCallsWarmup. It reads the
// current scrape alone, not whether the apiserver restarted since the
// baseline's: deliberately broader, since the metric only ever starts
// empty at a start, so a scrape this young misses what the apiserver
// has not been asked yet, restart or new cluster. An inventory
// without the apiserver's start time (an older agent's, a scrape that did
// not report it) or a collection time is judged as before it existed:
// not warming.
func callsWarmingUp(inv inventory.Inventory) bool {
	start := inv.APIServerStartTime
	return !start.IsZero() && !inv.CollectedAt.IsZero() && inv.CollectedAt.Before(start.Add(deprecatedCallsWarmup))
}

// warmupEnded reports whether e, an evaluation of inv's snapshot, was
// made before inv's apiserver had been up for deprecatedCallsWarmup,
// while inv was scraped after: a force-sync push, a duplicate of the
// snapshot that is the only news a quiet cluster sends. e may hold
// deprecated callers that inv resolves, so it is re-evaluated.
func warmupEnded(e store.Evaluation, inv inventory.Inventory) bool {
	start := inv.APIServerStartTime
	if start.IsZero() || inv.CollectedAt.IsZero() {
		return false
	}
	end := start.Add(deprecatedCallsWarmup)
	return e.EvaluatedAt.Before(end) && !inv.CollectedAt.Before(end)
}

// keepCarried stores what e's baseline carries forward in its report
// (withCarried) and returns it. Over maxReportBytes, or should the report
// not take it, it stores none and returns nil: the evaluation of a
// report that fits is never refused for the server's own bookkeeping,
// and its baseline is then its findings alone, as before carry-forward
// existed (a carried blocker may be resolved, or announced again).
func (s *Server) keepCarried(e *store.Evaluation, carried []findingHead) []findingHead {
	if len(carried) == 0 {
		return nil
	}
	b, err := withCarried(e.Report, carried)
	if err == nil && int64(len(b)) > s.maxReportBytes() {
		err = fmt.Errorf("the report would be over the %s limit for a report", sizeString(s.maxReportBytes()))
	}
	if err != nil {
		log.Printf("server: not carrying %d finding(s) forward (cluster %d, target %s): %v", len(carried), e.ClusterID, e.Target, err)
		return nil
	}
	e.Report = b
	return carried
}

// outboxFor turns one pass's merged deltas into one notification for the
// cluster, queued once per sink with the same delivery id.
func (s *Server) outboxFor(cluster store.Cluster, deltas *changeMerger, now time.Time) []store.OutboxMessage {
	n, ok := deltas.notification(cluster, now, newDeliveryID())
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
//
// snap.ServerVersion arrives as the inventory's own version. A degraded
// push that reported none (its versions collector failed) is judged at
// the previous snapshot's instead, so the cluster keeps its default
// target and its cells: they carry the engine's verdict on what the push
// did report — unknown at best, since versions is a required capability.
func (s *Server) ingestSnapshot(ctx context.Context, cluster store.Cluster, snap store.Snapshot, inv inventory.Inventory) (int64, bool, error) {
	// A duplicate (the agent's hourly force-sync) is the common push: go
	// straight to re-evaluating what is stale, instead of evaluating every
	// target for the commit to discard. The commit still checks the hash,
	// for a push racing this one.
	if cluster.ID != 0 {
		// The head only: its inventory is up to --max-snapshot-bytes, and
		// only a row stored before the server_version column needs it.
		latest, err := s.cfg.Store.LatestSnapshotHead(ctx, cluster.ID)
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			return 0, false, fmt.Errorf("loading latest snapshot (cluster %d): %w", cluster.ID, err)
		}
		if err == nil && snap.ServerVersion == "" {
			if latest.ServerVersion == "" {
				if latest, err = s.cfg.Store.LatestSnapshot(ctx, cluster.ID); err != nil {
					return 0, false, fmt.Errorf("loading latest snapshot (cluster %d): %w", cluster.ID, err)
				}
			}
			snap.ServerVersion = judgedVersion(latest)
		}
		if err == nil && latest.Hash == snap.Hash {
			snapID, dup, err := s.cfg.Store.CommitEvaluations(ctx, store.EvaluationBatch{Cluster: &cluster, Snapshot: &snap})
			if err != nil {
				return 0, false, err
			}
			// Not a duplicate after all (another push moved the cluster on
			// meanwhile): the snapshot is stored without evaluations, and
			// reevaluate fills in every target.
			return snapID, dup, s.reevaluate(ctx, cluster, snapID, snap.ServerVersion, inv)
		}
	}

	// Server-side team override (spec: labels + server override) — rewrite
	// namespace→team attribution before evaluation; stored reports carry the
	// mapped teams. The stored snapshot keeps the original labels.
	evalInv := inv
	evalInv.Namespaces = s.cfg.TeamMap.Apply(inv.Namespaces)
	now := s.now()
	batch := store.EvaluationBatch{Cluster: &cluster, Snapshot: &snap}
	var deltas changeMerger // each target's changes, merged as they come
	for _, target := range s.evalTargets(snap.ServerVersion) {
		e, rep, err := s.evaluation(cluster, evalInv, target, now)
		if err != nil {
			return 0, false, err
		}
		d := s.deltaFor(ctx, cluster, e, rep, baseline{}, unassessedIn(rep, evalInv))
		d.carried = s.keepCarried(&e, d.carried)
		batch.Insert = append(batch.Insert, e)
		deltas.add(d)
	}
	batch.Outbox = s.outboxFor(cluster, &deltas, now)
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
		return snapID, true, s.reevaluate(ctx, cluster, snapID, snap.ServerVersion, inv)
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
func (s *Server) reevaluate(ctx context.Context, cluster store.Cluster, snapID int64, serverVersion string, inv inventory.Inventory) error {
	evalInv := inv
	evalInv.Namespaces = s.cfg.TeamMap.Apply(inv.Namespaces)
	now := s.now()
	batch := store.EvaluationBatch{ClusterID: cluster.ID, SnapshotID: snapID, Current: map[string]int64{}}
	var deltas changeMerger // each target's changes, merged as they come
	for _, target := range s.evalTargets(serverVersion) {
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
		if found && !s.stale(cur, now) && !warmupEnded(cur, evalInv) {
			continue
		}
		// What sameResult and deltaFor read of the stored report: its
		// findings' heads, decoded before the new report is built, so the
		// stored bytes are not held while it is.
		var stored storedHeads
		decoded := false
		if found {
			stored, err = storedFindingHeads(cur.Report)
			decoded = err == nil
			cur.Report = nil
		}
		e, rep, err := s.evaluation(cluster, evalInv, target, now)
		var tooLarge *reportTooLargeError
		if errors.As(err, &tooLarge) {
			// A snapshot stored before the limit, or a newer knowledge base
			// that flags more of it: what is stored stays, and the next
			// pass tries again.
			log.Printf("server: re-evaluation of cluster %d skipped for target %s: %v", cluster.ID, target, err)
			continue
		}
		if err != nil {
			return err
		}
		// A decided current evaluation is the target's latest decided
		// one, the baseline deltaFor would load again: evaluations are
		// only added to the latest snapshot, so none is newer. An
		// unchanged result (below) has no changes, but may carry less.
		known := baseline{findings: stored.baseline(), ok: decoded && verdictOf(cur) != engine.VerdictUnknown}
		d := s.deltaFor(ctx, cluster, e, rep, known, unassessedIn(rep, evalInv))
		d.carried = s.keepCarried(&e, d.carried)
		batch.Current[target.String()] = cur.ID // 0 when not found
		if decoded && sameResult(cur, stored.Findings, rep) && sameHeads(stored.CarriedForward, d.carried) {
			e.ID = cur.ID
			batch.Refresh = append(batch.Refresh, e)
			continue
		}
		batch.Insert = append(batch.Insert, e)
		deltas.add(d)
	}
	if len(batch.Current) == 0 {
		return nil
	}
	batch.Outbox = s.outboxFor(cluster, &deltas, now)
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
		snap, inv, err := s.latestInventory(ctx, c.ID)
		if errors.Is(err, store.ErrNotFound) {
			continue
		}
		if err != nil {
			log.Printf("server: re-evaluation: latest snapshot of cluster %d: %v", c.ID, err)
			continue
		}
		if err := s.reevaluate(ctx, c, snap.ID, judgedAt(snap, inv), inv); err != nil {
			log.Printf("server: re-evaluation of cluster %d: %v", c.ID, err)
		}
	}
}

// runReevaluation is the background pass: one at startup (a new KB or
// config takes effect without waiting for pushes), then every
// reevaluateInterval and at each UTC midnight (nextPassIn), until ctx
// ends. A read that served an outdated verdict (kickReevaluation) starts
// the next pass early.
func (s *Server) runReevaluation(ctx context.Context) {
	for {
		s.reevaluateAll(ctx)
		timer := time.NewTimer(nextPassIn(s.now(), s.reevaluateInterval))
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		case <-s.reevaluateKick:
			timer.Stop()
		}
	}
}

// nextPassIn is how long after now the next background pass runs: after
// interval, or just past the next UTC midnight when that comes first. EOL
// math is day-granular, so midnight is when stored verdicts go out of
// date with no push.
func nextPassIn(now time.Time, interval time.Duration) time.Duration {
	midnight := now.UTC().Truncate(24 * time.Hour).Add(24 * time.Hour)
	return min(interval, midnight.Sub(now)+time.Second)
}

// kickReevaluation starts the next background pass now, unless one is
// already queued. Reads call it when they serve an outdated verdict.
func (s *Server) kickReevaluation() {
	select {
	case s.reevaluateKick <- struct{}{}:
	default:
	}
}

// outdated reports whether a stored evaluation served by a read is out of
// date (stale: evaluated before today UTC, in the future, or under another
// KB or team map), and if so starts the next pass early. The read still
// serves it — recomputing on a GET would let read traffic drive writes and
// notifications — but says so.
func (s *Server) outdated(e store.Evaluation, now time.Time) bool {
	if !s.stale(e, now) {
		return false
	}
	s.kickReevaluation()
	return true
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
