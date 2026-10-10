package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/abd-ulbasit/upgradescope/internal/kb"
)

const deviceClassManifest = `apiVersion: resource.k8s.io/v1
kind: DeviceClass
metadata:
  name: gpu
spec: {}
`

// The gate judges a manifest at an API version the target does not serve
// yet like one at a removed version: a blocker, 422, saying "not served
// until 1.34" (#266). A target that serves it is clean. Stored objects of a
// cluster are not judged so; only what the manifests propose is.
func TestGateManifestAtAPINotServedYetIsABlocker(t *testing.T) {
	k, err := kb.Load()
	if err != nil {
		t.Fatal(err)
	}
	s := newTestServer(t, newFakeStore(), func(c *Config) { c.KB = k })
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	resp, raw := postGate(t, ts, "?target=1.33", "", deviceClassManifest, "application/x-yaml")
	if resp.StatusCode != http.StatusUnprocessableEntity || resp.Header.Get("X-Upgradescope-Verdict") != "blocked" {
		t.Fatalf("1.33: status %d verdict %q (%s), want 422 blocked", resp.StatusCode, resp.Header.Get("X-Upgradescope-Verdict"), raw)
	}
	var body struct {
		Findings []struct {
			Category, Severity, Title, Remediation, Source string
		}
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Findings) != 1 || body.Findings[0].Severity != "blocker" || body.Findings[0].Category != "removed-api" ||
		!strings.Contains(body.Findings[0].Title, "is not served until 1.34") || body.Findings[0].Source != "manifest" ||
		!strings.Contains(body.Findings[0].Remediation, "resource.k8s.io/v1beta2 DeviceClass") {
		t.Fatalf("findings = %+v, want one manifest blocker \"not served until 1.34\" naming v1beta2", body.Findings)
	}

	for _, target := range []string{"1.34", "1.37"} {
		if resp, raw := postGate(t, ts, "?target="+target, "", deviceClassManifest, "application/x-yaml"); resp.StatusCode != http.StatusOK || resp.Header.Get("X-Upgradescope-Verdict") != "ready" {
			t.Errorf("%s: status %d verdict %q (%s), want 200 ready", target, resp.StatusCode, resp.Header.Get("X-Upgradescope-Verdict"), raw)
		}
	}
}
