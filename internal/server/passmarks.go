package server

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/abd-ulbasit/upgradescope/internal/server/store"
)

// reevaluateCooldown is the least time from the start of one background
// pass to a pass a read starts early (kickReevaluation): reads that serve
// outdated verdicts start at most one pass per cooldown, however many
// there are (#241). The hourly and UTC-midnight passes are not delayed by
// it.
const reevaluateCooldown = 5 * time.Minute

// unrefreshable remembers the clusters a background pass could not bring
// up to date, by the snapshot it judged and the UTC day: a report that
// would now be over --max-snapshot-bytes, a stored inventory that does not
// decode, an evaluation that failed. A store error does not mark: it is a
// blip, not something the same snapshot would meet again. Until the
// snapshot or the day changes
// (the KB and the team map are this process's), a pass would fail the
// same way, so passes skip the cluster without loading its inventory, and
// a read that serves its outdated rows says so without starting one. It
// is in memory: a restart tries each once more.
type unrefreshable struct {
	mu    sync.Mutex
	marks map[int64]unrefreshableMark // by cluster id
}

type unrefreshableMark struct {
	snapshotID int64
	day        time.Time
}

func utcDay(t time.Time) time.Time { return t.UTC().Truncate(24 * time.Hour) }

func (u *unrefreshable) mark(clusterID, snapshotID int64, now time.Time) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.marks == nil {
		u.marks = map[int64]unrefreshableMark{}
	}
	u.marks[clusterID] = unrefreshableMark{snapshotID: snapshotID, day: utcDay(now)}
}

// has reports whether the cluster's snapshotID was marked today; an older
// mark is forgotten.
func (u *unrefreshable) has(clusterID, snapshotID int64, now time.Time) bool {
	u.mu.Lock()
	defer u.mu.Unlock()
	m, ok := u.marks[clusterID]
	if !ok {
		return false
	}
	if m.snapshotID != snapshotID || !m.day.Equal(utcDay(now)) {
		delete(u.marks, clusterID)
		return false
	}
	return true
}

// keepOnly forgets the marks of clusters not in ids (deleted ones).
func (u *unrefreshable) keepOnly(ids map[int64]bool) {
	u.mu.Lock()
	defer u.mu.Unlock()
	for id := range u.marks {
		if !ids[id] {
			delete(u.marks, id)
		}
	}
}

// maxLegacyVersions bounds legacyVersions; past it the cache starts over.
const maxLegacyVersions = 4096

// legacyVersions caches the version a snapshot stored without one (its
// server_version column empty: a row from before migration 0006 that 0009
// could not backfill, or a cluster that never reported a version) is
// judged at, read once from its inventory (judgedVersion), so the fleet
// reads and the background pass do not load it again on every call. A
// snapshot's inventory never changes, so neither does the answer.
type legacyVersions struct {
	mu sync.Mutex
	m  map[int64]string // by snapshot id
}

// versionOf is the version head, a latest snapshot's head (no inventory),
// is judged at: its column, or, for a row without one, judgedVersion of the
// stored inventory, loaded once per snapshot.
func (s *Server) versionOf(ctx context.Context, head store.Snapshot) (string, error) {
	if head.ServerVersion != "" {
		return head.ServerVersion, nil
	}
	lv := &s.legacyVersions
	lv.mu.Lock()
	v, ok := lv.m[head.ID]
	lv.mu.Unlock()
	if ok {
		return v, nil
	}
	full, err := s.cfg.Store.LatestSnapshot(ctx, head.ClusterID)
	if errors.Is(err, store.ErrNotFound) {
		return "", nil // deleted meanwhile: no version, as before
	}
	if err != nil {
		return "", err
	}
	v = judgedVersion(full)
	lv.mu.Lock()
	if lv.m == nil || len(lv.m) >= maxLegacyVersions {
		lv.m = map[int64]string{}
	}
	lv.m[full.ID] = v
	lv.mu.Unlock()
	return v, nil
}

// needsPass reports whether the background pass must load the inventory
// of a cluster whose latest snapshot is head, judged at version: a target
// it evaluates has no current evaluation, or a stale one (stale), or one
// that carries a held deprecated caller whose hold the inventory may end
// (holdChanged reads both). It reads only evaluation summaries: no report,
// no inventory.
//
// The held case is a column (CarriesHold), not a decode of each report:
// whether the scrape the pass judges ends or moves a recorded hold is a
// question for the inventory (its collectedAt and apiserver start), so a
// held target has the pass load its cluster's inventory until the hold
// ends, and every other cluster loads and decodes nothing
// (TestPassLoadsTheInventoryOfAHeldTarget).
func (s *Server) needsPass(ctx context.Context, head store.Snapshot, version string, now time.Time) bool {
	for _, target := range s.evalTargets(version) {
		e, err := s.cfg.Store.CurrentEvaluationSummary(ctx, head.ClusterID, target.String())
		if err != nil || e.SnapshotID != head.ID || e.CarriesHold || s.stale(e, now) {
			return true // missing, moved on, or failing: reevaluate decides and logs
		}
	}
	return false
}
