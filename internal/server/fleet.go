package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/abd-ulbasit/upgradescope/internal/engine"
	"github.com/abd-ulbasit/upgradescope/internal/inventory"
	"github.com/abd-ulbasit/upgradescope/internal/server/store"
)

// fleetCell is one (cluster, target) cell of the fleet matrix, lifted from
// the cluster's current stored evaluation (of its latest snapshot) — never
// recomputed, so Source is always "stored".
type fleetCell struct {
	Score       int            `json:"score"`
	Ready       bool           `json:"ready"`
	Verdict     engine.Verdict `json:"verdict"` // ready | blocked | unknown — unknown is never ready
	Blockers    int            `json:"blockers"`
	EvaluatedAt time.Time      `json:"evaluatedAt"`
	SnapshotID  int64          `json:"snapshotId"`
	Source      string         `json:"source"`
	Outdated    bool           `json:"outdated,omitempty"` // evalSummary.Outdated
	// NotAssessed is the stored report's, bounded (gapsOf): why a cell is
	// unknown, or what a ready one did not cover. NotAssessedOmitted
	// counts the gaps not listed.
	NotAssessed        []summaryGap `json:"notAssessed,omitempty"`
	NotAssessedOmitted int          `json:"notAssessedOmitted,omitempty"`
}

type fleetRow struct {
	ClusterID     int64                 `json:"clusterId"`
	Name          string                `json:"name"`
	LastSeen      time.Time             `json:"lastSeen"`                // the agent's last push, duplicates included
	Stale         bool                  `json:"stale"`                   // no push within --stale-after: the cells are that old
	ServerVersion string                `json:"serverVersion,omitempty"` // the version the latest snapshot is judged at (judgedVersion)
	Cells         map[string]*fleetCell `json:"cells"`                   // target → cell; nil = no current evaluation (or not applicable)
	NotApplicable []string              `json:"notApplicable,omitempty"` // requested targets at or below ServerVersion
}

type fleetResponse struct {
	Targets []string `json:"targets"`
	// TargetsOmitted counts the default columns left out past
	// maxFleetTargets (never with ?targets=), which ?targets= can ask for.
	TargetsOmitted int        `json:"targetsOmitted,omitempty"`
	Clusters       []fleetRow `json:"clusters"`
}

// clusterState is a cluster with its latest snapshot's head — no
// inventory — and the version that snapshot is judged at (hasSnapshot
// false when it has none).
type clusterState struct {
	store.Cluster
	snap        store.Snapshot // Inventory is nil
	version     string         // judgedVersion
	hasSnapshot bool
}

// clusterStates loads the latest snapshot head of every cluster sc reads
// in one store call (and one more for a team scope's clusters). Fleet
// views never decode inventories up front: at 500 clusters that held
// hundreds of MiB per request (#125 SV-14); only a row stored before
// snapshots.server_version is read whole, for its version.
func (s *Server) clusterStates(ctx context.Context, sc readScope) ([]clusterState, error) {
	clusters, err := s.cfg.Store.ListClusters(ctx)
	if err != nil {
		return nil, err
	}
	inScope, err := s.scopeClusters(ctx, sc)
	if err != nil {
		return nil, err
	}
	heads, err := s.cfg.Store.LatestSnapshotHeads(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]clusterState, 0, len(clusters))
	for _, c := range clusters {
		if inScope != nil && !inScope[c.ID] {
			continue
		}
		cs := clusterState{Cluster: c}
		if head, ok := heads[c.ID]; ok {
			cs.snap, cs.version, cs.hasSnapshot = head, head.ServerVersion, true
			if cs.version == "" {
				full, err := s.cfg.Store.LatestSnapshot(ctx, c.ID)
				if err != nil && !errors.Is(err, store.ErrNotFound) {
					return nil, err
				}
				cs.version = judgedVersion(full)
			}
		}
		out = append(out, cs)
	}
	return out, nil
}

// handleFleet: GET /api/v1/fleet?targets=1.37,1.38 — score matrix across the
// fleet from current evaluations only (those of each cluster's latest
// snapshot). Rows = clusters, columns = requested targets (default: the
// union of every cluster's default next-minor target plus the server's
// extra targets, at most maxFleetTargets of them: fleetDefaultTargets).
// A cluster without a current evaluation for a column gets
// a null cell; nothing is recomputed. A column at or below a cluster's
// version is null too and listed in the row's notApplicable. Cells are
// read with CurrentEvaluationSummary, so no report is loaded: the matrix
// runs in a fleet slot, not the read slot, and costs about its response,
// which is clusters x targets cells. ?targets= takes at most
// maxFleetTargets distinct minors (422 above it), and the default columns
// are as many at most, the rest counted in targetsOmitted.
func (s *Server) handleFleet(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	states, err := s.clusterStates(ctx, scopeOf(r))
	if err != nil {
		internalErr(w, "listing clusters", err)
		return
	}

	var targets []inventory.Version
	omitted := 0
	if q := r.URL.Query().Get("targets"); q != "" {
		for _, raw := range strings.Split(q, ",") {
			v, err := inventory.ParseTarget(strings.TrimSpace(raw))
			if err != nil {
				errJSON(w, http.StatusUnprocessableEntity, "invalid targets entry "+raw+": "+err.Error())
				return
			}
			if !containsVersion(targets, v) {
				if len(targets) == maxFleetTargets {
					errJSON(w, http.StatusUnprocessableEntity, fmt.Sprintf(
						"targets lists more than %d distinct minors; ask for at most %d at a time", maxFleetTargets, maxFleetTargets))
					return
				}
				targets = append(targets, v)
			}
		}
	} else {
		targets, omitted = s.fleetDefaultTargets(states)
	}

	rows := make([]fleetRow, 0, len(states))
	now := s.now()
	for _, c := range states {
		row := fleetRow{ClusterID: c.ID, Name: c.Name, LastSeen: c.LastSeen, Stale: s.clusterStale(c.Cluster, now), Cells: map[string]*fleetCell{}}
		if c.hasSnapshot {
			row.ServerVersion = c.version
		}
		for _, t := range targets {
			row.Cells[t.String()] = nil // explicit null unless a current evaluation exists
			if c.hasSnapshot && notApplicable(c.version, t) {
				row.NotApplicable = append(row.NotApplicable, t.String())
				continue
			}
			e, err := s.cfg.Store.CurrentEvaluationSummary(ctx, c.ID, t.String())
			switch {
			case err == nil:
				gaps, gapsOmitted := gapsOf(e, fleetSummaryBytes)
				row.Cells[t.String()] = &fleetCell{
					Score: e.Score, Ready: e.Ready, Verdict: verdictOf(e), Blockers: e.Blockers,
					EvaluatedAt: e.EvaluatedAt, SnapshotID: e.SnapshotID, Source: sourceStored, Outdated: s.outdated(e, now),
					NotAssessed: gaps, NotAssessedOmitted: gapsOmitted,
				}
			case errors.Is(err, store.ErrNotFound):
			default:
				internalErr(w, "loading evaluation", err)
				return
			}
		}
		rows = append(rows, row)
	}
	names := make([]string, 0, len(targets))
	for _, t := range targets {
		names = append(names, t.String())
	}
	writeJSON(w, http.StatusOK, fleetResponse{Targets: names, TargetsOmitted: omitted, Clusters: rows})
}

// maxFleetTargets caps the distinct minors ?targets= may ask the fleet
// matrix for, and the columns it opens without ?targets=. Each is a
// column, a store query per cluster: unbounded but for the 64 KiB URL,
// 8,718 of them against 500 clusters held a fleet slot for 2m13s, grew
// the heap 418 MiB and answered 57 MiB; uncapped, the default columns of
// 500 clusters pushed at 500 minors grew it 36 MiB. Sixteen minors is
// four years of Kubernetes releases.
const maxFleetTargets = 16

func containsVersion(vs []inventory.Version, v inventory.Version) bool {
	for _, x := range vs {
		if x == v {
			return true
		}
	}
	return false
}

// fleetDefaultTargets unions each cluster's default next-minor target with
// the configured extra targets. An extra target every known cluster
// already runs is dropped (an all-n/a column); clusters whose default
// cannot be derived just contribute nothing. Of more than maxFleetTargets,
// it keeps those with the most clusters to fill them (a cluster fills its
// next minor's column and every extra target's above its version), the
// older minor on a tie, and returns how many it left out. The columns
// are sorted by version.
func (s *Server) fleetDefaultTargets(states []clusterState) ([]inventory.Version, int) {
	var versions []inventory.Version
	filled := map[inventory.Version]int{}
	add := func(v inventory.Version) {
		if !containsVersion(versions, v) {
			versions = append(versions, v)
		}
	}
	for _, c := range states {
		if !c.hasSnapshot {
			continue
		}
		if server, err := inventory.ParseVersion(c.version); err == nil {
			next := server.Next()
			add(next)
			filled[next]++
		}
	}
	for _, v := range s.extraTargets {
		applicable := false
		for _, c := range states {
			if !c.hasSnapshot || !notApplicable(c.version, v) {
				applicable = true
				if c.hasSnapshot && !isNextMinor(c.version, v) {
					filled[v]++
				}
			}
		}
		if applicable || len(states) == 0 {
			add(v)
		}
	}
	omitted := 0
	if len(versions) > maxFleetTargets {
		sort.SliceStable(versions, func(i, j int) bool {
			if filled[versions[i]] != filled[versions[j]] {
				return filled[versions[i]] > filled[versions[j]]
			}
			return versions[i].Compare(versions[j]) < 0
		})
		omitted = len(versions) - maxFleetTargets
		versions = versions[:maxFleetTargets]
	}
	sort.Slice(versions, func(i, j int) bool { return versions[i].Compare(versions[j]) < 0 })
	return versions, omitted
}

// isNextMinor reports whether v is the next minor of the cluster version
// version, the column its default target already counts it in.
func isNextMinor(version string, v inventory.Version) bool {
	server, err := inventory.ParseVersion(version)
	return err == nil && server.Next() == v
}

// fleetTeam aggregates one team across the fleet for a single target.
type fleetTeam struct {
	WorstScore int      `json:"worstScore"` // min team score across clusters
	Blockers   int      `json:"blockers"`   // summed across clusters
	Clusters   []string `json:"clusters"`   // sorted cluster names with findings for this team
}

// fleetTeamsSource records where one cluster's contribution came from.
type fleetTeamsSource struct {
	Name        string    `json:"name"`
	ClusterID   int64     `json:"clusterId"`
	Source      string    `json:"source"` // stored | what-if
	EvaluatedAt time.Time `json:"evaluatedAt"`
	SnapshotID  int64     `json:"snapshotId"`
}

// handleFleetTeams: GET /api/v1/fleet/teams?target=1.38 — per-team rollup
// across all clusters. Each cluster contributes the same report
// /clusters/{id}/teams serves: its current stored evaluation, else a
// what-if computed from its latest snapshot — so a target outside
// --targets is a real answer, not an empty one. The response says which
// clusters were evaluated and how (`evaluated`), which have no snapshot
// (`missing`), and which already run the target (`notApplicable`, left out
// of the rollup). target is required: team scores are only comparable at
// the same target.
func (s *Server) handleFleetTeams(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query().Get("target")
	if q == "" {
		errJSON(w, http.StatusUnprocessableEntity, "target query parameter is required")
		return
	}
	target, err := inventory.ParseTarget(q)
	if err != nil {
		errJSON(w, http.StatusUnprocessableEntity, "invalid target: "+err.Error())
		return
	}

	ctx := r.Context()
	states, err := s.clusterStates(ctx, scopeOf(r))
	if err != nil {
		internalErr(w, "listing clusters", err)
		return
	}

	teams := map[string]*fleetTeam{}
	evaluated := []fleetTeamsSource{}
	missing, notApp := []string{}, []string{}
	for _, c := range states {
		if !c.hasSnapshot {
			missing = append(missing, c.Name)
			continue
		}
		if notApplicable(c.version, target) {
			notApp = append(notApp, c.Name)
			continue
		}
		rep, src, err := s.fleetTeamsReport(ctx, c, target)
		var tooLarge *reportTooLargeError
		if errors.Is(err, errCorruptInventory) || errors.Is(err, store.ErrNotFound) || errors.As(err, &tooLarge) {
			// One bad row (or a cluster deleted meanwhile) must not take
			// the rollup down: it has nothing to contribute.
			log.Printf("server: fleet teams: %v", err)
			missing = append(missing, c.Name)
			continue
		}
		if err != nil {
			internalErr(w, "loading evaluation", err)
			return
		}
		evaluated = append(evaluated, src)
		for team, ts := range scopeOf(r).renderedTeams(rep) {
			agg := teams[team]
			if agg == nil {
				agg = &fleetTeam{WorstScore: ts.Score}
				teams[team] = agg
			} else if ts.Score < agg.WorstScore {
				agg.WorstScore = ts.Score
			}
			agg.Blockers += ts.Blockers
			agg.Clusters = append(agg.Clusters, c.Name)
		}
	}
	for _, agg := range teams {
		sort.Strings(agg.Clusters)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"target":        target.String(),
		"teams":         teams,
		"evaluated":     evaluated,
		"missing":       missing,
		"notApplicable": notApp,
	})
}

// fleetTeamsReport is one cluster's report for the teams rollup: the
// current stored evaluation, else a what-if from the latest snapshot. A
// corrupt stored report is logged and recomputed rather than failing the
// whole rollup.
func (s *Server) fleetTeamsReport(ctx context.Context, c clusterState, target inventory.Version) (engine.Report, fleetTeamsSource, error) {
	src := fleetTeamsSource{Name: c.Name, ClusterID: c.ID}
	e, err := s.cfg.Store.CurrentEvaluation(ctx, c.ID, target.String())
	switch {
	case err == nil:
		var rep engine.Report
		jerr := json.Unmarshal(e.Report, &rep)
		if jerr == nil {
			src.Source, src.EvaluatedAt, src.SnapshotID = sourceStored, e.EvaluatedAt, e.SnapshotID
			return rep, src, nil
		}
		log.Printf("server: fleet teams: corrupt report (cluster %d, evaluation %d), using a what-if: %v", c.ID, e.ID, jerr)
	case !errors.Is(err, store.ErrNotFound):
		return engine.Report{}, src, err
	}
	snap, inv, err := s.latestInventory(ctx, c.ID)
	if err != nil {
		return engine.Report{}, src, err
	}
	now := s.now().UTC() // as stored evaluations read back
	src.Source, src.EvaluatedAt, src.SnapshotID = sourceWhatIf, now, snap.ID
	rep, err := s.evaluateWhatIf(inv, target, now)
	return rep, src, err
}
