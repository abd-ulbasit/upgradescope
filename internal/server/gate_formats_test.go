package server

import (
	"mime"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/abd-ulbasit/upgradescope/internal/codequality/codequalitytest"
	"github.com/abd-ulbasit/upgradescope/internal/junit/junittest"
)

// TestGateCIFormats: the gate answers format=junit and
// format=gitlab-codequality as it answers sarif (#73): the same status and
// verdict header as JSON for every fail-on, a body the CI system reads,
// with the introduced findings located in ?path=, and responses the
// OpenAPI document describes.
func TestGateCIFormats(t *testing.T) {
	ts := httptest.NewServer(newTestServer(t, newFakeStore()).Handler())
	defer ts.Close()
	v := newSpecValidator(t)

	for _, q := range []string{
		"?target=1.35", "?target=1.35&fail-on=never", "?target=1.34&fail-on=blocker", "?target=1.34&fail-on=warning",
	} {
		for _, body := range []string{pspManifest, deploymentManifest} {
			want, _ := postGate(t, ts, q, "", body, "application/x-yaml")
			for _, format := range []string{"junit", "gitlab-codequality"} {
				resp, raw := postGate(t, ts, q+"&format="+format+"&path=deploy/rendered.yaml", "read-tok", body, "application/x-yaml")
				if resp.StatusCode != want.StatusCode || resp.Header.Get("X-Upgradescope-Verdict") != want.Header.Get("X-Upgradescope-Verdict") {
					t.Errorf("%s format=%s: %d verdict %q, want JSON's %d verdict %q", q, format, resp.StatusCode,
						resp.Header.Get("X-Upgradescope-Verdict"), want.StatusCode, want.Header.Get("X-Upgradescope-Verdict"))
				}
				v.check(q+" "+format, "/api/v1/gate", "post", resp, raw)
				mt, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
				switch format {
				case "junit":
					if mt != "application/xml" {
						t.Errorf("%s junit: Content-Type %q, want application/xml", q, mt)
					}
					junittest.Read(t, raw)
				default:
					if mt != "application/json" {
						t.Errorf("%s gitlab-codequality: Content-Type %q, want application/json", q, mt)
					}
					codequalitytest.AssertGitLabAcceptable(t, raw)
				}
			}
		}
	}

	// A removed API at the target: a JUnit failure, a critical Code
	// Quality entry on the rendered file.
	_, raw := postGate(t, ts, "?target=1.35&format=junit&path=deploy/rendered.yaml", "", pspManifest, "application/x-yaml")
	if got := junittest.Outcomes(junittest.Read(t, raw)); got["removed-api/removed-api/policy/v1beta1/PodSecurityPolicy"] != "failure" {
		t.Errorf("JUnit outcomes = %v, want the PSP blocker failing", got)
	}
	// fail-on=never: the gate passes, and so do the tests.
	_, raw = postGate(t, ts, "?target=1.35&format=junit&fail-on=never", "", pspManifest, "application/x-yaml")
	if doc := junittest.Read(t, raw); *doc.Failures+*doc.Errors != 0 {
		t.Errorf("fail-on=never: JUnit has %d failures, %d errors, want none", *doc.Failures, *doc.Errors)
	}
	_, raw = postGate(t, ts, "?target=1.35&format=gitlab-codequality&path=deploy/rendered.yaml", "", pspManifest, "application/x-yaml")
	entries := codequalitytest.AssertGitLabAcceptable(t, raw)
	if len(entries) != 1 || entries[0].Location.Path != "deploy/rendered.yaml" || entries[0].Location.Lines.Begin != 1 || entries[0].Severity != "critical" {
		t.Errorf("Code Quality = %+v, want the PSP blocker, critical, at deploy/rendered.yaml:1", entries)
	}
	// Without ?path= a posted stream has no file: the entry is on the
	// virtual path.
	_, raw = postGate(t, ts, "?target=1.35&format=gitlab-codequality", "", pspManifest, "application/x-yaml")
	if entries := codequalitytest.AssertGitLabAcceptable(t, raw); len(entries) != 1 || entries[0].Location.Path != "upgradescope/removed-api/policy/v1beta1/PodSecurityPolicy" {
		t.Errorf("Code Quality without ?path= = %+v, want the virtual path", entries)
	}

	resp, raw := postGate(t, ts, "?target=1.35&format=xml", "", pspManifest, "application/x-yaml")
	if resp.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(string(raw), "junit") || !strings.Contains(string(raw), "gitlab-codequality") {
		t.Errorf("format=xml = %d %s, want a 422 naming every format", resp.StatusCode, raw)
	}
}

// With ?cluster=, JUnit and Code Quality carry only what the manifests
// introduce, like SARIF: the cluster's own EOL add-on is not the PR's.
func TestGateCIFormatsClusterBaseline(t *testing.T) {
	st := newFakeStore()
	s := newTestServer(t, st, func(c *Config) { c.KB = istioKB() })
	s.now = func() time.Time { return dec15 }
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()
	if resp, out := postSnapshot(t, ts, "ingest-tok", pushReqBody(t, istioInventory()), false); resp.StatusCode != http.StatusAccepted {
		t.Fatalf("seed push = %d %v", resp.StatusCode, out)
	}

	resp, raw := postGate(t, ts, "?target=1.35&cluster=prod-eu-1&format=junit", "", deploymentManifest, "application/x-yaml")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("clean PR = %d, want 200: %s", resp.StatusCode, raw)
	}
	for k := range junittest.Outcomes(junittest.Read(t, raw)) {
		if strings.Contains(k, "istio") {
			t.Errorf("JUnit of a clean PR names the cluster's %s", k)
		}
	}
	resp, raw = postGate(t, ts, "?target=1.35&cluster=prod-eu-1&format=gitlab-codequality", "", pspManifest, "application/x-yaml")
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("PR adding a removed API = %d, want 422: %s", resp.StatusCode, raw)
	}
	entries := codequalitytest.AssertGitLabAcceptable(t, raw)
	if len(entries) != 1 || entries[0].CheckName != "removed-api/policy/v1beta1/PodSecurityPolicy" {
		t.Errorf("Code Quality = %+v, want only the PSP the PR introduces", entries)
	}
}
