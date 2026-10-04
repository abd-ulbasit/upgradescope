package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"regexp"
	"slices"
	"testing"

	"github.com/abd-ulbasit/upgradescope/internal/inventory"
)

// gateFormats are the gate's answer formats, as query suffixes.
var gateFormats = []string{"", "&format=sarif", "&format=junit", "&format=gitlab-codequality"}

// clusterUID matches the cluster uid pushScopeCluster gives a cluster,
// the one thing two clusters' gate answers may differ in by name.
var clusterUID = regexp.MustCompile(`uid-[a-z0-9-]+`)

func normUID(raw []byte) []byte { return clusterUID.ReplaceAll(raw, []byte("uid-x")) }

// widgetTeamsInventory is a cluster of payments (pay-prod) and web
// (web-prod) whose Widget CRD serves v1alpha1 and v1; withWeb adds web's
// one Widget at v1alpha1, in web-prod. Payments has no Widget.
func widgetTeamsInventory(withWeb bool) inventory.Inventory {
	inv := testInventory()
	inv.Namespaces = []inventory.NamespaceInfo{{Name: "pay-prod", Team: "payments"}, {Name: "web-prod", Team: "web"}}
	crd := inventory.CRD{
		Group: "example.com", Kind: "Widget", Plural: "widgets",
		Versions:       []inventory.CRDVersion{{Name: "v1alpha1", Served: true}, {Name: "v1", Served: true, Storage: true}},
		StoredVersions: []string{"v1"},
	}
	if withWeb {
		crd.Usage = []inventory.APIUsage{{
			Group: "example.com", Version: "v1alpha1", Kind: "Widget", Count: 1,
			Namespaces: map[string]int{"web-prod": 1}, Objects: []inventory.ObjectRef{{Namespace: "web-prod", Name: "web-widget"}},
		}}
	}
	inv.CRDs = []inventory.CRD{crd}
	return inv
}

// widgetsCRD is a PR's Widget CRD, its v1alpha1 entry with the fields in
// v1alpha1.
func widgetsCRD(v1alpha1 string) string {
	return `apiVersion: apiextensions.k8s.io/v1
kind: CustomResourceDefinition
metadata: {name: widgets.example.com}
spec:
  group: example.com
  scope: Namespaced
  names: {kind: Widget, plural: widgets}
  versions:
    - {name: v1alpha1, storage: false, ` + v1alpha1 + `}
    - {name: v1, served: true, storage: true}
`
}

// gateSummary is what decides a gate answer: its status, verdict header
// and the verdicts, score and findings of its JSON body.
type gateSummary struct {
	Status, Header string
	Verdict        string `json:"verdict"`
	ClusterVerdict string `json:"clusterVerdict"`
	Score          int    `json:"score"`
	Findings       []struct {
		Key, Severity, Source string
	} `json:"findings"`
}

func summarize(t *testing.T, resp *http.Response, raw []byte) gateSummary {
	t.Helper()
	var s gateSummary
	if err := json.Unmarshal(raw, &s); err != nil {
		t.Fatalf("%v: %s", err, raw)
	}
	s.Status, s.Header = resp.Status, resp.Header.Get("X-Upgradescope-Verdict")
	return s
}

// A team-scoped gate with ?cluster= is decided by the scope's share of the
// cluster and the PR alone (#72): its verdict, status, fail-on, cluster
// verdict and score never depend on another team's evidence. A PR that
// stops serving, or deprecates, a CRD version only web's custom resources
// use is ready for payments, with the same status and body in every
// format whether or not web has them; the fleet-wide token, which reads
// the whole cluster, is failed by web's. Where the cluster holds nothing
// but payments' evidence, payments and the fleet-wide token are decided
// alike.
func TestScopedGateIsDecidedByTheScopesShare(t *testing.T) {
	_, st, ts, _ := scopeServer(t)
	mintReadToken(t, st, "pay-tok", "payments")
	pushScopeCluster(t, ts, "crd-web", widgetTeamsInventory(true))
	pushScopeCluster(t, ts, "crd-none", widgetTeamsInventory(false))

	for _, c := range []struct{ name, manifest, failOn string }{
		{"unserved", widgetsCRD("served: false"), "blocker"},
		{"deprecated", widgetsCRD("served: true, deprecated: true"), "warning"},
	} {
		t.Run(c.name, func(t *testing.T) {
			for _, format := range gateFormats {
				q := "?target=1.35&fail-on=" + c.failOn + format + "&cluster="
				payWeb, rawWeb := postGate(t, ts, q+"crd-web", "pay-tok", c.manifest, "application/x-yaml")
				payNone, rawNone := postGate(t, ts, q+"crd-none", "pay-tok", c.manifest, "application/x-yaml")
				if fleet, raw := postGate(t, ts, q+"crd-web", "fleet-tok", c.manifest, "application/x-yaml"); fleet.StatusCode != http.StatusUnprocessableEntity {
					t.Fatalf("fleet-wide gate %s = %d, want 422 from web's Widget: the fixture does not test the scope\n%s", q+"crd-web", fleet.StatusCode, raw)
				}
				if payWeb.StatusCode != http.StatusOK || payNone.StatusCode != http.StatusOK {
					t.Errorf("payments-scoped gate %s = %d / %d, want 200: payments has no Widget\n%s", q, payWeb.StatusCode, payNone.StatusCode, rawWeb)
				}
				if h := payWeb.Header.Get("X-Upgradescope-Verdict"); h != "ready" || h != payNone.Header.Get("X-Upgradescope-Verdict") {
					t.Errorf("payments-scoped gate %s verdict header %q / %q, want ready", q, h, payNone.Header.Get("X-Upgradescope-Verdict"))
				}
				if !bytes.Equal(normUID(rawWeb), normUID(rawNone)) {
					t.Errorf("payments-scoped gate %s differs with web's Widget:\n%s\n%s", q, rawWeb, rawNone)
				}
				for _, s := range []string{"web-widget", "web-prod", `"web"`} {
					if bytes.Contains(rawWeb, []byte(s)) {
						t.Errorf("payments-scoped gate %s names %s:\n%s", q, s, rawWeb)
					}
				}
			}
			q := "?target=1.35&fail-on=" + c.failOn + "&cluster=crd-none"
			payResp, payRaw := postGate(t, ts, q, "pay-tok", c.manifest, "application/x-yaml")
			fleetResp, fleetRaw := postGate(t, ts, q, "fleet-tok", c.manifest, "application/x-yaml")
			pay, fleet := summarize(t, payResp, payRaw), summarize(t, fleetResp, fleetRaw)
			if pay.Status != fleet.Status || pay.Header != fleet.Header || pay.Verdict != fleet.Verdict ||
				pay.ClusterVerdict != fleet.ClusterVerdict || pay.Score != fleet.Score || !slices.Equal(pay.Findings, fleet.Findings) {
				t.Errorf("on a cluster of payments' evidence only, payments' gate %+v differs from the fleet-wide %+v", pay, fleet)
			}
		})
	}
}

// The score probe (#72): a payments PR that adds one CronJob at a removed
// API gets the same answer whether or not web already has a CronJob
// there. Were the gate's score the whole cluster's, the PR would leave it
// unchanged where web's CronJob already costs it, and posting one object
// per API would tell payments at which APIs other teams have findings.
func TestScopedGateScoreIsNoProbe(t *testing.T) {
	_, st, ts, _ := scopeServer(t)
	mintReadToken(t, st, "pay-tok", "payments")
	probe := func(webCron bool) inventory.Inventory {
		inv := testInventory()
		inv.Namespaces = []inventory.NamespaceInfo{{Name: "pay-prod", Team: "payments"}, {Name: "web-prod", Team: "web"}}
		if webCron {
			inv.APIUsage = []inventory.APIUsage{{Group: "batch", Version: "v1beta1", Kind: "CronJob", Count: 1,
				Namespaces: map[string]int{"web-prod": 1}, Objects: []inventory.ObjectRef{{Namespace: "web-prod", Name: "web-cron"}}}}
		}
		return inv
	}
	pushScopeCluster(t, ts, "probe-cron", probe(true))
	pushScopeCluster(t, ts, "probe-none", probe(false))
	const cron = "apiVersion: batch/v1beta1\nkind: CronJob\nmetadata: {name: pr-cron, namespace: pay-prod}\n"
	for _, format := range gateFormats {
		q := "?target=1.35&fail-on=never" + format + "&cluster="
		_, withWeb := postGate(t, ts, q+"probe-cron", "pay-tok", cron, "application/x-yaml")
		_, without := postGate(t, ts, q+"probe-none", "pay-tok", cron, "application/x-yaml")
		if !bytes.Equal(normUID(withWeb), normUID(without)) {
			t.Errorf("payments-scoped gate %s differs with web's CronJob:\n%s\n%s", q, withWeb, without)
		}
		if bytes.Contains(withWeb, []byte("web-cron")) {
			t.Errorf("payments-scoped gate %s names web's CronJob:\n%s", q, withWeb)
		}
	}
	_, a := postGate(t, ts, "?target=1.35&fail-on=never&cluster=probe-cron", "fleet-tok", cron, "application/x-yaml")
	_, b := postGate(t, ts, "?target=1.35&fail-on=never&cluster=probe-none", "fleet-tok", cron, "application/x-yaml")
	if bytes.Equal(normUID(a), normUID(b)) {
		t.Errorf("fleet-wide gate is the same for both clusters: the fixture does not test the probe")
	}
}
