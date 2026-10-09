package server

import (
	"bytes"
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

// verdictOf reads the verdict back from a stored row (engine.StoredVerdict,
// which the stores use for history points too).
func verdictOf(e store.Evaluation) engine.Verdict {
	return engine.StoredVerdict(e.Ready, e.Blockers)
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
		Teams:       clusterTeams(inv, rep),
	}, rep, nil
}

// futureTolerance is how far ahead of this server's clock a stored
// evaluation may be dated before it counts as future-dated: replicas'
// clocks differ by a little, a stepped-back clock by much more.
const futureTolerance = 5 * time.Minute

// stale reports whether a stored evaluation must be recomputed now. A row
// evaluated "in the future" (the clock stepped backwards since) is stale
// too: its EOL math used a date that has not come yet. So is one written
// before evaluations stored their teams, which no scoped read token reads.
func (s *Server) stale(e store.Evaluation, now time.Time) bool {
	today := now.UTC().Truncate(24 * time.Hour)
	return e.KBVersion != s.cfg.KB.Version || e.TeamMapHash != s.teamMapHash || e.TeamsUnknown ||
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
// heads and its gaps, recording what it read (targetDelta.baselines) for
// the commit to check. Failures are logged and never fail the pass.
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
	d.baselines = map[string]int64{}
	prev, err := s.cfg.Store.LatestKnownEvaluation(ctx, cluster.ID, target)
	if err == nil {
		d.baselines[target] = prev.ID
	}
	if errors.Is(err, store.ErrNotFound) {
		// First decided evaluation of this target: after an upgrade, the
		// previous default target's is the baseline (upgradeBaseline).
		d.baselines[target] = 0
		prev, err = s.upgradeBaseline(ctx, cluster.ID, cur, d.baselines)
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

// unassessedIn reports which baseline findings rep could not have seen:
// those a capability rep did not assess hides (engine.HiddenBy of its
// gaps) that the pass that last saw them did assess
// (findingHead.SeenWithout), and the deprecated callers hold keeps
// (callsHold.until).
//
// A capability the finding was seen without cannot hide it: then the
// finding came from the others, and its absence from them is as much
// evidence as its presence was. That is what tells a capability that
// went away since the finding was seen (#189) from one a supported
// install never has, such as helm with rbac.helmSecrets=false, whose
// add-on findings are then resolved and announced again as usual.
func unassessedIn(rep engine.Report, hold callsHold) func(findingHead) bool {
	return func(h findingHead) bool {
		if !hold.until(h).IsZero() {
			return true
		}
		return slices.ContainsFunc(engine.HiddenBy(rep.NotAssessed, h.Category, h.key()), func(c inventory.Capability) bool {
			return !slices.Contains(h.SeenWithout, c)
		})
	}
}

// deprecatedCallsHold is how long after an apiserver starts a deprecated
// caller missing from its /metrics is held instead of resolved.
// apiserver_requested_deprecated_apis starts empty at every apiserver
// start, and a client is counted again only at its next request: a
// controller or operator at once (its watches reconnect), an hourly or
// nightly CronJob, a daily backup or report within the day. A day covers
// all of them, and costs a genuinely fixed caller a day's delay in being
// resolved, which a fix only shows after the restart anyway: the gauge
// never drops a series while its apiserver runs. A client that calls
// less often than daily is still resolved after a day, and announced
// again when it next calls (docs/operations.md, Apiserver restarts).
//
// The window is far longer than the agent's --force-sync-every (default
// 1h): on a quiet cluster the force-sync duplicates are the only pushes,
// and the first one scraped after the window end resolves the caller, so
// a caller is resolved at most --force-sync-every (plus one --interval)
// after the window ends.
const deprecatedCallsHold = 24 * time.Hour

// startTimeTolerance is how far after the scrape's collectedAt an
// apiserver start time is still plausible: the agent stamps collectedAt
// when it starts collecting, and reads /metrics after api-usage, within
// one tick (at most 5m), on a node whose clock may differ from the
// apiserver's by a little.
const startTimeTolerance = 10 * time.Minute

// earliestAPIServerStart is before any apiserver: Kubernetes was first
// published in June 2014.
var earliestAPIServerStart = time.Date(2014, time.January, 1, 0, 0, 0, 0, time.UTC)

// apiServerStart is inv's APIServerStartTime when it is plausible for the
// scrape: not before 2014, and not later than collectedAt plus
// startTimeTolerance. Anything else is a buggy or skewed clock, and is
// treated as absent: a start time in the future would otherwise hold
// callers until it, however far away.
func apiServerStart(inv inventory.Inventory) (time.Time, bool) {
	start := inv.APIServerStartTime
	if start.IsZero() || inv.CollectedAt.IsZero() || start.Before(earliestAPIServerStart) ||
		start.After(inv.CollectedAt.Add(startTimeTolerance)) {
		return time.Time{}, false
	}
	return start, true
}

// callsHold is what one pass's inventory says about holding deprecated
// callers it does not have (#204): when it was scraped, and, when its
// apiserver had been up for less than deprecatedCallsHold then, when that
// apiserver's window ends.
//
// A missing caller is held when the scrape is from an apiserver too young
// to have been asked yet. Within one apiserver the gauge never drops a
// series, so a baseline caller missing from such a scrape was counted by
// an earlier apiserver process: the apiserver restarted since the
// baseline's scrape (or the agent reached a younger HA replica, whose
// gauge is as empty). A scrape from an apiserver up for longer, or that
// did not report its start time (an agent that predates the field),
// holds nothing new, as before.
//
// A caller of an API that apiserver no longer serves is not held: the
// restart that emptied the gauge was an upgrade past the API's removal,
// and nothing can request the API again, so no later scrape would ever
// count it (gone).
//
// The hold's end is a recorded instant, compared only with scrape times,
// never with this server's clock or with when an evaluation was made:
// the carried caller keeps it (findingHead.HoldUntil), and every pass
// compares it with its own inventory's collectedAt. A duplicate push
// re-evaluates with the pushed inventory, whose collectedAt is that
// scrape's (an identical scrape bar the times), so the agent's hourly
// force-sync ends the hold on a quiet cluster; a background pass judges
// the stored snapshot, scraped inside the window, and keeps the hold
// without stamping anything that would stop the next push from ending it
// (holdChanged reads the recorded end, not EvaluatedAt). The bound is
// deprecatedCallsHold after the last apiserver start the agent scraped,
// plus the time to the agent's next push; an end recorded by a clock that
// was ahead is bounded by the later pushes too (until).
//
// A scrape without a collectedAt (every real agent sets it) has nothing to
// judge a hold by: it holds nothing, and ends the holds it meets.
type callsHold struct {
	scraped time.Time // the inventory's collectedAt
	young   time.Time // its apiserver's start + deprecatedCallsHold, when after scraped; else zero
	pushed  bool      // scraped is a push's own, not a stored snapshot's
	// gone reports a deprecated caller (a finding's key) of an API that
	// the scraped apiserver's version no longer serves; nil: none is known.
	gone func(key string) bool
}

// callsHoldOf is the hold for inv, a pass at serverVersion (the
// snapshot's): pushed when inv is a push's own, not a stored snapshot's.
func (s *Server) callsHoldOf(inv inventory.Inventory, serverVersion string, pushed bool) callsHold {
	h := callsHold{scraped: inv.CollectedAt, pushed: pushed}
	if start, ok := apiServerStart(inv); ok && inv.CollectedAt.Before(start.Add(deprecatedCallsHold)) {
		h.young = start.Add(deprecatedCallsHold)
	}
	if running, err := inventory.ParseVersion(serverVersion); err == nil {
		k := s.cfg.KB
		h.gone = func(key string) bool {
			removed, ok := engine.RemovalOfCall(k, key)
			return ok && removed.Compare(running) <= 0
		}
	}
	return h
}

// until is when the hold on f, a baseline finding missing from the pass,
// ends: the later of the end f carries and this scrape's own, or zero
// when f is not a deprecated caller, when the hold ended at or before
// this scrape, when the scrape has no time to judge it by, or when f's
// API is no longer served at the scraped version.
//
// In a push, the end f carries is dropped when it is more than
// deprecatedCallsHold plus startTimeTolerance after the push's scrape: no
// scrape at or before it could have recorded it. It came from a clock that
// was ahead, such as a single-node cluster booted with its clock years
// ahead, whose collectedAt and apiserver start agree and so pass
// apiServerStart; kept, it would hold the caller until that clock's future
// once the clock is corrected. Dropped, the hold is this scrape's own, so
// it ends at most that bound after the first corrected scrape.
//
// A background pass judges the stored snapshot, whose scrape is older than
// the pushes that may have recorded the end (duplicate pushes move it and
// store no snapshot): it keeps the end, and the next push drops it if it
// is out of reach. Dropping it there would stamp an older end back that
// the next push moves forward again, a write at every pass.
func (c callsHold) until(f findingHead) time.Time {
	if f.Category != engine.CatDeprecatedAPIInUse || c.scraped.IsZero() {
		return time.Time{}
	}
	if c.gone != nil && c.gone(f.Key) {
		return time.Time{}
	}
	end := f.HoldUntil
	if c.pushed && end.After(c.scraped.Add(deprecatedCallsHold+startTimeTolerance)) {
		end = time.Time{}
	}
	if c.young.After(end) {
		end = c.young
	}
	if !c.scraped.Before(end) {
		return time.Time{}
	}
	return end
}

// stamp records on each carried finding the end of its hold (until). An
// ended hold is cleared, of a finding still carried for a capability gap
// too, so holdChanged does not re-evaluate it again at every push.
func (c callsHold) stamp(carried []findingHead) []findingHead {
	for i := range carried {
		carried[i].HoldUntil = c.until(carried[i])
	}
	return carried
}

// holdMarker is in every stored report that carries a held caller: the
// carried head's field name, which a JSON string in the report cannot
// contain unescaped.
var holdMarker = []byte(`"holdUntil":`)

// holdChanged reports whether report, a stored evaluation's, carries a
// held caller whose hold this scrape ends or moves (callsHold.until), so
// the evaluation is re-evaluated though it is not stale. A report without
// the marker is not decoded.
func holdChanged(report []byte, hold callsHold) bool {
	if !bytes.Contains(report, holdMarker) {
		return false
	}
	heads, err := storedFindingHeads(report)
	if err != nil {
		return false
	}
	return slices.ContainsFunc(heads.CarriedForward, func(f findingHead) bool {
		return !f.HoldUntil.IsZero() && !hold.until(f).Equal(f.HoldUntil)
	})
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
//
// token is the per-cluster ingest token the push was authenticated with
// ("" for the shared one): the commit re-checks it, and that cluster.ID is
// still the cluster under its name, in its transaction
// (store.ErrTokenRevoked, store.ErrClusterChanged), so a revoke, rename or
// delete that lands while the push is processed is never undone by it.
//
// With notification sinks, the commit also checks that the cluster's
// latest snapshot and every notification baseline the deltas were
// computed from are still what the pass read (store.Expectation): another
// replica's push, or this server's background pass, may land in between.
// Then nothing is written (store.ErrConflict) and the push is ingested
// again against what is there now, up to maxIngestAttempts times, so no
// became-ready or new-blocker is lost or sent twice. The last conflict is
// returned, and the agent retries the push.
func (s *Server) ingestSnapshot(ctx context.Context, cluster store.Cluster, snap store.Snapshot, inv inventory.Inventory, token string) (int64, bool, error) {
	for attempt := 1; ; attempt++ {
		id, dup, err := s.ingestOnce(ctx, cluster, snap, inv, token)
		if !errors.Is(err, store.ErrConflict) || attempt == maxIngestAttempts {
			return id, dup, err
		}
		log.Printf("server: push of cluster %q raced another writer (attempt %d), evaluating it again: %v", cluster.Name, attempt, err)
		if cluster.ID == 0 { // registered by the push that raced this one
			c, err := s.cfg.Store.ClusterByName(ctx, cluster.Name)
			if err != nil && !errors.Is(err, store.ErrNotFound) {
				return 0, false, fmt.Errorf("loading cluster %q: %w", cluster.Name, err)
			}
			cluster.ID = c.ID
		}
	}
}

// maxIngestAttempts bounds how often one push is evaluated again after
// its commit met a replaced baseline (ingestSnapshot). Each attempt
// re-reads what the last one met, so a second conflict needs a third
// writer inside the same window.
const maxIngestAttempts = 3

// ingestOnce is one attempt of ingestSnapshot.
func (s *Server) ingestOnce(ctx context.Context, cluster store.Cluster, snap store.Snapshot, inv inventory.Inventory, token string) (int64, bool, error) {
	var latestID int64 // the latest snapshot this attempt read; 0 for none
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
		if err == nil {
			latestID = latest.ID
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
			// Not a duplicate after all when another push moved the
			// cluster on meanwhile: then nothing is written, and the push
			// is evaluated in full (ingestSnapshot).
			snapID, dup, err := s.cfg.Store.CommitEvaluations(ctx, store.EvaluationBatch{Cluster: &cluster, Snapshot: &snap, IngestToken: token,
				Expect: &store.Expectation{LatestSnapshotID: latestID}})
			if err != nil {
				return 0, false, err
			}
			_, err = s.reevaluate(ctx, cluster, snapID, snap.ServerVersion, inv, true)
			return snapID, dup, err
		}
	}

	// Server-side team override (spec: labels + server override) — rewrite
	// namespace→team attribution before evaluation; stored reports carry the
	// mapped teams. The stored snapshot keeps the original labels.
	evalInv := inv
	evalInv.Namespaces = s.cfg.TeamMap.Apply(inv.Namespaces)
	now := s.now()
	hold := s.callsHoldOf(inv, snap.ServerVersion, true)
	batch := store.EvaluationBatch{Cluster: &cluster, Snapshot: &snap, IngestToken: token}
	if len(s.sinks) > 0 {
		batch.Expect = &store.Expectation{LatestSnapshotID: latestID, Baselines: map[string]int64{}}
	}
	var deltas changeMerger // each target's changes, merged as they come
	for _, target := range s.evalTargets(snap.ServerVersion) {
		e, rep, err := s.evaluation(cluster, evalInv, target, now)
		if err != nil {
			return 0, false, err
		}
		d := s.deltaFor(ctx, cluster, e, rep, baseline{}, unassessedIn(rep, hold))
		d.carried = s.keepCarried(&e, hold.stamp(d.carried))
		batch.Insert = append(batch.Insert, e)
		expectBaselines(batch.Expect, d)
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
		_, err = s.reevaluate(ctx, cluster, snapID, snap.ServerVersion, inv, true)
		return snapID, true, err
	}
	if len(batch.Outbox) > 0 {
		s.kickOutbox()
	}
	return snapID, false, nil
}

// reevaluate brings a stored snapshot's evaluations up to date: targets
// with no evaluation, or a stale one, or one whose restart hold inv's
// scrape ends or moves (holdChanged), are recomputed; unchanged results
// are refreshed in place, changed ones inserted with their notifications.
// inv is the snapshot's inventory: on a duplicate push (pushed) the pushed
// one, whose collectedAt is the new scrape's. A concurrent writer that got
// there first (store.ErrConflict) has done the same work, so the pass is
// dropped. left is true when a target's report would be over the limit,
// so what is stored stays, outdated (unrefreshable).
func (s *Server) reevaluate(ctx context.Context, cluster store.Cluster, snapID int64, serverVersion string, inv inventory.Inventory, pushed bool) (left bool, err error) {
	evalInv := inv
	evalInv.Namespaces = s.cfg.TeamMap.Apply(inv.Namespaces)
	now := s.now()
	hold := s.callsHoldOf(inv, serverVersion, pushed)
	batch := store.EvaluationBatch{ClusterID: cluster.ID, SnapshotID: snapID, Current: map[string]int64{}}
	var deltas changeMerger // each target's changes, merged as they come
	for _, target := range s.evalTargets(serverVersion) {
		cur, err := s.cfg.Store.CurrentEvaluation(ctx, cluster.ID, target.String())
		found := err == nil
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			return false, fmt.Errorf("loading current evaluation (cluster %d, target %s): %w", cluster.ID, target, err)
		}
		if found && cur.SnapshotID != snapID {
			// A newer snapshot arrived meanwhile; its own ingest evaluated it.
			log.Printf("server: re-evaluation of cluster %d skipped: snapshot %d is no longer the latest", cluster.ID, snapID)
			return false, nil
		}
		if found && !s.stale(cur, now) && !holdChanged(cur.Report, hold) {
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
			// push, or the first pass of the next UTC day, tries again.
			log.Printf("server: re-evaluation of cluster %d skipped for target %s: %v", cluster.ID, target, err)
			left = true
			continue
		}
		if err != nil {
			return false, err
		}
		// A decided current evaluation is the target's latest decided
		// one, the baseline deltaFor would load again: evaluations are
		// only added to the latest snapshot, so none is newer. An
		// unchanged result (below) has no changes, but may carry less.
		// Without sinks nothing is carried, and what the row carried is
		// not compared: no notification reads that baseline.
		known := baseline{findings: stored.baseline(), ok: decoded && verdictOf(cur) != engine.VerdictUnknown}
		d := s.deltaFor(ctx, cluster, e, rep, known, unassessedIn(rep, hold))
		d.carried = s.keepCarried(&e, hold.stamp(d.carried))
		batch.Current[target.String()] = cur.ID // 0 when not found
		if len(d.baselines) > 0 {
			// An upgrade baseline is another target's, which Current does
			// not cover.
			if batch.Expect == nil {
				batch.Expect = &store.Expectation{Baselines: map[string]int64{}}
			}
			expectBaselines(batch.Expect, d)
		}
		if decoded && sameResult(cur, stored.Findings, rep) && (len(s.sinks) == 0 || sameHeads(stored.CarriedForward, d.carried)) {
			e.ID = cur.ID
			batch.Refresh = append(batch.Refresh, e)
			continue
		}
		batch.Insert = append(batch.Insert, e)
		deltas.add(d)
	}
	if len(batch.Current) == 0 {
		return left, nil
	}
	batch.Outbox = s.outboxFor(cluster, &deltas, now)
	_, _, err = s.cfg.Store.CommitEvaluations(ctx, batch)
	if errors.Is(err, store.ErrConflict) {
		log.Printf("server: re-evaluation of cluster %d skipped: %v", cluster.ID, err)
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if len(batch.Outbox) > 0 {
		s.kickOutbox()
	}
	return left, nil
}

// expectBaselines adds the baselines d's delta was computed from to the
// commit's expectation (nil: none is checked).
func expectBaselines(x *store.Expectation, d targetDelta) {
	if x == nil {
		return
	}
	for target, id := range d.baselines {
		x.Baselines[target] = id
	}
}

// reevaluateAll runs reevaluate over every cluster's latest snapshot. It
// reads the snapshot heads in one query and each target's evaluation
// summary first (needsPass), and loads a cluster's inventory only when
// something is stale, missing or held: most passes find nothing, and an
// inventory is up to --max-snapshot-bytes to load and decode. A cluster
// the pass cannot bring up to date is marked (unrefreshable) and skipped
// until its snapshot or the UTC day changes. One cluster's failure is
// logged and never stops the others.
func (s *Server) reevaluateAll(ctx context.Context) {
	clusters, err := s.cfg.Store.ListClusters(ctx)
	if err != nil {
		log.Printf("server: re-evaluation: listing clusters: %v", err)
		return
	}
	heads, err := s.cfg.Store.LatestSnapshotHeads(ctx)
	if err != nil {
		log.Printf("server: re-evaluation: latest snapshots: %v", err)
		return
	}
	now := s.now()
	listed := make(map[int64]bool, len(clusters))
	for _, c := range clusters {
		listed[c.ID] = true
	}
	s.unrefreshable.keepOnly(listed)
	for _, c := range clusters {
		if ctx.Err() != nil {
			return
		}
		head, ok := heads[c.ID]
		if !ok || s.unrefreshable.has(c.ID, head.ID, now) {
			continue
		}
		version, err := s.versionOf(ctx, head)
		if err != nil {
			log.Printf("server: re-evaluation: latest snapshot of cluster %d: %v", c.ID, err)
			continue
		}
		if !s.needsPass(ctx, head, version, now) {
			continue
		}
		snap, inv, err := s.latestInventory(ctx, c.ID)
		if errors.Is(err, store.ErrNotFound) {
			continue
		}
		if err != nil {
			log.Printf("server: re-evaluation: latest snapshot of cluster %d: %v", c.ID, err)
			if errors.Is(err, errCorruptInventory) {
				s.unrefreshable.mark(c.ID, head.ID, now)
			}
			continue
		}
		left, err := s.reevaluate(ctx, c, snap.ID, judgedAt(snap, inv), inv, false)
		if err != nil {
			log.Printf("server: re-evaluation of cluster %d: %v", c.ID, err)
		}
		if err != nil || left {
			s.unrefreshable.mark(c.ID, snap.ID, now)
		}
	}
}

// runReevaluation is the background pass: one at startup (a new KB or
// config takes effect without waiting for pushes), then every
// reevaluateInterval and at each UTC midnight (nextPassIn), until ctx
// ends. A read that served an outdated verdict (kickReevaluation) starts
// the next pass early, but no sooner than reevaluateCooldown after the
// last one started. A kick made before or during a pass is that pass's:
// it brings the row the read served up to date, or marks it
// unrefreshable, so it starts no second one.
func (s *Server) runReevaluation(ctx context.Context) {
	drainKick := func() {
		select {
		case <-s.reevaluateKick:
		default:
		}
	}
	for {
		started := time.Now()
		drainKick()
		s.reevaluateAll(ctx)
		drainKick()
		timer := time.NewTimer(nextPassIn(s.now(), s.reevaluateInterval))
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		case <-s.reevaluateKick:
			if early := s.reevaluateCooldown - time.Since(started); early > 0 {
				cooldown := time.NewTimer(early)
				select {
				case <-ctx.Done():
					cooldown.Stop()
					timer.Stop()
					return
				case <-timer.C: // the scheduled pass came first
				case <-cooldown.C:
				}
				cooldown.Stop()
			}
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
// KB or team map), and if so starts the next pass early, unless a pass
// today could not refresh its cluster's snapshot (unrefreshable). The read
// still serves it — recomputing on a GET would let read traffic drive
// writes and notifications — but says so.
func (s *Server) outdated(e store.Evaluation, now time.Time) bool {
	if !s.stale(e, now) {
		return false
	}
	if !s.unrefreshable.has(e.ClusterID, e.SnapshotID, now) {
		s.kickReevaluation()
	}
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
