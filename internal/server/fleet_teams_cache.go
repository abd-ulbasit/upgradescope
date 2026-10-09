package server

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/abd-ulbasit/upgradescope/internal/engine"
	"github.com/abd-ulbasit/upgradescope/internal/inventory"
)

// The fleet teams rollup (GET /api/v1/fleet/teams) for a target with no
// stored evaluation computes a what-if for every cluster (#241). Its work
// is bounded three ways:
//
//   - each cluster's contribution is computed in the read slot, which it
//     takes and gives back per cluster, so a per-cluster read waits at
//     most one cluster's compute; the request as a whole holds a slot of
//     its own (maxConcurrentTeamsRollups), not the fleet reads';
//   - each contribution is cached (fleetTeamsCache), so the next request
//     for that target redoes only what changed;
//   - a request that has run fleetTeamsBudget gets 503 with Retry-After
//     instead of holding on; what it computed stays cached, so the retry
//     goes on from there.
const (
	fleetTeamsBudget          = 20 * time.Second
	maxConcurrentTeamsRollups = 1
	// maxFleetTeamsEntries and maxFleetTeamsTeams bound the cache: entries,
	// and team scores across them (one is about 100 bytes), oldest first
	// out.
	maxFleetTeamsEntries = 4096
	maxFleetTeamsTeams   = 1 << 16
)

// errFleetTeamsBudget is a rollup past fleetTeamsBudget.
var errFleetTeamsBudget = errors.New("the fleet teams rollup ran past its time bound")

// errReadSlotBusy is a rollup that waited readQueueTimeout for the read
// slot, as a per-cluster read gives up.
var errReadSlotBusy = errors.New("too many concurrent reads")

// fleetTeamsKey identifies one cluster's contribution. A stored one is its
// evaluation as last written (a refresh moves evaluatedAt). A what-if is
// its snapshot judged at the target with this server's KB and team map on
// one UTC day (EOL windows are day-granular): the inputs evaluateWhatIf
// reads, so a cached answer is the one it would compute.
type fleetTeamsKey struct {
	evaluationID int64     // a stored evaluation; 0 for a what-if
	evaluatedAt  time.Time // of the stored evaluation
	snapshotID   int64     // a what-if's snapshot
	target       inventory.Version
	day          time.Time
	kbVersion    string
	teamMapHash  string
}

// fleetTeamsEntry is one cluster's cached contribution: its team scores
// before any scope cuts them (engine.TeamScores), where they came from,
// or why the cluster is left out.
type fleetTeamsEntry struct {
	scores   map[string]engine.TeamScore
	src      fleetTeamsSource // Name and ClusterID are the request's
	excluded string           // excludedTooLarge or excludedUnreadable; "" = included
}

type fleetTeamsCache struct {
	mu    sync.Mutex
	m     map[fleetTeamsKey]fleetTeamsEntry
	order []fleetTeamsKey // insertion order, for eviction
	teams int
}

func (c *fleetTeamsCache) get(k fleetTeamsKey) (fleetTeamsEntry, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.m[k]
	return e, ok
}

func (c *fleetTeamsCache) put(k fleetTeamsKey, e fleetTeamsEntry) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(e.scores) > maxFleetTeamsTeams {
		return // never fits: computed again each time, as before the cache
	}
	if c.m == nil {
		c.m = map[fleetTeamsKey]fleetTeamsEntry{}
	}
	if old, ok := c.m[k]; ok {
		c.teams -= len(old.scores)
	} else {
		c.order = append(c.order, k)
	}
	c.m[k] = e
	c.teams += len(e.scores)
	for len(c.m) > maxFleetTeamsEntries || c.teams > maxFleetTeamsTeams {
		oldest := c.order[0]
		c.order = c.order[1:]
		c.teams -= len(c.m[oldest].scores)
		delete(c.m, oldest)
	}
}

// withReadSlot runs f in the read slot, waiting for it as long as a
// per-cluster read does (errReadSlotBusy), but not past deadline
// (errFleetTeamsBudget), or until ctx ends.
func (s *Server) withReadSlot(ctx context.Context, deadline time.Time, f func() error) error {
	wait, late := s.readQueueTimeout, errReadSlotBusy
	if left := time.Until(deadline); left < wait {
		wait, late = left, errFleetTeamsBudget
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case s.readSlots <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return late
	}
	defer func() { <-s.readSlots }()
	return f()
}
