package server

import (
	"context"
	"encoding/json"
	"errors"
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
}

type fleetRow struct {
	ClusterID     int64                 `json:"clusterId"`
	Name          string                `json:"name"`
	LastSeen      time.Time             `json:"lastSeen"`                // the agent's last push, duplicates included
	Stale         bool                  `json:"stale"`                   // no push within --stale-after: the cells are that old
	ServerVersion string                `json:"serverVersion,omitempty"` // of the latest snapshot
	Cells         map[string]*fleetCell `json:"cells"`                   // target → cell; nil = no current evaluation (or not applicable)
	NotApplicable []string              `json:"notApplicable,omitempty"` // requested targets at or below ServerVersion
}

type fleetResponse struct {
	Targets  []string   `json:"targets"`
	Clusters []fleetRow `json:"clusters"`
}

// clusterState is a cluster with its decoded latest snapshot (hasSnapshot
// false when it has none, or it cannot be decoded).
type clusterState struct {
	store.Cluster
	snap        store.Snapshot
	inv         inventory.Inventory
	hasSnapshot bool
}

// clusterStates loads every cluster's latest snapshot once. A corrupt
// stored inventory is logged and treated as no snapshot so one bad row
// cannot take the fleet views down; any other store error is returned.
func (s *Server) clusterStates(ctx context.Context) ([]clusterState, error) {
	clusters, err := s.cfg.Store.ListClusters(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]clusterState, 0, len(clusters))
	for _, c := range clusters {
		cs := clusterState{Cluster: c}
		snap, inv, err := s.latestInventory(ctx, c.ID)
		switch {
		case err == nil:
			cs.snap, cs.inv, cs.hasSnapshot = snap, inv, true
		case errors.Is(err, store.ErrNotFound):
		case errors.Is(err, errCorruptInventory):
			log.Printf("server: fleet: %v", err)
		default:
			return nil, err
		}
		out = append(out, cs)
	}
	return out, nil
}

// handleFleet: GET /api/v1/fleet?targets=1.37,1.38 — score matrix across the
// fleet from current evaluations only (those of each cluster's latest
// snapshot). Rows = clusters, columns = requested targets (default: the
// union of every cluster's default next-minor target plus the server's
// extra targets). A cluster without a current evaluation for a column gets
// a null cell; nothing is recomputed. A column at or below a cluster's
// version is null too and listed in the row's notApplicable.
func (s *Server) handleFleet(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	states, err := s.clusterStates(ctx)
	if err != nil {
		internalErr(w, "listing clusters", err)
		return
	}

	var targets []inventory.Version
	if q := r.URL.Query().Get("targets"); q != "" {
		for _, raw := range strings.Split(q, ",") {
			v, err := inventory.ParseTarget(strings.TrimSpace(raw))
			if err != nil {
				errJSON(w, http.StatusUnprocessableEntity, "invalid targets entry "+raw+": "+err.Error())
				return
			}
			if !containsVersion(targets, v) {
				targets = append(targets, v)
			}
		}
	} else {
		targets = s.fleetDefaultTargets(states)
	}

	rows := make([]fleetRow, 0, len(states))
	now := s.now()
	for _, c := range states {
		row := fleetRow{ClusterID: c.ID, Name: c.Name, LastSeen: c.LastSeen, Stale: s.clusterStale(c.Cluster, now), Cells: map[string]*fleetCell{}}
		if c.hasSnapshot {
			row.ServerVersion = c.inv.ServerVersion
		}
		for _, t := range targets {
			row.Cells[t.String()] = nil // explicit null unless a current evaluation exists
			if c.hasSnapshot && notApplicable(c.inv, t) {
				row.NotApplicable = append(row.NotApplicable, t.String())
				continue
			}
			e, err := s.cfg.Store.CurrentEvaluation(ctx, c.ID, t.String())
			switch {
			case err == nil:
				row.Cells[t.String()] = &fleetCell{
					Score: e.Score, Ready: e.Ready, Verdict: verdictOf(e), Blockers: e.Blockers,
					EvaluatedAt: e.EvaluatedAt, SnapshotID: e.SnapshotID, Source: sourceStored,
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
	writeJSON(w, http.StatusOK, fleetResponse{Targets: names, Clusters: rows})
}

func containsVersion(vs []inventory.Version, v inventory.Version) bool {
	for _, x := range vs {
		if x == v {
			return true
		}
	}
	return false
}

// fleetDefaultTargets unions each cluster's default next-minor target with
// the configured extra targets, sorted by version. An extra target every
// known cluster already runs is dropped (an all-n/a column); clusters
// whose default cannot be derived just contribute nothing.
func (s *Server) fleetDefaultTargets(states []clusterState) []inventory.Version {
	var versions []inventory.Version
	add := func(v inventory.Version) {
		if !containsVersion(versions, v) {
			versions = append(versions, v)
		}
	}
	for _, c := range states {
		if !c.hasSnapshot {
			continue
		}
		if server, err := inventory.ParseVersion(c.inv.ServerVersion); err == nil {
			add(server.Next())
		}
	}
	for _, v := range s.extraTargets {
		applicable := false
		for _, c := range states {
			if !c.hasSnapshot || !notApplicable(c.inv, v) {
				applicable = true
				break
			}
		}
		if applicable || len(states) == 0 {
			add(v)
		}
	}
	sort.Slice(versions, func(i, j int) bool { return versions[i].Compare(versions[j]) < 0 })
	return versions
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
	states, err := s.clusterStates(ctx)
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
		if notApplicable(c.inv, target) {
			notApp = append(notApp, c.Name)
			continue
		}
		rep, src, err := s.fleetTeamsReport(ctx, c, target)
		if err != nil {
			internalErr(w, "loading evaluation", err)
			return
		}
		evaluated = append(evaluated, src)
		for team, ts := range renderTeamScores(engine.TeamScores(rep)) {
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
	now := s.now()
	src.Source, src.EvaluatedAt, src.SnapshotID = sourceWhatIf, now, c.snap.ID
	return evaluateWhatIf(c.inv, s.cfg.KB, s.cfg.TeamMap, target, now), src, nil
}
