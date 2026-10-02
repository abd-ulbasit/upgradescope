package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

const deploymentManifest = `apiVersion: apps/v1
kind: Deployment
metadata:
  name: web
  namespace: payments-prod
`

type failOnGateResponse struct {
	Ready          bool   `json:"ready"`
	Verdict        string `json:"verdict"`
	ClusterVerdict string `json:"clusterVerdict"`
	Score          int    `json:"score"`
	Findings       []struct {
		Key      string `json:"key"`
		Severity string `json:"severity"`
		Source   string `json:"source"`
	} `json:"findings"`
}

func decodeGate(t *testing.T, raw []byte) failOnGateResponse {
	t.Helper()
	var b failOnGateResponse
	if err := json.Unmarshal(raw, &b); err != nil {
		t.Fatalf("gate body is not JSON: %v\n%s", err, raw)
	}
	return b
}

// TestGateFailOn: fail-on defaults to blocker, like scan --fail-on, so a
// bare request fails CI (#120 FS-04): reaching the threshold is a 422
// carrying the full report, so `curl --fail-with-body` fails the CI step
// and keeps the body. fail-on=never keeps the always-200 contract. The
// verdict header is always set.
func TestGateFailOn(t *testing.T) {
	ts := httptest.NewServer(newTestServer(t, newFakeStore()).Handler())
	defer ts.Close()

	cases := []struct {
		name, query, body string
		wantStatus        int
		wantVerdict       string
	}{
		{"no fail-on fails on a blocker", "?target=1.35", pspManifest, http.StatusUnprocessableEntity, "blocked"},
		{"no fail-on passes a clean stream", "?target=1.35", deploymentManifest, http.StatusOK, "ready"},
		{"never keeps 200", "?target=1.35&fail-on=never", pspManifest, http.StatusOK, "blocked"},
		{"blocker hit", "?target=1.35&fail-on=blocker", pspManifest, http.StatusUnprocessableEntity, "blocked"},
		{"blocker clean", "?target=1.35&fail-on=blocker", deploymentManifest, http.StatusOK, "ready"},
		// PSP is removed in 1.35, so for target 1.34 it is a warning.
		{"warning below blocker threshold", "?target=1.34&fail-on=blocker", pspManifest, http.StatusOK, "ready"},
		{"warning hit", "?target=1.34&fail-on=warning", pspManifest, http.StatusUnprocessableEntity, "ready"},
		{"sarif blocker hit", "?target=1.35&fail-on=blocker&format=sarif", pspManifest, http.StatusUnprocessableEntity, "blocked"},
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
			if len(raw) == 0 || raw[0] != '{' {
				t.Fatalf("want the full report body even on failure, got %s", raw)
			}
		})
	}

	resp, raw := postGate(t, ts, "?target=1.35&fail-on=sometimes", "", pspManifest, "application/x-yaml")
	if resp.StatusCode != http.StatusUnprocessableEntity || !json.Valid(raw) || resp.Header.Get("X-Upgradescope-Verdict") != "" {
		t.Fatalf("invalid fail-on = %d %s, want a 422 JSON error", resp.StatusCode, raw)
	}
}

// TestGateClusterBaseline: with ?cluster= the gate judges only what the
// manifests introduce. A cluster with an existing EOL add-on blocker must
// not fail a clean PR; its finding stays in the body, tagged cluster.
func TestGateClusterBaseline(t *testing.T) {
	st := newFakeStore()
	s := newTestServer(t, st, func(c *Config) { c.KB = istioKB() })
	s.now = func() time.Time { return dec15 } // Istio is past its EOL date
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()
	if resp, out := postSnapshot(t, ts, "ingest-tok", pushReqBody(t, istioInventory()), false); resp.StatusCode != http.StatusAccepted {
		t.Fatalf("seed push = %d %v", resp.StatusCode, out)
	}

	resp, raw := postGate(t, ts, "?target=1.35&cluster=prod-eu-1&fail-on=blocker", "", deploymentManifest, "application/x-yaml")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("clean PR against a blocked cluster = %d, want 200 (body %s)", resp.StatusCode, raw)
	}
	b := decodeGate(t, raw)
	if !b.Ready || b.Verdict != "ready" || b.ClusterVerdict != "blocked" || resp.Header.Get("X-Upgradescope-Verdict") != "ready" {
		t.Errorf("clean PR = ready %v verdict %q clusterVerdict %q header %q, want ready / ready / blocked / ready",
			b.Ready, b.Verdict, b.ClusterVerdict, resp.Header.Get("X-Upgradescope-Verdict"))
	}
	if len(b.Findings) != 1 || b.Findings[0].Key != "eol-addon/istio" || b.Findings[0].Source != "cluster" {
		t.Errorf("findings = %+v, want the Istio blocker tagged source cluster", b.Findings)
	}

	resp, raw = postGate(t, ts, "?target=1.35&cluster=prod-eu-1&fail-on=blocker", "", pspManifest, "application/x-yaml")
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("PR adding a removed API = %d, want 422 (body %s)", resp.StatusCode, raw)
	}
	b = decodeGate(t, raw)
	sources := map[string]string{}
	for _, f := range b.Findings {
		sources[f.Key] = f.Source
	}
	if sources["removed-api/policy/v1beta1/PodSecurityPolicy"] != "manifest" || sources["eol-addon/istio"] != "cluster" {
		t.Errorf("finding sources = %v, want PSP from manifest, Istio from cluster", sources)
	}
	if b.Ready || b.Verdict != "blocked" {
		t.Errorf("PR verdict = %q ready %v, want blocked", b.Verdict, b.Ready)
	}

	// SARIF becomes code-scanning alerts on the PR, so it carries only what
	// the PR introduces: the cluster's Istio blocker must not show up there.
	for _, tc := range []struct {
		name, manifest string
		wantReady      bool
		wantRules      []string
	}{
		{"clean PR", deploymentManifest, true, nil},
		{"PR adding a removed API", pspManifest, false, []string{"removed-api/policy/v1beta1/PodSecurityPolicy"}},
	} {
		_, raw = postGate(t, ts, "?target=1.35&cluster=prod-eu-1&format=sarif", "", tc.manifest, "application/x-yaml")
		var doc struct {
			Runs []struct {
				Invocations []struct {
					Notifications []struct {
						Properties struct {
							FindingKey string `json:"findingKey"`
						} `json:"properties"`
					} `json:"toolExecutionNotifications"`
				} `json:"invocations"`
				Results []struct {
					RuleID string `json:"ruleId"`
				} `json:"results"`
				Properties struct {
					Ready    bool `json:"ready"`
					Findings int  `json:"findings"`
				} `json:"properties"`
			} `json:"runs"`
		}
		if err := json.Unmarshal(raw, &doc); err != nil || len(doc.Runs) != 1 || len(doc.Runs[0].Invocations) != 1 {
			t.Fatalf("%s: SARIF = %v %s", tc.name, err, raw)
		}
		run := doc.Runs[0]
		var rules []string // finding keys, as results or notifications
		for _, n := range run.Invocations[0].Notifications {
			rules = append(rules, n.Properties.FindingKey)
		}
		for _, r := range run.Results {
			rules = append(rules, r.RuleID)
		}
		if run.Properties.Ready != tc.wantReady || run.Properties.Findings != len(tc.wantRules) || len(rules) != len(tc.wantRules) ||
			(len(rules) > 0 && rules[0] != tc.wantRules[0]) {
			t.Errorf("%s: SARIF ready %v findings %d keys %v, want ready %v and only %v",
				tc.name, run.Properties.Ready, run.Properties.Findings, rules, tc.wantReady, tc.wantRules)
		}
	}
}
