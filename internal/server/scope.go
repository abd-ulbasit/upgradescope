package server

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"net/netip"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/abd-ulbasit/upgradescope/internal/engine"
	"github.com/abd-ulbasit/upgradescope/internal/inventory"
	"github.com/abd-ulbasit/upgradescope/internal/server/store"
)

// Read scopes (#72). Every read is authorized for a scope: the whole fleet
// (--read-token, the admin token, a read token minted for "*", or an open
// read API), or a set of teams (a read token minted for them, or the team
// header of a trusted proxy). A team scope reads:
//
//   - the clusters it is attributed to: those whose current evaluations
//     (each target's newest of the latest snapshot), written with the
//     current --team-map (store.ClustersOfTeams), name one of its teams
//     in their Teams, the teams the evaluated inventory attributes a
//     namespace to (its team label, or the --team-map rule that overrides
//     it, as the engine attributes findings). A cluster of a team with no
//     finding is in scope; one with no current evaluation is in no
//     team's scope. Any other cluster answers exactly as an unknown one
//     (404) and is left out of every list and rollup.
//   - of a cluster's report, the findings attributed to one of its teams,
//     and the team scores of its teams. Findings no team owns are another
//     team's as far as it can tell, so they are left out too; its team
//     verdict still counts them (engine.TeamScores), as the fleet-wide
//     view does. A finding that spans teams is cut to the scope's teams,
//     their namespaces and the objects in them (keep.cut). What
//     describes the cluster as a whole stays: its score, verdict, blocker
//     and warning counts, capability gaps, history; but not its
//     unrecognized images, which name any team's workloads, nor the helm
//     capability's reason and skipped list, which name releases
//     (readScope.withholds).
//
// /metrics, whose per-cluster series name every cluster, takes a
// fleet-wide scope only (403 otherwise).

// scopeHeader names the teams a scoped read was answered for, so the
// dashboard can say its view is filtered. Fleet-wide reads carry none.
const scopeHeader = "X-Upgradescope-Teams"

// readScope is what one read request may see.
type readScope struct {
	teams map[string]bool // nil: the whole fleet
}

// fleetScope reads everything.
var fleetScope = readScope{}

// scopeOfTeams is the scope of a read token's or the proxy header's teams:
// the whole fleet when they include store.ReadScopeFleet.
func scopeOfTeams(teams []string) readScope {
	if slices.Contains(teams, store.ReadScopeFleet) {
		return fleetScope
	}
	m := make(map[string]bool, len(teams))
	for _, t := range teams {
		if t != "" {
			m[t] = true
		}
	}
	return readScope{teams: m}
}

func (sc readScope) fleet() bool { return sc.teams == nil }

// names is the scope's teams, sorted.
func (sc readScope) names() []string {
	out := make([]string, 0, len(sc.teams))
	for t := range sc.teams {
		out = append(out, t)
	}
	slices.Sort(out)
	return out
}

// owns reports whether something attributed to teams is in the scope.
func (sc readScope) owns(teams []string) bool {
	return sc.fleet() || slices.ContainsFunc(teams, func(t string) bool { return sc.teams[t] })
}

// report is rep as the scope sees it: the findings and suppressed
// findings it owns, each cut to the scope (cut), with ns the evaluated
// inventory's namespace teams (namespaceTeams); no unrecognized images,
// which no namespace is attributed (an image is any workload's); and the
// helm capability's own words withheld (scopeGaps). The rest of it
// describes the cluster and is kept. A fleet-wide scope gets rep as it is.
func (sc readScope) report(rep engine.Report, ns map[string]string) engine.Report {
	if sc.fleet() {
		return rep
	}
	k := sc.clusterKeep(ns)
	rep.Findings = sc.findings(rep.Findings, ns)
	if rep.Suppressed != nil {
		rep.Suppressed = sc.suppressed(rep.Suppressed, k, func(engine.Finding) bool { return false })
	}
	rep.UnrecognizedImages, rep.UnrecognizedImagesOmitted = nil, 0
	rep.NotAssessed = sc.scopeGaps(rep.NotAssessed)
	return rep
}

// findings is the findings the scope owns, each cut to it (never nil).
func (sc readScope) findings(fs []engine.Finding, ns map[string]string) []engine.Finding {
	if sc.fleet() {
		return fs
	}
	k := sc.clusterKeep(ns)
	out := []engine.Finding{}
	for _, f := range fs {
		if sc.owns(f.Teams) {
			cut, _ := k.cut(f)
			out = append(out, cut)
		}
	}
	return out
}

// suppressed is the suppressed findings the scope sees: those it owns or
// mine says are the caller's own, each cut by k. One whose accepted
// objects are all cut away is left out: the objects it accepted are
// another team's, whatever else its finding covers.
func (sc readScope) suppressed(fs []engine.SuppressedFinding, k keep, mine func(engine.Finding) bool) []engine.SuppressedFinding {
	kept := []engine.SuppressedFinding{}
	for _, f := range fs {
		if !mine(f.Finding) && !sc.owns(f.Teams) {
			continue
		}
		cut, _ := k.cut(f.Finding)
		if len(f.Objects) > 0 && len(cut.Objects) == 0 {
			continue
		}
		f.Finding = cut
		kept = append(kept, f)
	}
	return kept
}

// keep says what of a finding a scoped read may see: which of its teams,
// namespaces and objects.
type keep struct {
	team      func(string) bool
	namespace func(string) bool
	object    func(inventory.ObjectRef) bool
}

// clusterKeep keeps of a cluster's finding the scope's teams, the
// namespaces ns attributes to them, and the objects in those namespaces.
// A namespace no team is attributed is no team's, so it is cut too.
func (sc readScope) clusterKeep(ns map[string]string) keep {
	inScope := func(name string) bool { t := ns[name]; return t != "" && sc.teams[t] }
	return keep{
		team:      func(t string) bool { return sc.teams[t] },
		namespace: inScope,
		object:    func(o inventory.ObjectRef) bool { return inScope(o.Namespace) },
	}
}

// cut is f with only the teams, namespaces and objects k keeps, and
// whether anything was cut. The engine aggregates one finding over every
// namespace an API, add-on or release line is used in, so a finding the
// scope owns can still name other teams' namespaces, objects and teams;
// those are removed. An object without a namespace stays only when no
// namespace was cut. A cut finding's title no longer counts the objects
// the scope does not see (it counts the scope's when every object was
// listed), and its detail, which names and counts everything the finding
// covers, is replaced by one that names only what is kept. Lists the
// engine capped (NamespacesOmitted, ObjectsOmitted) cannot be divided by
// team, so a cut finding counts none omitted and its detail says more of
// the scope's may be affected. A finding whose namespace list was capped
// is always cut, since its omitted namespaces may be outside the scope.
func (k keep) cut(f engine.Finding) (engine.Finding, bool) {
	teams := slices.DeleteFunc(slices.Clone(f.Teams), func(t string) bool { return !k.team(t) })
	namespaces := slices.DeleteFunc(slices.Clone(f.Namespaces), func(n string) bool { return !k.namespace(n) })
	nsCut := len(namespaces) < len(f.Namespaces)
	objects := slices.DeleteFunc(slices.Clone(f.Objects), func(o inventory.ObjectRef) bool {
		return !k.object(o) && (o.Namespace != "" || nsCut)
	})
	// A capped namespace list is cut even when every namespace it lists is
	// kept: the namespaces it does not list may be no team's or another's,
	// and the title's count, NamespacesOmitted and the detail's "and N
	// more" would tell a scoped read how much of the finding is theirs.
	if len(teams) == len(f.Teams) && !nsCut && len(objects) == len(f.Objects) && f.NamespacesOmitted == 0 {
		return f, false
	}
	capped := f.NamespacesOmitted > 0 || f.ObjectsOmitted > 0
	f.Title = cutTitle(f.Title, f.ObjectsOmitted == 0 && len(f.Objects) > 0, len(f.Objects), len(objects))
	f.Teams, f.Namespaces, f.Objects = nilIfEmpty(teams), nilIfEmpty(namespaces), nilIfEmpty(objects)
	f.NamespacesOmitted, f.ObjectsOmitted = 0, 0
	f.Detail = cutDetail(namespaces, len(objects), capped)
	return f, true
}

func nilIfEmpty[T any](s []T) []T {
	if len(s) == 0 {
		return nil
	}
	return s
}

// titleCount matches the object count the engine ends an API finding's
// title with (pluralObjects): "... (3 objects)".
var titleCount = regexp.MustCompile(`^(.*) \((\d+) objects?\)$`)

// cutTitle is a cut finding's title: the engine's count of every object
// replaced by the scope's (listed of all, exact) or dropped.
func cutTitle(title string, listed bool, all, kept int) string {
	m := titleCount.FindStringSubmatch(title)
	if m == nil {
		return title
	}
	if n, err := strconv.Atoi(m[2]); err == nil && listed && n == all {
		noun := "objects"
		if kept == 1 {
			noun = "object"
		}
		return fmt.Sprintf("%s (%d %s in scope)", m[1], kept, noun)
	}
	return m[1]
}

// cutDetail is a cut finding's detail.
func cutDetail(namespaces []string, objects int, capped bool) string {
	where := "none of its namespaces is listed"
	if len(namespaces) > 0 {
		where = "its namespaces in scope are " + strings.Join(namespaces, ", ")
	}
	d := fmt.Sprintf("Cut to this read's teams: the finding also covers namespaces or objects outside them, "+
		"which a team-scoped read does not show, so its evidence is not repeated; %s, with %d object(s) listed.", where, objects)
	if capped {
		d += " Its lists were capped before the cut, so more of this read's teams' may be affected than are listed."
	}
	return d
}

// withheldGapReason replaces the helm capability's reason for a scoped
// read.
const withheldGapReason = "withheld from a team-scoped read: the helm collector names the releases it could not read by namespace and name, which can be other teams'"

// withholds reports whether a scoped read is shown a capability's reason
// and skipped list: not the helm collector's, which name releases as
// namespace/name, of any team. The other collectors name APIs, components
// and resources.
func (sc readScope) withholds(c inventory.Capability) bool {
	return !sc.fleet() && c == inventory.CapHelm
}

// scopeGaps is gaps as the scope sees them (withholds).
func (sc readScope) scopeGaps(gaps []engine.CapabilityGap) []engine.CapabilityGap {
	if !slices.ContainsFunc(gaps, func(g engine.CapabilityGap) bool { return sc.withholds(g.Capability) }) {
		return gaps
	}
	out := slices.Clone(gaps)
	for i, g := range out {
		if sc.withholds(g.Capability) {
			out[i].Reason, out[i].Skipped = withheldIfSet(g.Reason), nil
		}
	}
	return out
}

// summaryGaps is a summary's gaps as the scope sees them (withholds):
// the skipped entries it does not show are counted as omitted.
func (sc readScope) summaryGaps(gaps []summaryGap) []summaryGap {
	for i, g := range gaps {
		if sc.withholds(g.Capability) {
			gaps[i].Reason = withheldIfSet(g.Reason)
			gaps[i].SkippedOmitted += len(g.Skipped)
			gaps[i].Skipped = nil
		}
	}
	return gaps
}

// capabilities is a snapshot's capability map as the scope sees it
// (withholds).
func (sc readScope) capabilities(caps map[inventory.Capability]inventory.CapabilityStatus) map[inventory.Capability]inventory.CapabilityStatus {
	st, ok := caps[inventory.CapHelm]
	if !ok || !sc.withholds(inventory.CapHelm) {
		return caps
	}
	out := maps.Clone(caps)
	st.Reason, st.Skipped = withheldIfSet(st.Reason), nil
	out[inventory.CapHelm] = st
	return out
}

func withheldIfSet(reason string) string {
	if reason == "" {
		return ""
	}
	return withheldGapReason
}

// namespaceTeams maps every namespace of snap's inventory to its team as
// its evaluations attribute it (the namespace's team label, or the
// --team-map rule that overrides it), "" for none. Only the namespaces
// are decoded. A namespace the map does not name is no team's, so a
// snapshot newer than the evaluation it is read with cuts more, never
// less.
func (s *Server) namespaceTeams(snap store.Snapshot) (map[string]string, error) {
	var v struct {
		Namespaces []inventory.NamespaceInfo `json:"namespaces"`
	}
	if err := json.Unmarshal(snap.Inventory, &v); err != nil {
		return nil, fmt.Errorf("cluster %d (snapshot %d): %w: %v", snap.ClusterID, snap.ID, errCorruptInventory, err)
	}
	return namespaceTeamsOf(s.cfg.TeamMap.Apply(v.Namespaces)), nil
}

// namespaceTeamsOf maps each of namespaces to its team.
func namespaceTeamsOf(namespaces []inventory.NamespaceInfo) map[string]string {
	m := make(map[string]string, len(namespaces))
	for _, n := range namespaces {
		m[n.Name] = n.Team
	}
	return m
}

// teamScores is the scores of the scope's teams among engine.TeamScores'
// (before renderTeamScores: the "" team is no team's).
func (sc readScope) teamScores(scores map[string]engine.TeamScore) map[string]engine.TeamScore {
	if sc.fleet() {
		return scores
	}
	out := make(map[string]engine.TeamScore, len(sc.teams))
	for team, ts := range scores {
		if sc.teams[team] {
			out[team] = ts
		}
	}
	return out
}

// renderedTeams is a report's team scores for the wire, as the scope sees
// them: computed from the whole report, then cut to the scope's teams.
func (sc readScope) renderedTeams(rep engine.Report) map[string]engine.TeamScore {
	return renderTeamScores(sc.teamScores(engine.TeamScores(rep)))
}

type scopeKey struct{}

// withScope is r carrying sc for the handler.
func withScope(r *http.Request, sc readScope) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), scopeKey{}, sc))
}

// scopeOf is the scope readAuth put on r. A request it did not pass reads
// nothing, so a handler wired without readAuth fails closed.
func scopeOf(r *http.Request) readScope {
	if sc, ok := r.Context().Value(scopeKey{}).(readScope); ok {
		return sc
	}
	return readScope{teams: map[string]bool{}}
}

// readAuth authorizes a read (readScope) and runs next with its scope. A
// scoped answer names its teams in scopeHeader and is never stored by a
// cache (Cache-Control: private, no-store). In the trusted-proxy mode
// every answer also varies on the team header and Authorization: the
// proxy's requests carry no Authorization, so a shared cache in front of
// it would otherwise key one team's answer by the URL alone.
func (s *Server) readAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.cfg.TrustTeamHeader != "" {
			w.Header().Add("Vary", s.cfg.TrustTeamHeader)
			w.Header().Add("Vary", "Authorization")
		}
		sc, ok := s.readScope(w, r)
		if !ok {
			return
		}
		if !sc.fleet() {
			w.Header().Set(scopeHeader, encodeTeams(sc.names()))
			w.Header().Set("Cache-Control", "private, no-store")
		}
		next(w, withScope(r, sc))
	}
}

// readScope decides what a read may see, writing the 401 (or 500) itself:
//
//  1. a bearer that is --read-token or the admin token: the whole fleet;
//  2. a bearer that is an active read token: its teams;
//  3. a request from a trusted proxy that carries the team header: the
//     teams it lists, never the whole fleet (proxyScope);
//  4. otherwise the whole fleet when the read API is open (readOpen),
//     whatever bearer was sent, as before read tokens existed; else 401.
func (s *Server) readScope(w http.ResponseWriter, r *http.Request) (readScope, bool) {
	if token := bearerToken(r); token != "" {
		if equalToken(token, s.cfg.ReadToken) || equalToken(token, s.cfg.AdminToken) {
			return fleetScope, true
		}
		teams, valid, err := s.cfg.Store.ValidReadToken(r.Context(), token)
		if err != nil {
			internalErr(w, "validating read token", err)
			return readScope{}, false
		}
		if valid {
			return scopeOfTeams(teams), true
		}
	}
	if sc, ok := s.proxyScope(r); ok {
		return sc, true
	}
	open, err := s.readOpen(r.Context())
	if err != nil {
		internalErr(w, "listing read tokens", err)
		return readScope{}, false
	}
	if open {
		return fleetScope, true
	}
	errJSON(w, http.StatusUnauthorized, "invalid or missing bearer token")
	return readScope{}, false
}

// equalToken is a constant-time comparison with a configured token; an
// unset one ("") matches nothing.
func equalToken(presented, configured string) bool {
	return configured != "" && subtle.ConstantTimeCompare([]byte(presented), []byte(configured)) == 1
}

// readOpen reports whether the read API needs no credential: no
// --read-token, no trusted-proxy header, and no read token ever minted in
// the store. Rows are never deleted, so once one is minted the answer stays
// false (remembered, so it is asked until then only), and revoking the last
// one does not open the read API again.
func (s *Server) readOpen(ctx context.Context) (bool, error) {
	if s.cfg.ReadToken != "" || s.cfg.TrustTeamHeader != "" || s.readTokensMinted.Load() {
		return false, nil
	}
	toks, err := s.cfg.Store.ListReadTokens(ctx)
	if err != nil {
		return false, err
	}
	if len(toks) > 0 {
		s.readTokensMinted.Store(true)
		return false, nil
	}
	return true, nil
}

// proxyScope is the trusted-proxy mode (Config.TrustTeamHeader): a request
// whose TCP peer, never a forwarded-for header, is in Config.TrustedProxies
// and that carries the team header reads as the teams it lists, in the
// team list encoding (decodeTeams), every copy of the header counted
// (oauth2-proxy sends one per group). Anywhere else the header is ignored: a client that reaches the
// server directly cannot spoof it. Only a proxy that strips the header from
// what clients send makes it safe. The header always names teams: "*" in it
// is a team called "*", never the whole fleet, so whoever can name an
// identity-provider group cannot grant fleet-wide reads with it; a person
// who needs the fleet uses a fleet-wide read token.
func (s *Server) proxyScope(r *http.Request) (readScope, bool) {
	if s.cfg.TrustTeamHeader == "" || !s.trustedPeer(r.RemoteAddr) {
		return readScope{}, false
	}
	var teams []string
	for _, v := range r.Header.Values(s.cfg.TrustTeamHeader) {
		teams = append(teams, decodeTeams(v)...)
	}
	if len(teams) == 0 {
		return readScope{}, false
	}
	m := make(map[string]bool, len(teams))
	for _, t := range teams {
		m[t] = true
	}
	return readScope{teams: m}, true
}

// The team list encoding, of the trusted-proxy header and of scopeHeader:
// team names separated by commas, each percent-encoded as a URL path
// segment is (url.PathEscape: a comma, percent sign, space, control or
// non-ASCII byte as %XX, UTF-8). A team name is free text (a --team-map
// team can be "Platform Team" or "Équipe, Paris"), so it is encoded to
// be named in a list unambiguously and in an ASCII header value.

// encodeTeams is teams in the team list encoding.
func encodeTeams(teams []string) string {
	enc := make([]string, len(teams))
	for i, t := range teams {
		enc[i] = url.PathEscape(t)
	}
	return strings.Join(enc, ",")
}

// decodeTeams is the teams v lists in the team list encoding. Whitespace
// around an entry is a list's optional whitespace and is trimmed (a name
// that starts or ends with a space spells it %20); an empty entry names
// no team. An entry that is not valid percent-encoding ("50%off") names
// no team the server could know, so it is dropped: read as written, it
// could be a team whose encoded name it is not. A name sent unencoded
// reads as itself when it holds no comma or percent sign ("Platform
// Team", raw UTF-8).
func decodeTeams(v string) []string {
	var teams []string
	for _, e := range strings.Split(v, ",") {
		if e = strings.TrimSpace(e); e == "" {
			continue
		}
		if t, err := url.PathUnescape(e); err == nil && t != "" {
			teams = append(teams, t)
		}
	}
	return teams
}

// trustedPeer reports whether remoteAddr (http.Request.RemoteAddr) is in a
// trusted proxy range.
func (s *Server) trustedPeer(remoteAddr string) bool {
	ap, err := netip.ParseAddrPort(remoteAddr)
	if err != nil {
		return false
	}
	ip := ap.Addr().Unmap()
	return slices.ContainsFunc(s.cfg.TrustedProxies, func(p netip.Prefix) bool { return p.Contains(ip) })
}

// fleetOnly answers 403 to a scoped read of an endpoint that names every
// cluster (/metrics).
func fleetOnly(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !scopeOf(r).fleet() {
			errJSON(w, http.StatusForbidden, "this endpoint names every cluster: it needs a fleet-wide read credential")
			return
		}
		next(w, r)
	}
}

// scopeClusters returns the ids of the clusters sc reads, nil (all of them)
// for the whole fleet.
func (s *Server) scopeClusters(ctx context.Context, sc readScope) (map[int64]bool, error) {
	if sc.fleet() {
		return nil, nil
	}
	ids, err := s.cfg.Store.ClustersOfTeams(ctx, sc.names(), s.teamMapHash)
	if err != nil {
		return nil, err
	}
	out := make(map[int64]bool, len(ids))
	for _, id := range ids {
		out[id] = true
	}
	return out, nil
}

// inScope reports whether sc reads the cluster id.
func (s *Server) inScope(ctx context.Context, sc readScope, id int64) (bool, error) {
	ids, err := s.scopeClusters(ctx, sc)
	return ids == nil || ids[id], err
}

// clusterTeams is the teams an evaluated inventory attributes something
// to, stored with its evaluations (store.Evaluation.Teams): every team a
// namespace is attributed to, which includes every team a finding names,
// and the findings' own. Sorted, deduplicated.
func clusterTeams(inv inventory.Inventory, rep engine.Report) []string {
	var teams []string
	for _, ns := range inv.Namespaces {
		if ns.Team != "" {
			teams = append(teams, ns.Team)
		}
	}
	for _, f := range rep.Findings {
		teams = append(teams, f.Teams...)
	}
	slices.Sort(teams)
	return slices.Compact(teams)
}

// clusterShare is inv, a cluster's inventory with its namespaces' teams
// applied (TeamMap.Apply), reduced to the scope's share: the evidence in
// the namespaces it attributes to one of the scope's teams. A team-scoped
// gate merges its manifests into this, so the engine counts, titles and
// details a finding the PR's objects join from the scope's evidence and
// the PR's alone, never another team's (#72). Left out: every other
// namespace (so the PR's objects there are attributed to no team), the
// API usage, custom resources, Helm releases, GitOps charts and add-on
// installs in them, cluster-scoped usage and add-ons detected without a
// namespace (no team's), the apiserver's deprecated-call rows (cluster
// wide, of any client) and the unrecognized images (any workload's).
// What describes the cluster as a whole stays: its version, nodes,
// control plane, CRD definitions and capabilities. inv is not modified.
// The fleet-wide scope's share is inv.
func (sc readScope) clusterShare(inv inventory.Inventory) inventory.Inventory {
	if sc.fleet() {
		return inv
	}
	teams := namespaceTeamsOf(inv.Namespaces)
	in := func(ns string) bool { t := teams[ns]; return ns != "" && t != "" && sc.teams[t] }
	out := inv
	out.Namespaces = slices.DeleteFunc(slices.Clone(inv.Namespaces), func(n inventory.NamespaceInfo) bool { return !in(n.Name) })
	out.APIUsage = usageShare(inv.APIUsage, in)
	out.APIAuthorshipUnknown = usageShare(inv.APIAuthorshipUnknown, in)
	out.CRDs = make([]inventory.CRD, 0, len(inv.CRDs))
	for _, c := range inv.CRDs {
		c.Usage = usageShare(c.Usage, in)
		out.CRDs = append(out.CRDs, c)
	}
	out.HelmReleases = slices.DeleteFunc(slices.Clone(inv.HelmReleases), func(r inventory.HelmRelease) bool { return !in(r.Namespace) })
	out.GitOpsCharts = slices.DeleteFunc(slices.Clone(inv.GitOpsCharts), func(c inventory.GitOpsChart) bool {
		return !in(c.Namespace) || c.Target != "" && !in(c.Target)
	})
	out.AddOns = nil
	for _, a := range inv.AddOns {
		a.Namespaces = slices.DeleteFunc(slices.Clone(a.Namespaces), func(ns string) bool { return !in(ns) })
		if len(a.Namespaces) > 0 {
			out.AddOns = append(out.AddOns, a)
		}
	}
	out.DeprecatedCalls = nil
	out.UnrecognizedImages, out.UnrecognizedImagesOmitted = nil, 0
	return out
}

// usageShare is the rows of us in the namespaces in keeps: each row's
// count, per-namespace counts and objects of those namespaces only, and
// the objects of theirs it did not list counted as omitted. A row without
// per-namespace counts cannot be divided, so it is left out, as is
// cluster-scoped usage (the "" namespace).
func usageShare(us []inventory.APIUsage, in func(string) bool) []inventory.APIUsage {
	var out []inventory.APIUsage
	for _, u := range us {
		namespaces, count := map[string]int{}, 0
		for ns, n := range u.Namespaces {
			if in(ns) && n > 0 {
				namespaces[ns], count = n, count+n
			}
		}
		if count == 0 {
			continue
		}
		u.Objects = slices.DeleteFunc(slices.Clone(u.Objects), func(o inventory.ObjectRef) bool { return !in(o.Namespace) })
		u.Count, u.Namespaces, u.ObjectsOmitted = count, namespaces, max(0, count-len(u.Objects))
		out = append(out, u)
	}
	return out
}
