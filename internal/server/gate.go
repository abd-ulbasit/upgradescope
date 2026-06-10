package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"mime"
	"net/http"
	"strconv"

	"github.com/abd-ulbasit/upgradescope/internal/collect"
	"github.com/abd-ulbasit/upgradescope/internal/engine"
	"github.com/abd-ulbasit/upgradescope/internal/inventory"
	"github.com/abd-ulbasit/upgradescope/internal/sarif"
	"github.com/abd-ulbasit/upgradescope/internal/server/store"
)

// yamlContentTypes are the media types the gate accepts for a manifest
// stream. JSON is included: a JSON manifest is a valid single-document YAML
// stream. Empty Content-Type is also accepted (curl --data-binary default
// is application/x-www-form-urlencoded, so that one is rejected loudly).
var yamlContentTypes = map[string]bool{
	"application/x-yaml": true,
	"application/yaml":   true,
	"text/yaml":          true,
	"text/x-yaml":        true,
	"application/json":   true,
}

// handleGate implements POST /api/v1/gate?target=&cluster=&format= — the CI
// gate: evaluate a concatenated YAML manifest stream (request body) against
// a target version, optionally inside a known cluster's stored context.
// Nothing is persisted. Auth: read token (the gate reads cluster context;
// it never writes).
//
// With ?cluster=<id|name>, the cluster's latest inventory provides the
// evaluation context (server version, nodes, add-ons, deprecated calls,
// namespace team labels) and only APIUsage is replaced by the manifests —
// "would THESE manifests block THIS cluster's upgrade". Without it, the
// manifests are evaluated standalone (api-usage only, like scan --files).
func (s *Server) handleGate(w http.ResponseWriter, r *http.Request) {
	if ct := r.Header.Get("Content-Type"); ct != "" {
		mt, _, err := mime.ParseMediaType(ct)
		if err != nil || !yamlContentTypes[mt] {
			errJSON(w, http.StatusUnsupportedMediaType,
				fmt.Sprintf("unsupported Content-Type %q (send a YAML manifest stream as application/x-yaml)", ct))
			return
		}
	}
	targetQ := r.URL.Query().Get("target")
	if targetQ == "" {
		errJSON(w, http.StatusUnprocessableEntity, "target query parameter is required")
		return
	}
	target, err := inventory.ParseVersion(targetQ)
	if err != nil {
		errJSON(w, http.StatusUnprocessableEntity, "invalid target: "+err.Error())
		return
	}
	format := r.URL.Query().Get("format")
	switch format {
	case "", "json", "sarif":
	default:
		errJSON(w, http.StatusUnprocessableEntity, fmt.Sprintf("invalid format %q (want json or sarif)", format))
		return
	}

	manifests, err := collect.CollectManifests(http.MaxBytesReader(w, r.Body, maxSnapshotBody))
	if err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			errJSON(w, http.StatusRequestEntityTooLarge, "manifest stream exceeds the 20MiB limit")
			return
		}
		errJSON(w, http.StatusUnprocessableEntity, "invalid manifest stream: "+err.Error())
		return
	}

	inv := manifests
	if ref := r.URL.Query().Get("cluster"); ref != "" {
		clusterInv, ok := s.gateClusterContext(w, r, ref)
		if !ok {
			return
		}
		// Merge: cluster context + manifest API usage. The manifests are the
		// proposed state, so they fully replace APIUsage — including the
		// cluster's existing residencies — while every other signal (server
		// version, nodes, add-ons, deprecated calls, namespaces) stays.
		inv = clusterInv
		inv.APIUsage = manifests.APIUsage
		if inv.Capabilities == nil {
			inv.Capabilities = map[inventory.Capability]inventory.CapabilityStatus{}
		}
		inv.Capabilities[inventory.CapAPIUsage] = inventory.CapabilityStatus{Available: true}
	}
	inv.Namespaces = s.cfg.TeamMap.Apply(inv.Namespaces)

	rep := engine.Evaluate(inv, s.cfg.KB, target, s.now())
	if format == "sarif" {
		w.Header().Set("Content-Type", "application/sarif+json")
		w.WriteHeader(http.StatusOK)
		_ = sarif.Write(w, rep, s.cfg.Version)
		return
	}
	writeJSON(w, http.StatusOK, withTeams(rep))
}

// gateClusterContext resolves ?cluster=<id|name> to the cluster's latest
// stored inventory, writing the error response (404/500) itself on failure.
func (s *Server) gateClusterContext(w http.ResponseWriter, r *http.Request, ref string) (inventory.Inventory, bool) {
	ctx := r.Context()
	cluster, err := func() (store.Cluster, error) {
		if id, perr := strconv.ParseInt(ref, 10, 64); perr == nil {
			return s.cfg.Store.GetCluster(ctx, id)
		}
		clusters, lerr := s.cfg.Store.ListClusters(ctx)
		if lerr != nil {
			return store.Cluster{}, lerr
		}
		for _, c := range clusters {
			if c.Name == ref {
				return c, nil
			}
		}
		return store.Cluster{}, store.ErrNotFound
	}()
	if errors.Is(err, store.ErrNotFound) {
		errJSON(w, http.StatusNotFound, "cluster not found")
		return inventory.Inventory{}, false
	}
	if err != nil {
		internalErr(w, "resolving gate cluster", err)
		return inventory.Inventory{}, false
	}

	snap, err := s.cfg.Store.LatestSnapshot(ctx, cluster.ID)
	if errors.Is(err, store.ErrNotFound) {
		errJSON(w, http.StatusNotFound, "no snapshots for cluster")
		return inventory.Inventory{}, false
	}
	if err != nil {
		internalErr(w, "loading gate cluster snapshot", err)
		return inventory.Inventory{}, false
	}
	var inv inventory.Inventory
	if err := json.Unmarshal(snap.Inventory, &inv); err != nil {
		internalErr(w, "decoding gate cluster inventory", fmt.Errorf("snapshot %d: %w", snap.ID, err))
		return inventory.Inventory{}, false
	}
	return inv, true
}
