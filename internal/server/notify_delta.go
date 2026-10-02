package server

import (
	"cmp"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"slices"
	"time"

	"github.com/abd-ulbasit/upgradescope/internal/engine"
	"github.com/abd-ulbasit/upgradescope/internal/inventory"
	"github.com/abd-ulbasit/upgradescope/internal/server/notify"
	"github.com/abd-ulbasit/upgradescope/internal/server/store"
)

// Per-notification caps on noisy kinds: the overflow is counted in
// Notification.Omitted instead of listed.
const (
	maxBlockerChanges = 5
	maxEOLChanges     = 5
)

// becameReadyTitle is the became-ready change's title, the same for every
// target so the change merges across them (each target's score is in
// Notification.Targets).
const becameReadyTitle = "ready: all blockers resolved"

// ComputeDelta implements the notification delta rules for one target:
//
//   - prev == nil (first-ever evaluation of this cluster+target) → nothing.
//   - Blocker findings added since prev (diff by stable finding key) → one
//     new-blocker change each.
//   - Blocker count went >0 → 0 AND curr.Verdict is ready → one became-ready
//     change. Zero blockers with verdict unknown (a required check was not
//     assessed, e.g. a transient collector failure) is not readiness.
//   - eol-approaching warnings added since prev (by key) → one change each.
//
// Identity is Finding.Key — deliberately count-free, so a title-only change
// ("3 objects" → "2 objects") never re-alerts the same blocker. Findings
// with an empty Key (reports stored before Key existed) fall back to Title.
// Duplicate keys within one report give one change (first occurrence wins).
//
// Every change lists curr's target; buildNotification merges and caps the
// changes of one pass across targets. Order is deterministic: new-blockers
// in report order, became-ready, eol-approaching in report order.
func ComputeDelta(prev *engine.Report, curr engine.Report) []notify.Change {
	if prev == nil {
		return nil
	}
	target := []string{curr.Target.String()}

	prevBlockers := keySet(prev.Findings, isBlocker)
	prevEOL := keySet(prev.Findings, isEOLApproaching)

	var changes []notify.Change
	currBlockerCount := 0
	seenBlockers := map[string]bool{}
	for _, f := range curr.Findings {
		if !isBlocker(f) {
			continue
		}
		currBlockerCount++
		k := findingKey(f)
		if seenBlockers[k] {
			continue
		}
		seenBlockers[k] = true
		if !prevBlockers[k] {
			changes = append(changes, change(notify.KindNewBlocker, f, target))
		}
	}

	if len(prevBlockers) > 0 && currBlockerCount == 0 && curr.Verdict == engine.VerdictReady {
		changes = append(changes, notify.Change{Kind: notify.KindBecameReady, Title: becameReadyTitle, Targets: target})
	}

	seenEOL := map[string]bool{}
	for _, f := range curr.Findings {
		if !isEOLApproaching(f) {
			continue
		}
		k := findingKey(f)
		if seenEOL[k] {
			continue
		}
		seenEOL[k] = true
		if !prevEOL[k] {
			changes = append(changes, change(notify.KindEOLApproaching, f, target))
		}
	}
	return changes
}

// upgradeLookback is how many minors below a new default target
// upgradeBaseline looks for the cluster's previous one: one for an
// upgrade the agent pushed on both sides of, more when it missed a push
// across several upgrades.
const upgradeLookback = 3

// upgradeBaseline is the notification baseline of a default target with
// no decided evaluation yet, because the cluster upgraded (1.35 → 1.36
// makes the default target 1.37): the latest decided evaluation of the
// nearest lower target within upgradeLookback minors, which was the
// default target before the upgrade. So a blocker the new target adds
// (an API removed in 1.37) is announced, and blockers the cluster had
// already been told about at the old target are not announced again.
//
// The other candidate, the previous snapshot evaluated at the new
// target, would hide exactly the blockers that are news: the cluster
// already used that API before the upgrade.
//
// Only the default target (the next minor above cur.ServerVersion) has
// one. An extra target's first evaluation stays the silent baseline: a
// target added to --targets is a new question, and announcing it would
// page every cluster in the fleet on the restart. ErrNotFound when there
// is no baseline.
func (s *Server) upgradeBaseline(ctx context.Context, clusterID int64, cur engine.Report) (store.Evaluation, error) {
	server, err := inventory.ParseVersion(cur.ServerVersion)
	if err != nil || server.Next() != cur.Target {
		return store.Evaluation{}, store.ErrNotFound
	}
	for minor := cur.Target.Minor - 1; minor >= max(cur.Target.Minor-upgradeLookback, 0); minor-- {
		prev, err := s.cfg.Store.LatestKnownEvaluation(ctx, clusterID, inventory.Version{Major: cur.Target.Major, Minor: minor}.String())
		if !errors.Is(err, store.ErrNotFound) {
			return prev, err
		}
	}
	return store.Evaluation{}, store.ErrNotFound
}

func change(kind string, f engine.Finding, targets []string) notify.Change {
	return notify.Change{Kind: kind, Key: f.Key, Severity: string(f.Severity), Title: f.Title, Detail: f.Detail, Targets: targets}
}

// targetDelta is one target's verdict after a pass and its changes.
type targetDelta struct {
	target  notify.Target
	changes []notify.Change
}

// kindRank orders a notification's changes: blockers first.
var kindRank = map[string]int{notify.KindNewBlocker: 0, notify.KindBecameReady: 1, notify.KindEOLApproaching: 2}

// kindCap is the per-notification cap of a kind (0 = none).
var kindCap = map[string]int{notify.KindNewBlocker: maxBlockerChanges, notify.KindEOLApproaching: maxEOLChanges}

// buildNotification groups one pass's deltas for one cluster into a single
// notification: a change found for several targets with the same wording
// (an EOL add-on is a blocker for each of them) becomes one change listing
// them all, then
// new-blocker and eol-approaching changes are capped, the rest counted in
// Omitted. ok is false when the pass changed nothing.
func buildNotification(cluster store.Cluster, deltas []targetDelta, now time.Time, deliveryID string) (n notify.Notification, ok bool) {
	n = notify.Notification{
		SchemaVersion: notify.SchemaVersion,
		DeliveryID:    deliveryID,
		Type:          notify.TypeReadinessChanged,
		Timestamp:     now.UTC(),
		Cluster:       notify.Cluster{ID: cluster.ID, Name: cluster.Name},
	}
	merged := map[string]int{} // identity → index in n.Changes
	for _, d := range deltas {
		if len(d.changes) == 0 {
			continue
		}
		n.Targets = append(n.Targets, d.target)
		for _, c := range d.changes {
			// Same finding, same wording: one change for all its targets.
			// A title or detail that names the target keeps a change per
			// target, so no target is described in another's words.
			id := c.Kind + "\x00" + c.Key + "\x00" + c.Title + "\x00" + c.Detail
			if i, seen := merged[id]; seen {
				n.Changes[i].Targets = append(n.Changes[i].Targets, c.Targets...)
				continue
			}
			merged[id] = len(n.Changes)
			c.Targets = slices.Clone(c.Targets)
			n.Changes = append(n.Changes, c)
		}
	}
	if len(n.Changes) == 0 {
		return notify.Notification{}, false
	}
	slices.SortStableFunc(n.Changes, func(a, b notify.Change) int { return cmp.Compare(kindRank[a.Kind], kindRank[b.Kind]) })
	kept := n.Changes[:0]
	count := map[string]int{}
	for _, c := range n.Changes {
		count[c.Kind]++
		if limit := kindCap[c.Kind]; limit > 0 && count[c.Kind] > limit {
			if n.Omitted == nil {
				n.Omitted = map[string]int{}
			}
			n.Omitted[c.Kind]++
			continue
		}
		kept = append(kept, c)
	}
	n.Changes = kept
	return n, true
}

// newDeliveryID returns a random 128-bit id, hex: the notification's
// identity across retries and sinks.
func newDeliveryID() string {
	var b [16]byte
	_, _ = rand.Read(b[:]) // crypto/rand.Read never fails (Go 1.24+)
	return hex.EncodeToString(b[:])
}

func isBlocker(f engine.Finding) bool { return f.Severity == engine.SevBlocker }

func isEOLApproaching(f engine.Finding) bool {
	return f.Category == engine.CatEOLApproaching && f.Severity == engine.SevWarning
}

// findingKey is the delta identity: the stable count-free Key when set,
// else Title — a defensive fallback for reports stored before Finding.Key
// existed (those deltas keep the old title-diff behavior).
func findingKey(f engine.Finding) string {
	if f.Key != "" {
		return f.Key
	}
	return f.Title
}

func keySet(fs []engine.Finding, keep func(engine.Finding) bool) map[string]bool {
	s := make(map[string]bool)
	for _, f := range fs {
		if keep(f) {
			s[findingKey(f)] = true
		}
	}
	return s
}
