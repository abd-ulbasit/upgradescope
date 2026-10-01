package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
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
// "would THESE manifests block THIS cluster's upgrade". The cluster as it is
// is evaluated too (the baseline): findings it already has are tagged
// source "cluster" and kept for context, and the verdict judges only the
// findings the manifests introduce (source "manifest"), so a cluster's
// existing EOL add-on does not fail every PR. Without it, the manifests
// are evaluated standalone (api-usage only, like scan --files).
//
// ?fail-on=blocker|warning makes the gate fail like `scan --fail-on`: an
// introduced finding at or above the threshold, or an unknown verdict,
// answers 422 with the full body, so `curl -f` fails the CI step. Without
// it the status stays 200 (the v0.1 contract). X-Upgradescope-Verdict
// always carries the verdict (ready | blocked | unknown).
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
	failOn := r.URL.Query().Get("fail-on")
	switch failOn {
	case "", "blocker", "warning":
	default:
		errJSON(w, http.StatusUnprocessableEntity, fmt.Sprintf("invalid fail-on %q (want blocker or warning)", failOn))
		return
	}

	// The body stays charged to the shared buffered-body budget until it is
	// decoded, and the evaluation slot is held only for decoding and
	// evaluation: a client that stops reading the response must not pin it.
	// Both releases are idempotent; the defers cover the early returns.
	body, releaseBody, ok := s.readManifestBody(w, r)
	if !ok {
		return
	}
	defer releaseBody()
	releaseSlot, ok := s.acquireGateSlot(w, r)
	if !ok {
		return
	}
	defer releaseSlot()
	manifests, err := collect.CollectManifests(body.reader())
	releaseBody()
	if err != nil {
		errJSON(w, http.StatusUnprocessableEntity, "invalid manifest stream: "+err.Error())
		return
	}

	inv := manifests
	var baseline *engine.Report
	if ref := r.URL.Query().Get("cluster"); ref != "" {
		clusterInv, ok := s.gateClusterContext(w, r, ref)
		if !ok {
			return
		}
		clusterInv.Namespaces = s.cfg.TeamMap.Apply(clusterInv.Namespaces)
		// Baseline: the cluster as it is, so findings it already has are
		// not blamed on the PR.
		base := engine.Evaluate(clusterInv, s.cfg.KB, target, s.now())
		baseline = &base
		// Merge: cluster context + manifest API usage. The manifests are the
		// proposed state, so they fully replace APIUsage — including the
		// cluster's existing residencies — while every other signal (server
		// version, nodes, add-ons, deprecated calls, namespaces) stays.
		inv = clusterInv
		inv.APIUsage = manifests.APIUsage
		inv.Capabilities = maps.Clone(clusterInv.Capabilities)
		if inv.Capabilities == nil {
			inv.Capabilities = map[inventory.Capability]inventory.CapabilityStatus{}
		}
		inv.Capabilities[inventory.CapAPIUsage] = inventory.CapabilityStatus{Available: true}
	} else {
		inv.Namespaces = s.cfg.TeamMap.Apply(inv.Namespaces)
	}

	rep := engine.Evaluate(inv, s.cfg.KB, target, s.now())
	releaseSlot()
	resp := gateResult(rep, baseline)
	w.Header().Set("X-Upgradescope-Verdict", string(resp.Verdict))
	status := http.StatusOK
	if gateFails(resp, failOn) {
		status = http.StatusUnprocessableEntity
	}
	if format == "sarif" {
		w.Header().Set("Content-Type", "application/sarif+json")
		w.WriteHeader(status)
		_ = sarif.Write(w, sarifReport(rep, resp), s.cfg.Version)
		return
	}
	writeJSON(w, status, resp)
}

// Finding sources in a gate response: introduced by the manifests, or
// already present in the cluster without them.
const (
	sourceManifest = "manifest"
	sourceCluster  = "cluster"
)

type gateFinding struct {
	engine.Finding
	Source string `json:"source"` // sourceManifest | sourceCluster
}

// gateResponse is the proposed state's report (cluster + manifests) whose
// verdict judges only what the manifests introduce: Verdict and Ready
// shadow the report's, and ClusterVerdict keeps the whole proposed state's.
// Score stays the whole proposed state's, cluster findings included: gate
// on verdict (or fail-on), not on score.
type gateResponse struct {
	reportWithTeams
	Findings       []gateFinding  `json:"findings"`
	Verdict        engine.Verdict `json:"verdict"`
	Ready          bool           `json:"ready"`
	ClusterVerdict engine.Verdict `json:"clusterVerdict,omitempty"` // with ?cluster= only
}

// gateResult tags every finding of rep with its source and computes the
// verdict of the introduced ones. Without a baseline (no ?cluster=) every
// finding comes from the manifests. A finding is the cluster's when the
// baseline already has its key. The verdict is blocked on an introduced
// blocker, else unknown when the proposed state has a required gap (a
// blocker may have gone unseen), else ready.
func gateResult(rep engine.Report, baseline *engine.Report) gateResponse {
	existing := map[string]bool{}
	if baseline != nil {
		existing = keySet(baseline.Findings, func(engine.Finding) bool { return true })
	}
	resp := gateResponse{reportWithTeams: withTeams(rep), Findings: []gateFinding{}, Verdict: engine.VerdictReady}
	for _, f := range rep.Findings {
		src := sourceManifest
		if existing[findingKey(f)] {
			src = sourceCluster
		}
		resp.Findings = append(resp.Findings, gateFinding{Finding: f, Source: src})
		if src == sourceManifest && f.Severity == engine.SevBlocker {
			resp.Verdict = engine.VerdictBlocked
		}
	}
	if resp.Verdict == engine.VerdictReady {
		for _, g := range rep.NotAssessed {
			if g.Required {
				resp.Verdict = engine.VerdictUnknown
			}
		}
	}
	resp.Ready = resp.Verdict == engine.VerdictReady
	if baseline != nil {
		resp.ClusterVerdict = rep.Verdict
	}
	return resp
}

// sarifReport is what the SARIF answer carries. SARIF becomes code-scanning
// alerts on the PR, so it holds only the findings the manifests introduce,
// with the gate's verdict; findings the cluster already has stay in the
// JSON answer, tagged source cluster. Score stays the proposed state's.
func sarifReport(rep engine.Report, resp gateResponse) engine.Report {
	out := rep
	out.Findings = []engine.Finding{}
	for _, f := range resp.Findings {
		if f.Source == sourceManifest {
			out.Findings = append(out.Findings, f.Finding)
		}
	}
	out.Verdict, out.Ready = resp.Verdict, resp.Ready
	return out
}

// gateFails applies ?fail-on: "" never fails (the v0.1 always-200
// contract); blocker fails on an introduced blocker, warning on an
// introduced blocker or warning; both fail when the verdict is unknown,
// like `scan --fail-on`.
func gateFails(resp gateResponse, failOn string) bool {
	if failOn == "" {
		return false
	}
	if resp.Verdict != engine.VerdictReady {
		return true
	}
	if failOn == "warning" {
		for _, f := range resp.Findings {
			if f.Source == sourceManifest && f.Severity == engine.SevWarning {
				return true
			}
		}
	}
	return false
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
