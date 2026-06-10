package server

import (
	"compress/gzip"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/abd-ulbasit/upgradescope/internal/engine"
	"github.com/abd-ulbasit/upgradescope/internal/inventory"
	"github.com/abd-ulbasit/upgradescope/internal/server/store"
)

// maxSnapshotBody caps the snapshot push body — enforced on the wire bytes
// AND on the decompressed stream (gzip-bomb guard).
const maxSnapshotBody = 20 << 20 // 20 MiB

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func errJSON(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

// bearerOK does a constant-time check of "Authorization: Bearer <token>".
func bearerOK(r *http.Request, token string) bool {
	const prefix = "Bearer "
	h := r.Header.Get("Authorization")
	if len(h) <= len(prefix) || !strings.HasPrefix(h, prefix) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(h[len(prefix):]), []byte(token)) == 1
}

// pushRequest is the snapshot push protocol body (schemaVersion 1).
type pushRequest struct {
	SchemaVersion int             `json:"schemaVersion"`
	ClusterName   string          `json:"clusterName"`
	AgentVersion  string          `json:"agentVersion"`
	KBVersion     string          `json:"kbVersion"`
	Inventory     json.RawMessage `json:"inventory"`
}

// handleIngest implements POST /api/v1/snapshots: bearer auth, gzip or
// identity body, schema validation, canonical-JSON content-hash dedup,
// upsert+insert, then synchronous evaluation fan-out for accepted snapshots.
func (s *Server) handleIngest(w http.ResponseWriter, r *http.Request) {
	if !bearerOK(r, s.cfg.IngestToken) {
		errJSON(w, http.StatusUnauthorized, "invalid or missing bearer token")
		return
	}
	body := http.MaxBytesReader(w, r.Body, maxSnapshotBody)
	var reader io.Reader = body
	switch enc := r.Header.Get("Content-Encoding"); enc {
	case "", "identity":
	case "gzip":
		gz, err := gzip.NewReader(body)
		if err != nil {
			errJSON(w, http.StatusUnprocessableEntity, "body is not valid gzip")
			return
		}
		defer gz.Close()
		// Cap the decompressed stream too: a tiny gzip bomb must not bypass
		// the wire-byte limit. Read one byte past the cap so overflow is
		// detectable below.
		reader = io.LimitReader(gz, maxSnapshotBody+1)
	default:
		errJSON(w, http.StatusUnsupportedMediaType, fmt.Sprintf("unsupported Content-Encoding %q (use gzip or identity)", enc))
		return
	}
	raw, err := io.ReadAll(reader)
	if err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			errJSON(w, http.StatusRequestEntityTooLarge, "snapshot exceeds the 20MiB limit")
			return
		}
		errJSON(w, http.StatusUnprocessableEntity, "reading body: "+err.Error())
		return
	}
	if len(raw) > maxSnapshotBody {
		errJSON(w, http.StatusRequestEntityTooLarge, "snapshot exceeds the 20MiB limit after decompression")
		return
	}
	var req pushRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		errJSON(w, http.StatusUnprocessableEntity, "invalid JSON: "+err.Error())
		return
	}
	if req.SchemaVersion != 1 {
		errJSON(w, http.StatusUnprocessableEntity, fmt.Sprintf("unsupported schemaVersion %d (want 1)", req.SchemaVersion))
		return
	}
	if req.ClusterName == "" {
		errJSON(w, http.StatusUnprocessableEntity, "clusterName is required")
		return
	}
	var inv inventory.Inventory
	if err := json.Unmarshal(req.Inventory, &inv); err != nil {
		errJSON(w, http.StatusUnprocessableEntity, "invalid inventory: "+err.Error())
		return
	}
	// Canonical form: re-marshal the parsed inventory so wire key order and
	// whitespace never change the dedup hash. Struct fields marshal in
	// declared order; map keys marshal sorted. CollectedAt is zeroed to match
	// the agent's snapshotHash canonical form (it changes every tick; hashing
	// it would make force-sync pushes never dedup to 200 duplicate).
	inv.CollectedAt = time.Time{}
	canonical, err := json.Marshal(inv)
	if err != nil {
		errJSON(w, http.StatusInternalServerError, "canonicalizing inventory: "+err.Error())
		return
	}
	hash := fmt.Sprintf("%x", sha256.Sum256(canonical))

	ctx := r.Context()
	now := s.now()
	clusterID, err := s.cfg.Store.UpsertCluster(ctx, store.Cluster{
		Name:       req.ClusterName,
		ClusterUID: inv.ClusterID,
		LastSeen:   now,
	})
	if err != nil {
		errJSON(w, http.StatusInternalServerError, "storing cluster: "+err.Error())
		return
	}
	snapID, duplicate, err := s.cfg.Store.InsertSnapshot(ctx, store.Snapshot{
		ClusterID:    clusterID,
		Hash:         hash,
		KBVersion:    req.KBVersion,
		AgentVersion: req.AgentVersion,
		ReceivedAt:   now,
		Inventory:    canonical,
	})
	if err != nil {
		errJSON(w, http.StatusInternalServerError, "storing snapshot: "+err.Error())
		return
	}
	if duplicate {
		writeJSON(w, http.StatusOK, map[string]any{"snapshotId": snapID, "duplicate": true})
		return
	}
	cluster := store.Cluster{ID: clusterID, Name: req.ClusterName, ClusterUID: inv.ClusterID}
	s.evaluateSnapshot(ctx, cluster, snapID, inv)
	writeJSON(w, http.StatusAccepted, map[string]any{"snapshotId": snapID})
}

// evaluateSnapshot evaluates an accepted snapshot against the default target
// (next minor above the inventory's server version; skipped when
// unparseable) plus every configured extra target (deduped), stores one
// Evaluation per target, and fires the notifier delta. Per-target failures
// are logged and skipped — the snapshot is already stored, and ingest must
// not fail because one target could not be evaluated.
func (s *Server) evaluateSnapshot(ctx context.Context, cluster store.Cluster, snapshotID int64, inv inventory.Inventory) {
	targets := make([]inventory.Version, 0, len(s.extraTargets)+1)
	if server, err := inventory.ParseVersion(inv.ServerVersion); err == nil {
		targets = append(targets, server.Next())
	}
	targets = append(targets, s.extraTargets...)

	seen := map[inventory.Version]bool{}
	now := s.now()
	for _, target := range targets {
		if seen[target] {
			continue
		}
		seen[target] = true
		rep := engine.Evaluate(inv, s.cfg.KB, target, now)
		repJSON, err := json.Marshal(rep)
		if err != nil {
			log.Printf("server: marshaling report (cluster %d, target %s): %v", cluster.ID, target, err)
			continue
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
		var prev *store.Evaluation
		if p, err := s.cfg.Store.LatestEvaluation(ctx, cluster.ID, target.String()); err == nil {
			prev = &p
		} else if !errors.Is(err, store.ErrNotFound) {
			log.Printf("server: loading previous evaluation (cluster %d, target %s): %v", cluster.ID, target, err)
		}
		cur := store.Evaluation{
			ClusterID:  cluster.ID,
			SnapshotID: snapshotID,
			Target:     target.String(),
			KBVersion:  s.cfg.KB.Version,
			Score:      rep.Score,
			Ready:      rep.Ready,
			Blockers:   blockers,
			Warnings:   warnings,
			Report:     repJSON,
			CreatedAt:  now,
		}
		id, err := s.cfg.Store.InsertEvaluation(ctx, cur)
		if err != nil {
			log.Printf("server: storing evaluation (cluster %d, target %s): %v", cluster.ID, target, err)
			continue
		}
		cur.ID = id
		s.notifyDelta(ctx, cluster, target.String(), prev, cur)
	}
}

// notifyDelta compares prev (nil ⇔ the cluster's first-ever evaluation for
// this target) against cur and emits Config.Notifier events per the delta
// rule (new-blocker, became-ready, eol-approaching).
//
// This body is a no-op stub: the NOTIFY-CLI section's "wire notifyDelta"
// task replaces it with the real delta computation. The signature is the
// fixed contract between the two sections — do not change it there.
func (s *Server) notifyDelta(ctx context.Context, cluster store.Cluster, target string, prev *store.Evaluation, cur store.Evaluation) {
	_, _, _, _, _ = ctx, cluster, target, prev, cur
}

// ----- read API -----

// readAuth gates a read handler behind Config.ReadToken when configured;
// an empty ReadToken leaves the read API open (the CLI documents this loudly).
func (s *Server) readAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.cfg.ReadToken != "" && !bearerOK(r, s.cfg.ReadToken) {
			errJSON(w, http.StatusUnauthorized, "invalid or missing bearer token")
			return
		}
		next(w, r)
	}
}

// handleHealthz is always unauthenticated: liveness probes carry no tokens.
func (s *Server) handleHealthz(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// pathClusterID parses the {id} path value, writing the 400 itself on failure.
func (s *Server) pathClusterID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		errJSON(w, http.StatusBadRequest, "invalid cluster id")
		return 0, false
	}
	return id, true
}

// requireCluster 404s (JSON) for unknown clusters so every per-cluster
// endpoint shares one existence check.
func (s *Server) requireCluster(w http.ResponseWriter, r *http.Request) (store.Cluster, bool) {
	id, ok := s.pathClusterID(w, r)
	if !ok {
		return store.Cluster{}, false
	}
	c, err := s.cfg.Store.GetCluster(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		errJSON(w, http.StatusNotFound, "cluster not found")
		return store.Cluster{}, false
	}
	if err != nil {
		errJSON(w, http.StatusInternalServerError, "loading cluster: "+err.Error())
		return store.Cluster{}, false
	}
	return c, true
}

// defaultTarget computes a cluster's default evaluation target (next minor
// above the latest snapshot's server version) and returns the parsed latest
// inventory alongside so callers don't unmarshal twice. Errors:
// store.ErrNotFound (no snapshots) or a corrupt/unparseable-version error.
func (s *Server) defaultTarget(ctx context.Context, clusterID int64) (inventory.Version, inventory.Inventory, error) {
	snap, err := s.cfg.Store.LatestSnapshot(ctx, clusterID)
	if err != nil {
		return inventory.Version{}, inventory.Inventory{}, err
	}
	var inv inventory.Inventory
	if err := json.Unmarshal(snap.Inventory, &inv); err != nil {
		return inventory.Version{}, inventory.Inventory{}, fmt.Errorf("stored inventory for cluster %d is corrupt: %w", clusterID, err)
	}
	server, err := inventory.ParseVersion(inv.ServerVersion)
	if err != nil {
		return inventory.Version{}, inv, fmt.Errorf("latest snapshot has no parseable server version: %w", err)
	}
	return server.Next(), inv, nil
}

// resolveTarget picks the evaluation target: explicit ?target= (422 when
// unparseable), else the cluster's default target (404 when there is no
// snapshot to derive one from). Writes the error response itself.
func (s *Server) resolveTarget(w http.ResponseWriter, r *http.Request, clusterID int64) (inventory.Version, bool) {
	if q := r.URL.Query().Get("target"); q != "" {
		v, err := inventory.ParseVersion(q)
		if err != nil {
			errJSON(w, http.StatusUnprocessableEntity, "invalid target: "+err.Error())
			return inventory.Version{}, false
		}
		return v, true
	}
	target, _, err := s.defaultTarget(r.Context(), clusterID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			errJSON(w, http.StatusNotFound, "no snapshots for cluster")
		} else {
			errJSON(w, http.StatusUnprocessableEntity, "cannot derive default target: "+err.Error())
		}
		return inventory.Version{}, false
	}
	return target, true
}

// evalSummary is the read API's compact evaluation view.
type evalSummary struct {
	Target      string    `json:"target"`
	Score       int       `json:"score"`
	Ready       bool      `json:"ready"`
	Blockers    int       `json:"blockers"`
	Warnings    int       `json:"warnings"`
	KBVersion   string    `json:"kbVersion"`
	EvaluatedAt time.Time `json:"evaluatedAt"`
}

func summarize(e store.Evaluation) evalSummary {
	return evalSummary{
		Target:      e.Target,
		Score:       e.Score,
		Ready:       e.Ready,
		Blockers:    e.Blockers,
		Warnings:    e.Warnings,
		KBVersion:   e.KBVersion,
		EvaluatedAt: e.CreatedAt,
	}
}

type clusterSummary struct {
	store.Cluster
	Latest *evalSummary `json:"latest,omitempty"` // default-target evaluation, if any
}

// handleListClusters: GET /api/v1/clusters — every cluster plus its latest
// default-target score summary (omitted when no snapshot/evaluation exists).
func (s *Server) handleListClusters(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	clusters, err := s.cfg.Store.ListClusters(ctx)
	if err != nil {
		errJSON(w, http.StatusInternalServerError, "listing clusters: "+err.Error())
		return
	}
	out := make([]clusterSummary, 0, len(clusters))
	for _, c := range clusters {
		cs := clusterSummary{Cluster: c}
		if target, _, err := s.defaultTarget(ctx, c.ID); err == nil {
			if e, err := s.cfg.Store.LatestEvaluation(ctx, c.ID, target.String()); err == nil {
				sum := summarize(e)
				cs.Latest = &sum
			}
		}
		out = append(out, cs)
	}
	writeJSON(w, http.StatusOK, out)
}

type clusterDetail struct {
	store.Cluster
	Capabilities map[inventory.Capability]inventory.CapabilityStatus `json:"capabilities,omitempty"`
	Evaluations  []evalSummary                                       `json:"evaluations"`
}

// handleGetCluster: GET /api/v1/clusters/{id} — cluster row, the latest
// snapshot's capability map, and latest evaluation summaries for the default
// target plus every configured extra target.
func (s *Server) handleGetCluster(w http.ResponseWriter, r *http.Request) {
	c, ok := s.requireCluster(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	detail := clusterDetail{Cluster: c, Evaluations: []evalSummary{}}
	targets := make([]string, 0, len(s.extraTargets)+1)
	if snap, err := s.cfg.Store.LatestSnapshot(ctx, c.ID); err == nil {
		var inv inventory.Inventory
		if json.Unmarshal(snap.Inventory, &inv) == nil {
			detail.Capabilities = inv.Capabilities
			if server, err := inventory.ParseVersion(inv.ServerVersion); err == nil {
				targets = append(targets, server.Next().String())
			}
		}
	}
	for _, t := range s.extraTargets {
		targets = append(targets, t.String())
	}
	seen := map[string]bool{}
	for _, t := range targets {
		if seen[t] {
			continue
		}
		seen[t] = true
		if e, err := s.cfg.Store.LatestEvaluation(ctx, c.ID, t); err == nil {
			detail.Evaluations = append(detail.Evaluations, summarize(e))
		}
	}
	writeJSON(w, http.StatusOK, detail)
}

// loadOrComputeReport returns the stored evaluation's report for (cluster,
// target) when one exists, else computes a what-if from the latest snapshot.
// A store.ErrNotFound result means the cluster has no snapshots at all.
func (s *Server) loadOrComputeReport(ctx context.Context, clusterID int64, target inventory.Version) (engine.Report, error) {
	if e, err := s.cfg.Store.LatestEvaluation(ctx, clusterID, target.String()); err == nil {
		var rep engine.Report
		if err := json.Unmarshal(e.Report, &rep); err != nil {
			return engine.Report{}, fmt.Errorf("stored report for evaluation %d is corrupt: %w", e.ID, err)
		}
		return rep, nil
	}
	return WhatIf(ctx, s.cfg.Store, s.cfg.KB, clusterID, target, s.now())
}

// reportForRequest is the shared resolve-cluster → resolve-target → load/
// compute pipeline behind the report and findings endpoints.
func (s *Server) reportForRequest(w http.ResponseWriter, r *http.Request) (engine.Report, bool) {
	c, ok := s.requireCluster(w, r)
	if !ok {
		return engine.Report{}, false
	}
	target, ok := s.resolveTarget(w, r, c.ID)
	if !ok {
		return engine.Report{}, false
	}
	rep, err := s.loadOrComputeReport(r.Context(), c.ID, target)
	if errors.Is(err, store.ErrNotFound) {
		errJSON(w, http.StatusNotFound, "no snapshots for cluster")
		return engine.Report{}, false
	}
	if err != nil {
		errJSON(w, http.StatusInternalServerError, err.Error())
		return engine.Report{}, false
	}
	return rep, true
}

// handleReport: GET /api/v1/clusters/{id}/report?target= — full engine.Report.
func (s *Server) handleReport(w http.ResponseWriter, r *http.Request) {
	rep, ok := s.reportForRequest(w, r)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, rep)
}

// handleFindings: GET /api/v1/clusters/{id}/findings?target=&severity=&category=
// — the report's findings, exact-match filtered. Unknown filter values simply
// match nothing.
func (s *Server) handleFindings(w http.ResponseWriter, r *http.Request) {
	rep, ok := s.reportForRequest(w, r)
	if !ok {
		return
	}
	severity := r.URL.Query().Get("severity")
	category := r.URL.Query().Get("category")
	findings := []engine.Finding{} // non-nil so JSON renders []
	for _, f := range rep.Findings {
		if severity != "" && string(f.Severity) != severity {
			continue
		}
		if category != "" && string(f.Category) != category {
			continue
		}
		findings = append(findings, f)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"target":   rep.Target.String(),
		"findings": findings,
	})
}

// handleHistory: GET /api/v1/clusters/{id}/history?target=&limit= —
// []store.ScorePoint, oldest first, default limit 100.
func (s *Server) handleHistory(w http.ResponseWriter, r *http.Request) {
	c, ok := s.requireCluster(w, r)
	if !ok {
		return
	}
	target, ok := s.resolveTarget(w, r, c.ID)
	if !ok {
		return
	}
	limit := 100
	if q := r.URL.Query().Get("limit"); q != "" {
		n, err := strconv.Atoi(q)
		if err != nil || n < 1 {
			errJSON(w, http.StatusUnprocessableEntity, "limit must be a positive integer")
			return
		}
		limit = n
	}
	points, err := s.cfg.Store.ScoreHistory(r.Context(), c.ID, target.String(), limit)
	if err != nil {
		errJSON(w, http.StatusInternalServerError, "loading history: "+err.Error())
		return
	}
	if points == nil {
		points = []store.ScorePoint{}
	}
	writeJSON(w, http.StatusOK, points)
}
