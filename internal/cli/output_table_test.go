package cli

import (
	"bytes"
	"fmt"
	"strings"
	"testing"

	"github.com/abd-ulbasit/upgradescope/internal/engine"
	"github.com/abd-ulbasit/upgradescope/internal/inventory"
)

func TestWriteTableGolden(t *testing.T) {
	r := engine.Report{
		ClusterID: "prod-eu-1",
		Target:    inventory.Version{Major: 1, Minor: 36},
		KBVersion: "k8s-1.36+registry-2026-06-10",
		Score:     70,
		Ready:     false,
		Findings: []engine.Finding{
			{
				Category: engine.CatEOLAddon, Severity: engine.SevBlocker,
				Title:  "ingress-nginx is past end of life",
				Detail: "EOL 2026-03-24; detected v1.9.4 in namespace ingress-nginx",
				Teams:  []string{"platform"},
			},
			{
				Category: engine.CatVersionSkew, Severity: engine.SevWarning,
				Title: "kubelet 3 minors behind apiserver",
			},
			{
				Category: engine.CatDeprecatedAPI, Severity: engine.SevInfo,
				Title: "batch/v1beta1 CronJob is deprecated",
			},
		},
		NotAssessed: []engine.CapabilityGap{
			{Capability: inventory.CapDeprecatedCalls, Reason: "GET /metrics forbidden"},
		},
	}

	var buf bytes.Buffer
	WriteTable(&buf, r)

	want := `upgradescope upgrade readiness report

Cluster:  prod-eu-1
Target:   1.36
KB:       k8s-1.36+registry-2026-06-10

SCORE  70/100
READY  no

BLOCKER (1)
  [eol-addon] ingress-nginx is past end of life
      EOL 2026-03-24; detected v1.9.4 in namespace ingress-nginx
      teams: platform

WARNING (1)
  [version-skew] kubelet 3 minors behind apiserver

INFO (1)
  [deprecated-api] batch/v1beta1 CronJob is deprecated

TEAMS
  platform       75/100  blocked  blockers 1  warnings 0
  unattributed   95/100  ready    blockers 0  warnings 1

NOT ASSESSED
  deprecated-calls: GET /metrics forbidden
`
	if got := buf.String(); got != want {
		t.Errorf("table output mismatch\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

// Files mode: each finding names the affected objects (file:line, first
// few, then a count), the fix and the citations; an Ingress and a CronJob
// without metadata.namespace read "namespace unset", never cluster-scoped.
func TestWriteTableFilesModeGolden(t *testing.T) {
	guide := "https://kubernetes.io/docs/reference/using-api/deprecation-guide/"
	var many []inventory.ObjectRef
	for i := range 6 {
		many = append(many, inventory.ObjectRef{Name: fmt.Sprintf("web-%d", i), File: "rendered/ingress.yaml", Line: 1 + 7*i})
	}
	many[0].Namespace = "shop"
	r := engine.Report{
		ClusterID: "files",
		Target:    inventory.Version{Major: 1, Minor: 36},
		KBVersion: "test-kb",
		Score:     50,
		Findings: []engine.Finding{
			{
				Category: engine.CatRemovedAPI, Severity: engine.SevBlocker,
				Title:       "batch/v1beta1 CronJob removed in 1.25 (1 object)",
				Detail:      "1 manifest object(s) use this API: namespace unset (1).",
				Remediation: "migrate to batch/v1 CronJob",
				Citations:   []string{guide},
				Objects:     []inventory.ObjectRef{{Name: "nightly", File: "rendered/all.yaml", Line: 3, RenderedFrom: "demo/templates/cronjob.yaml"}},
			},
			{
				Category: engine.CatRemovedAPI, Severity: engine.SevBlocker,
				Title:          "networking.k8s.io/v1beta1 Ingress removed in 1.22 (8 objects)",
				Detail:         "8 manifest object(s) use this API: namespace unset (7), shop (1).",
				Remediation:    "migrate to networking.k8s.io/v1 Ingress",
				Citations:      []string{guide, "https://example.com/ingress-migration"},
				Objects:        many,
				ObjectsOmitted: 2,
			},
		},
		NotAssessed: []engine.CapabilityGap{{Capability: inventory.CapVersions, Reason: "files mode"}},
	}

	var buf bytes.Buffer
	WriteTable(&buf, r)

	want := `upgradescope upgrade readiness report

Cluster:  files
Target:   1.36
KB:       test-kb

SCORE  50/100
READY  no

BLOCKER (2)
  [removed-api] batch/v1beta1 CronJob removed in 1.25 (1 object)
      1 manifest object(s) use this API: namespace unset (1).
      - nightly  rendered/all.yaml:3 (rendered from demo/templates/cronjob.yaml)
      fix: migrate to batch/v1 CronJob
      see: https://kubernetes.io/docs/reference/using-api/deprecation-guide/
  [removed-api] networking.k8s.io/v1beta1 Ingress removed in 1.22 (8 objects)
      8 manifest object(s) use this API: namespace unset (7), shop (1).
      - shop/web-0  rendered/ingress.yaml:1
      - web-1  rendered/ingress.yaml:8
      - web-2  rendered/ingress.yaml:15
      - web-3  rendered/ingress.yaml:22
      - web-4  rendered/ingress.yaml:29
      …and 3 more
      fix: migrate to networking.k8s.io/v1 Ingress
      see: https://kubernetes.io/docs/reference/using-api/deprecation-guide/
      see: https://example.com/ingress-migration

NOT ASSESSED
  versions: files mode
`
	if got := buf.String(); got != want {
		t.Errorf("table output mismatch\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
	if strings.Contains(buf.String(), "cluster-scoped") || strings.Contains(buf.String(), "stored/served") {
		t.Error("files-mode table must not use cluster wording")
	}
}

// No named team anywhere → no TEAMS section, even with findings present.
func TestWriteTableNoTeamsSectionWithoutNamedTeams(t *testing.T) {
	r := engine.Report{
		ClusterID: "c",
		Target:    inventory.Version{Major: 1, Minor: 36},
		Findings: []engine.Finding{
			{Category: engine.CatVersionSkew, Severity: engine.SevWarning, Title: "w"},
		},
	}
	var buf bytes.Buffer
	WriteTable(&buf, r)
	if bytes.Contains(buf.Bytes(), []byte("TEAMS")) {
		t.Fatalf("TEAMS section rendered without any named team:\n%s", buf.String())
	}
}

func TestWriteTableNoFindings(t *testing.T) {
	r := engine.Report{
		ClusterID: "files",
		Target:    inventory.Version{Major: 1, Minor: 36},
		KBVersion: "test-kb",
		Score:     100,
		Ready:     true,
	}
	var buf bytes.Buffer
	WriteTable(&buf, r)
	want := `upgradescope upgrade readiness report

Cluster:  files
Target:   1.36
KB:       test-kb

SCORE  100/100
READY  yes

No findings.
`
	if got := buf.String(); got != want {
		t.Errorf("table output mismatch\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

// Issue #122: a partial capability is a gap like any other, marked
// partial, with what it skipped; the required one is named under the
// unknown verdict.
func TestWriteTablePartialGaps(t *testing.T) {
	r := engine.Report{
		ClusterID: "c",
		Target:    inventory.Version{Major: 1, Minor: 25},
		KBVersion: "test-kb",
		Score:     100,
		Verdict:   engine.VerdictUnknown,
		NotAssessed: []engine.CapabilityGap{
			{Capability: inventory.CapAPIUsage, Reason: "list policy/v1beta1 podsecuritypolicies: forbidden", Partial: true, Required: true,
				Skipped: []string{"policy/v1beta1 PodSecurityPolicy"}},
			{Capability: inventory.CapHelm, Reason: "helm releases: 1 via secrets; 1 release(s) not decodable, first a/b: gunzip", Partial: true,
				Skipped: []string{"a/b"}},
		},
	}
	var buf bytes.Buffer
	WriteTable(&buf, r)
	want := `upgradescope upgrade readiness report

Cluster:  c
Target:   1.25
KB:       test-kb

SCORE  100/100
READY  unknown (required checks were not assessed)
  api-usage (partial, required): list policy/v1beta1 podsecuritypolicies: forbidden

No findings.

NOT ASSESSED
  api-usage (partial, required): list policy/v1beta1 podsecuritypolicies: forbidden
      skipped: policy/v1beta1 PodSecurityPolicy
  helm (partial): helm releases: 1 via secrets; 1 release(s) not decodable, first a/b: gunzip
      skipped: a/b
`
	if got := buf.String(); got != want {
		t.Errorf("table output mismatch\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

// A ready verdict with gaps says so next to the verdict, not only at the
// end of the report.
func TestWriteTableReadyNamesGaps(t *testing.T) {
	r := engine.Report{
		ClusterID: "c",
		Target:    inventory.Version{Major: 1, Minor: 25},
		Score:     100,
		Ready:     true,
		Verdict:   engine.VerdictReady,
		NotAssessed: []engine.CapabilityGap{
			{Capability: inventory.CapDeprecatedCalls, Reason: "upgradescope lists policy/v1beta1 podsecuritypolicies itself", Partial: true},
			{Capability: inventory.CapHelm, Reason: "secrets list forbidden"},
		},
	}
	var buf bytes.Buffer
	WriteTable(&buf, r)
	const want = "READY  yes\n       not fully assessed: deprecated-calls (partial), helm (see NOT ASSESSED)\n"
	if !strings.Contains(buf.String(), want) {
		t.Errorf("table lacks %q:\n%s", want, buf.String())
	}
}

// #94: a live scan's header says which cluster it read, by kubeconfig
// context and API server; a files scan has neither.
func TestWriteTableNamesTheCluster(t *testing.T) {
	r := engine.Report{ClusterID: "uid-1", ServerVersion: "v1.34.2", Target: inventory.Version{Major: 1, Minor: 35}, KBVersion: "kb",
		KubeContext: "prod-eu", APIServer: "https://10.0.0.1:6443"}
	var buf bytes.Buffer
	if err := WriteTable(&buf, r); err != nil {
		t.Fatal(err)
	}
	const want = "Cluster:  uid-1\nContext:  prod-eu (API server https://10.0.0.1:6443)\nServer:   v1.34.2\n"
	if !strings.Contains(buf.String(), want) {
		t.Errorf("table lacks %q:\n%s", want, buf.String())
	}

	r.KubeContext, r.APIServer = "", ""
	buf.Reset()
	if err := WriteTable(&buf, r); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(buf.String(), "Context:") {
		t.Errorf("no context or API server: table has a Context line:\n%s", buf.String())
	}
}
