package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/abd-ulbasit/upgradescope/internal/sarif/sariftest"
)

// ?path= names the repository file the posted stream was rendered to, so
// gate SARIF places each introduced finding there, at its stream line, and
// code scanning shows it on the PR (#120). Without it a posted stream has
// no file, and findings stay notifications (TestGateSARIF).
func TestGateSARIFPath(t *testing.T) {
	ts := httptest.NewServer(newTestServer(t, newFakeStore()).Handler())
	defer ts.Close()

	stream := deploymentManifest + "---\n" + pspManifest // the PSP's apiVersion is on line 7
	resp, raw := postGate(t, ts, "?target=1.35&format=sarif&path=deploy/rendered.yaml", "", stream, "application/x-yaml")
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422 (body %s)", resp.StatusCode, raw)
	}
	sariftest.AssertGitHubAcceptable(t, raw)
	var log struct {
		Runs []struct {
			Results []struct {
				RuleID    string `json:"ruleId"`
				Locations []struct {
					PhysicalLocation struct {
						ArtifactLocation struct {
							URI string `json:"uri"`
						} `json:"artifactLocation"`
						Region struct {
							StartLine int `json:"startLine"`
						} `json:"region"`
					} `json:"physicalLocation"`
				} `json:"locations"`
			} `json:"results"`
		} `json:"runs"`
	}
	if err := json.Unmarshal(raw, &log); err != nil || len(log.Runs) != 1 {
		t.Fatalf("SARIF = %v %s", err, raw)
	}
	results := log.Runs[0].Results
	if len(results) != 1 || results[0].RuleID != "removed-api/policy/v1beta1/PodSecurityPolicy" || len(results[0].Locations) != 1 {
		t.Fatalf("results = %+v, want the PSP as one located result", results)
	}
	if loc := results[0].Locations[0].PhysicalLocation; loc.ArtifactLocation.URI != "deploy/rendered.yaml" || loc.Region.StartLine != 7 {
		t.Errorf("location = %+v, want deploy/rendered.yaml line 7", loc)
	}

	// The JSON answer names the file too.
	_, raw = postGate(t, ts, "?target=1.35&fail-on=never&path=deploy/rendered.yaml", "", stream, "application/x-yaml")
	b := struct {
		Findings []struct {
			Objects []struct {
				File string `json:"file"`
				Line int    `json:"line"`
			} `json:"objects"`
		} `json:"findings"`
	}{}
	if err := json.Unmarshal(raw, &b); err != nil || len(b.Findings) != 1 || len(b.Findings[0].Objects) != 1 ||
		b.Findings[0].Objects[0].File != "deploy/rendered.yaml" || b.Findings[0].Objects[0].Line != 7 {
		t.Errorf("JSON findings = %+v (%v), want the PSP at deploy/rendered.yaml:7", b.Findings, err)
	}

	// Only a clean, relative path inside the repository names a file.
	for _, bad := range []string{"/etc/rendered.yaml", "../rendered.yaml", "a/../b.yaml", "./a.yaml", "a//b.yaml", `a\b.yaml`, ".", "a\nb", "a\xffb.yaml"} {
		resp, raw := postGate(t, ts, "?target=1.35&path="+url.QueryEscape(bad), "", pspManifest, "application/x-yaml")
		if resp.StatusCode != http.StatusUnprocessableEntity || !json.Valid(raw) || resp.Header.Get("X-Upgradescope-Verdict") != "" {
			t.Errorf("path=%q: status %d (%s), want a 422 JSON error", bad, resp.StatusCode, raw)
		}
	}
}

// Every object ref in the answer carries ?path=, so its length multiplied
// the answer: with the KB's 136 deprecated or removed GVKs at 100 refs
// each, a 60 KB path made a 1.2 MB stream an 817 MB answer and a 2.4 GB
// server. A path is at most maxArtifactPathBytes, or 422 before the body
// is read.
func TestGatePathLengthIsCapped(t *testing.T) {
	ts := httptest.NewServer(newTestServer(t, newFakeStore()).Handler())
	defer ts.Close()
	longest := strings.Repeat("d/", maxArtifactPathBytes/2-3) + "x.yaml"
	if len(longest) != maxArtifactPathBytes {
		t.Fatalf("test path is %d bytes, want %d", len(longest), maxArtifactPathBytes)
	}
	if resp, raw := postGate(t, ts, "?target=1.35&fail-on=never&path="+url.QueryEscape(longest), "", pspManifest, "application/x-yaml"); resp.StatusCode != http.StatusOK {
		t.Fatalf("a %d-byte path: status %d (%s), want 200", len(longest), resp.StatusCode, raw)
	}
	resp, raw := postGate(t, ts, "?target=1.35&path="+url.QueryEscape("d/"+longest), "", pspManifest, "application/x-yaml")
	if resp.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(string(raw), "512 bytes") {
		t.Fatalf("a %d-byte path: status %d (%s), want 422 naming the limit", len(longest)+2, resp.StatusCode, raw)
	}
}
