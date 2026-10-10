package server

import (
	"bytes"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"

	"github.com/abd-ulbasit/upgradescope/internal/inventory"
	"github.com/abd-ulbasit/upgradescope/internal/server/store"
)

// Cluster administration: DELETE and PATCH /api/v1/clusters/{id}, behind
// Config.AdminToken. Decommissioning a cluster or renaming it rewrites
// history every reader sees, so neither the read token nor an ingest
// token may do it, and a server started without an admin token refuses
// both outright rather than leaving them open.

// maxAdminBody caps a PATCH body: {"name": "..."} needs well under 1 KiB.
const maxAdminBody = 4 << 10

// adminAuth gates a handler behind Config.AdminToken: 403 when no admin
// token is configured or a different token is presented, 401 when none is.
func (s *Server) adminAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		admin := s.tokens.admin() // read once: one request, one value
		if admin == "" {
			errJSON(w, http.StatusForbidden, "cluster administration is disabled: start the server with --admin-token")
			return
		}
		presented := bearerToken(r)
		if presented == "" {
			errJSON(w, http.StatusUnauthorized, "invalid or missing bearer token")
			return
		}
		if subtle.ConstantTimeCompare([]byte(presented), []byte(admin)) != 1 {
			errJSON(w, http.StatusForbidden, "this endpoint needs the admin token")
			return
		}
		next(w, withScope(r, fleetScope)) // the admin token administers every cluster
	}
}

// handleDeleteCluster: DELETE /api/v1/clusters/{id} — removes the cluster
// with its snapshots, evaluations, queued notifications and the ingest
// tokens minted for its name. 204 on success.
func (s *Server) handleDeleteCluster(w http.ResponseWriter, r *http.Request) {
	c, ok := s.requireCluster(w, r)
	if !ok {
		return
	}
	err := s.cfg.Store.DeleteCluster(r.Context(), c.Name)
	if errors.Is(err, store.ErrNotFound) {
		errJSON(w, http.StatusNotFound, "cluster not found")
		return
	}
	if err != nil {
		internalErr(w, "deleting cluster", err)
		return
	}
	log.Printf("server: admin: deleted cluster %q (id %d) with its history and ingest tokens", c.Name, c.ID)
	w.WriteHeader(http.StatusNoContent)
}

// renameRequest is the PATCH /api/v1/clusters/{id} body.
type renameRequest struct {
	Name *string `json:"name"`
}

// handleRenameCluster: PATCH /api/v1/clusters/{id} {"name": "new"} —
// renames the cluster; its history and ingest tokens move with it. The
// agent keeps pushing its own --cluster-name, so change that too: until
// then its pushes are refused (a per-cluster token is now bound to the
// new name) or, with the shared ingest token, register the old name anew.
func (s *Server) handleRenameCluster(w http.ResponseWriter, r *http.Request) {
	c, ok := s.requireCluster(w, r)
	if !ok {
		return
	}
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxAdminBody))
	if err != nil {
		errJSON(w, http.StatusRequestEntityTooLarge, "request body too large")
		return
	}
	var req renameRequest
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil || req.Name == nil {
		errJSON(w, http.StatusUnprocessableEntity, `body must be {"name": "<new cluster name>"}`)
		return
	}
	if msg := checkClusterName(*req.Name); msg != "" {
		errJSON(w, http.StatusUnprocessableEntity, msg)
		return
	}
	err = s.cfg.Store.RenameCluster(r.Context(), c.Name, *req.Name)
	switch {
	case errors.Is(err, store.ErrClusterNameTaken):
		errJSON(w, http.StatusConflict, fmt.Sprintf("cluster name %q is already registered", *req.Name))
		return
	case errors.Is(err, store.ErrNotFound):
		errJSON(w, http.StatusNotFound, "cluster not found")
		return
	case err != nil:
		internalErr(w, "renaming cluster", err)
		return
	}
	log.Printf("server: admin: renamed cluster %q (id %d) to %q", c.Name, c.ID, *req.Name)
	renamed, err := s.cfg.Store.GetCluster(r.Context(), c.ID)
	if err != nil {
		internalErr(w, "loading renamed cluster", err)
		return
	}
	writeJSON(w, http.StatusOK, renamed)
}

// checkClusterName returns why name cannot be a cluster's name, or "":
// the rule pushes and `tokens create` apply, an RFC 1123 subdomain.
func checkClusterName(name string) string {
	if err := inventory.ValidateClusterName(name); err != nil {
		return "name: " + strings.TrimPrefix(err.Error(), "clusterName: ")
	}
	return ""
}
