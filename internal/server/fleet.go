package server

import (
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"sort"
	"strings"

	"github.com/abd-ulbasit/upgradescope/internal/engine"
	"github.com/abd-ulbasit/upgradescope/internal/inventory"
	"github.com/abd-ulbasit/upgradescope/internal/server/store"
)

// fleetCell is one (cluster, target) cell of the fleet matrix, lifted from
// the latest stored evaluation — never recomputed.
type fleetCell struct {
	Score    int  `json:"score"`
	Ready    bool `json:"ready"`
	Blockers int  `json:"blockers"`
}

type fleetRow struct {
	ClusterID int64                 `json:"clusterId"`
	Name      string                `json:"name"`
	Cells     map[string]*fleetCell `json:"cells"` // target → cell; nil = no evaluation
}

type fleetResponse struct {
	Targets  []string   `json:"targets"`
	Clusters []fleetRow `json:"clusters"`
}

// handleFleet: GET /api/v1/fleet?targets=1.37,1.38 — score matrix across the
// fleet from latest evaluations only. Rows = clusters, columns = requested
// targets (default: the union of every cluster's stored targets — its
// default next-minor target plus the server's extra targets). A cluster
// without an evaluation for a column gets a null cell; nothing is recomputed.
func (s *Server) handleFleet(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	clusters, err := s.cfg.Store.ListClusters(ctx)
	if err != nil {
		internalErr(w, "listing clusters", err)
		return
	}

	var targets []string
	if q := r.URL.Query().Get("targets"); q != "" {
		seen := map[string]bool{}
		for _, raw := range strings.Split(q, ",") {
			v, err := inventory.ParseVersion(strings.TrimSpace(raw))
			if err != nil {
				errJSON(w, http.StatusUnprocessableEntity, "invalid targets entry "+raw+": "+err.Error())
				return
			}
			if t := v.String(); !seen[t] {
				seen[t] = true
				targets = append(targets, t)
			}
		}
	} else {
		targets = s.fleetDefaultTargets(r, clusters)
	}

	rows := make([]fleetRow, 0, len(clusters))
	for _, c := range clusters {
		row := fleetRow{ClusterID: c.ID, Name: c.Name, Cells: map[string]*fleetCell{}}
		for _, t := range targets {
			e, err := s.cfg.Store.LatestEvaluation(ctx, c.ID, t)
			switch {
			case err == nil:
				row.Cells[t] = &fleetCell{Score: e.Score, Ready: e.Ready, Blockers: e.Blockers}
			case errors.Is(err, store.ErrNotFound):
				row.Cells[t] = nil // explicit null: never evaluated for this target
			default:
				internalErr(w, "loading evaluation", err)
				return
			}
		}
		rows = append(rows, row)
	}
	writeJSON(w, http.StatusOK, fleetResponse{Targets: targets, Clusters: rows})
}

// fleetDefaultTargets unions each cluster's candidate targets (default
// next-minor from its latest snapshot, plus the configured extra targets),
// sorted by version. Clusters whose default cannot be derived (no snapshot,
// unparseable version) just contribute nothing — the matrix still renders.
func (s *Server) fleetDefaultTargets(r *http.Request, clusters []store.Cluster) []string {
	seen := map[inventory.Version]bool{}
	var versions []inventory.Version
	add := func(v inventory.Version) {
		if !seen[v] {
			seen[v] = true
			versions = append(versions, v)
		}
	}
	for _, c := range clusters {
		if target, _, err := s.defaultTarget(r.Context(), c.ID); err == nil {
			add(target)
		}
	}
	for _, v := range s.extraTargets {
		add(v)
	}
	sort.Slice(versions, func(i, j int) bool { return versions[i].Compare(versions[j]) < 0 })
	out := make([]string, 0, len(versions))
	for _, v := range versions {
		out = append(out, v.String())
	}
	return out
}

// fleetTeam aggregates one team across the fleet for a single target.
type fleetTeam struct {
	WorstScore int      `json:"worstScore"` // min team score across clusters
	Blockers   int      `json:"blockers"`   // summed across clusters
	Clusters   []string `json:"clusters"`   // sorted cluster names with findings for this team
}

// handleFleetTeams: GET /api/v1/fleet/teams?target=1.38 — per-team rollup
// across all clusters, from latest stored evaluations only (clusters with no
// evaluation for the target are skipped, never recomputed). target is
// required: team scores are only comparable at the same target.
func (s *Server) handleFleetTeams(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query().Get("target")
	if q == "" {
		errJSON(w, http.StatusUnprocessableEntity, "target query parameter is required")
		return
	}
	target, err := inventory.ParseVersion(q)
	if err != nil {
		errJSON(w, http.StatusUnprocessableEntity, "invalid target: "+err.Error())
		return
	}

	ctx := r.Context()
	clusters, err := s.cfg.Store.ListClusters(ctx)
	if err != nil {
		internalErr(w, "listing clusters", err)
		return
	}

	teams := map[string]*fleetTeam{}
	for _, c := range clusters {
		e, err := s.cfg.Store.LatestEvaluation(ctx, c.ID, target.String())
		if errors.Is(err, store.ErrNotFound) {
			continue
		}
		if err != nil {
			internalErr(w, "loading evaluation", err)
			return
		}
		var rep engine.Report
		if err := json.Unmarshal(e.Report, &rep); err != nil {
			// One corrupt stored report must not 500 the whole rollup; log
			// and keep aggregating the healthy clusters.
			log.Printf("server: fleet teams: corrupt report (cluster %d, evaluation %d): %v", c.ID, e.ID, err)
			continue
		}
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
		"target": target.String(),
		"teams":  teams,
	})
}
