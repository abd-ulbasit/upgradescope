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
	"unicode/utf8"

	"github.com/abd-ulbasit/upgradescope/internal/codequality"
	"github.com/abd-ulbasit/upgradescope/internal/collect"
	"github.com/abd-ulbasit/upgradescope/internal/engine"
	"github.com/abd-ulbasit/upgradescope/internal/inventory"
	"github.com/abd-ulbasit/upgradescope/internal/junit"
	"github.com/abd-ulbasit/upgradescope/internal/sarif"
	"github.com/abd-ulbasit/upgradescope/internal/server/store"
	"github.com/abd-ulbasit/upgradescope/internal/suppress"
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
// standalone, like scan --files (API usage, add-ons and CRDs). A
// team-scoped read's cluster is the scope's share of it
// (readScope.clusterShare), for every part of the answer, verdict and
// status included (gateResponse.scope).
//
// ?fail-on=blocker|warning|never makes the gate fail like `scan --fail-on`,
// whose default it shares (blocker): an introduced finding at or above the
// threshold, or an unknown verdict, answers 422 with the full body, so
// `curl --fail-with-body` fails the CI step and keeps the report. never
// always answers 200 (the v0.1 contract, which no longer is the default:
// a bare request must be able to fail CI). X-Upgradescope-Verdict always
// carries the verdict (ready | blocked | unknown).
//
// ?allow-incomplete=true (true or false, default false) is `scan
// --allow-incomplete`: the gate decides on findings alone, so an unknown
// verdict without a finding at the threshold answers 200. The verdict, its
// header and the required gaps in the body still say unknown, and a target
// that is not an upgrade of the cluster (the target gap) still fails.
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
//
// An answer whose bound in its format (gateAnswerBound) is over
// --max-gate-bytes is 413 before it is encoded, and ?path= is at most
// maxArtifactPathBytes: both multiply what the answer lists.
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
	allowIncomplete, ok := gateAllowIncomplete(w, r)
	if !ok {
		return
	}
	artifact := r.URL.Query().Get("path")
	if len(artifact) > maxArtifactPathBytes {
		errJSON(w, http.StatusUnprocessableEntity, fmt.Sprintf(
			"path is %d bytes, over the %d bytes a repository path may have here (every object in the answer carries it)",
			len(artifact), maxArtifactPathBytes))
		return
	}
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
	// decoded, and the evaluation slot is held for measuring what its
	// aliases expand to, decoding, evaluation and encoding the response,
	// the steps whose memory follows the YAML's structure and the cluster's
	// stored inventory. The response is written to memory in the slot and
	// sent as a read's is (send): a client that stops reading holds only
	// its bytes, under the held-response budget, never the slot nor the
	// reports it was built from. Both releases are idempotent; the defers
	// cover the early returns.
	body, shape, releaseBody, ok := s.readManifestBody(w, r)
	if !ok {
		return
	}
	defer releaseBody()
	releaseSlot, ok := acquireSlot(w, r.Context().Done(), s.gateSlots, s.gateQueueTimeout, "too many concurrent gate evaluations; retry shortly")
	if !ok {
		return
	}
	defer releaseSlot()
	resp := newHeldResponse()
	s.evaluateGate(resp, r, gateRequest{body: body, shape: shape, releaseBody: releaseBody,
		target: target, format: format, failOn: failOn, allowIncomplete: allowIncomplete, artifact: artifact, rules: rules})
	s.send(w, resp, releaseSlot)
}

// gateRequest is a /gate request whose parameters are checked and whose
// body is read.
type gateRequest struct {
	body        bufferedBody
	shape       *manifestShape
	releaseBody func()
	target      inventory.Version
	format      string
	failOn      string
	artifact    string
	rules       []suppress.Rule // ?config='s ignore rules

	allowIncomplete bool // ?allow-incomplete=true
}

// gateAllowIncomplete reads ?allow-incomplete: true or false, given once,
// absent meaning false. Anything else is a 422, like a bad fail-on, so a
// typo cannot silently leave the gate stricter or laxer than asked.
func gateAllowIncomplete(w http.ResponseWriter, r *http.Request) (value, ok bool) {
	switch vals := r.URL.Query()["allow-incomplete"]; {
	case len(vals) == 0:
		return false, true
	case len(vals) == 1 && vals[0] == "true":
		return true, true
	case len(vals) == 1 && vals[0] == "false":
		return false, true
	}
	errJSON(w, http.StatusUnprocessableEntity, "invalid allow-incomplete (want true or false, once)")
	return false, false
}

// evaluateGate decodes and evaluates g, in the evaluation slot, and
// writes the answer to w. What it builds the answer from is garbage once
// it returns.
func (s *Server) evaluateGate(w http.ResponseWriter, r *http.Request, g gateRequest) {
	if status, msg := g.shape.checkAliases(s.maxGateBytes()); status != 0 {
		errJSON(w, status, msg)
		return
	}
	manifests, err := collect.CollectManifests(g.body.reader(), s.cfg.KB.AddOns)
	g.releaseBody()
	if err != nil {
		errJSON(w, http.StatusUnprocessableEntity, "invalid manifest stream: "+err.Error())
		return
	}
	for _, u := range manifests.APIUsage { // refs carry stream lines; ?path= names their file
		for i := range u.Objects {
			u.Objects[i].File = g.artifact
		}
	}

	target := g.target
	sc := scopeOf(r)
	var ev gateEval
	var cut *gateCut // a team-scoped ?cluster= gate's: what its caller may see of the share
	if ref := r.URL.Query().Get("cluster"); ref != "" {
		clusterInv, ok := s.gateClusterContext(w, r, ref)
		if !ok {
			return
		}
		clusterInv.Namespaces = s.cfg.TeamMap.Apply(clusterInv.Namespaces)
		// A team-scoped gate evaluates the manifests within the scope's
		// share of the cluster only, never the whole cluster: the engine
		// counts and words every finding, and the answer's verdict,
		// status, cluster verdict and score are decided, from the scope's
		// evidence and the PR's alone (readScope.clusterShare). Not even
		// a 413 from the whole cluster's evaluation can tell the caller
		// of another team's. The fleet-wide share is the cluster.
		clusterInv = sc.clusterShare(clusterInv)
		if ev, ok = s.gateWithin(w, clusterInv, manifests, target, g.rules, ref); !ok {
			return
		}
		if !sc.fleet() {
			cut = &gateCut{namespaces: namespaceTeamsOf(clusterInv.Namespaces), mine: ev.mine}
		}
	} else {
		inv := manifests
		inv.Namespaces = s.cfg.TeamMap.Apply(inv.Namespaces)
		full, err := s.evaluateWithin(inv, target, s.now())
		if err != nil {
			errJSON(w, http.StatusRequestEntityTooLarge, err.Error())
			return
		}
		ev.full = full
	}

	rep, warnings := s.suppressGate(ev.full, g.rules)
	resp := gateResult(rep, ev.baseline, ev.introduced)
	if cut != nil { // without ?cluster= the answer is all the caller's own
		resp.scope(sc, *cut)
	}
	resp.reportWithTeams = s.versioned(resp.reportWithTeams)
	resp.Warnings = warnings
	bound := gateAnswerBound(resp, g.format)
	if s.observeGateBound != nil {
		s.observeGateBound(bound)
	}
	if limit := s.gateAnswerLimit(); bound > limit {
		errJSON(w, http.StatusRequestEntityTooLarge, fmt.Sprintf(
			"the answer to this stream could take up to %d bytes, over the %s limit for a /gate answer "+
				"(it lists every object the findings name, with its name, namespace and path); split the stream into several requests"+
				" (with ?cluster=, the cluster's own findings count too)", bound, sizeString(limit)))
		return
	}
	w.Header().Set("X-Upgradescope-Verdict", string(resp.Verdict))
	status := http.StatusOK
	if gateFails(resp, g.failOn, g.allowIncomplete) {
		status = http.StatusUnprocessableEntity
	}
	if g.format == "sarif" {
		w.Header().Set("Content-Type", "application/sarif+json")
		w.WriteHeader(status)
		_ = sarif.Write(w, sarifReport(resp.Report, resp), s.cfg.Version)
		return
	}
	if g.format == "junit" {
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(status)
		_ = junit.Write(w, sarifReport(resp.Report, resp), junit.Options{FailOn: g.failOn, AllowIncomplete: g.allowIncomplete})
		return
	}
	if g.format == "gitlab-codequality" {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = codequality.Write(w, sarifReport(resp.Report, resp))
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
// suppressed, counted in SuppressedCount) count toward none of them. For
// a team-scoped read the proposed state is the scope's share of the
// cluster with the manifests (gateResponse.scope).
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

// gateCut is what scope needs of a team-scoped ?cluster= gate: the
// share's namespace teams and what the manifests themselves hold.
type gateCut struct {
	namespaces map[string]string
	mine       manifestContent
}

// manifestContent is what a gate's manifests hold: their objects, the
// namespaces they name and their unrecognized images.
type manifestContent struct {
	objects    map[inventory.ObjectRef]bool
	namespaces map[string]bool
	images     map[string]bool
}

// manifestsOwn collects the objects and namespaces of the manifests and of
// their side of the merge (mergeManifests), whose CRD usage holds the
// manifests' custom resources.
func manifestsOwn(invs ...inventory.Inventory) manifestContent {
	m := manifestContent{objects: map[inventory.ObjectRef]bool{}, namespaces: map[string]bool{}, images: map[string]bool{}}
	usage := func(us []inventory.APIUsage) {
		for _, u := range us {
			for ns := range u.Namespaces {
				m.namespaces[ns] = true
			}
			for _, o := range u.Objects {
				m.objects[o], m.namespaces[o.Namespace] = true, true
			}
		}
	}
	for _, inv := range invs {
		usage(inv.APIUsage)
		for _, c := range inv.CRDs {
			usage(c.Usage)
		}
		for _, a := range inv.AddOns {
			for _, ns := range a.Namespaces {
				m.namespaces[ns] = true
			}
		}
		for _, n := range inv.Namespaces {
			m.namespaces[n.Name] = true
		}
		for _, img := range inv.UnrecognizedImages {
			m.images[img] = true
		}
	}
	delete(m.namespaces, "")
	return m
}

// scope makes g, the answer to a ?cluster= gate over the scope's share of
// the cluster (readScope.clusterShare), the answer sc reads. The share
// holds only the scope's evidence and what describes the cluster as a
// whole (its version, nodes, control plane, CRD definitions and
// capabilities), and the manifests are the caller's own, so all of g is
// decided from those alone: every finding is counted, titled and
// detailed from the scope's evidence and the PR's, never merged with
// another team's, one no team owns or the cluster's apiserver callers,
// and the gate's verdict (with its status and ?fail-on), the cluster
// verdict and the score judge the share with the PR, never the whole
// cluster. A CI status therefore depends on who asks: a PR that breaks
// only another team's workloads (say a shared CRD that stops serving a
// version only they write) passes a team-scoped gate, and fails the
// fleet-wide one. Of g's findings, one of the cluster's own is kept when
// sc owns it; the verdict, which judges only what the manifests
// introduce, is unchanged by that. Each is still cut (keep.cut) to sc's
// teams, the namespaces the cluster attributes to them and the
// manifests' namespaces and objects, which leaves the share's as they
// are: the cut is a second line, for evidence a share would let through.
// The team scores are sc's teams', the capability gaps have the helm
// collector's own words withheld (readScope.withholds), and the
// unrecognized images are the ones the manifests carry.
func (g *gateResponse) scope(sc readScope, c gateCut) {
	if sc.fleet() {
		return
	}
	cluster := sc.clusterKeep(c.namespaces)
	k := keep{
		team:      cluster.team,
		namespace: func(ns string) bool { return cluster.namespace(ns) || c.mine.namespaces[ns] },
		object:    func(o inventory.ObjectRef) bool { return cluster.object(o) || c.mine.objects[o] },
	}
	introduced := map[string]bool{}
	for _, f := range g.introducedSuppressed {
		introduced[findingKey(f.Finding)] = true
	}
	g.Teams = sc.renderedTeams(g.Report)
	findings := g.Findings
	g.Findings, g.Report.Findings = []gateFinding{}, []engine.Finding{}
	for _, f := range findings {
		if f.Source == sourceCluster && !sc.owns(f.Teams) {
			continue
		}
		f.Finding, _ = k.cut(f.Finding)
		g.Findings = append(g.Findings, f)
		g.Report.Findings = append(g.Report.Findings, f.Finding)
	}
	g.Report.Suppressed = sc.suppressed(g.Report.Suppressed, k, func(f engine.Finding) bool { return introduced[findingKey(f)] })
	g.SuppressedCount = len(g.Report.Suppressed)
	g.introducedSuppressed = sc.suppressed(g.introducedSuppressed, k, func(engine.Finding) bool { return true })
	g.Report.UnrecognizedImages = slices.DeleteFunc(slices.Clone(g.Report.UnrecognizedImages), func(img string) bool { return !c.mine.images[img] })
	g.Report.UnrecognizedImagesOmitted = 0
	g.Report.NotAssessed = sc.scopeGaps(g.Report.NotAssessed)
}

// gateEval is a gate's evaluations of the manifests within one cluster
// inventory (gateWithin): the proposed state's, unsuppressed; the
// baseline, the cluster as it is; what the manifests introduce; and what
// they hold. Without ?cluster=, full alone is set.
type gateEval struct {
	full       engine.Report
	baseline   *engine.Report
	introduced gateSide
	mine       manifestContent
}

// gateWithin evaluates the manifests within clusterInv (of the cluster
// ref names, its namespaces' teams applied), writing the 413 itself when
// an evaluation is over the report limit. Baseline: the cluster as it
// is, so findings it already has are not blamed on the PR. Merge: the
// manifest objects are upserted into the cluster's API usage (see
// upsertUsage), and every other signal (server version, nodes,
// deprecated calls, namespaces) stays; the manifests' add-ons and CRDs
// are merged too (mergeManifests), and the findings the manifests' own
// content produces, once suppressed, are introduced by the PR
// (suppressSide). clusterInv is not modified.
func (s *Server) gateWithin(w http.ResponseWriter, clusterInv, manifests inventory.Inventory, target inventory.Version,
	rules []suppress.Rule, ref string) (gateEval, bool) {
	base, err := s.evaluateWithin(clusterInv, target, s.now())
	if err != nil {
		errJSON(w, http.StatusRequestEntityTooLarge, "?cluster="+ref+": "+err.Error())
		return gateEval{}, false
	}
	inv := clusterInv
	inv.APIUsage = upsertUsage(clusterInv.APIUsage, manifests.APIUsage)
	inv.Capabilities = maps.Clone(clusterInv.Capabilities)
	if inv.Capabilities == nil {
		inv.Capabilities = map[inventory.Capability]inventory.CapabilityStatus{}
	}
	inv.Capabilities[inventory.CapAPIUsage] = inventory.CapabilityStatus{Available: true}
	side := mergeManifests(&inv, manifests)
	sideRep, err := s.evaluateWithin(side, target, s.now())
	if err != nil {
		errJSON(w, http.StatusRequestEntityTooLarge, err.Error())
		return gateEval{}, false
	}
	full, err := s.evaluateWithin(inv, target, s.now())
	if err != nil {
		errJSON(w, http.StatusRequestEntityTooLarge, err.Error())
		return gateEval{}, false
	}
	return gateEval{full: full, baseline: &base, introduced: s.suppressSide(sideRep, rules), mine: manifestsOwn(manifests, side)}, true
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

// maxArtifactPathBytes caps ?path=. Every object ref in the answer carries
// the path, up to inventory.MaxObjectRefs per finding, so its length
// multiplies the answer's: unbounded, a 60 KB path and a 1.2 MB stream
// (the KB's 136 deprecated or removed GVKs, 100 objects each) made an
// 817 MB answer. Repository paths are rarely over 200 bytes.
const maxArtifactPathBytes = 512

// repoPath reports whether p can name a file in the repository: relative,
// slash-separated, clean, inside the repository, valid UTF-8 without
// control characters.
func repoPath(p string) bool {
	return !strings.HasPrefix(p, "/") && !strings.Contains(p, `\`) && path.Clean(p) == p &&
		p != "." && p != ".." && !strings.HasPrefix(p, "../") && !strings.ContainsFunc(p, unicode.IsControl) && utf8.ValidString(p)
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
// (cluster refs are dropped first), so SARIF can still place them; a row
// that then lists only manifest refs while it counts the cluster's objects
// too is worded as both, not as manifests alone (engine.listedObjects).
// The cluster refs dropped from the listing stay in Unlisted, still live:
// a Helm release's stored copy of one is left to the live finding as in
// the baseline. Otherwise the room the manifests take could make a
// release's manifest finding new in the proposed state, which gateResult
// blames on the PR (fail closed), and the verdict would turn on how many
// of the cluster's refs at that API the collector listed. What
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
		u.Objects, u.Unlisted, u.Namespaces = slices.Clone(u.Objects), slices.Clone(u.Unlisted), maps.Clone(u.Namespaces)
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
			u.Unlisted = append(u.Unlisted, kept[room:]...)
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
// like `scan --fail-on`. With allowIncomplete (?allow-incomplete=true) an
// unknown verdict does not fail on its own, as in `scan --allow-incomplete`;
// a target that is not an upgrade of the cluster is not a coverage limit, so
// it still does.
func gateFails(resp gateResponse, failOn string, allowIncomplete bool) bool {
	if failOn == "never" {
		return false
	}
	switch {
	case resp.Verdict == engine.VerdictBlocked:
		return true
	case resp.Verdict == engine.VerdictUnknown && !allowIncomplete:
		return true
	case resp.Verdict == engine.VerdictUnknown:
		for _, g := range resp.NotAssessed {
			if g.Capability == engine.GapTarget {
				return true
			}
		}
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
// cluster id 1. A scoped request resolves ref among the scope's clusters
// only: a name matches an in-scope cluster's, so an out-of-scope cluster
// named like an in-scope id neither shadows that id nor shows by a 404
// that it exists, and an id outside the scope is never looked up.
func (s *Server) gateClusterContext(w http.ResponseWriter, r *http.Request, ref string) (inventory.Inventory, bool) {
	ctx := r.Context()
	// The scope first, whatever ref names, so a cluster outside it costs
	// what an unknown one does.
	scoped, err := s.scopeClusters(ctx, scopeOf(r))
	if err != nil {
		internalErr(w, "resolving gate cluster", err)
		return inventory.Inventory{}, false
	}
	in := func(id int64) bool { return scoped == nil || scoped[id] }
	cluster, err := func() (store.Cluster, error) {
		clusters, lerr := s.cfg.Store.ListClusters(ctx)
		if lerr != nil {
			return store.Cluster{}, lerr
		}
		for _, c := range clusters {
			if c.Name == ref && in(c.ID) {
				return c, nil
			}
		}
		if id, perr := strconv.ParseInt(ref, 10, 64); perr == nil && in(id) {
			return s.cfg.Store.GetCluster(ctx, id)
		}
		// Outside the scope: as unknown as a cluster that does not exist.
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
	inv.CutFreeText() // as ingest judged it (decodeInventory)
	return inv, true
}
