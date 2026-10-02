package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"mime"
	"net/http"
	"path"
	"slices"
	"strconv"
	"strings"
	"unicode"

	"github.com/abd-ulbasit/upgradescope/internal/codequality"
	"github.com/abd-ulbasit/upgradescope/internal/collect"
	"github.com/abd-ulbasit/upgradescope/internal/engine"
	"github.com/abd-ulbasit/upgradescope/internal/inventory"
	"github.com/abd-ulbasit/upgradescope/internal/junit"
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
// With ?cluster=<name|id>, the cluster's latest inventory provides the
// evaluation context (server version, nodes, add-ons, deprecated calls,
// namespace team labels) and the manifest objects are upserted into its
// API usage, the add-ons they deploy into its add-ons, and their CRDs and
// custom resources into its CRDs (mergeManifests) — "would THESE
// manifests block THIS cluster's upgrade". The cluster as it is is
// evaluated too (the baseline): findings it already has are tagged source
// "cluster" and kept for context, and the verdict judges only the findings
// the manifests introduce (source "manifest"), so a cluster's existing EOL
// add-on does not fail every PR. A removed or deprecated API that a
// manifest object uses, an add-on the manifests deploy and a custom
// resource they write at a version the CRDs do not serve are always
// introduced, even when the cluster already has the same (see
// introducedKeys and gateResult). Without it, the manifests are evaluated
// standalone, like scan --files (API usage, add-ons and CRDs).
//
// ?fail-on=blocker|warning|never makes the gate fail like `scan --fail-on`,
// whose default it shares (blocker): an introduced finding at or above the
// threshold, or an unknown verdict, answers 422 with the full body, so
// `curl --fail-with-body` fails the CI step and keeps the report. never
// always answers 200 (the v0.1 contract, which no longer is the default:
// a bare request must be able to fail CI). X-Upgradescope-Verdict always
// carries the verdict (ready | blocked | unknown).
//
// ?path=<file> names the repository file the stream was rendered to (e.g.
// deploy/rendered.yaml): manifest objects carry it with their stream line,
// so format=sarif places introduced findings there and code scanning shows
// them on the PR. Without it a posted stream has no file, and SARIF lists
// its findings as tool execution notifications only.
//
// format=junit (JUnit XML whose outcomes follow fail-on) and
// format=gitlab-codequality (GitLab Code Quality; findings without a file
// are on the virtual upgradescope/ path) answer like sarif: the introduced
// findings only, with the gate's status and verdict.
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
	case "", "json", "sarif", "junit", "gitlab-codequality":
	default:
		errJSON(w, http.StatusUnprocessableEntity, fmt.Sprintf("invalid format %q (want json, sarif, junit or gitlab-codequality)", format))
		return
	}
	failOn := r.URL.Query().Get("fail-on")
	switch failOn {
	case "":
		failOn = "blocker"
	case "blocker", "warning", "never":
	default:
		errJSON(w, http.StatusUnprocessableEntity, fmt.Sprintf("invalid fail-on %q (want blocker, warning or never)", failOn))
		return
	}
	artifact := r.URL.Query().Get("path")
	if artifact != "" && !repoPath(artifact) {
		errJSON(w, http.StatusUnprocessableEntity,
			fmt.Sprintf("invalid path %q (want the repository-relative file the stream was rendered to, e.g. deploy/rendered.yaml)", artifact))
		return
	}
	rules, ok := gateIgnoreRules(w, r)
	if !ok {
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
	releaseSlot, ok := acquireSlot(w, r.Context().Done(), s.gateSlots, s.gateQueueTimeout, "too many concurrent gate evaluations; retry shortly")
	if !ok {
		return
	}
	defer releaseSlot()
	manifests, err := collect.CollectManifests(body.reader(), s.cfg.KB.AddOns)
	releaseBody()
	if err != nil {
		errJSON(w, http.StatusUnprocessableEntity, "invalid manifest stream: "+err.Error())
		return
	}
	for _, u := range manifests.APIUsage { // refs carry stream lines; ?path= names their file
		for i := range u.Objects {
			u.Objects[i].File = artifact
		}
	}

	inv := manifests
	var baseline *engine.Report
	var introduced gateSide
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
		// Merge: cluster context + manifest API usage. The manifest objects
		// are upserted into the cluster's API usage (see upsertUsage), and
		// every other signal (server version, nodes, deprecated calls,
		// namespaces) stays.
		inv = clusterInv
		inv.APIUsage = upsertUsage(clusterInv.APIUsage, manifests.APIUsage)
		inv.Capabilities = maps.Clone(clusterInv.Capabilities)
		if inv.Capabilities == nil {
			inv.Capabilities = map[inventory.Capability]inventory.CapabilityStatus{}
		}
		inv.Capabilities[inventory.CapAPIUsage] = inventory.CapabilityStatus{Available: true}
		// The manifests' add-ons and CRDs are merged too (mergeManifests),
		// and the findings the manifests' own content produces, once
		// suppressed, are introduced by the PR (suppressSide).
		side := mergeManifests(&inv, manifests)
		introduced = s.suppressSide(engine.Evaluate(side, s.cfg.KB, target, s.now()), rules)
	} else {
		inv.Namespaces = s.cfg.TeamMap.Apply(inv.Namespaces)
	}

	rep, warnings := s.suppressGate(engine.Evaluate(inv, s.cfg.KB, target, s.now()), rules)
	releaseSlot()
	resp := gateResult(rep, baseline, introduced)
	resp.reportWithTeams = s.versioned(resp.reportWithTeams)
	resp.Warnings = warnings
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
	if format == "junit" {
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(status)
		_ = junit.Write(w, sarifReport(rep, resp), junit.Options{FailOn: failOn})
		return
	}
	if format == "gitlab-codequality" {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = codequality.Write(w, sarifReport(rep, resp))
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
// on verdict (or fail-on), not on score. Suppressed findings (the report's
// suppressed, counted in SuppressedCount) count toward none of them.
type gateResponse struct {
	reportWithTeams
	Findings        []gateFinding  `json:"findings"`
	Verdict         engine.Verdict `json:"verdict"`
	Ready           bool           `json:"ready"`
	ClusterVerdict  engine.Verdict `json:"clusterVerdict,omitempty"` // with ?cluster= only
	SuppressedCount int            `json:"suppressedCount"`          // len(Suppressed)
	// Warnings are suppression's (see suppressGate): scan prints them on
	// stderr, the gate's JSON answer carries them.
	Warnings []string `json:"warnings,omitempty"`
	// introducedSuppressed are the suppressed findings attributed to the
	// manifests, for the formats that hold only what they introduce.
	introducedSuppressed []engine.SuppressedFinding
}

// gateResult tags every finding of rep with its source and computes the
// verdict of the introduced ones. Without a baseline (no ?cluster=) every
// finding comes from the manifests. With one, attribution is per object: a
// finding the manifests' own objects produce (introduced, by key) is the
// manifests', whatever the cluster already has at that API. Of the rest,
// deprecated-api-in-use is the cluster's (only the cluster's apiserver
// metrics supply caller rows), as is any finding whose key the baseline
// has; anything else is the manifests' (fail closed). The verdict is
// blocked on an introduced blocker, else unknown when the proposed state
// has a required gap (a blocker may have gone unseen), else ready. The
// suppressed findings the CI formats carry are all of rep's without a
// baseline, else the manifests' side's (introduced.suppressed).
func gateResult(rep engine.Report, baseline *engine.Report, introduced gateSide) gateResponse {
	existing := map[string]bool{}
	if baseline != nil {
		existing = keySet(baseline.Findings, func(engine.Finding) bool { return true })
	}
	source := func(f engine.Finding) string {
		if baseline != nil && !introduced.keys[findingKey(f)] && (f.Category == engine.CatDeprecatedAPIInUse || existing[findingKey(f)]) {
			return sourceCluster
		}
		return sourceManifest
	}
	resp := gateResponse{reportWithTeams: withTeams(rep), Findings: []gateFinding{}, Verdict: engine.VerdictReady, SuppressedCount: len(rep.Suppressed)}
	resp.introducedSuppressed = rep.Suppressed
	if baseline != nil {
		resp.introducedSuppressed = introduced.suppressed
	}
	for _, f := range rep.Findings {
		src := source(f)
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
// The JUnit and Code Quality answers, which land on the PR too, carry it.
// Suppressed findings are kept on the same terms: the manifests' only.
func sarifReport(rep engine.Report, resp gateResponse) engine.Report {
	out := rep
	out.Suppressed = resp.introducedSuppressed
	out.Findings = []engine.Finding{}
	for _, f := range resp.Findings {
		if f.Source == sourceManifest {
			out.Findings = append(out.Findings, f.Finding)
		}
	}
	out.Verdict, out.Ready = resp.Verdict, resp.Ready
	return out
}

// repoPath reports whether p can name a file in the repository: relative,
// slash-separated, clean, inside the repository, without control
// characters.
func repoPath(p string) bool {
	return !strings.HasPrefix(p, "/") && !strings.Contains(p, `\`) && path.Clean(p) == p &&
		p != "." && p != ".." && !strings.HasPrefix(p, "../") && !strings.ContainsFunc(p, unicode.IsControl)
}

// usageKeys returns the keys of the API-usage findings (removed or
// deprecated API) in rep.
func usageKeys(rep engine.Report) map[string]bool {
	return keySet(rep.Findings, func(f engine.Finding) bool {
		return f.Category == engine.CatRemovedAPI || f.Category == engine.CatDeprecatedAPI
	})
}

// upsertUsage is the proposed state's API usage: the cluster's, with each
// manifest object upserted by group/version/kind and namespace/name — it
// replaces the cluster's listed object of that identity, or is added. The
// cluster's rows keep their order and new GVKs come after them, so a row
// the engine folds apiserver caller evidence into is the same row in the
// baseline and the proposed state: the fold is identical on both sides,
// and caller evidence never resurfaces as standalone findings blamed on
// the PR. Manifest refs keep their place under the MaxObjectRefs cap
// (cluster refs are dropped first), so SARIF can still place them. What
// the manifests delete or move to another API stays invisible: a stream
// says what it applies, not what it removes. Identity is the exact
// namespace and name, so a rendered manifest without a namespace (applied
// to kubectl's default) does not replace its namespaced twin in the
// cluster and is counted beside it: Count and Namespaces overstate by one.
func upsertUsage(cluster, manifests []inventory.APIUsage) []inventory.APIUsage {
	type gvk struct{ group, version, kind string }
	out := make([]inventory.APIUsage, 0, len(cluster)+len(manifests))
	at := map[gvk]int{}
	for _, u := range cluster {
		u.Objects, u.Namespaces = slices.Clone(u.Objects), maps.Clone(u.Namespaces)
		at[gvk{u.Group, u.Version, u.Kind}] = len(out)
		out = append(out, u)
	}
	for _, m := range manifests {
		i, ok := at[gvk{m.Group, m.Version, m.Kind}]
		if !ok {
			at[gvk{m.Group, m.Version, m.Kind}] = len(out)
			out = append(out, m)
			continue
		}
		u := &out[i]
		if u.Namespaces == nil {
			u.Namespaces = map[string]int{}
		}
		var kept []inventory.ObjectRef
		for _, o := range u.Objects {
			if o.Name != "" && slices.ContainsFunc(m.Objects, func(n inventory.ObjectRef) bool { return n.Name == o.Name && n.Namespace == o.Namespace }) {
				u.Count--
				if u.Namespaces[o.Namespace]--; u.Namespaces[o.Namespace] <= 0 {
					delete(u.Namespaces, o.Namespace)
				}
				continue
			}
			kept = append(kept, o)
		}
		u.Count += m.Count
		for ns, n := range m.Namespaces {
			u.Namespaces[ns] += n
		}
		if room := max(0, inventory.MaxObjectRefs-len(m.Objects)); len(kept) > room {
			u.ObjectsOmitted += len(kept) - room
			kept = kept[:room]
		}
		u.Objects = append(kept, m.Objects...)
		u.ObjectsOmitted += m.ObjectsOmitted
	}
	return out
}

// gateFails applies ?fail-on: never never fails (the v0.1 always-200
// contract); blocker fails on an introduced blocker, warning on an
// introduced blocker or warning; both fail when the verdict is unknown,
// like `scan --fail-on`.
func gateFails(resp gateResponse, failOn string) bool {
	if failOn == "never" {
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

// gateClusterContext resolves ?cluster=<name|id> to the cluster's latest
// stored inventory, writing the error response (404/500) itself on failure.
// A name wins over an id, so a cluster named "1" is never mistaken for
// cluster id 1.
func (s *Server) gateClusterContext(w http.ResponseWriter, r *http.Request, ref string) (inventory.Inventory, bool) {
	ctx := r.Context()
	cluster, err := func() (store.Cluster, error) {
		clusters, lerr := s.cfg.Store.ListClusters(ctx)
		if lerr != nil {
			return store.Cluster{}, lerr
		}
		for _, c := range clusters {
			if c.Name == ref {
				return c, nil
			}
		}
		if id, perr := strconv.ParseInt(ref, 10, 64); perr == nil {
			return s.cfg.Store.GetCluster(ctx, id)
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
