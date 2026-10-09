package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"slices"
	"testing"

	"github.com/abd-ulbasit/upgradescope/internal/inventory"
)

// Free-text team names (#72): a --team-map team is free text, as it was
// before read scopes, so a team like "Platform Team", one with a comma
// and a non-ASCII one each read their own cluster and findings through a
// read token and through the trusted header (in the team list encoding),
// and a map's "*" team reads as StarTeam. X-Upgradescope-Teams names
// them encoded.
func TestScopedReadsOfFreeTextTeams(t *testing.T) {
	const header = "X-Forwarded-Groups"
	tm, err := ParseTeamMap([]byte("- pattern: \"plat-*\"\n  team: Platform Team\n" +
		"- pattern: \"fr-*\"\n  team: \"Équipe, Paris\"\n- pattern: \"uni-*\"\n  team: équipe\n- pattern: \"star-*\"\n  team: \"*\"\n"))
	if err != nil {
		t.Fatal(err)
	}
	_, st, ts, _ := scopeServer(t, func(c *Config) {
		c.TeamMap, c.TrustTeamHeader, c.TrustedProxies = tm, header, []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")}
	})
	inv := testInventory()
	inv.Namespaces = []inventory.NamespaceInfo{{Name: "plat-a"}, {Name: "fr-a"}, {Name: "uni-a"}, {Name: "star-a"}}
	inv.APIUsage = []inventory.APIUsage{usage("extensions", "Ingress", "plat-a"), usage("batch", "CronJob", "fr-a"), usage("policy", "PodSecurityPolicy", "uni-a")}
	pushScopeCluster(t, ts, "freetext", inv)

	clusters := func(raw []byte) []string {
		var got []struct{ Name string }
		_ = json.Unmarshal(raw, &got)
		var out []string
		for _, c := range got {
			out = append(out, c.Name)
		}
		return out
	}
	for _, tc := range []struct {
		team, encoded string
		headers       []string // the trusted header's values that name the team
		namespace     string   // the one namespace its findings may name
	}{
		{"Platform Team", "Platform%20Team", []string{"Platform Team", "Platform%20Team", " Platform%20Team , nobody"}, "plat-a"},
		{"Équipe, Paris", "%C3%89quipe%2C%20Paris", []string{"%C3%89quipe%2C%20Paris"}, "fr-a"},
		{"équipe", "%C3%A9quipe", []string{"équipe", "%C3%A9quipe"}, "uni-a"},
		{StarTeam, "%28%2A%29", []string{StarTeam}, "star-a"},
	} {
		tok := "tok-" + tc.namespace
		mintReadToken(t, st, tok, tc.team)
		resp, raw := fetch(t, ts, "/api/v1/clusters", tok)
		if !slices.Equal(clusters(raw), []string{"freetext"}) || resp.Header.Get(scopeHeader) != tc.encoded {
			t.Errorf("token for %q: clusters %v, %s %q, want [freetext], %q", tc.team, clusters(raw), scopeHeader, resp.Header.Get(scopeHeader), tc.encoded)
		}
		for _, v := range tc.headers {
			resp, raw := fetch(t, ts, "/api/v1/clusters", "", header, v)
			if resp.StatusCode != http.StatusOK || !slices.Equal(clusters(raw), []string{"freetext"}) {
				t.Errorf("%s: %q = %d %v, want [freetext]", header, v, resp.StatusCode, clusters(raw))
			}
		}
		id := "/api/v1/clusters/" + clusterID(t, ts, "freetext")
		_, raw = fetch(t, ts, id+"/findings", tok)
		var body struct {
			Findings []struct{ Namespaces []string }
		}
		if err := json.Unmarshal(raw, &body); err != nil {
			t.Fatal(err)
		}
		for _, f := range body.Findings {
			if !slices.Equal(f.Namespaces, []string{tc.namespace}) {
				t.Errorf("token for %q reads a finding of %v, want %s's only", tc.team, f.Namespaces, tc.namespace)
			}
		}
		if want := tc.namespace != "star-a"; want != (len(body.Findings) == 1) {
			t.Errorf("token for %q reads %d findings: %s", tc.team, len(body.Findings), raw)
		}
	}

	// A team with a comma sent unencoded is two teams; one that is not
	// valid percent-encoding is none; a header spelled with underscores
	// is another header.
	for _, h := range [][2]string{{header, "Équipe, Paris"}, {header, "50%off"}, {"X_Forwarded_Groups", "Platform Team"}} {
		resp, raw := fetch(t, ts, "/api/v1/clusters", "", h[0], h[1])
		if bytes.Contains(raw, []byte("freetext")) {
			t.Errorf("%s: %q = %d %s, want no cluster", h[0], h[1], resp.StatusCode, raw)
		}
	}
}

// clusterID is the id of the cluster called name, as the fleet-wide token
// lists it.
func clusterID(t *testing.T, ts *httptest.Server, name string) string {
	t.Helper()
	_, raw := fetch(t, ts, "/api/v1/clusters", "fleet-tok")
	var got []struct {
		ID   json.Number
		Name string
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	for _, c := range got {
		if c.Name == name {
			return c.ID.String()
		}
	}
	t.Fatalf("no cluster %s in %s", name, raw)
	return ""
}

// Only a list's optional whitespace, space and tab, is trimmed around an
// entry. strings.TrimSpace also removed Unicode spaces, so a group named
// "payments" followed by a no-break space read as the team payments
// (#250). The comma and percent aliasing is oauth2-proxy's format, which
// the docs make a condition of the mode; this closes the one the server
// added.
func TestDecodeTeamsTrimsOnlySpaceAndTab(t *testing.T) {
	for v, want := range map[string][]string{
		" payments\t, web ": {"payments", "web"},
		"payments ":         {"payments "},
		" payments":         {" payments"},
		"payments　,web":     {"payments　", "web"},
		"payments\u0085":    {"payments\u0085"},
		"payments%C2%A0":    {"payments "},
		"\t":                nil,
	} {
		if got := decodeTeams(v); !slices.Equal(got, want) {
			t.Errorf("decodeTeams(%q) = %q, want %q", v, got, want)
		}
	}
}
