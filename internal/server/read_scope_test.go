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

	"github.com/abd-ulbasit/upgradescope/internal/engine"
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
	{"shared", sharedInventory},
}

// sharedInventory is a cluster whose one finding spans teams (#72): an
// Ingress at extensions/v1beta1 in a payments namespace, in a web one
// (annotated as accepted, so a gate suppresses it) and in one no team
// owns. It also has an unrecognized image of web's and a partial helm
// capability whose message and skipped list name a web release. A
// payments-scoped read must see none of web's, nor of the unowned
// namespace (sharedSecrets).
func sharedInventory() inventory.Inventory {
	inv := testInventory()
	inv.Namespaces = []inventory.NamespaceInfo{{Name: "pay-prod", Team: "payments"}, {Name: "web-secret-ns", Team: "web"}, {Name: "shared-tools"}}
	inv.APIUsage = []inventory.APIUsage{{
		Group: "extensions", Version: "v1beta1", Kind: "Ingress", Count: 3,
		Namespaces: map[string]int{"pay-prod": 1, "web-secret-ns": 1, "shared-tools": 1},
		Objects: []inventory.ObjectRef{
			{Namespace: "pay-prod", Name: "pay-ingress"},
			{Namespace: "web-secret-ns", Name: "web-secret-ingress", Ignore: "removed-api", IgnoreReason: "web-secret-reason"},
			{Namespace: "shared-tools", Name: "tools-ingress"},
		},
	}}
	inv.UnrecognizedImages = []string{"registry.example.com/web/secret-app"}
	inv.Capabilities[inventory.CapHelm] = inventory.CapabilityStatus{
		Available: true, Partial: true,
		Reason:  "1 release(s) not read, first web-secret-ns/secret-release",
		Skipped: []string{"web-secret-ns/secret-release"},
	}
	return inv
}

// sharedSecrets are what the shared cluster says of web and of the
// namespace no team owns.
var sharedSecrets = []string{"web-secret", "secret-app", "secret-release", "shared-tools", "tools-ingress"}

// leaked is the first of sharedSecrets that body contains, or "".
func leaked(body []byte) string {
	for _, s := range sharedSecrets {
		if bytes.Contains(body, []byte(s)) {
			return s
		}
	}
	return ""
}

// ingressManifest is a PR's Ingress at extensions/v1beta1 in a namespace
// the cluster does not have yet: with ?cluster=shared, the cluster's
// Ingress objects join its finding, and the PR's namespace and object are
// the caller's own, whatever its scope.
const ingressManifest = `apiVersion: extensions/v1beta1
kind: Ingress
metadata:
  name: pr-ingress
  namespace: pr-new-ns
`

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
		if !slices.Equal(names, []string{"calm", "mixed", "shared"}) {
			t.Errorf("clusters = %v, want [calm mixed shared]: calm has a payments namespace and no finding", names)
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
		if !slices.Equal(names, []string{"calm", "mixed", "shared"}) {
			t.Errorf("fleet rows = %v, want [calm mixed shared]", names)
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
		// Without ?cluster= the answer is all the caller's own manifests:
		// a scoped caller reads it exactly as the fleet-wide one does,
		// whatever namespaces they name.
		const webManifest = `apiVersion: extensions/v1beta1
kind: Ingress
metadata:
  name: web-ingress
  namespace: web-prod
`
		resp, raw := postGate(t, ts, "?target=1.35&fail-on=never", "pay-tok", webManifest, "application/x-yaml")
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status %d, %s", resp.StatusCode, raw)
		}
		_, fleet := postGate(t, ts, "?target=1.35&fail-on=never", "fleet-tok", webManifest, "application/x-yaml")
		if !bytes.Equal(raw, fleet) || !bytes.Contains(raw, []byte("web-prod (1)")) {
			t.Errorf("payments-scoped gate without ?cluster=:\n%s\nwant the fleet-wide answer:\n%s", raw, fleet)
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

// A finding that spans teams is cut to the scope's: a payments-scoped
// read of the shared cluster lists the Ingress finding with payments'
// namespace and object only, from every endpoint that carries findings,
// and none of web's namespaces, objects, team, accepted objects,
// unrecognized images or Helm releases, nor the unowned namespace's. The
// fleet-wide token reads all of it.
func TestScopedReadCutsFindingsThatSpanTeams(t *testing.T) {
	_, st, ts, ids := scopeServer(t)
	mintReadToken(t, st, "pay-tok", "payments")
	shared := "/api/v1/clusters/" + fmt.Sprint(ids["shared"])

	reads := []string{
		"/api/v1/clusters", shared, shared + "/report", shared + "/report?target=1.36", shared + "/findings",
		shared + "/teams", shared + "/history", shared + "/export?format=csv", shared + "/export?format=html",
		"/api/v1/fleet", "/api/v1/fleet/teams?target=1.35",
	}
	for _, p := range reads {
		resp, raw := fetch(t, ts, p, "pay-tok")
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s = %d %s", p, resp.StatusCode, raw)
		}
		if s := leaked(raw); s != "" {
			t.Errorf("payments-scoped %s names %q:\n%s", p, s, raw)
		}
		if bytes.Contains(raw, []byte(`"web"`)) {
			t.Errorf("payments-scoped %s names the web team:\n%s", p, raw)
		}
	}
	// What the fleet-wide token reads has them all: the fixture holds
	// what the test looks for.
	// (Exports list namespaces, not objects.)
	for _, p := range []string{shared + "/report", shared + "/findings", shared + "/export?format=csv", shared + "/export?format=html"} {
		_, raw := fetch(t, ts, p, "fleet-tok")
		want := []string{"web-secret-ns", "shared-tools", "pay-prod"}
		if !strings.Contains(p, "/export") {
			want = append(want, "web-secret-ingress", "tools-ingress", "pay-ingress")
		}
		for _, s := range want {
			if !bytes.Contains(raw, []byte(s)) {
				t.Errorf("fleet-wide %s does not name %q", p, s)
			}
		}
	}
	for _, p := range []string{shared + "/report", "/api/v1/clusters", shared} {
		_, raw := fetch(t, ts, p, "fleet-tok")
		for _, s := range []string{"secret-release"} {
			if !bytes.Contains(raw, []byte(s)) {
				t.Errorf("fleet-wide %s does not name %q", p, s)
			}
		}
	}
	if _, raw := fetch(t, ts, shared+"/report", "fleet-tok"); !bytes.Contains(raw, []byte("secret-app")) {
		t.Errorf("fleet-wide report has no unrecognized image")
	}

	// What payments does see: its own object, under the finding's key,
	// counted as the scope's.
	for _, p := range []string{shared + "/report", shared + "/findings", shared + "/report?target=1.36"} {
		_, raw := fetch(t, ts, p, "pay-tok")
		var body struct {
			Findings []struct {
				Key        string
				Title      string
				Teams      []string
				Namespaces []string
				Objects    []struct{ Namespace, Name string }
			}
		}
		if err := json.Unmarshal(raw, &body); err != nil {
			t.Fatal(err)
		}
		if len(body.Findings) != 1 {
			t.Fatalf("%s: %d findings, want the Ingress one: %s", p, len(body.Findings), raw)
		}
		f := body.Findings[0]
		if f.Key != "removed-api/extensions/v1beta1/Ingress" || !slices.Equal(f.Teams, []string{"payments"}) ||
			!slices.Equal(f.Namespaces, []string{"pay-prod"}) || len(f.Objects) != 1 || f.Objects[0].Name != "pay-ingress" {
			t.Errorf("%s: finding = %+v, want the Ingress finding cut to pay-prod's pay-ingress", p, f)
		}
		if !strings.HasSuffix(f.Title, "(1 object in scope)") {
			t.Errorf("%s: title %q, want the scope's count of objects", p, f.Title)
		}
	}
	if _, raw := fetch(t, ts, shared+"/export?format=csv", "pay-tok"); !bytes.Contains(raw, []byte("pay-prod")) {
		t.Errorf("payments-scoped CSV export does not name payments' own namespace:\n%s", raw)
	}

	// The gate: the cluster's finding, and the PR's that the cluster's
	// objects join, are cut alike; the PR's own object stays, and web's
	// accepted object is suppressed out of sight.
	for _, manifest := range []string{pspManifest, ingressManifest} {
		for _, format := range []string{"", "&format=sarif", "&format=junit", "&format=gitlab-codequality"} {
			q := "?target=1.35&fail-on=never&cluster=shared" + format
			resp, raw := postGate(t, ts, q, "pay-tok", manifest, "application/x-yaml")
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("gate %s = %d %s", q, resp.StatusCode, raw)
			}
			if s := leaked(raw); s != "" {
				t.Errorf("payments-scoped gate %s names %q:\n%s", q, s, raw)
			}
			if manifest == ingressManifest && (!bytes.Contains(raw, []byte("pr-ingress")) || !bytes.Contains(raw, []byte("pr-new-ns"))) {
				t.Errorf("payments-scoped gate %s lost the PR's own object:\n%s", q, raw)
			}
			if format == "" && !bytes.Contains(raw, []byte("pay-ingress")) {
				t.Errorf("payments-scoped gate %s lost payments' cluster object:\n%s", q, raw)
			}
			if format == "" && manifest == ingressManifest {
				var body struct {
					Findings []struct {
						Key, Source string
						Namespaces  []string
						Objects     []struct{ Name string }
					}
				}
				if err := json.Unmarshal(raw, &body); err != nil {
					t.Fatal(err)
				}
				var names []string
				for _, f := range body.Findings {
					if f.Key != "removed-api/extensions/v1beta1/Ingress" {
						continue
					}
					for _, o := range f.Objects {
						names = append(names, o.Name)
					}
					if f.Source != sourceManifest || !slices.Equal(f.Namespaces, []string{"pay-prod", "pr-new-ns"}) {
						t.Errorf("gate's Ingress finding: source %s, namespaces %v, want the manifests', pay-prod and pr-new-ns", f.Source, f.Namespaces)
					}
				}
				if slices.Sort(names); !slices.Equal(names, []string{"pay-ingress", "pr-ingress"}) {
					t.Errorf("gate's Ingress objects = %v, want payments' and the PR's", names)
				}
			}
		}
		_, raw := postGate(t, ts, "?target=1.35&fail-on=never&cluster=shared", "fleet-tok", manifest, "application/x-yaml")
		var body struct {
			Suppressed []struct{ Objects []struct{ Name string } }
		}
		if err := json.Unmarshal(raw, &body); err != nil {
			t.Fatal(err)
		}
		if len(body.Suppressed) == 0 || !bytes.Contains(raw, []byte("web-secret-reason")) {
			t.Errorf("fleet-wide gate has no suppressed web object: the fixture does not test the cut:\n%s", raw)
		}
	}
}

// readScope.report cuts the suppressed findings as it cuts the findings:
// one of another team's is left out, one spanning teams keeps only the
// scope's objects, and one whose accepted objects are all another team's
// is left out, though its finding spans the scope's team too. The
// fleet-wide scope keeps every one.
func TestScopeCutsSuppressedFindings(t *testing.T) {
	ns := map[string]string{"pay-prod": "payments", "web-prod": "web"}
	sup := func(teams, namespaces []string, objects ...inventory.ObjectRef) engine.SuppressedFinding {
		return engine.SuppressedFinding{Finding: engine.Finding{
			Category: engine.CatRemovedAPI, Severity: engine.SevBlocker, Key: "removed-api/x", Title: "x removed (2 objects)",
			Detail: "2 object(s) still stored/served at this version.", Teams: teams, Namespaces: namespaces, Objects: objects,
		}, Reason: "accepted", Source: "annotation"}
	}
	payObj := inventory.ObjectRef{Namespace: "pay-prod", Name: "pay-obj"}
	webObj := inventory.ObjectRef{Namespace: "web-prod", Name: "web-obj"}
	rep := engine.Report{Findings: []engine.Finding{}, Suppressed: []engine.SuppressedFinding{
		sup([]string{"web"}, []string{"web-prod"}, webObj),
		sup([]string{"payments", "web"}, []string{"pay-prod", "web-prod"}, payObj, webObj),
		sup([]string{"payments", "web"}, []string{"pay-prod", "web-prod"}, webObj),
		sup([]string{"payments"}, []string{"pay-prod"}, payObj),
	}}

	if got := fleetScope.report(rep, ns); len(got.Suppressed) != 4 {
		t.Errorf("fleet-wide: %d suppressed findings, want all 4", len(got.Suppressed))
	}
	got := scopeOfTeams([]string{"payments"}).report(rep, ns).Suppressed
	if len(got) != 2 {
		t.Fatalf("payments: %d suppressed findings, want 2: %+v", len(got), got)
	}
	for _, f := range got {
		raw, _ := json.Marshal(f)
		if bytes.Contains(raw, []byte("web")) {
			t.Errorf("payments' suppressed finding names web: %s", raw)
		}
		if len(f.Objects) != 1 || f.Objects[0] != payObj || f.Reason != "accepted" {
			t.Errorf("payments' suppressed finding = %+v, want pay-obj, still accepted", f)
		}
	}
	if got[1].Detail != rep.Suppressed[3].Detail {
		t.Errorf("a suppressed finding of payments' only was rewritten: %q", got[1].Detail)
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
			{"payments", []string{"calm", "mixed", "shared"}},
			{"web, other", []string{"mixed", "shared", "web"}},
			// The header names teams only: "*" is a team nobody owns.
			{"*", nil},
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
		if _, raw := fetch(t, ts, "/api/v1/clusters", "pay-tok"); !slices.Equal(names(raw), []string{"calm", "mixed", "shared"}) {
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
			if _, raw := fetch(t, ts, "/api/v1/clusters", "pay-tok", header, value); !slices.Equal(names(raw), []string{"calm", "mixed", "shared"}) {
				t.Errorf("pay-tok with a spoofed %s: %s = %v, want the token's scope only", header, value, names(raw))
			}
		}
		// The same request from inside the range is trusted.
		req := httptest.NewRequest(http.MethodGet, "/api/v1/clusters", nil)
		req.RemoteAddr = "10.1.2.3:41000"
		req.Header.Set(header, "web")
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusOK || !slices.Equal(names(rec.Body.Bytes()), []string{"mixed", "shared", "web"}) {
			t.Errorf("from 10.1.2.3 = %d %v, want 200 [mixed shared web]", rec.Code, names(rec.Body.Bytes()))
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
