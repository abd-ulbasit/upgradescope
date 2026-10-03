package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/abd-ulbasit/upgradescope/internal/inventory"
	"github.com/abd-ulbasit/upgradescope/internal/kb"
	"github.com/abd-ulbasit/upgradescope/internal/server/store"
)

// scopeKB is testKB with two more APIs removed by 1.35, so that one
// cluster has a blocker for each team and one no team owns.
func scopeKB() kb.KB {
	k := testKB()
	for _, e := range []struct {
		group, kind string
		removed     int
	}{{"extensions", "Ingress", 22}, {"batch", "CronJob", 25}} {
		removed := inventory.Version{Major: 1, Minor: e.removed}
		k.APILifecycle = append(k.APILifecycle, kb.APILifecycleEntry{
			Group: e.group, Version: "v1beta1", Kind: e.kind,
			Introduced: inventory.Version{Major: 1, Minor: 1}, Removed: &removed,
		})
	}
	return k
}

func usage(group, kind, namespace string) inventory.APIUsage {
	return inventory.APIUsage{Group: group, Version: "v1beta1", Kind: kind, Count: 1, Namespaces: map[string]int{namespace: 1}}
}

// The scope fixture: three clusters on v1.34, so the default target 1.35.
//   - mixed: a payments blocker (PSP in pay-prod), a web one (Ingress in
//     web-prod) and one no team owns (CronJob in ops, which has no team).
//   - web: a web blocker only.
//   - calm: a payments namespace and no finding at all.
var scopeClusters = []struct {
	name string
	inv  func() inventory.Inventory
}{
	{"mixed", func() inventory.Inventory {
		inv := testInventory()
		inv.Namespaces = []inventory.NamespaceInfo{{Name: "pay-prod", Team: "payments"}, {Name: "web-prod", Team: "web"}, {Name: "ops"}}
		inv.APIUsage = []inventory.APIUsage{
			usage("policy", "PodSecurityPolicy", "pay-prod"), usage("extensions", "Ingress", "web-prod"), usage("batch", "CronJob", "ops"),
		}
		return inv
	}},
	{"web", func() inventory.Inventory {
		inv := testInventory()
		inv.Namespaces = []inventory.NamespaceInfo{{Name: "web-prod", Team: "web"}}
		inv.APIUsage = []inventory.APIUsage{usage("extensions", "Ingress", "web-prod")}
		return inv
	}},
	{"calm", func() inventory.Inventory {
		inv := testInventory()
		inv.Namespaces = []inventory.NamespaceInfo{{Name: "pay-dev", Team: "payments"}}
		return inv
	}},
}

// scopeServer is a server on a real SQLite store (so ClustersOfTeams runs
// its SQL) with the fleet-wide --read-token "fleet-tok", the admin token
// "admin-tok", the scope fixture pushed, and its cluster ids by name.
func scopeServer(t *testing.T, opts ...func(*Config)) (*Server, store.Store, *httptest.Server, map[string]int64) {
	t.Helper()
	st := openSQLite(t)
	cfg := Config{Store: st, KB: scopeKB(), IngestToken: "ingest-tok", ReadToken: "fleet-tok", AdminToken: "admin-tok"}
	for _, o := range opts {
		o(&cfg)
	}
	s, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	s.now = func() time.Time { return time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC) }
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	ids := map[string]int64{}
	for _, c := range scopeClusters {
		inv := c.inv()
		inv.ClusterID = "uid-" + c.name
		body, err := json.Marshal(map[string]any{"schemaVersion": 1, "clusterName": c.name, "agentVersion": "v0.2.0-test", "inventory": inv})
		if err != nil {
			t.Fatal(err)
		}
		if resp, out := postSnapshot(t, ts, "ingest-tok", body, false); resp.StatusCode != http.StatusAccepted {
			t.Fatalf("push %s = %d %v", c.name, resp.StatusCode, out)
		}
		got, err := st.ClusterByName(context.Background(), c.name)
		if err != nil {
			t.Fatal(err)
		}
		ids[c.name] = got.ID
	}
	return s, st, ts, ids
}

// mintReadToken stores a read token scoped to teams.
func mintReadToken(t *testing.T, st store.Store, token string, teams ...string) int64 {
	t.Helper()
	id, err := st.CreateReadToken(context.Background(), teams, token)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// fetch GETs path with token and returns the response and its body.
func fetch(t *testing.T, ts *httptest.Server, path, token string, header ...string) (*http.Response, []byte) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, ts.URL+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for i := 0; i+1 < len(header); i += 2 {
		req.Header.Add(header[i], header[i+1])
	}
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp, raw
}

// findingTeams lists each finding's teams ("-" for none) in a report-shaped
// body's findings.
func findingTeams(t *testing.T, raw []byte) []string {
	t.Helper()
	var body struct {
		Findings []struct {
			Teams []string `json:"teams"`
		} `json:"findings"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("decode %s: %v", raw, err)
	}
	var out []string
	for _, f := range body.Findings {
		out = append(out, cmpOr(strings.Join(f.Teams, "+"), "-"))
	}
	slices.Sort(out)
	return out
}

func cmpOr(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

// teamKeys lists the keys of a body's teams object.
func teamKeys(t *testing.T, raw []byte) []string {
	t.Helper()
	var body struct {
		Teams map[string]json.RawMessage `json:"teams"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("decode %s: %v", raw, err)
	}
	var out []string
	for k := range body.Teams {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

// A token scoped to payments reads, from every read endpoint, only the
// clusters some payments namespace is in and only payments' findings and
// team scores: none of web's, none no team owns (#72).
func TestScopedTokenReadsOnlyItsTeams(t *testing.T) {
	_, st, ts, ids := scopeServer(t)
	mintReadToken(t, st, "pay-tok", "payments")
	mixed := fmt.Sprint(ids["mixed"])

	// The fleet-wide token sees all of it: the fixture is what it claims.
	if _, raw := fetch(t, ts, "/api/v1/clusters/"+mixed+"/findings", "fleet-tok"); !slices.Equal(findingTeams(t, raw), []string{"-", "payments", "web"}) {
		t.Fatalf("fleet-wide findings of mixed = %v, want one per team and one no team owns", findingTeams(t, raw))
	}

	t.Run("cluster list", func(t *testing.T) {
		resp, raw := fetch(t, ts, "/api/v1/clusters", "pay-tok")
		var got []struct{ Name string }
		if err := json.Unmarshal(raw, &got); err != nil || resp.StatusCode != http.StatusOK {
			t.Fatalf("status %d, %s", resp.StatusCode, raw)
		}
		var names []string
		for _, c := range got {
			names = append(names, c.Name)
		}
		if !slices.Equal(names, []string{"calm", "mixed"}) {
			t.Errorf("clusters = %v, want [calm mixed]: calm has a payments namespace and no finding", names)
		}
		if h := resp.Header.Get("X-Upgradescope-Teams"); h != "payments" {
			t.Errorf("X-Upgradescope-Teams = %q, want payments", h)
		}
	})
	t.Run("cluster detail", func(t *testing.T) {
		if resp, raw := fetch(t, ts, "/api/v1/clusters/"+mixed, "pay-tok"); resp.StatusCode != http.StatusOK {
			t.Errorf("status %d, %s", resp.StatusCode, raw)
		}
	})
	for _, path := range []string{"/report", "/findings", "/report?target=1.36"} { // 1.36 has no stored evaluation: a what-if
		t.Run(path, func(t *testing.T) {
			resp, raw := fetch(t, ts, "/api/v1/clusters/"+mixed+path, "pay-tok")
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status %d, %s", resp.StatusCode, raw)
			}
			if got := findingTeams(t, raw); !slices.Equal(got, []string{"payments"}) {
				t.Errorf("findings' teams = %v, want [payments]", got)
			}
			if strings.HasPrefix(path, "/report") {
				if got := teamKeys(t, raw); !slices.Equal(got, []string{"payments"}) {
					t.Errorf("teams = %v, want [payments]", got)
				}
			}
		})
	}
	t.Run("teams", func(t *testing.T) {
		_, raw := fetch(t, ts, "/api/v1/clusters/"+mixed+"/teams", "pay-tok")
		if got := teamKeys(t, raw); !slices.Equal(got, []string{"payments"}) {
			t.Errorf("teams = %v, want [payments]", got)
		}
		// The team's verdict still counts the blocker no team owns, as
		// the fleet-wide view does.
		_, fleet := fetch(t, ts, "/api/v1/clusters/"+mixed+"/teams", "fleet-tok")
		var a, b struct{ Teams map[string]json.RawMessage }
		_ = json.Unmarshal(raw, &a)
		_ = json.Unmarshal(fleet, &b)
		if !bytes.Equal(a.Teams["payments"], b.Teams["payments"]) {
			t.Errorf("payments' score = %s scoped, %s fleet-wide, want the same", a.Teams["payments"], b.Teams["payments"])
		}
	})
	t.Run("history", func(t *testing.T) {
		if resp, raw := fetch(t, ts, "/api/v1/clusters/"+mixed+"/history", "pay-tok"); resp.StatusCode != http.StatusOK {
			t.Errorf("status %d, %s", resp.StatusCode, raw)
		}
	})
	t.Run("export csv", func(t *testing.T) {
		resp, raw := fetch(t, ts, "/api/v1/clusters/"+mixed+"/export?format=csv", "pay-tok")
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status %d, %s", resp.StatusCode, raw)
		}
		if !strings.Contains(string(raw), "PodSecurityPolicy") || strings.Contains(string(raw), "Ingress") ||
			strings.Contains(string(raw), "CronJob") || strings.Contains(string(raw), "web-prod") {
			t.Errorf("CSV export, want payments' finding only:\n%s", raw)
		}
	})
	t.Run("export html", func(t *testing.T) {
		resp, raw := fetch(t, ts, "/api/v1/clusters/"+mixed+"/export?format=html", "pay-tok")
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status %d, %s", resp.StatusCode, raw)
		}
		html := string(raw)
		if !strings.Contains(html, "PodSecurityPolicy") || strings.Contains(html, "Ingress") || strings.Contains(html, "CronJob") ||
			strings.Contains(html, ">web<") || strings.Contains(html, "unattributed") {
			t.Errorf("HTML export, want payments' finding and team only")
		}
	})
	t.Run("fleet", func(t *testing.T) {
		_, raw := fetch(t, ts, "/api/v1/fleet", "pay-tok")
		var got struct{ Clusters []struct{ Name string } }
		if err := json.Unmarshal(raw, &got); err != nil {
			t.Fatal(err)
		}
		var names []string
		for _, c := range got.Clusters {
			names = append(names, c.Name)
		}
		if !slices.Equal(names, []string{"calm", "mixed"}) {
			t.Errorf("fleet rows = %v, want [calm mixed]", names)
		}
	})
	t.Run("fleet teams", func(t *testing.T) {
		_, raw := fetch(t, ts, "/api/v1/fleet/teams?target=1.35", "pay-tok")
		if got := teamKeys(t, raw); !slices.Equal(got, []string{"payments"}) {
			t.Errorf("teams = %v, want [payments]", got)
		}
		if strings.Contains(string(raw), `"web"`) {
			t.Errorf("fleet teams names the web cluster or team: %s", raw)
		}
	})
	t.Run("gate with cluster", func(t *testing.T) {
		resp, raw := postGate(t, ts, "?target=1.35&fail-on=never&cluster=mixed", "pay-tok", pspManifest, "application/x-yaml")
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status %d, %s", resp.StatusCode, raw)
		}
		var body struct {
			Findings []struct {
				Source string   `json:"source"`
				Teams  []string `json:"teams"`
			} `json:"findings"`
		}
		_ = json.Unmarshal(raw, &body)
		for _, f := range body.Findings {
			if f.Source == sourceCluster && !slices.Contains(f.Teams, "payments") {
				t.Errorf("cluster finding of teams %v in a payments-scoped gate answer", f.Teams)
			}
		}
		if got := teamKeys(t, raw); slices.ContainsFunc(got, func(k string) bool { return k != "payments" }) {
			t.Errorf("gate teams = %v, want payments' at most", got)
		}
	})
	t.Run("gate without cluster", func(t *testing.T) {
		if resp, raw := postGate(t, ts, "?target=1.35&fail-on=never", "pay-tok", pspManifest, "application/x-yaml"); resp.StatusCode != http.StatusOK {
			t.Errorf("status %d, %s", resp.StatusCode, raw)
		}
	})
	t.Run("registry", func(t *testing.T) {
		if resp, raw := fetch(t, ts, "/api/v1/registry", "pay-tok"); resp.StatusCode != http.StatusOK {
			t.Errorf("status %d, %s", resp.StatusCode, raw)
		}
	})
	t.Run("metrics", func(t *testing.T) {
		// Per-cluster series name every cluster: a fleet-wide token only.
		if resp, raw := fetch(t, ts, "/metrics", "pay-tok"); resp.StatusCode != http.StatusForbidden {
			t.Errorf("scoped /metrics = %d %s, want 403", resp.StatusCode, raw)
		}
		if resp, _ := fetch(t, ts, "/metrics", "fleet-tok"); resp.StatusCode != http.StatusOK {
			t.Errorf("fleet-wide /metrics = %d, want 200", resp.StatusCode)
		}
	})
}

// A cluster outside the token's scope answers exactly as a cluster that
// does not exist, on every per-cluster endpoint and the gate, so its name
// and id stay hidden.
func TestScopedTokenOutOfScopeClusterIsNotFound(t *testing.T) {
	_, st, ts, ids := scopeServer(t)
	mintReadToken(t, st, "pay-tok", "payments")
	web := fmt.Sprint(ids["web"])
	const unknown = "999999"
	for _, suffix := range []string{"", "/report", "/findings", "/teams", "/history", "/export?format=csv", "/export?format=html", "/report?target=1.36"} {
		resp, raw := fetch(t, ts, "/api/v1/clusters/"+web+suffix, "pay-tok")
		wantResp, want := fetch(t, ts, "/api/v1/clusters/"+unknown+suffix, "pay-tok")
		if resp.StatusCode != http.StatusNotFound || wantResp.StatusCode != http.StatusNotFound || !bytes.Equal(raw, want) {
			t.Errorf("%s of an out-of-scope cluster = %d %s, want the unknown cluster's %d %s", suffix, resp.StatusCode, raw, wantResp.StatusCode, want)
		}
		if resp, _ := fetch(t, ts, "/api/v1/clusters/"+web+suffix, "fleet-tok"); resp.StatusCode != http.StatusOK {
			t.Errorf("%s with the fleet-wide token = %d, want 200", suffix, resp.StatusCode)
		}
	}
	for _, ref := range []string{"web", web} {
		resp, raw := postGate(t, ts, "?target=1.35&fail-on=never&cluster="+ref, "pay-tok", pspManifest, "application/x-yaml")
		wantResp, want := postGate(t, ts, "?target=1.35&fail-on=never&cluster=nonesuch", "pay-tok", pspManifest, "application/x-yaml")
		if resp.StatusCode != http.StatusNotFound || !bytes.Equal(raw, want) || wantResp.StatusCode != http.StatusNotFound {
			t.Errorf("gate ?cluster=%s = %d %s, want the unknown cluster's %s", ref, resp.StatusCode, raw, want)
		}
	}
	for _, path := range []string{"/api/v1/clusters", "/api/v1/fleet", "/api/v1/fleet/teams?target=1.35"} {
		if _, raw := fetch(t, ts, path, "pay-tok"); strings.Contains(string(raw), `"web"`) {
			t.Errorf("%s names the out-of-scope cluster: %s", path, raw)
		}
	}
}

// Every fleet-wide credential (--read-token, a stored "*" read token, the
// admin token) reads exactly what --read-token read before scoped tokens
// existed: the same bytes, and no scope header.
func TestFleetWideTokensReadAsBefore(t *testing.T) {
	_, st, ts, ids := scopeServer(t)
	mixed := fmt.Sprint(ids["mixed"])
	paths := []string{
		"/api/v1/clusters", "/api/v1/clusters/" + mixed, "/api/v1/clusters/" + mixed + "/report",
		"/api/v1/clusters/" + mixed + "/report?target=1.36", "/api/v1/clusters/" + mixed + "/findings",
		"/api/v1/clusters/" + mixed + "/teams", "/api/v1/clusters/" + mixed + "/history",
		"/api/v1/clusters/" + mixed + "/export?format=csv", "/api/v1/clusters/" + mixed + "/export?format=html",
		"/api/v1/fleet", "/api/v1/fleet/teams?target=1.35", "/api/v1/registry",
	}
	before := map[string][]byte{}
	for _, p := range paths {
		resp, raw := fetch(t, ts, p, "fleet-tok")
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s = %d %s", p, resp.StatusCode, raw)
		}
		before[p] = raw
	}
	_, gateBefore := postGate(t, ts, "?target=1.35&fail-on=never&cluster=mixed", "fleet-tok", pspManifest, "application/x-yaml")

	// Read tokens exist from here on.
	mintReadToken(t, st, "star-tok", store.ReadScopeFleet)
	mintReadToken(t, st, "pay-tok", "payments")
	for _, tok := range []string{"fleet-tok", "star-tok", "admin-tok"} {
		for _, p := range paths {
			resp, raw := fetch(t, ts, p, tok)
			if resp.StatusCode != http.StatusOK || !bytes.Equal(raw, before[p]) {
				t.Errorf("%s with %s = %d, %d bytes, want the %d bytes --read-token read before", p, tok, resp.StatusCode, len(raw), len(before[p]))
			}
			if h := resp.Header.Values("X-Upgradescope-Teams"); h != nil {
				t.Errorf("%s with %s: X-Upgradescope-Teams = %v, want none on a fleet-wide read", p, tok, h)
			}
		}
		if _, raw := postGate(t, ts, "?target=1.35&fail-on=never&cluster=mixed", tok, pspManifest, "application/x-yaml"); !bytes.Equal(raw, gateBefore) {
			t.Errorf("gate with %s differs from --read-token's before", tok)
		}
		if resp, _ := fetch(t, ts, "/metrics", tok); resp.StatusCode != http.StatusOK {
			t.Errorf("/metrics with %s = %d, want 200", tok, resp.StatusCode)
		}
	}
}

// A revoked read token reads nothing, and revoking the last one does not
// open the read API again.
func TestRevokedReadTokenIsRejected(t *testing.T) {
	_, st, ts, _ := scopeServer(t, func(c *Config) { c.ReadToken = "" })
	if resp, _ := fetch(t, ts, "/api/v1/clusters", ""); resp.StatusCode != http.StatusOK {
		t.Fatalf("no read credential at all: anonymous read = %d, want 200 (open)", resp.StatusCode)
	}
	id := mintReadToken(t, st, "pay-tok", "payments")
	if resp, _ := fetch(t, ts, "/api/v1/clusters", ""); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("with a read token minted: anonymous read = %d, want 401", resp.StatusCode)
	}
	if resp, _ := fetch(t, ts, "/api/v1/clusters", "pay-tok"); resp.StatusCode != http.StatusOK {
		t.Fatalf("active read token = %d, want 200", resp.StatusCode)
	}
	if err := st.RevokeReadToken(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	for _, tok := range []string{"pay-tok", ""} {
		if resp, raw := fetch(t, ts, "/api/v1/clusters", tok); resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("after revoking the only read token, token %q = %d %s, want 401", tok, resp.StatusCode, raw)
		}
	}
}

// The trusted-proxy mode: a team header sets the scope only on a request
// whose peer is in a trusted proxy range. The same header from anywhere
// else is ignored, so it cannot be spoofed past the proxy.
func TestTrustedTeamHeader(t *testing.T) {
	loopback := []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")}
	elsewhere := []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")}
	header := "X-Forwarded-Groups"
	names := func(raw []byte) []string {
		var got []struct{ Name string }
		_ = json.Unmarshal(raw, &got)
		var out []string
		for _, c := range got {
			out = append(out, c.Name)
		}
		return out
	}

	t.Run("from the proxy", func(t *testing.T) {
		_, st, ts, _ := scopeServer(t, func(c *Config) { c.ReadToken, c.TrustTeamHeader, c.TrustedProxies = "", header, loopback })
		mintReadToken(t, st, "pay-tok", "payments")
		for _, tc := range []struct {
			value string
			want  []string
		}{
			{"payments", []string{"calm", "mixed"}},
			{"web, other", []string{"mixed", "web"}},
			{"*", []string{"calm", "mixed", "web"}},
		} {
			resp, raw := fetch(t, ts, "/api/v1/clusters", "", header, tc.value)
			if resp.StatusCode != http.StatusOK || !slices.Equal(names(raw), tc.want) {
				t.Errorf("%s: %s = %d %v, want %v", header, tc.value, resp.StatusCode, names(raw), tc.want)
			}
		}
		// The proxy authenticated nobody: no header is no scope.
		if resp, _ := fetch(t, ts, "/api/v1/clusters", ""); resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("from the proxy without the header = %d, want 401", resp.StatusCode)
		}
		// A token still reads as its own scope.
		if _, raw := fetch(t, ts, "/api/v1/clusters", "pay-tok"); !slices.Equal(names(raw), []string{"calm", "mixed"}) {
			t.Errorf("a scoped token through the proxy = %v, want its own scope", names(raw))
		}
	})

	t.Run("spoofed from outside the proxy range", func(t *testing.T) {
		s, st, ts, _ := scopeServer(t, func(c *Config) { c.ReadToken, c.TrustTeamHeader, c.TrustedProxies = "", header, elsewhere })
		mintReadToken(t, st, "pay-tok", "payments")
		for _, value := range []string{"web", "*"} {
			if resp, raw := fetch(t, ts, "/api/v1/clusters", "", header, value); resp.StatusCode != http.StatusUnauthorized {
				t.Errorf("%s: %s from 127.0.0.1 (proxies 10.0.0.0/8) = %d %s, want 401", header, value, resp.StatusCode, raw)
			}
			// With a valid scoped token, the spoofed header widens nothing.
			if _, raw := fetch(t, ts, "/api/v1/clusters", "pay-tok", header, value); !slices.Equal(names(raw), []string{"calm", "mixed"}) {
				t.Errorf("pay-tok with a spoofed %s: %s = %v, want the token's scope only", header, value, names(raw))
			}
		}
		// The same request from inside the range is trusted.
		req := httptest.NewRequest(http.MethodGet, "/api/v1/clusters", nil)
		req.RemoteAddr = "10.1.2.3:41000"
		req.Header.Set(header, "web")
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusOK || !slices.Equal(names(rec.Body.Bytes()), []string{"mixed", "web"}) {
			t.Errorf("from 10.1.2.3 = %d %v, want 200 [mixed web]", rec.Code, names(rec.Body.Bytes()))
		}
	})

	t.Run("needs both flags", func(t *testing.T) {
		for _, cfg := range []Config{
			{Store: newFakeStore(), TrustTeamHeader: header},
			{Store: newFakeStore(), TrustedProxies: loopback},
		} {
			if _, err := New(cfg); err == nil {
				t.Errorf("New(header %q, proxies %v) succeeded, want an error: the header mode needs both", cfg.TrustTeamHeader, cfg.TrustedProxies)
			}
		}
	})
}

// A server whose reads need a stored read token or the proxy's header is
// not an open read API: it starts on a non-loopback address.
func TestStartWithScopedReadsOnNonLoopback(t *testing.T) {
	st := newFakeStore()
	mintReadToken(t, st, "pay-tok", "payments")
	s := newTestServer(t, st, func(c *Config) { c.Listen = "localhost:0" })
	fakeBind(t, s, "172.17.0.2")
	startServer(t, s)

	s = newTestServer(t, newFakeStore(), func(c *Config) {
		c.Listen, c.TrustTeamHeader, c.TrustedProxies = "localhost:0", "X-Forwarded-Groups", []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")}
	})
	fakeBind(t, s, "172.17.0.2")
	startServer(t, s)
}

// An evaluation written before the teams column existed is stale, so the
// next pass writes its teams and scoped tokens see its cluster.
func TestEvaluationWithUnknownTeamsIsStale(t *testing.T) {
	s := newTestServer(t, newFakeStore())
	now := s.now()
	e := store.Evaluation{KBVersion: s.cfg.KB.Version, EvaluatedAt: now}
	if s.stale(e, now) {
		t.Fatal("a current evaluation is stale")
	}
	e.TeamsUnknown = true
	if !s.stale(e, now) {
		t.Error("an evaluation with unknown teams is not stale")
	}
}
