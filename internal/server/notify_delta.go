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
//   - Blocker findings added since prev (diff by title) → one new-blocker
//     event each, capped at maxBlockerEvents, then a single "and N more".
//   - Blocker count went >0 → 0 → one became-ready event.
//   - eol-approaching warnings added since prev (by title) → one event each.
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

	prevBlockers := titleSet(prev.Findings, isBlocker)
	prevEOL := titleSet(prev.Findings, isEOLApproaching)

	var events []notify.Event

	var newBlockers []engine.Finding
	currBlockerCount := 0
	for _, f := range curr.Findings {
		if !isBlocker(f) {
			continue
		}
		currBlockerCount++
		if !prevBlockers[f.Title] {
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

	for _, f := range curr.Findings {
		if isEOLApproaching(f) && !prevEOL[f.Title] {
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

func titleSet(fs []engine.Finding, keep func(engine.Finding) bool) map[string]bool {
	s := make(map[string]bool)
	for _, f := range fs {
		if keep(f) {
			s[f.Title] = true
		}
	}
	return s
}
