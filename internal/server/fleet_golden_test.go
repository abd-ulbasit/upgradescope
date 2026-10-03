package server

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/abd-ulbasit/upgradescope/internal/server/store"
)

// fleetGoldenDir holds what --read-token read of the scope fixture
// (scopeServer) before read scopes existed: the answers of the server at
// 7958e18, main before #72, written there by TestDumpFleetGolden (with the
// fixture and this file's dump half copied in). A fleet-wide read must
// still answer them byte for byte. Regenerate them only on a commit
// without read scopes, or the test proves nothing.
const fleetGoldenDir = "testdata/fleet-wide"

// fleetGoldenReads are the reads the golden files hold, by file name:
// every read endpoint, on the cluster whose finding spans teams and on the
// fleet, and the gate with ?cluster= and without.
func fleetGoldenReads(ids map[string]int64) map[string]string {
	shared := fmt.Sprint("/api/v1/clusters/", ids["shared"])
	mixed := fmt.Sprint("/api/v1/clusters/", ids["mixed"])
	return map[string]string{
		"clusters.json":        "/api/v1/clusters",
		"shared.json":          shared,
		"shared-report.json":   shared + "/report",
		"shared-whatif.json":   shared + "/report?target=1.36",
		"shared-findings.json": shared + "/findings",
		"shared-teams.json":    shared + "/teams",
		"shared-history.json":  shared + "/history",
		"shared-export.csv":    shared + "/export?format=csv",
		"shared-export.html":   shared + "/export?format=html",
		"mixed-report.json":    mixed + "/report",
		"mixed-export.csv":     mixed + "/export?format=csv",
		"fleet.json":           "/api/v1/fleet",
		"fleet-teams.json":     "/api/v1/fleet/teams?target=1.35",
		"gate-cluster.json":    "POST ?target=1.35&fail-on=never&cluster=shared",
		"gate-cluster.sarif":   "POST ?target=1.35&fail-on=never&cluster=shared&format=sarif",
		"gate-no-cluster.json": "POST ?target=1.35&fail-on=never",
	}
}

// readGolden answers one of fleetGoldenReads with token: GETs, or the gate
// with ingressManifest for "POST <query>".
func readGolden(t *testing.T, ts *httptest.Server, path, token string) []byte {
	t.Helper()
	var resp *http.Response
	var raw []byte
	if q, ok := strings.CutPrefix(path, "POST "); ok {
		resp, raw = postGate(t, ts, q, token, ingressManifest, "application/x-yaml")
	} else {
		resp, raw = fetch(t, ts, path, token)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("%s = %d %s", path, resp.StatusCode, raw)
	}
	return raw
}

// TestDumpFleetGolden writes the golden files from this build into the
// directory UPGRADESCOPE_FLEET_GOLDEN names; run on main before read
// scopes, it wrote testdata/fleet-wide.
func TestDumpFleetGolden(t *testing.T) {
	dir := os.Getenv("UPGRADESCOPE_FLEET_GOLDEN")
	if dir == "" {
		t.Skip("UPGRADESCOPE_FLEET_GOLDEN unset")
	}
	_, _, ts, ids := scopeServer(t)
	for name, path := range fleetGoldenReads(ids) {
		if err := os.WriteFile(filepath.Join(dir, name), readGolden(t, ts, path, "fleet-tok"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// Every fleet-wide credential reads the scope fixture exactly as
// --read-token did at main before read scopes existed, with read tokens
// minted, a team-scoped one among them.
func TestFleetWideReadsMatchMainBeforeReadScopes(t *testing.T) {
	_, st, ts, ids := scopeServer(t)
	mintReadToken(t, st, "star-tok", store.ReadScopeFleet)
	mintReadToken(t, st, "pay-tok", "payments")
	for name, path := range fleetGoldenReads(ids) {
		want, err := os.ReadFile(filepath.Join(fleetGoldenDir, name))
		if err != nil {
			t.Fatal(err)
		}
		for _, tok := range []string{"fleet-tok", "star-tok", "admin-tok"} {
			if got := readGolden(t, ts, path, tok); !bytes.Equal(got, want) {
				t.Errorf("%s with %s differs from main's %s:\n got %s\nwant %s", path, tok, name, got, want)
			}
		}
	}
}
