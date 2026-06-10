package server

import (
	"fmt"

	"github.com/abd-ulbasit/upgradescope/internal/engine"
	"github.com/abd-ulbasit/upgradescope/internal/server/notify"
)

// maxBlockerEvents caps per-evaluation new-blocker noise; the overflow is
// summarized as one "and N more new blockers" event.
const maxBlockerEvents = 5

// ComputeDelta implements the notification delta rules (shared contract):
//
//   - prev == nil (first-ever evaluation of this cluster+target) → no events.
//   - Blocker findings added since prev (diff by stable finding key) → one
//     new-blocker event each, capped at maxBlockerEvents, then a single
//     "and N more".
//   - Blocker count went >0 → 0 → one became-ready event.
//   - eol-approaching warnings added since prev (by key) → one event each.
//
// Identity is Finding.Key — deliberately count-free, so a title-only change
// ("3 objects" → "2 objects") never re-alerts the same blocker. Findings
// with an empty Key (reports stored before Key existed) fall back to Title.
// Duplicate keys within one report emit one event (first occurrence wins).
//
// Event.Cluster is filled with curr.ClusterID (the inventory UID); the
// caller (notifyDelta) overwrites it with the human cluster name from the
// push envelope before delivery. Order is deterministic: new-blockers in
// report order, summary, became-ready, eol-approaching in report order.
func ComputeDelta(prev *engine.Report, curr engine.Report) []notify.Event {
	if prev == nil {
		return nil
	}
	cluster, target := curr.ClusterID, curr.Target.String()

	prevBlockers := keySet(prev.Findings, isBlocker)
	prevEOL := keySet(prev.Findings, isEOLApproaching)

	var events []notify.Event

	var newBlockers []engine.Finding
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
			newBlockers = append(newBlockers, f)
		}
	}
	for i, f := range newBlockers {
		if i == maxBlockerEvents {
			events = append(events, notify.Event{
				Cluster: cluster, Target: target, Kind: notify.KindNewBlocker,
				Title: fmt.Sprintf("and %d more new blockers", len(newBlockers)-maxBlockerEvents),
			})
			break
		}
		events = append(events, notify.Event{
			Cluster: cluster, Target: target, Kind: notify.KindNewBlocker,
			Title: f.Title, Detail: f.Detail,
		})
	}

	if len(prevBlockers) > 0 && currBlockerCount == 0 {
		events = append(events, notify.Event{
			Cluster: cluster, Target: target, Kind: notify.KindBecameReady,
			Title:  fmt.Sprintf("ready for %s: all blockers resolved", target),
			Detail: fmt.Sprintf("score %d", curr.Score),
		})
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
			events = append(events, notify.Event{
				Cluster: cluster, Target: target, Kind: notify.KindEOLApproaching,
				Title: f.Title, Detail: f.Detail,
			})
		}
	}
	return events
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
