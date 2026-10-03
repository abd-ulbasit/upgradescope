// Package chart holds Go tests for the Helm chart's agent RBAC. They keep
// the chart's KB-derived read rules in step with the embedded knowledge base
// and check the rendered ClusterRole with the upstream RBAC rule matcher.
// The package has no non-test code, and .helmignore keeps these files out of
// the packaged chart.
package chart

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	rbacv1 "k8s.io/api/rbac/v1"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/component-helpers/auth/rbac/validation"
	"sigs.k8s.io/yaml"

	"github.com/abd-ulbasit/upgradescope/internal/crd"
	"github.com/abd-ulbasit/upgradescope/internal/kb"
)

var update = flag.Bool("update", false, "rewrite files/kb-rbac-rules.yaml from the embedded KB")

const kbRulesFile = "files/kb-rbac-rules.yaml"

// notListable are KB kinds with no list endpoint of their own: review and
// discovery payloads, and kinds served only as subresources (pods/eviction,
// */scale, deployments/rollback). The api-usage collector only lists
// resources whose discovery entry has the list verb, so these never need a
// rule. Keyed by "group/Kind".
var notListable = map[string]bool{
	"/EphemeralContainers":                          true,
	"/PodStatusResult":                              true,
	"batch/JobTemplate":                             true,
	"extensions/ReplicationControllerDummy":         true,
	"admission.k8s.io/AdmissionReview":              true,
	"apidiscovery.k8s.io/APIGroupDiscovery":         true,
	"apiextensions.k8s.io/ConversionReview":         true,
	"apps/DeploymentRollback":                       true,
	"apps/Scale":                                    true,
	"authentication.k8s.io/SelfSubjectReview":       true,
	"authentication.k8s.io/TokenReview":             true,
	"authorization.k8s.io/LocalSubjectAccessReview": true,
	"authorization.k8s.io/SelfSubjectAccessReview":  true,
	"authorization.k8s.io/SelfSubjectRulesReview":   true,
	"authorization.k8s.io/SubjectAccessReview":      true,
	"extensions/DeploymentRollback":                 true,
	"extensions/Scale":                              true,
	"policy/Eviction":                               true,
}

// irregularPlurals are kinds whose resource name is not the regular plural.
var irregularPlurals = map[string]string{
	"Endpoints":               "endpoints",
	"ResourceClaimParameters": "resourceclaimparameters",
	"ResourceClassParameters": "resourceclassparameters",
}

// resourceFor returns the REST resource name for a kind: lower-case
// English plural, the convention every built-in API follows.
func resourceFor(kind string) string {
	if r, ok := irregularPlurals[kind]; ok {
		return r
	}
	s := strings.ToLower(kind)
	switch {
	case strings.HasSuffix(s, "s"), strings.HasSuffix(s, "x"), strings.HasSuffix(s, "ch"), strings.HasSuffix(s, "sh"):
		return s + "es"
	case strings.HasSuffix(s, "y") && len(s) > 1 && !strings.ContainsRune("aeiou", rune(s[len(s)-2])):
		return s[:len(s)-1] + "ies"
	}
	return s + "s"
}

// kbGroupResources returns group -> sorted resources for every listable
// kind the KB flags as deprecated or removed: exactly what the api-usage
// collector may list.
func kbGroupResources(t *testing.T) map[string][]string {
	t.Helper()
	k, err := kb.Load()
	if err != nil {
		t.Fatalf("kb.Load: %v", err)
	}
	seen := map[string]map[string]bool{}
	for _, e := range k.APILifecycle {
		if e.Deprecated == nil && e.Removed == nil {
			continue
		}
		if notListable[e.Group+"/"+e.Kind] {
			continue
		}
		if seen[e.Group] == nil {
			seen[e.Group] = map[string]bool{}
		}
		seen[e.Group][resourceFor(e.Kind)] = true
	}
	out := map[string][]string{}
	for g, rs := range seen {
		for r := range rs {
			out[g] = append(out[g], r)
		}
		sort.Strings(out[g])
	}
	return out
}

func quoteAll(ss []string) string {
	q := make([]string, len(ss))
	for i, s := range ss {
		q[i] = fmt.Sprintf("%q", s)
	}
	return strings.Join(q, ", ")
}

// renderKBRules renders the rules file: one get/list rule per API group.
func renderKBRules(groups map[string][]string) []byte {
	names := make([]string, 0, len(groups))
	for g := range groups {
		names = append(names, g)
	}
	sort.Strings(names)
	var b bytes.Buffer
	b.WriteString("# GENERATED from the embedded KB, do not edit. Regenerate with:\n")
	b.WriteString("#   go test ./deploy/chart -run TestKBRBACRulesInSync -update\n")
	b.WriteString("# get/list on every group/resource the KB flags as deprecated or removed,\n")
	b.WriteString("# so the api-usage collector can count objects still stored at them.\n")
	for _, g := range names {
		fmt.Fprintf(&b, "- apiGroups: [%q]\n  resources: [%s]\n  verbs: [\"get\", \"list\"]\n", g, quoteAll(groups[g]))
	}
	return b.Bytes()
}

// TestKBRBACRulesInSync fails when the KB gains or drops a deprecated
// group/resource and the chart's read rules were not regenerated, so the
// allowlist cannot silently rot (a missing rule would degrade api-usage to
// 403s; an extra one grants reads nothing needs).
func TestKBRBACRulesInSync(t *testing.T) {
	want := renderKBRules(kbGroupResources(t))
	if *update {
		if err := os.WriteFile(kbRulesFile, want, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	got, err := os.ReadFile(kbRulesFile)
	if err != nil {
		t.Fatalf("read %s: %v", kbRulesFile, err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("%s is out of date with the embedded KB; run: go test ./deploy/chart -run TestKBRBACRulesInSync -update\n--- want\n%s", kbRulesFile, want)
	}
	var rules []rbacv1.PolicyRule
	if err := yaml.UnmarshalStrict(got, &rules); err != nil {
		t.Fatalf("%s is not a list of PolicyRules: %v", kbRulesFile, err)
	}
}

func TestResourceFor(t *testing.T) {
	for kind, want := range map[string]string{
		"Endpoints":                  "endpoints",
		"ComponentStatus":            "componentstatuses",
		"Ingress":                    "ingresses",
		"NetworkPolicy":              "networkpolicies",
		"CSIStorageCapacity":         "csistoragecapacities",
		"PriorityLevelConfiguration": "prioritylevelconfigurations",
		"Gateway":                    "gateways",
		"IPAddress":                  "ipaddresses",
	} {
		if got := resourceFor(kind); got != want {
			t.Errorf("resourceFor(%q) = %q, want %q", kind, got, want)
		}
	}
}

// --- rendered ClusterRole checks (need the helm binary) ---

// helmBin returns the helm binary, skipping the test when it is absent
// unless UPGRADESCOPE_CHART_TEST=1 (hack/test-chart.sh sets it, so CI
// cannot skip silently).
func helmBin(t *testing.T) string {
	t.Helper()
	p, err := exec.LookPath("helm")
	if err != nil {
		if os.Getenv("UPGRADESCOPE_CHART_TEST") == "1" {
			t.Fatal("helm not found in PATH (UPGRADESCOPE_CHART_TEST=1)")
		}
		t.Skip("helm not found in PATH; run hack/test-chart.sh")
	}
	return p
}

// renderClusterRole renders the chart with the given --set flags and
// returns the agent ClusterRole's rules (nil when none is rendered).
func renderClusterRole(t *testing.T, sets ...string) []rbacv1.PolicyRule {
	t.Helper()
	args := []string{"template", "upgradescope", ".", "--namespace", "upgradescope"}
	for _, s := range sets {
		args = append(args, "--set", s)
	}
	cmd := exec.Command(helmBin(t), args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("helm %s: %v\n%s", strings.Join(args, " "), err, stderr.String())
	}
	dec := utilyaml.NewYAMLOrJSONDecoder(bytes.NewReader(out), 4096)
	for {
		var role rbacv1.ClusterRole
		if err := dec.Decode(&role); err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			t.Fatalf("decode rendered manifests: %v", err)
		}
		if role.Kind == "ClusterRole" {
			return role.Rules
		}
	}
}

func res(group, resource string, verbs ...string) rbacv1.PolicyRule {
	return rbacv1.PolicyRule{APIGroups: []string{group}, Resources: []string{resource}, Verbs: verbs}
}

func named(r rbacv1.PolicyRule, name string) rbacv1.PolicyRule {
	r.ResourceNames = []string{name}
	return r
}

func url(path string) rbacv1.PolicyRule {
	return rbacv1.PolicyRule{NonResourceURLs: []string{path}, Verbs: []string{"get"}}
}

func ruleString(r rbacv1.PolicyRule) string {
	if len(r.NonResourceURLs) > 0 {
		return fmt.Sprintf("%v %v", r.Verbs, r.NonResourceURLs)
	}
	s := fmt.Sprintf("%v %v/%v", r.Verbs, r.APIGroups, r.Resources)
	if len(r.ResourceNames) > 0 {
		s += fmt.Sprintf(" names=%v", r.ResourceNames)
	}
	return s
}

func assertAllowed(t *testing.T, rules []rbacv1.PolicyRule, want ...rbacv1.PolicyRule) {
	t.Helper()
	for _, w := range want {
		if ok, _ := validation.Covers(rules, []rbacv1.PolicyRule{w}); !ok {
			t.Errorf("DENIED, want allowed: %s", ruleString(w))
		}
	}
}

// assertDenied requires every single verb/resource/name of each rule to be
// denied (Covers on the whole rule would pass if just one verb were).
func assertDenied(t *testing.T, rules []rbacv1.PolicyRule, deny ...rbacv1.PolicyRule) {
	t.Helper()
	for _, d := range deny {
		for _, sub := range validation.BreakdownRule(d) {
			if ok, _ := validation.Covers(rules, []rbacv1.PolicyRule{sub}); ok {
				t.Errorf("ALLOWED, want denied: %s", ruleString(sub))
			}
		}
	}
}

const ourCRD = "clusterreadinesses.upgradescope.basit.engineer"

// collectorCalls are the requests the agent's collectors and CR writer make.
func collectorCalls(t *testing.T) []rbacv1.PolicyRule {
	calls := []rbacv1.PolicyRule{
		url("/version"), // versions: server version
		url("/metrics"), // deprecated-calls: apiserver_requested_deprecated_apis
		named(res("", "namespaces", "get"), "kube-system"), // cluster ID
		res("", "namespaces", "list"),                      // team attribution
		res("", "nodes", "list"),                           // kubelet versions
		res("", "pods", "list"),                            // add-on images and labels, control-plane pods
		// add-ons: IngressClass controllers. Granted by the KB rules
		// (networking.k8s.io/v1beta1 IngressClass is removed), pinned here
		// so a KB change cannot drop it unnoticed.
		res("networking.k8s.io", "ingressclasses", "list"),
		// crds: CRD versions and status.storedVersions. Granted by the KB
		// rules (apiextensions.k8s.io/v1beta1 is removed), pinned here so
		// a KB change cannot drop it unnoticed. Custom resources are not
		// granted (no wildcards): the agent reports crds partial for CRDs
		// with a deprecated or unserved version.
		res("apiextensions.k8s.io", "customresourcedefinitions", "list"),
		res("upgradescope.basit.engineer", "clusterreadinesses", "get", "create"),
		named(res("upgradescope.basit.engineer", "clusterreadinesses", "update", "patch"), "cluster"), // spec.targets, and the status-error annotation
		named(res("upgradescope.basit.engineer", "clusterreadinesses/status", "get", "update"), "cluster"),
	}
	for g, rs := range kbGroupResources(t) {
		for _, r := range rs {
			calls = append(calls, res(g, r, "list"))
		}
	}
	return calls
}

// neverAllowed holds grants the agent must not have under any values.
var neverAllowed = []rbacv1.PolicyRule{
	res("", "nodes/proxy", "get"), // kubelet API: exec into any pod
	res("", "nodes/proxy", "create"),
	res("", "pods/log", "get"),
	res("", "pods/exec", "get", "create"),
	res("", "pods/attach", "get", "create"),
	res("", "pods/portforward", "get", "create"),
	res("", "serviceaccounts/token", "create"),
	res("", "configmaps", "create", "update", "patch", "delete", "watch"),
	res("", "pods", "watch", "create", "delete", "patch"),
	res("apiextensions.k8s.io", "customresourcedefinitions", "create", "delete"),
	named(res("apiextensions.k8s.io", "customresourcedefinitions", "update", "patch"), "certificates.cert-manager.io"),
	named(res("upgradescope.basit.engineer", "clusterreadinesses", "update", "patch", "delete"), "someone-else"),
	res("upgradescope.basit.engineer", "clusterreadinesses", "delete", "deletecollection", "watch"),
	res("rbac.authorization.k8s.io", "clusterroles", "escalate", "bind", "create"),
	url("/logs"),
	url("/debug/pprof"),
}

func assertNoWildcards(t *testing.T, rules []rbacv1.PolicyRule) {
	t.Helper()
	for _, r := range rules {
		for _, field := range [][]string{r.APIGroups, r.Resources, r.Verbs, r.NonResourceURLs, r.ResourceNames} {
			for _, v := range field {
				if strings.Contains(v, "*") {
					t.Errorf("wildcard %q in rule %s", v, ruleString(r))
				}
			}
		}
	}
}

func TestRenderedRBACDefault(t *testing.T) {
	rules := renderClusterRole(t)
	if len(rules) == 0 {
		t.Fatal("no ClusterRole rendered")
	}
	assertNoWildcards(t, rules)
	assertAllowed(t, rules, collectorCalls(t)...)
	assertAllowed(t, rules,
		res("", "secrets", "get", "list"),    // rbac.helmSecrets defaults on: Helm secrets driver
		res("", "configmaps", "get", "list"), // and configmaps driver
		named(res("apiextensions.k8s.io", "customresourcedefinitions", "get", "update", "patch"), ourCRD),
		// agent.manageCRD defaults on: the agent looks up the pre-v0.2.0
		// CRD to say it can be deleted (#68). Granted by the KB rules'
		// get/list on all CRDs, pinned here so a KB change cannot turn
		// the check into a silent "could not check".
		named(res("apiextensions.k8s.io", "customresourcedefinitions", "get"), crd.LegacyCRDName),
	)
	assertDenied(t, rules, neverAllowed...)
	assertDenied(t, rules, res("", "secrets", "watch", "create", "update"))
}

// The non-resource URLs granted are exactly the two the collectors read
// (RB-06 in docs/claims.md); discovery comes from system:discovery.
func TestRenderedRBACNonResourceURLs(t *testing.T) {
	var got []string
	for _, r := range renderClusterRole(t) {
		got = append(got, r.NonResourceURLs...)
	}
	sort.Strings(got)
	if want := []string{"/metrics", "/version"}; strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("non-resource URLs granted = %v, want exactly %v", got, want)
	}
}

func TestRenderedRBACHelmSecretsOff(t *testing.T) {
	rules := renderClusterRole(t, "rbac.helmSecrets=false")
	assertNoWildcards(t, rules)
	assertAllowed(t, rules, collectorCalls(t)...)
	assertDenied(t, rules, res("", "secrets", "get", "list"), res("", "configmaps", "get", "list"))
	assertDenied(t, rules, neverAllowed...)
}

// #70: the GitOps chart sources are read only when asked for, and only the
// resources that name a chart: Argo CD Applications, Flux HelmReleases and
// the OCIRepositories their chartRefs point at (listed since #248, a get by
// name where only that is granted).
func TestRenderedRBACGitOpsOptIn(t *testing.T) {
	argo := res("argoproj.io", "applications", "get", "list")
	flux := []rbacv1.PolicyRule{
		res("helm.toolkit.fluxcd.io", "helmreleases", "get", "list"),
		res("source.toolkit.fluxcd.io", "ocirepositories", "get", "list"),
	}
	for _, tc := range []struct {
		name       string
		sets       []string
		argo, flux bool
	}{
		{"default", nil, false, false},
		{"argocd", []string{"rbac.gitops.argocd=true"}, true, false},
		{"flux", []string{"rbac.gitops.flux=true"}, false, true},
		{"both", []string{"rbac.gitops.argocd=true", "rbac.gitops.flux=true"}, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rules := renderClusterRole(t, tc.sets...)
			assertNoWildcards(t, rules)
			assertAllowed(t, rules, collectorCalls(t)...)
			assertDenied(t, rules, neverAllowed...)
			// Never writes or watches a custom resource of either tool, nor
			// reads the rest of their groups (AppProjects, Secrets of repos).
			assertDenied(t, rules,
				res("argoproj.io", "applications", "watch", "create", "update", "patch", "delete"),
				res("argoproj.io", "appprojects", "get", "list"),
				res("argoproj.io", "applicationsets", "get", "list"),
				res("helm.toolkit.fluxcd.io", "helmreleases", "watch", "create", "update", "patch", "delete"),
				res("source.toolkit.fluxcd.io", "helmrepositories", "get", "list"),
				res("source.toolkit.fluxcd.io", "ocirepositories", "watch", "create", "update", "patch", "delete"),
			)
			check := assertDenied
			if tc.argo {
				check = assertAllowed
			}
			check(t, rules, argo)
			check = assertDenied
			if tc.flux {
				check = assertAllowed
			}
			check(t, rules, flux...)
		})
	}
}

func TestRenderedRBACManageCRDOff(t *testing.T) {
	rules := renderClusterRole(t, "agent.manageCRD=false")
	assertAllowed(t, rules, collectorCalls(t)...)
	assertDenied(t, rules, named(res("apiextensions.k8s.io", "customresourcedefinitions", "update", "patch"), ourCRD))
	assertDenied(t, rules, neverAllowed...)
}

func TestRenderedRBACCustomCRName(t *testing.T) {
	rules := renderClusterRole(t, "agent.crName=prod")
	assertAllowed(t, rules,
		named(res("upgradescope.basit.engineer", "clusterreadinesses", "update", "patch"), "prod"),
		named(res("upgradescope.basit.engineer", "clusterreadinesses/status", "get", "update", "patch"), "prod"),
	)
	assertDenied(t, rules,
		named(res("upgradescope.basit.engineer", "clusterreadinesses", "update", "patch"), "cluster"),
		named(res("upgradescope.basit.engineer", "clusterreadinesses/status", "update"), "cluster"),
	)
}

// --- the prose that describes this role (#130 RB-01) ---

// rbacDocs are the pages that tell users what the agent's ClusterRole
// grants. They must say what TestRenderedRBACDefault proves.
var rbacDocs = []string{"README.md", "SECURITY.md", "docs/operations/security-model-and-rbac.md"}

var (
	// A paragraph break, a table cell, or the end of a sentence or clause
	// (a dot inside a name such as rbac.helmSecrets is not followed by a
	// space).
	clauseEnd = regexp.MustCompile(`\n\s*\n|\||[.;:](\s|$)`)
	// watch, with the (at most two) words before it: "never `watch`",
	// "and no `watch`", "never watches".
	watchWord = regexp.MustCompile("(?i)((?:\\S+\\s+){0,2})`?\\bwatch(?:es|ed|ing)?\\b")
	negation  = regexp.MustCompile(`(?i)\b(never|no|not|nor|without)\b`)
	// "..., `watch`, and Secrets ... are denied": the clause denies a list.
	deniedEnd = regexp.MustCompile(`(?i)\b(are|is) denied\s*$`)
	// kubectl's --watch flag: what the reader runs, not what the role grants.
	watchFlag = regexp.MustCompile(`--watch\b`)
)

// rbacDocProblems returns what a page describing the agent's role gets
// wrong: a clause that grants watch (the role has get and list only: each
// mention must be negated by one of the two words before it, or be in a
// clause ending "are denied"), a CRD grant "not restricted by
// resourceNames" (both CRD write rules are), or no mention of the
// cluster-wide ConfigMaps read that rbac.helmSecrets adds. A --watch flag
// (`kubectl get ... --watch`) is not a grant.
func rbacDocProblems(doc string) []string {
	var out []string
	for _, clause := range clauseEnd.Split(watchFlag.ReplaceAllString(doc, ""), -1) {
		if deniedEnd.MatchString(clause) {
			continue
		}
		for _, m := range watchWord.FindAllStringSubmatch(clause, -1) {
			if !negation.MatchString(m[1]) {
				out = append(out, fmt.Sprintf("grants watch (the role has get and list only): %q", strings.Join(strings.Fields(clause), " ")))
				break
			}
		}
	}
	flat := strings.ToLower(strings.Join(strings.Fields(strings.ReplaceAll(doc, "`", "")), " "))
	if strings.Contains(flat, "not restricted by resourcenames") || strings.Contains(flat, "not limited by resourcenames") {
		out = append(out, "says a CRD grant is not restricted by resourceNames; every write on a CRD is")
	}
	if !strings.Contains(flat, "configmap") {
		out = append(out, "does not mention the cluster-wide ConfigMaps get/list that rbac.helmSecrets grants")
	}
	return out
}

func TestRBACDocProblems(t *testing.T) {
	const cm = " Helm needs ConfigMaps."
	for _, tc := range []struct {
		name, doc string
		bad       bool
	}{
		{"get list only", "It reads with `get` and `list`, never `watch`." + cm, false},
		{"never watches", "Reads are get and list only; the agent polls and never watches." + cm, false},
		{"denied in the same sentence", "Other CRDs, `watch`, and Secrets with `rbac.helmSecrets=false` are denied." + cm, false},
		{"no watch clause", "- `get`/`list` on pods; no wildcards and no `watch`;" + cm, false},
		{"slash list", "The agent has get/list/watch on pods." + cm, true},
		{"comma list", "It reads with `get`, `list` and `watch` on all resources." + cm, true},
		{"watch in a table cell", "| `get`, `list`, `watch` on `pods` | no reason |" + cm, true},
		{"negation in another sentence", "It never writes. It may watch pods." + cm, true},
		{"negation of another item", "On pods; no wildcards, and `watch` on everything;" + cm, true},
		{"watch then a negation", "It can `watch` pods, but not nodes." + cm, true},
		{"not restricted", "CRD create/update/patch, not restricted by `resourceNames`." + cm, true},
		{"not restricted across lines", "CRD writes, not\n    restricted by `resourceNames`." + cm, true},
		{"no configmaps", "It reads with `get` and `list`, never `watch`, and Secrets.", true},
		// kubectl's --watch flag is the reader's, not a grant to the agent.
		{"kubectl --watch", "Follow it with `kubectl get clusterreadiness cluster --watch`." + cm, false},
		{"--watch beside a grant", "Run it with --watch; the agent can watch pods." + cm, true},
	} {
		if got := rbacDocProblems(tc.doc); (len(got) > 0) != tc.bad {
			t.Errorf("%s: problems = %q, want bad=%v", tc.name, got, tc.bad)
		}
	}
}

// TestRBACDocsMatchRole: README.md, SECURITY.md and the security page
// describe the role this chart renders (#130 RB-01).
func TestRBACDocsMatchRole(t *testing.T) {
	for _, page := range rbacDocs {
		b, err := os.ReadFile(filepath.Join("..", "..", page))
		if err != nil {
			t.Fatal(err)
		}
		for _, p := range rbacDocProblems(string(b)) {
			t.Errorf("%s %s", page, p)
		}
	}
}
