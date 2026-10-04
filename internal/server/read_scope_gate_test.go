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

// leakyInventory is a cluster with payments' Ingress in pay-prod and a
// Widget CRD that serves v1alpha1 and v1. Its variants differ only in
// what is not payments': other teams' and no team's evidence at every API
// a PR can join, in a and b: Ingresses in web's namespaces and in ops (no
// team), CronJobs in web-prod only, cluster-scoped PodSecurityPolicies,
// and (a only) an apiserver caller row the engine folds into the Ingress
// finding; a and b have the same findings, so the same score and
// verdicts. c is b with web's Widget at v1alpha1, which a PR that stops
// serving it makes a blocker of the whole cluster's gate; d has no other
// team, no unowned namespace and nothing cluster scoped, so the whole
// cluster scores better there. A payments-scoped gate answer that
// differs between any two tells payments something of another team's.
func leakyInventory(variant string) inventory.Inventory {
	inv := testInventory()
	webTeam, webOther := "web", "web-alpha"
	if variant != "a" {
		webTeam, webOther = "webteam", "web-beta"
	}
	inv.Namespaces = []inventory.NamespaceInfo{
		{Name: "pay-prod", Team: "payments"}, {Name: "web-prod", Team: webTeam}, {Name: webOther, Team: webTeam}, {Name: "ops"},
	}
	n := 1
	if variant != "a" {
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
	crd := widgetCRD()
	crd.Versions[0].Served = true
	if variant == "c" {
		crd.Usage = []inventory.APIUsage{{
			Group: "example.com", Version: "v1alpha1", Kind: "Widget", Count: 1,
			Namespaces: map[string]int{"web-prod": 1}, Objects: objs("web-prod", "widget-leak", 1),
		}}
	}
	inv.CRDs = []inventory.CRD{crd}
	if variant == "d" {
		inv.Namespaces = inv.Namespaces[:1]
		ingress.Count, ingress.Namespaces, ingress.Objects = 1, map[string]int{"pay-prod": 1}, ingress.Objects[:1]
		inv.APIUsage = []inventory.APIUsage{ingress}
	}
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
	"web-leak", "ops-leak", "cron-leak", "psp-leak", "widget-leak", "web-alpha", "web-beta", `"web"`, `"webteam"`,
	"also records requests to", // the folded apiserver caller row
}

// A payments-scoped gate with ?cluster= answers from payments' share of
// the cluster and the PR alone (#72). The cluster's evidence in another
// team's namespace, in one no team owns, cluster-scoped or from its
// apiserver metrics never reaches the answer, not even through a finding
// the PR's own objects are in, nor through its status, verdicts or
// score: clusters that differ only there give payments the same answer,
// byte for byte, in every format, even where the whole cluster's
// findings, score and verdict differ; the fleet-wide token sees them
// differ.
func TestScopedGateAnswersFromTheScopesShareOnly(t *testing.T) {
	_, st, ts, _ := scopeServer(t)
	mintReadToken(t, st, "pay-tok", "payments")
	variants := []string{"a", "b", "c", "d"}
	for _, v := range variants {
		pushScopeCluster(t, ts, "leaky-"+v, leakyInventory(v))
	}
	// The leaky PR joins every API the cluster has evidence at; the CRD
	// PR stops serving v1alpha1 of the Widgets only web has (c), which
	// fails the whole cluster's gate there and nowhere else.
	unservedCRD := widgetsCRD("served: false")
	for _, manifest := range []string{leakyManifest, unservedCRD} {
		for _, format := range gateFormats {
			for _, failOn := range []string{"never", "warning"} {
				q := "?target=1.35&fail-on=" + failOn + format + "&cluster="
				respA, a := postGate(t, ts, q+"leaky-a", "pay-tok", manifest, "application/x-yaml")
				_, fa := postGate(t, ts, q+"leaky-a", "fleet-tok", manifest, "application/x-yaml")
				for _, v := range variants {
					resp, raw := postGate(t, ts, q+"leaky-"+v, "pay-tok", manifest, "application/x-yaml")
					if resp.StatusCode != respA.StatusCode || resp.Header.Get("X-Upgradescope-Verdict") != respA.Header.Get("X-Upgradescope-Verdict") ||
						!bytes.Equal(normUID(a), normUID(raw)) {
						t.Errorf("payments-scoped gate %s differs between clusters a (%d) and %s (%d), which differ only in what is not payments':\n%s\n%s",
							q, respA.StatusCode, v, resp.StatusCode, a, raw)
					}
					for _, s := range leakySecrets {
						if bytes.Contains(raw, []byte(s)) {
							t.Errorf("payments-scoped gate %s names %s:\n%s", q+"leaky-"+v, s, raw)
						}
					}
					// The CI formats hold only what the PR introduces: the
					// JSON answer shows the fixture's differences.
					if v == "a" || format != "" || manifest == unservedCRD && v != "c" && v != "d" {
						continue
					}
					if _, fv := postGate(t, ts, q+"leaky-"+v, "fleet-tok", manifest, "application/x-yaml"); bytes.Equal(normUID(fa), normUID(fv)) {
						t.Errorf("fleet-wide gate %s is the same for clusters a and %s: the fixture does not test the scope", q, v)
					}
				}
				if manifest != leakyManifest || format != "" {
					continue
				}
				for _, own := range []string{"pr-pay", "pr-web", "pr-ops", "pr-new", "pr-psp", "pr-cron"} {
					if !bytes.Contains(a, []byte(own)) {
						t.Errorf("payments-scoped gate %s lost the PR's own %s:\n%s", q, own, a)
					}
				}
				if !bytes.Contains(a, []byte("pay-ingress")) {
					t.Errorf("payments-scoped gate %s lost payments' own cluster object:\n%s", q, a)
				}
			}
		}
	}
	// What the whole cluster's answers to the CRD PR differ in: findings,
	// score, verdict and status, none of which the payments-scoped answers
	// above do.
	whole := map[string]gateSummary{}
	for _, v := range variants {
		resp, raw := postGate(t, ts, "?target=1.35&cluster=leaky-"+v, "fleet-tok", unservedCRD, "application/x-yaml")
		whole[v] = summarize(t, resp, raw)
	}
	if whole["c"].Verdict == whole["a"].Verdict || whole["c"].Status == whole["a"].Status ||
		len(whole["c"].Findings) == len(whole["b"].Findings) || whole["d"].Score == whole["a"].Score {
		t.Errorf("the whole cluster's verdicts, statuses, findings and scores do not differ between variants: %+v", whole)
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
