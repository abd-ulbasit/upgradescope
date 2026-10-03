package server

import (
	"context"
	"crypto/subtle"
	"net/http"
	"net/netip"
	"slices"
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
//     (each target's newest of the latest snapshot) name one of its teams
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
//     view does. What describes the cluster as a whole stays: its score,
//     verdict, blocker and warning counts, capability gaps, history.
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

// report is rep with only the findings and suppressed findings the scope
// owns; the rest of it describes the cluster and is kept. A fleet-wide
// scope gets rep as it is.
func (sc readScope) report(rep engine.Report) engine.Report {
	if sc.fleet() {
		return rep
	}
	rep.Findings = sc.findings(rep.Findings)
	if rep.Suppressed != nil {
		kept := []engine.SuppressedFinding{}
		for _, f := range rep.Suppressed {
			if sc.owns(f.Teams) {
				kept = append(kept, f)
			}
		}
		rep.Suppressed = kept
	}
	return rep
}

// findings is the findings the scope owns (never nil).
func (sc readScope) findings(fs []engine.Finding) []engine.Finding {
	if sc.fleet() {
		return fs
	}
	out := []engine.Finding{}
	for _, f := range fs {
		if sc.owns(f.Teams) {
			out = append(out, f)
		}
	}
	return out
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
// scoped answer names its teams in scopeHeader.
func (s *Server) readAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sc, ok := s.readScope(w, r)
		if !ok {
			return
		}
		if !sc.fleet() {
			w.Header().Set(scopeHeader, strings.Join(sc.names(), ","))
		}
		next(w, withScope(r, sc))
	}
}

// readScope decides what a read may see, writing the 401 (or 500) itself:
//
//  1. a bearer that is --read-token or the admin token: the whole fleet;
//  2. a bearer that is an active read token: its teams;
//  3. a request from a trusted proxy that carries the team header: the
//     teams it lists (proxyScope);
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
// and that carries the team header reads as the teams it lists, comma
// separated, every copy of the header counted. Anywhere else the header is
// ignored: a client that reaches the server directly cannot spoof it. Only
// a proxy that strips the header from what clients send makes it safe.
func (s *Server) proxyScope(r *http.Request) (readScope, bool) {
	if s.cfg.TrustTeamHeader == "" || !s.trustedPeer(r.RemoteAddr) {
		return readScope{}, false
	}
	var teams []string
	for _, v := range r.Header.Values(s.cfg.TrustTeamHeader) {
		for _, t := range strings.Split(v, ",") {
			if t = strings.TrimSpace(t); t != "" {
				teams = append(teams, t)
			}
		}
	}
	if len(teams) == 0 {
		return readScope{}, false
	}
	return scopeOfTeams(teams), true
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
	ids, err := s.cfg.Store.ClustersOfTeams(ctx, sc.names())
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
