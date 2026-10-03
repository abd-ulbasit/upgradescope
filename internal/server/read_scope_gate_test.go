package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/abd-ulbasit/upgradescope/internal/inventory"
)

// pushScopeCluster pushes inv as the cluster called name.
func pushScopeCluster(t *testing.T, ts *httptest.Server, name string, inv inventory.Inventory) {
	t.Helper()
	inv.ClusterID = "uid-" + name
	body, err := json.Marshal(map[string]any{"schemaVersion": 1, "clusterName": name, "agentVersion": "v0.2.0-test", "inventory": inv})
	if err != nil {
		t.Fatal(err)
	}
	if resp, out := postSnapshot(t, ts, "ingest-tok", body, false); resp.StatusCode != http.StatusAccepted {
		t.Fatalf("push %s = %d %v", name, resp.StatusCode, out)
	}
}

// leakyInventory is a cluster with payments' Ingress in pay-prod and, in
// two variants that differ only in what is not payments', other teams'
// and no team's evidence at every API a PR can join: Ingresses in web's
// namespaces and in ops (no team), CronJobs in web-prod only, cluster-
// scoped PodSecurityPolicies, and (variant a only) an apiserver caller
// row the engine folds into the Ingress finding. Each variant has the
// same findings, so the same score and verdicts: a payments-scoped gate
// answer that differs between them tells payments something of web's.
func leakyInventory(variant string) inventory.Inventory {
	inv := testInventory()
	webTeam, webOther := "web", "web-alpha"
	if variant == "b" {
		webTeam, webOther = "webteam", "web-beta"
	}
	inv.Namespaces = []inventory.NamespaceInfo{
		{Name: "pay-prod", Team: "payments"}, {Name: "web-prod", Team: webTeam}, {Name: webOther, Team: webTeam}, {Name: "ops"},
	}
	n := 1
	if variant == "b" {
		n = 3
	}
	objs := func(ns, prefix string, count int) []inventory.ObjectRef {
		var out []inventory.ObjectRef
		for i := range count {
			out = append(out, inventory.ObjectRef{Namespace: ns, Name: fmt.Sprintf("%s-%s%d", prefix, variant, i)})
		}
		return out
	}
	ingress := inventory.APIUsage{
		Group: "extensions", Version: "v1beta1", Kind: "Ingress", Count: 1 + 2*n + n + n,
		Namespaces: map[string]int{"pay-prod": 1, "web-prod": 2 * n, webOther: n, "ops": n},
		Objects: append(append(append([]inventory.ObjectRef{{Namespace: "pay-prod", Name: "pay-ingress"}},
			objs("web-prod", "web-leak", 2*n)...), objs(webOther, "web-leak-other", n)...), objs("ops", "ops-leak", n)...),
	}
	cron := inventory.APIUsage{
		Group: "batch", Version: "v1beta1", Kind: "CronJob", Count: n,
		Namespaces: map[string]int{"web-prod": n}, Objects: objs("web-prod", "cron-leak", n),
	}
	psp := inventory.APIUsage{
		Group: "policy", Version: "v1beta1", Kind: "PodSecurityPolicy", Count: 2 * n,
		Namespaces: map[string]int{"": 2 * n}, Objects: objs("", "psp-leak", 2*n),
	}
	inv.APIUsage = []inventory.APIUsage{ingress, cron, psp}
	if variant == "a" {
		inv.DeprecatedCalls = []inventory.DeprecatedCall{{Group: "extensions", Version: "v1beta1", Resource: "ingresses", RemovedRelease: "1.22"}}
	}
	return inv
}

// leakyManifest is a PR that joins every API the leaky cluster has
// evidence at: an Ingress in payments' namespace, in web's, in the one no
// team owns and in a new one, a cluster-scoped PodSecurityPolicy, and a
// CronJob in payments' namespace. What it names is the caller's own.
const leakyManifest = `apiVersion: extensions/v1beta1
kind: Ingress
metadata: {name: pr-pay, namespace: pay-prod}
---
apiVersion: extensions/v1beta1
kind: Ingress
metadata: {name: pr-web, namespace: web-prod}
---
apiVersion: extensions/v1beta1
kind: Ingress
metadata: {name: pr-ops, namespace: ops}
---
apiVersion: extensions/v1beta1
kind: Ingress
metadata: {name: pr-new, namespace: pr-new-ns}
---
apiVersion: policy/v1beta1
kind: PodSecurityPolicy
metadata: {name: pr-psp}
---
apiVersion: batch/v1beta1
kind: CronJob
metadata: {name: pr-cron, namespace: pay-prod}
`

// What only the leaky cluster's other teams and unowned namespaces say:
// none of it may reach a payments-scoped answer. (web-prod and ops are
// not here: the PR names them itself.)
var leakySecrets = []string{
	"web-leak", "ops-leak", "cron-leak", "psp-leak", "web-alpha", "web-beta", `"web"`, `"webteam"`,
	"also records requests to", // the folded apiserver caller row
}

// A payments-scoped gate with ?cluster= answers from payments' share of
// the cluster and the PR alone (#72). The cluster's evidence in another
// team's namespace, in one no team owns, cluster-scoped or from its
// apiserver metrics never reaches the answer, not even through a finding
// the PR's own objects are in: two clusters that differ only there give
// payments the same answer, byte for byte, in every format, while the
// fleet-wide token sees them differ.
func TestScopedGateAnswersFromTheScopesShareOnly(t *testing.T) {
	_, st, ts, _ := scopeServer(t)
	mintReadToken(t, st, "pay-tok", "payments")
	pushScopeCluster(t, ts, "leaky-a", leakyInventory("a"))
	pushScopeCluster(t, ts, "leaky-b", leakyInventory("b"))
	norm := func(raw []byte) []byte {
		raw = bytes.ReplaceAll(raw, []byte("uid-leaky-a"), []byte("uid-leaky-x"))
		return bytes.ReplaceAll(raw, []byte("uid-leaky-b"), []byte("uid-leaky-x"))
	}
	for _, format := range []string{"", "&format=sarif", "&format=junit", "&format=gitlab-codequality"} {
		q := "?target=1.35&fail-on=never" + format + "&cluster="
		respA, a := postGate(t, ts, q+"leaky-a", "pay-tok", leakyManifest, "application/x-yaml")
		respB, b := postGate(t, ts, q+"leaky-b", "pay-tok", leakyManifest, "application/x-yaml")
		if respA.StatusCode != http.StatusOK || respB.StatusCode != http.StatusOK {
			t.Fatalf("gate %s = %d %s / %d %s", q, respA.StatusCode, a, respB.StatusCode, b)
		}
		if !bytes.Equal(norm(a), norm(b)) {
			t.Errorf("payments-scoped gate %s differs between clusters that differ only in what is not payments':\n%s\n%s", q, a, b)
		}
		for _, raw := range [][]byte{a, b} {
			for _, s := range leakySecrets {
				if bytes.Contains(raw, []byte(s)) {
					t.Errorf("payments-scoped gate %s names %s:\n%s", q, s, raw)
				}
			}
		}
		for _, own := range []string{"pr-pay", "pr-web", "pr-ops", "pr-new", "pr-psp", "pr-cron"} {
			if format == "" && !bytes.Contains(a, []byte(own)) {
				t.Errorf("payments-scoped gate %s lost the PR's own %s:\n%s", q, own, a)
			}
		}
		if format == "" && !bytes.Contains(a, []byte("pay-ingress")) {
			t.Errorf("payments-scoped gate %s lost payments' own cluster object:\n%s", q, a)
		}
		_, fa := postGate(t, ts, q+"leaky-a", "fleet-tok", leakyManifest, "application/x-yaml")
		_, fb := postGate(t, ts, q+"leaky-b", "fleet-tok", leakyManifest, "application/x-yaml")
		if bytes.Equal(norm(fa), norm(fb)) {
			t.Errorf("fleet-wide gate %s is the same for both clusters: the fixture does not test the scope", q)
		}
	}

	// What payments reads of each finding: counts and evidence of its own
	// and the PR's only.
	_, raw := postGate(t, ts, "?target=1.35&fail-on=never&cluster=leaky-a", "pay-tok", leakyManifest, "application/x-yaml")
	want := map[string]struct {
		source, title string
		namespaces    []string
		objects       int
	}{
		"removed-api/extensions/v1beta1/Ingress":       {sourceManifest, "(5 objects)", []string{"ops", "pay-prod", "pr-new-ns", "web-prod"}, 5},
		"removed-api/batch/v1beta1/CronJob":            {sourceManifest, "(1 object)", []string{"pay-prod"}, 1},
		"removed-api/policy/v1beta1/PodSecurityPolicy": {sourceManifest, "(1 object)", nil, 1},
	}
	var body struct {
		Findings []struct {
			Key, Source, Title, Detail string
			Teams                      []string
			Namespaces                 []string
			Objects                    []inventory.ObjectRef
			ObjectsOmitted             int
		}
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Findings) != len(want) {
		t.Errorf("payments-scoped gate has %d findings, want %d: %s", len(body.Findings), len(want), raw)
	}
	for _, f := range body.Findings {
		w, ok := want[f.Key]
		if !ok {
			t.Errorf("unexpected finding %s in %s", f.Key, raw)
			continue
		}
		if f.Source != w.source || !strings.HasSuffix(f.Title, w.title) || fmt.Sprint(f.Namespaces) != fmt.Sprint(w.namespaces) ||
			len(f.Objects) != w.objects || f.ObjectsOmitted != 0 {
			t.Errorf("%s: source %s, title %q, namespaces %v, %d objects (+%d), want %s, %q, %v, %d",
				f.Key, f.Source, f.Title, f.Namespaces, len(f.Objects), f.ObjectsOmitted, w.source, w.title, w.namespaces, w.objects)
		}
		if strings.Contains(f.Title, "in scope") || strings.Contains(f.Detail, "Cut to this read's teams") {
			t.Errorf("%s was cut after the fact (%q, %q): it should be evaluated from the scope's share", f.Key, f.Title, f.Detail)
		}
		for _, team := range f.Teams {
			if team != "payments" {
				t.Errorf("%s names team %q", f.Key, team)
			}
		}
	}
}

// The reported leak (#72): a payments-scoped gate posting an Ingress
// into web's namespace of the mixed cluster read web's Ingress there
// through the PR's finding: team web, a count of 2, "web-prod (2)".
func TestScopedGateDoesNotMergeAnotherTeamsUsage(t *testing.T) {
	_, st, ts, _ := scopeServer(t)
	mintReadToken(t, st, "pay-tok", "payments")
	const webManifest = `apiVersion: extensions/v1beta1
kind: Ingress
metadata:
  name: pr-ingress
  namespace: web-prod
`
	resp, raw := postGate(t, ts, "?target=1.35&fail-on=never&cluster=mixed", "pay-tok", webManifest, "application/x-yaml")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d %s", resp.StatusCode, raw)
	}
	for _, s := range []string{`"web"`, "(2 objects)", "web-prod (2)"} {
		if bytes.Contains(raw, []byte(s)) {
			t.Errorf("payments-scoped gate names %s of web's:\n%s", s, raw)
		}
	}
	if !bytes.Contains(raw, []byte("pr-ingress")) || !bytes.Contains(raw, []byte("web-prod (1)")) {
		t.Errorf("payments-scoped gate lost the PR's own Ingress:\n%s", raw)
	}
	_, fleet := postGate(t, ts, "?target=1.35&fail-on=never&cluster=mixed", "fleet-tok", webManifest, "application/x-yaml")
	if !bytes.Contains(fleet, []byte("web-prod (2)")) {
		t.Errorf("fleet-wide gate does not merge the cluster's Ingress: the fixture does not test the leak:\n%s", fleet)
	}
}

// clusterShare keeps of every kind of namespaced evidence the scope's
// namespaces' only, counts what it keeps exactly, drops what no team owns
// and what is cluster wide of any client, and leaves its input alone.
func TestClusterShare(t *testing.T) {
	inv := leakyInventory("a")
	inv.APIUsage = append(inv.APIUsage, inventory.APIUsage{Group: "x", Version: "v1", Kind: "Undivided", Count: 4})
	inv.CRDs = []inventory.CRD{{Group: "example.com", Kind: "Widget", Usage: []inventory.APIUsage{{
		Group: "example.com", Version: "v1alpha1", Kind: "Widget", Count: 3,
		Namespaces: map[string]int{"pay-prod": 2, "web-prod": 1},
		Objects:    []inventory.ObjectRef{{Namespace: "pay-prod", Name: "w1"}, {Namespace: "web-prod", Name: "w2"}},
	}}}}
	inv.HelmReleases = []inventory.HelmRelease{{Name: "pay", Namespace: "pay-prod"}, {Name: "web", Namespace: "web-prod"}}
	inv.GitOpsCharts = []inventory.GitOpsChart{
		{Name: "pay", Namespace: "pay-prod", Target: "pay-prod"}, {Name: "into-web", Namespace: "pay-prod", Target: "web-prod"},
		{Name: "web", Namespace: "web-prod"},
	}
	inv.AddOns = []inventory.AddOnInstance{
		{ID: "ingress-nginx", Namespaces: []string{"pay-prod", "web-prod"}}, {ID: "cert-manager", Namespaces: []string{"web-prod"}},
		{ID: "ingressclass-only"},
	}
	inv.UnrecognizedImages = []string{"registry.example.com/web/app"}
	before, _ := json.Marshal(inv)

	if got, _ := json.Marshal(fleetScope.clusterShare(inv)); !bytes.Equal(got, before) {
		t.Errorf("the fleet-wide share is not the cluster")
	}
	share := scopeOfTeams([]string{"payments"}).clusterShare(inv)
	if after, _ := json.Marshal(inv); !bytes.Equal(after, before) {
		t.Errorf("clusterShare modified its input")
	}
	got, _ := json.Marshal(share)
	for _, s := range []string{"web-prod", "web-alpha", `"ops"`, "psp-leak", "Undivided", `"w2"`, "into-web", "cert-manager", "ingressclass-only", "registry.example.com", "ingresses"} {
		if bytes.Contains(got, []byte(s)) {
			t.Errorf("payments' share names %s: %s", s, got)
		}
	}
	if len(share.APIUsage) != 1 || share.APIUsage[0].Count != 1 || len(share.APIUsage[0].Objects) != 1 || share.APIUsage[0].ObjectsOmitted != 0 {
		t.Errorf("payments' API usage = %+v, want its one Ingress", share.APIUsage)
	}
	if u := share.CRDs[0].Usage; len(u) != 1 || u[0].Count != 2 || len(u[0].Objects) != 1 || u[0].ObjectsOmitted != 1 {
		t.Errorf("payments' Widget usage = %+v, want 2 counted, 1 listed, 1 omitted", u)
	}
	if len(share.HelmReleases) != 1 || len(share.GitOpsCharts) != 1 || len(share.AddOns) != 1 || len(share.AddOns[0].Namespaces) != 1 {
		t.Errorf("payments' releases %v, charts %v, add-ons %v", share.HelmReleases, share.GitOpsCharts, share.AddOns)
	}
	if share.ServerVersion != inv.ServerVersion || len(share.Namespaces) != 1 {
		t.Errorf("payments' share: version %q, namespaces %v", share.ServerVersion, share.Namespaces)
	}
}
