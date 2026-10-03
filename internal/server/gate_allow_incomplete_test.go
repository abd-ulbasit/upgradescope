package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// pastHorizon is a target newer than testKB's newest known minor (1.99): a
// required kb-coverage gap, so the verdict is unknown unless a blocker
// decides it.
const pastHorizon = "?target=1.100"

// TestGateAllowIncomplete: ?allow-incomplete=true is `scan --allow-incomplete`
// (#177): the gate decides on findings alone. An unknown verdict with no
// finding at the fail-on threshold answers 200, with the verdict header,
// the verdict and the required gaps still saying so; a blocker still
// answers 422. Without it the answer is unchanged.
func TestGateAllowIncomplete(t *testing.T) {
	ts := httptest.NewServer(newTestServer(t, newFakeStore()).Handler())
	defer ts.Close()

	cases := []struct {
		name, query, body string
		wantStatus        int
		wantVerdict       string
		wantGap           bool
	}{
		{"unknown without it fails", pastHorizon, deploymentManifest, http.StatusUnprocessableEntity, "unknown", true},
		{"false is the default", pastHorizon + "&allow-incomplete=false", deploymentManifest, http.StatusUnprocessableEntity, "unknown", true},
		{"unknown with it passes", pastHorizon + "&allow-incomplete=true", deploymentManifest, http.StatusOK, "unknown", true},
		{"a blocker still fails", pastHorizon + "&allow-incomplete=true", pspManifest, http.StatusUnprocessableEntity, "blocked", true},
		{"a warning below the threshold passes", "?target=1.34&allow-incomplete=true", pspManifest, http.StatusOK, "ready", false},
		{"fail-on warning still fails on a warning", "?target=1.34&fail-on=warning&allow-incomplete=true", pspManifest, http.StatusUnprocessableEntity, "ready", false},
		{"fail-on never is 200 either way", pastHorizon + "&fail-on=never&allow-incomplete=true", deploymentManifest, http.StatusOK, "unknown", true},
		{"sarif follows it", pastHorizon + "&format=sarif&allow-incomplete=true", deploymentManifest, http.StatusOK, "unknown", false},
		{"junit follows it", pastHorizon + "&format=junit&allow-incomplete=true", deploymentManifest, http.StatusOK, "unknown", false},
		{"code quality follows it", pastHorizon + "&format=gitlab-codequality&allow-incomplete=true", deploymentManifest, http.StatusOK, "unknown", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp, raw := postGate(t, ts, tc.query, "", tc.body, "application/x-yaml")
			if resp.StatusCode != tc.wantStatus {
				t.Fatalf("status = %d, want %d (body %s)", resp.StatusCode, tc.wantStatus, raw)
			}
			if got := resp.Header.Get("X-Upgradescope-Verdict"); got != tc.wantVerdict {
				t.Errorf("X-Upgradescope-Verdict = %q, want %q", got, tc.wantVerdict)
			}
			if !strings.Contains(tc.query, "format=") {
				var body struct {
					Verdict     string `json:"verdict"`
					Ready       bool   `json:"ready"`
					NotAssessed []struct {
						Capability string `json:"capability"`
						Required   bool   `json:"required"`
					} `json:"notAssessed"`
				}
				if err := json.Unmarshal(raw, &body); err != nil {
					t.Fatalf("body is not JSON: %v\n%s", err, raw)
				}
				if body.Verdict != tc.wantVerdict || body.Ready != (tc.wantVerdict == "ready") {
					t.Errorf("body verdict %q ready %v, want %q", body.Verdict, body.Ready, tc.wantVerdict)
				}
				gap := false
				for _, g := range body.NotAssessed {
					gap = gap || g.Required && g.Capability == "kb-coverage"
				}
				if gap != tc.wantGap {
					t.Errorf("required kb-coverage gap listed = %v, want %v (%+v)", gap, tc.wantGap, body.NotAssessed)
				}
			}
			if strings.Contains(tc.query, "format=junit") && strings.Contains(string(raw), "<error") {
				t.Errorf("junit has an <error> for a gap the gate excuses:\n%s", raw)
			}
		})
	}
}

// TestGateAllowIncompleteNotAnUpgrade: as in `scan`, a target that is not an
// upgrade of the cluster is a user error, not a coverage limit, so
// allow-incomplete does not excuse it.
func TestGateAllowIncompleteNotAnUpgrade(t *testing.T) {
	ts := httptest.NewServer(newTestServer(t, newFakeStore(), func(c *Config) { c.ReadToken = "read-tok" }).Handler())
	defer ts.Close()
	if resp, out := postSnapshot(t, ts, "ingest-tok", pushReqBody(t, testInventory()), false); resp.StatusCode != http.StatusAccepted {
		t.Fatalf("seed push = %d %v", resp.StatusCode, out) // the cluster runs v1.34.2
	}
	resp, raw := postGate(t, ts, "?target=1.34&cluster=prod-eu-1&allow-incomplete=true", "read-tok", deploymentManifest, "application/x-yaml")
	if resp.StatusCode != http.StatusUnprocessableEntity || resp.Header.Get("X-Upgradescope-Verdict") != "unknown" {
		t.Fatalf("same-minor target with allow-incomplete = %d, verdict %q, want 422 unknown (body %s)",
			resp.StatusCode, resp.Header.Get("X-Upgradescope-Verdict"), raw)
	}
}

// TestGateAllowIncompleteInvalid: only true and false are values, as in the
// Action's input; anything else is refused like a bad fail-on, as is a
// repeated parameter.
func TestGateAllowIncompleteInvalid(t *testing.T) {
	ts := httptest.NewServer(newTestServer(t, newFakeStore()).Handler())
	defer ts.Close()
	for _, q := range []string{"yes", "1", "TRUE", "", "true,false"} {
		resp, raw := postGate(t, ts, "?target=1.35&allow-incomplete="+q, "", deploymentManifest, "application/x-yaml")
		if resp.StatusCode != http.StatusUnprocessableEntity || !json.Valid(raw) || resp.Header.Get("X-Upgradescope-Verdict") != "" {
			t.Errorf("allow-incomplete=%q = %d %s, want a 422 JSON error", q, resp.StatusCode, raw)
		}
	}
	resp, raw := postGate(t, ts, "?target=1.35&allow-incomplete=true&allow-incomplete=false", "", deploymentManifest, "application/x-yaml")
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Errorf("repeated allow-incomplete = %d %s, want 422", resp.StatusCode, raw)
	}
}
