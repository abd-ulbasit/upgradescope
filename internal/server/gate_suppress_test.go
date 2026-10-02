package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/abd-ulbasit/upgradescope/internal/codequality/codequalitytest"
	"github.com/abd-ulbasit/upgradescope/internal/junit/junittest"
	"github.com/abd-ulbasit/upgradescope/internal/sarif/sariftest"
)

// annotatedPSP is pspManifest accepted by its own upgradescope.dev/ignore
// annotation.
const annotatedPSP = `apiVersion: policy/v1beta1
kind: PodSecurityPolicy
metadata:
  name: restricted
  namespace: payments-prod
  annotations:
    upgradescope.dev/ignore: removed-api
    upgradescope.dev/ignore-reason: deleted with the 1.35 upgrade
`

// pspRule is a .upgradescope.yaml accepting pspManifest's blocker.
const pspRule = `ignore:
  - key: removed-api/policy/v1beta1/PodSecurityPolicy
    reason: PSPs are removed before the upgrade
`

// suppressedGateResponse is the part of a gate answer suppression changes.
type suppressedGateResponse struct {
	Verdict         string `json:"verdict"`
	ClusterVerdict  string `json:"clusterVerdict"`
	SuppressedCount int    `json:"suppressedCount"`
	Findings        []struct {
		Key    string `json:"key"`
		Source string `json:"source"`
	} `json:"findings"`
	Suppressed []struct {
		Key    string `json:"key"`
		Reason string `json:"reason"`
		Source string `json:"source"`
	} `json:"suppressed"`
	Warnings []string `json:"warnings"`
}

func decodeSuppressed(t *testing.T, raw []byte) suppressedGateResponse {
	t.Helper()
	var b suppressedGateResponse
	if err := json.Unmarshal(raw, &b); err != nil {
		t.Fatalf("gate body is not JSON: %v\n%s", err, raw)
	}
	return b
}

// withConfig is ?config= carrying a .upgradescope.yaml.
func withConfig(config string) string {
	return "&config=" + url.QueryEscape(config)
}

// #44: /gate applies the upgradescope.dev/ignore annotation as scan
// --files does: the annotated object's blocker is suppressed, so the gate
// passes, and the answer lists it with a count.
func TestGateAnnotationSuppresses(t *testing.T) {
	ts := httptest.NewServer(newTestServer(t, newFakeStore()).Handler())
	defer ts.Close()

	resp, raw := postGate(t, ts, "?target=1.35&fail-on=blocker", "", annotatedPSP, "application/x-yaml")
	if resp.StatusCode != http.StatusOK || resp.Header.Get("X-Upgradescope-Verdict") != "ready" {
		t.Fatalf("status %d verdict %q, want 200 ready (body %s)", resp.StatusCode, resp.Header.Get("X-Upgradescope-Verdict"), raw)
	}
	b := decodeSuppressed(t, raw)
	if b.Verdict != "ready" || b.SuppressedCount != 1 || len(b.Suppressed) != 1 ||
		b.Suppressed[0].Key != "removed-api/policy/v1beta1/PodSecurityPolicy" ||
		b.Suppressed[0].Reason != "deleted with the 1.35 upgrade" || b.Suppressed[0].Source != "annotation" {
		t.Errorf("answer = %+v, want ready with the PSP suppressed by its annotation", b)
	}
	for _, f := range b.Findings {
		if f.Key == "removed-api/policy/v1beta1/PodSecurityPolicy" {
			t.Errorf("the suppressed PSP is still a finding: %+v", b.Findings)
		}
	}

	// An annotation without a reason is not applied, and says so.
	noReason := strings.Replace(annotatedPSP, "    upgradescope.dev/ignore-reason: deleted with the 1.35 upgrade\n", "", 1)
	resp, raw = postGate(t, ts, "?target=1.35&fail-on=blocker", "", noReason, "application/x-yaml")
	b = decodeSuppressed(t, raw)
	if resp.StatusCode != http.StatusUnprocessableEntity || b.SuppressedCount != 0 || len(b.Warnings) != 1 ||
		!strings.Contains(b.Warnings[0], "without upgradescope.dev/ignore-reason") {
		t.Errorf("no reason: %d %+v, want 422, nothing suppressed and a warning", resp.StatusCode, b)
	}
}

// #44: ignore rules travel in ?config= as a .upgradescope.yaml and work as
// scan --config: a rule with a reason suppresses, an expired one warns and
// does not, and an invalid config is refused before anything is judged.
func TestGateConfigRules(t *testing.T) {
	ts := httptest.NewServer(newTestServer(t, newFakeStore()).Handler()) // now: 2026-06-10
	defer ts.Close()

	resp, raw := postGate(t, ts, "?target=1.35"+withConfig(pspRule), "", pspManifest, "application/x-yaml")
	b := decodeSuppressed(t, raw)
	if resp.StatusCode != http.StatusOK || b.Verdict != "ready" || b.SuppressedCount != 1 ||
		b.Suppressed[0].Source != "config" || b.Suppressed[0].Reason != "PSPs are removed before the upgrade" || len(b.Warnings) != 0 {
		t.Errorf("rule: %d %+v, want 200 ready with the PSP suppressed by the config", resp.StatusCode, b)
	}

	// Expired (the day after expires): it warns and suppresses nothing.
	expired := pspRule + "    expires: 2026-06-09\n"
	resp, raw = postGate(t, ts, "?target=1.35"+withConfig(expired), "", pspManifest, "application/x-yaml")
	b = decodeSuppressed(t, raw)
	if resp.StatusCode != http.StatusUnprocessableEntity || b.Verdict != "blocked" || b.SuppressedCount != 0 ||
		len(b.Warnings) != 1 || !strings.Contains(b.Warnings[0], "expired on 2026-06-09") {
		t.Errorf("expired rule: %d %+v, want 422 blocked, nothing suppressed, an expiry warning", resp.StatusCode, b)
	}
	// On its expires day it still applies.
	resp, _ = postGate(t, ts, "?target=1.35"+withConfig(pspRule+"    expires: 2026-06-10\n"), "", pspManifest, "application/x-yaml")
	if resp.StatusCode != http.StatusOK {
		t.Errorf("rule expiring today: %d, want 200", resp.StatusCode)
	}

	for name, config := range map[string]string{
		"no reason":     "ignore:\n  - key: removed-api/policy/v1beta1/PodSecurityPolicy\n",
		"unknown field": pspRule + "    expiry: 2026-12-31\n",
		"not yaml":      "ignore: [\n",
	} {
		resp, raw := postGate(t, ts, "?target=1.35"+withConfig(config), "", pspManifest, "application/x-yaml")
		var e struct {
			Error string `json:"error"`
		}
		if err := json.Unmarshal(raw, &e); err != nil || resp.StatusCode != http.StatusUnprocessableEntity || !strings.HasPrefix(e.Error, "invalid config: ") {
			t.Errorf("%s: %d %s, want 422 invalid config", name, resp.StatusCode, raw)
		}
	}
}

// #44: suppressed findings leave the CI formats as they leave scan's:
// SARIF results with an external suppression, JUnit test cases skipped,
// Code Quality entries left out. All of them pass the gate.
func TestGateSuppressedCIFormats(t *testing.T) {
	ts := httptest.NewServer(newTestServer(t, newFakeStore()).Handler())
	defer ts.Close()
	q := "?target=1.35&path=deploy/rendered.yaml" + withConfig(pspRule)

	resp, raw := postGate(t, ts, q+"&format=sarif", "", pspManifest, "application/x-yaml")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("sarif: %d, want 200 (body %s)", resp.StatusCode, raw)
	}
	sariftest.AssertGitHubAcceptable(t, raw)
	var log struct {
		Runs []struct {
			Results []struct {
				RuleID       string `json:"ruleId"`
				Suppressions []struct {
					Kind          string `json:"kind"`
					Justification string `json:"justification"`
				} `json:"suppressions"`
			} `json:"results"`
		} `json:"runs"`
	}
	if err := json.Unmarshal(raw, &log); err != nil || len(log.Runs) != 1 {
		t.Fatalf("SARIF = %v %s", err, raw)
	}
	if r := log.Runs[0].Results; len(r) != 1 || r[0].RuleID != "removed-api/policy/v1beta1/PodSecurityPolicy" || len(r[0].Suppressions) != 1 ||
		r[0].Suppressions[0].Kind != "external" || r[0].Suppressions[0].Justification != "PSPs are removed before the upgrade" {
		t.Errorf("SARIF results = %+v, want the PSP with an external suppression", r)
	}

	resp, raw = postGate(t, ts, q+"&format=junit", "", pspManifest, "application/x-yaml")
	if got := junittest.Outcomes(junittest.Read(t, raw)); resp.StatusCode != http.StatusOK ||
		got["removed-api/removed-api/policy/v1beta1/PodSecurityPolicy (suppressed)"] != "skipped" {
		t.Errorf("junit: %d outcomes %v, want 200 with the PSP skipped", resp.StatusCode, got)
	}

	resp, raw = postGate(t, ts, q+"&format=gitlab-codequality", "", pspManifest, "application/x-yaml")
	if entries := codequalitytest.AssertGitLabAcceptable(t, raw); resp.StatusCode != http.StatusOK || len(entries) != 0 {
		t.Errorf("gitlab-codequality: %d %+v, want 200 and no entry", resp.StatusCode, entries)
	}
}

// TestGateDocsConfigExample runs the suppressions guide's server gate
// example as written (#44): the PSP blocker in rendered.yaml fails it
// without a .upgradescope.yaml accepting it, and passes with one.
func TestGateDocsConfigExample(t *testing.T) {
	if _, err := exec.LookPath("curl"); err != nil {
		t.Skip("curl not installed")
	}
	guide, err := os.ReadFile("../../docs/guides/suppressions-and-baselines.md")
	if err != nil {
		t.Fatal(err)
	}
	command := ""
	for _, block := range strings.Split(string(guide), "```sh\n")[1:] {
		if block, _, _ = strings.Cut(block, "```"); strings.Contains(block, "/api/v1/gate") {
			command = block
		}
	}
	if !strings.Contains(command, "config@.upgradescope.yaml") {
		t.Fatalf("the guide has no sh block that sends .upgradescope.yaml to /api/v1/gate:\n%s", command)
	}
	ts := httptest.NewServer(newTestServer(t, newFakeStore(), func(c *Config) { c.ReadToken = "read-tok" }).Handler())
	defer ts.Close()
	if resp, out := postSnapshot(t, ts, "ingest-tok", pushReqBody(t, testInventory()), false); resp.StatusCode != http.StatusAccepted {
		t.Fatalf("seed push = %d %v", resp.StatusCode, out)
	}
	run := func(config string) ([]byte, error) {
		dir := t.TempDir()
		for name, content := range map[string]string{"rendered.yaml": pspManifest, ".upgradescope.yaml": config} {
			if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		cmd := exec.Command("sh", "-ec", command)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "SERVER="+ts.URL, "READ_TOKEN=read-tok", "NO_PROXY=*", "no_proxy=*")
		return cmd.CombinedOutput()
	}
	if out, err := run("ignore: []\n"); err == nil {
		t.Errorf("without a rule the documented command passed the PSP:\n%s", out)
	}
	out, err := run(pspRule)
	if err != nil {
		t.Fatalf("with the rule the documented command failed: %v\n%s", err, out)
	}
	if b := decodeSuppressed(t, out); b.Verdict != "ready" || b.SuppressedCount != 1 {
		t.Errorf("answer = %+v, want ready with the PSP suppressed", b)
	}
}

// #44: with ?cluster=, rules apply to the proposed state (the cluster's own
// findings too), so a waived cluster finding stops failing clusterVerdict;
// the CI formats, which hold only what the manifests introduce, leave out
// what was suppressed from the cluster.
func TestGateClusterSuppression(t *testing.T) {
	st := newFakeStore()
	ts := httptest.NewServer(newTestServer(t, st).Handler())
	defer ts.Close()
	if resp, out := postSnapshot(t, ts, "ingest-tok", pushReqBody(t, testInventoryWithPSP()), false); resp.StatusCode != http.StatusAccepted {
		t.Fatalf("seed push = %d %v", resp.StatusCode, out)
	}

	q := "?target=1.35&cluster=prod-eu-1" + withConfig(pspRule)
	resp, raw := postGate(t, ts, q, "", deploymentManifest, "application/x-yaml")
	b := decodeSuppressed(t, raw)
	if resp.StatusCode != http.StatusOK || b.Verdict != "ready" || b.ClusterVerdict != "ready" || b.SuppressedCount != 1 {
		t.Errorf("answer = %d %+v, want 200, ready / ready, the cluster's PSP suppressed", resp.StatusCode, b)
	}
	_, raw = postGate(t, ts, q+"&format=sarif", "", deploymentManifest, "application/x-yaml")
	if strings.Contains(string(raw), "PodSecurityPolicy") {
		t.Errorf("SARIF mentions the cluster's suppressed PSP:\n%s", raw)
	}

	// A manifest PSP with the rule: introduced, suppressed, and in SARIF
	// as a suppressed result.
	_, raw = postGate(t, ts, q+"&format=sarif&path=deploy/rendered.yaml", "", pspManifest, "application/x-yaml")
	if !strings.Contains(string(raw), "PSPs are removed before the upgrade") {
		t.Errorf("SARIF lacks the manifest PSP's suppression:\n%s", raw)
	}
}
