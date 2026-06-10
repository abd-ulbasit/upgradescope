package cli

import (
	"bytes"
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

NOT ASSESSED
  deprecated-calls: GET /metrics forbidden
`
	if got := buf.String(); got != want {
		t.Errorf("table output mismatch\n--- got ---\n%s\n--- want ---\n%s", got, want)
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
