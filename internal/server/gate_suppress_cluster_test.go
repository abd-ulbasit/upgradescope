package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/abd-ulbasit/upgradescope/internal/codequality/codequalitytest"
	"github.com/abd-ulbasit/upgradescope/internal/inventory"
	"github.com/abd-ulbasit/upgradescope/internal/sarif/sariftest"
)

const pspKey = "removed-api/policy/v1beta1/PodSecurityPolicy"

// sarifResults decodes a SARIF answer's results: rule and whether each is
// suppressed.
func sarifResults(t *testing.T, raw []byte) map[string][]bool {
	t.Helper()
	sariftest.AssertGitHubAcceptable(t, raw)
	var log struct {
		Runs []struct {
			Results []struct {
				RuleID       string            `json:"ruleId"`
				Suppressions []json.RawMessage `json:"suppressions"`
			} `json:"results"`
		} `json:"runs"`
	}
	if err := json.Unmarshal(raw, &log); err != nil || len(log.Runs) != 1 {
		t.Fatalf("SARIF = %v %s", err, raw)
	}
	out := map[string][]bool{}
	for _, r := range log.Runs[0].Results {
		out[r.RuleID] = append(out[r.RuleID], len(r.Suppressions) > 0)
	}
	return out
}

// assertClusterRemainder checks a gate answer in which the manifests' only
// object of key is suppressed while the cluster still has objects there:
// the PR passes, the remaining finding is the cluster's, and the CI
// formats carry the manifests' suppressed object only, never the
// cluster's remainder as a PR result.
func assertClusterRemainder(t *testing.T, ts *httptest.Server, q, manifest, key string) {
	t.Helper()
	resp, raw := postGate(t, ts, q, "", manifest, "application/x-yaml")
	b := decodeSuppressed(t, raw)
	src := ""
	for _, f := range b.Findings {
		if f.Key == key {
			src = f.Source
		}
	}
	if resp.StatusCode != http.StatusOK || resp.Header.Get("X-Upgradescope-Verdict") != "ready" || b.Verdict != "ready" ||
		b.SuppressedCount != 1 || src != sourceCluster {
		t.Errorf("json: %d verdict %q suppressed %d, %s from %q; want 200 ready, 1 suppressed, the remainder the cluster's\n%s",
			resp.StatusCode, b.Verdict, b.SuppressedCount, key, src, raw)
	}

	resp, raw = postGate(t, ts, q+"&format=sarif", "", manifest, "application/x-yaml")
	if got := sarifResults(t, raw)[key]; resp.StatusCode != http.StatusOK || len(got) != 1 || !got[0] {
		t.Errorf("sarif: %d %s results (suppressed?) %v, want 200 and only the manifests' suppressed object\n%s", resp.StatusCode, key, got, raw)
	}
	resp, raw = postGate(t, ts, q+"&format=gitlab-codequality", "", manifest, "application/x-yaml")
	if entries := codequalitytest.AssertGitLabAcceptable(t, raw); resp.StatusCode != http.StatusOK || len(entries) != 0 {
		t.Errorf("gitlab-codequality: %d %+v, want 200 and no entry", resp.StatusCode, entries)
	}
}

// #44 with ?cluster=: what the manifests introduce is decided after
// suppression. A posted PSP accepted by its annotation, or by a rule
// scoped to its name, leaves the cluster's live PSP as the finding's only
// object; that remainder is the cluster's, so the PR passes.
func TestGateClusterSuppressedManifestObject(t *testing.T) {
	ts := httptest.NewServer(newTestServer(t, newFakeStore()).Handler())
	defer ts.Close()
	inv := testInventory()
	inv.APIUsage = []inventory.APIUsage{{
		Group: "policy", Version: "v1beta1", Kind: "PodSecurityPolicy", Count: 1,
		Namespaces: map[string]int{"": 1}, Objects: []inventory.ObjectRef{{Name: "legacy"}},
	}}
	if resp, out := postSnapshot(t, ts, "ingest-tok", pushReqBody(t, inv), false); resp.StatusCode != http.StatusAccepted {
		t.Fatalf("seed push = %d %v", resp.StatusCode, out)
	}
	q := "?target=1.35&cluster=prod-eu-1&path=deploy/rendered.yaml"

	// Unsuppressed, the posted PSP is the PR's blocker.
	if resp, raw := postGate(t, ts, q, "", pspManifest, "application/x-yaml"); resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("unsuppressed PSP: %d, want 422\n%s", resp.StatusCode, raw)
	}
	t.Run("annotation", func(t *testing.T) {
		assertClusterRemainder(t, ts, q, annotatedPSP, pspKey)
	})
	t.Run("name-scoped rule", func(t *testing.T) {
		rule := "ignore:\n  - key: " + pspKey + "\n    name: restricted\n    reason: replaced in the 1.35 upgrade\n"
		assertClusterRemainder(t, ts, q+withConfig(rule), pspManifest, pspKey)
	})
	t.Run("whole-finding rule", func(t *testing.T) {
		// A rule without selectors takes the cluster's objects too: nothing
		// remains, and SARIF carries the manifests' object as suppressed.
		resp, raw := postGate(t, ts, q+withConfig(pspRule)+"&format=sarif", "", pspManifest, "application/x-yaml")
		if got := sarifResults(t, raw)[pspKey]; resp.StatusCode != http.StatusOK || len(got) != 1 || !got[0] {
			t.Errorf("sarif: %d %s results (suppressed?) %v, want 200 and the manifests' suppressed object\n%s", resp.StatusCode, pspKey, got, raw)
		}
	})
}

// #44 and #150 with ?cluster=: a posted custom resource at a version the
// cluster's CRD does not serve, accepted by its annotation, does not make
// the cluster's live custom resources at that version the PR's.
func TestGateClusterSuppressedCustomResource(t *testing.T) {
	ts := widgetCluster(t, func(c *inventory.CRD) {
		c.Usage = []inventory.APIUsage{{Group: "example.com", Version: "v1alpha1", Kind: "Widget", Count: 1,
			Namespaces: map[string]int{"shop": 1}, Objects: []inventory.ObjectRef{{Namespace: "shop", Name: "legacy"}}}}
	})
	annotated := strings.Replace(widget("v1alpha1", "new"), "metadata: {name: new, namespace: shop}",
		"metadata:\n  name: new\n  namespace: shop\n  annotations:\n    upgradescope.basit.engineer/ignore: crd-version\n    upgradescope.basit.engineer/ignore-reason: converted before the upgrade", 1)
	if !strings.Contains(annotated, "ignore-reason") {
		t.Fatalf("annotation not added:\n%s", annotated)
	}
	assertClusterRemainder(t, ts, "?target=1.35&cluster=prod-eu-1&path=deploy/rendered.yaml", annotated, unservedWidget)
}
