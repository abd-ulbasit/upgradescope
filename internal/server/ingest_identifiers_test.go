package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/abd-ulbasit/upgradescope/internal/inventory"
)

// pushNamed is pushReqBody with another clusterName.
func pushNamed(t *testing.T, name string, inv inventory.Inventory) []byte {
	t.Helper()
	var req map[string]any
	if err := json.Unmarshal(pushReqBody(t, inv), &req); err != nil {
		t.Fatal(err)
	}
	req["clusterName"] = name
	b, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// A push's clusterName is an RFC 1123 subdomain of at most 253 bytes:
// "../<script>x" registered a cluster of that name (#37). An invalid one
// is 422 and registers nothing.
func TestIngestRefusesInvalidClusterNames(t *testing.T) {
	for _, name := range []string{"../<script>x", strings.Repeat("a", 254), "Prod-EU", "prod eu", "prod_eu"} {
		st := newFakeStore()
		s := newTestServer(t, st)
		rec := httptest.NewRecorder()
		serveIngest(s, rec, pushNamed(t, name, testInventoryWithPSP()), false)
		if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "clusterName") ||
			!strings.Contains(rec.Body.String(), "RFC 1123 subdomain") {
			t.Fatalf("clusterName %.40q: status %d (%s), want 422 naming the rule", name, rec.Code, rec.Body)
		}
		if clusters, err := st.ListClusters(context.Background()); err != nil || len(clusters) != 0 {
			t.Fatalf("clusterName %.40q: %d clusters registered (%v), want none", name, len(clusters), err)
		}
	}
	for _, name := range []string{"prod-eu-1", strings.Repeat("a", 63) + "." + strings.Repeat("b", 63) + "." + strings.Repeat("c", 63) + "." + strings.Repeat("d", 61)} {
		rec := httptest.NewRecorder()
		serveIngest(newTestServer(t, newFakeStore()), rec, pushNamed(t, name, testInventoryWithPSP()), false)
		if rec.Code != http.StatusAccepted {
			t.Fatalf("clusterName %.40q: status %d (%s), want 202", name, rec.Code, rec.Body)
		}
	}
}

// An identifier that is not valid for what it names is not from a
// genuine inventory: 422 with the field and the rule, before anything is
// stored. A namespace "name" of 190 apostrophes in every finding made a
// 21 MB push three 42 MB reports and a 200 MB HTML export.
func TestIngestRefusesInvalidIdentifiers(t *testing.T) {
	for _, tc := range []struct {
		name  string
		edit  func(*inventory.Inventory)
		field string
	}{
		{"namespace key of apostrophes", func(inv *inventory.Inventory) {
			inv.APIUsage[0].Namespaces[strings.Repeat("'", 190)] = 1
		}, "inventory.apiUsage[0].namespaces"},
		{"object namespace", func(inv *inventory.Inventory) {
			inv.APIUsage[0].Objects = []inventory.ObjectRef{{Namespace: "Team A", Name: "x"}}
		}, "inventory.apiUsage[0].objects[0].namespace"},
		{"object name", func(inv *inventory.Inventory) {
			inv.APIUsage[0].Objects = []inventory.ObjectRef{{Name: "../x"}}
		}, "inventory.apiUsage[0].objects[0].name"},
		{"team label value", func(inv *inventory.Inventory) {
			inv.Namespaces = []inventory.NamespaceInfo{{Name: "web", Team: "<b>"}}
		}, "inventory.namespaces[0].team"},
		{"Helm release namespace", func(inv *inventory.Inventory) {
			inv.HelmReleases = []inventory.HelmRelease{{Name: "web", Namespace: strings.Repeat("n", 64)}}
		}, "inventory.helmReleases[0].namespace"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := newFakeStore()
			s := newTestServer(t, st)
			inv := testInventoryWithPSP()
			tc.edit(&inv)
			rec := httptest.NewRecorder()
			serveIngest(s, rec, pushReqBody(t, inv), false)
			if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), tc.field+": ") {
				t.Fatalf("status %d (%.400s), want 422 naming %s", rec.Code, rec.Body, tc.field)
			}
			if clusters, err := st.ListClusters(context.Background()); err != nil || len(clusters) != 0 || len(st.snapshots) != 0 {
				t.Fatalf("%d clusters, %d snapshots stored (%v), want none", len(clusters), len(st.snapshots), err)
			}
		})
	}
}

// A value beyond what any collector records is refused the same way:
// a kubeVersion of 20 MB of quotes made a push three 84 MB reports and a
// 126 MB HTML export.
func TestIngestRefusesValuesBeyondLimits(t *testing.T) {
	for _, tc := range []struct {
		name  string
		edit  func(*inventory.Inventory)
		field string
	}{
		{"kubeVersion of quotes", func(inv *inventory.Inventory) {
			inv.HelmReleases = []inventory.HelmRelease{{Name: "web", Namespace: "web", KubeVersion: strings.Repeat(`"`, inventory.MaxStringBytes+1)}}
		}, "inventory.helmReleases[0].kubeVersion"},
		{"manager", func(inv *inventory.Inventory) {
			inv.APIUsage[0].Objects = []inventory.ObjectRef{{Name: "x", Manager: strings.Repeat("'", 200)}}
		}, "inventory.apiUsage[0].objects[0].manager"},
		{"a group/version/kind twice", func(inv *inventory.Inventory) {
			inv.APIUsage = append(inv.APIUsage, inv.APIUsage[0])
		}, "inventory.apiUsage[1]"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := newFakeStore()
			s := newTestServer(t, st)
			inv := testInventoryWithPSP()
			tc.edit(&inv)
			rec := httptest.NewRecorder()
			serveIngest(s, rec, pushReqBody(t, inv), false)
			if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), tc.field+": ") {
				t.Fatalf("status %d (%.400s), want 422 naming %s", rec.Code, rec.Body, tc.field)
			}
			if clusters, err := st.ListClusters(context.Background()); err != nil || len(clusters) != 0 || len(st.snapshots) != 0 {
				t.Fatalf("%d clusters, %d snapshots stored (%v), want none", len(clusters), len(st.snapshots), err)
			}
		})
	}
}

// Every inventory the engine's golden tests judge, which the collectors'
// own tests produce the shapes of, is accepted: the identifier rules and
// the limits refuse nothing a cluster can hold.
func TestIngestAcceptsTheGoldenInventories(t *testing.T) {
	paths, err := filepath.Glob("../engine/testdata/*/inventory.json")
	if err != nil || len(paths) < 10 {
		t.Fatalf("found %d golden inventories (%v)", len(paths), err)
	}
	for _, p := range paths {
		raw, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		var inv inventory.Inventory
		if err := json.Unmarshal(raw, &inv); err != nil {
			t.Fatalf("%s: %v", p, err)
		}
		if err := inv.ValidateIdentifiers(); err != nil {
			t.Errorf("%s: %v", p, err)
		}
		if err := inv.ValidateLimits(); err != nil {
			t.Errorf("%s: %v", p, err)
		}
	}
}
