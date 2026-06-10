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
