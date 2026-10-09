package chart

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
)

// Tests for the chart hardening of #242: the SQLite temp volume, token
// rotation, the stale threshold, ports, long names, the NetworkPolicy
// guards, high availability and chart-to-chart upgrades.

func podTemplate(t *testing.T, objs []unstructured.Unstructured, deployment string) map[string]any {
	t.Helper()
	d := find(objs, "Deployment", deployment)
	if d == nil {
		t.Fatalf("Deployment %s not rendered (have %v)", deployment, kinds(objs))
	}
	tpl, _, _ := unstructured.NestedMap(d.Object, "spec", "template")
	return tpl
}

func podAnnotation(t *testing.T, objs []unstructured.Unstructured, deployment, key string) (string, bool) {
	t.Helper()
	ann, _, _ := unstructured.NestedStringMap(podTemplate(t, objs, deployment), "metadata", "annotations")
	v, ok := ann[key]
	return v, ok
}

func volumeNamed(t *testing.T, objs []unstructured.Unstructured, deployment, name string) map[string]any {
	t.Helper()
	vols, _, _ := unstructured.NestedSlice(podTemplate(t, objs, deployment), "spec", "volumes")
	for _, v := range vols {
		if m := v.(map[string]any); m["name"] == name {
			return m
		}
	}
	return nil
}

func mountPath(c map[string]any, name string) string {
	ms, _, _ := unstructured.NestedSlice(c, "volumeMounts")
	for _, m := range ms {
		if mm := m.(map[string]any); mm["name"] == name {
			s, _ := mm["mountPath"].(string)
			return s
		}
	}
	return ""
}

func writeValues(t *testing.T, body string) string {
	t.Helper()
	f := filepath.Join(t.TempDir(), "values.yaml")
	if err := os.WriteFile(f, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return "-f=" + f
}

// The server's root filesystem is read-only. SQLite spills a large delete (the
// retention prune, `clusters delete`) into a temp file, and with no writable
// temp directory that fails with "disk I/O error (6410)" and history grows
// without bound (internal/server/store/sqlite_tempdir_test.go reproduces it).
func TestServerSQLiteHasAWritableTempDir(t *testing.T) {
	objs := render(t, "server.enabled=true")
	c := container(t, objs, "upgradescope-server")
	if v, ok := envVar(c, "SQLITE_TMPDIR"); !ok || v != "/tmp" {
		t.Errorf("SQLITE_TMPDIR = %q (set %v), want /tmp", v, ok)
	}
	if got := mountPath(c, "tmp"); got != "/tmp" {
		t.Errorf("tmp volume mounted at %q, want /tmp", got)
	}
	vol := volumeNamed(t, objs, "upgradescope-server", "tmp")
	if vol == nil {
		t.Fatal("no tmp volume")
	}
	if size, _, _ := unstructured.NestedString(vol, "emptyDir", "sizeLimit"); size != "1Gi" {
		t.Errorf("tmp emptyDir sizeLimit = %q, want 1Gi", size)
	}
	if ro, _, _ := unstructured.NestedBool(c, "securityContext", "readOnlyRootFilesystem"); !ro {
		t.Error("the root filesystem is not read-only: the temp volume is not what makes the delete work")
	}

	objs = render(t, "server.enabled=true", "server.tmp.sizeLimit=4Gi")
	if size, _, _ := unstructured.NestedString(volumeNamed(t, objs, "upgradescope-server", "tmp"), "emptyDir", "sizeLimit"); size != "4Gi" {
		t.Errorf("server.tmp.sizeLimit=4Gi renders sizeLimit %q", size)
	}

	// Postgres does no SQLite work: no temp volume, no variable.
	objs = render(t, "server.enabled=true", "server.database.existingSecret=pg")
	c = container(t, objs, "upgradescope-server")
	if _, ok := envVar(c, "SQLITE_TMPDIR"); ok || mountPath(c, "tmp") != "" || volumeNamed(t, objs, "upgradescope-server", "tmp") != nil {
		t.Error("a Postgres server gets the SQLite temp directory")
	}
}

func secretData(t *testing.T, objs []unstructured.Unstructured, name string) map[string]any {
	t.Helper()
	s := find(objs, "Secret", name)
	if s == nil {
		t.Fatalf("Secret %s not rendered (have %v)", name, kinds(objs))
	}
	if _, ok := s.Object["stringData"]; ok {
		t.Errorf("Secret %s has stringData: the API server turns it into data and never removes a key an upgrade drops", name)
	}
	d, _, _ := unstructured.NestedMap(s.Object, "data")
	return d
}

// The Deployments read their tokens from Secrets at container start, so
// rotating a chart-managed token has to change a pod template or the old
// token keeps working until some other event restarts the pod.
func TestTokenRotationRollsThePods(t *testing.T) {
	base := []string{"server.enabled=true", "server.ingestToken=ingest-1", "server.readToken=read-1", "server.adminToken=admin-1",
		"server.slackWebhook=https://hooks.example.com/1", "server.webhook=https://hooks.example.com/2", "server.webhookSecret=sign-1"}
	checksum := func(objs []unstructured.Unstructured, dep, key string) string {
		t.Helper()
		v, ok := podAnnotation(t, objs, dep, key)
		if !ok || v == "" {
			t.Fatalf("%s has no %s annotation", dep, key)
		}
		return v
	}
	before := render(t, base...)
	serverSum := checksum(before, "upgradescope-server", "checksum/secret")
	agentSum := checksum(before, "upgradescope-agent", "checksum/token")

	rotate := func(key, val string) []string {
		out := slices.Clone(base)
		i := slices.IndexFunc(out, func(s string) bool { return strings.HasPrefix(s, key+"=") })
		out[i] = key + "=" + val
		return out
	}
	rotated := func(key string) string {
		if key == "server.slackWebhook" || key == "server.webhook" {
			return "https://hooks.example.com/rotated"
		}
		return "rotated"
	}
	for _, key := range []string{"server.readToken", "server.adminToken", "server.ingestToken", "server.slackWebhook", "server.webhook", "server.webhookSecret"} {
		after := render(t, rotate(key, rotated(key))...)
		if got := checksum(after, "upgradescope-server", "checksum/secret"); got == serverSum {
			t.Errorf("rotating %s leaves the server pod template as it was", key)
		}
		agentChanged := checksum(after, "upgradescope-agent", "checksum/token") != agentSum
		// The in-chart agent pushes with the shared ingest token, so only
		// that one rolls it.
		if want := key == "server.ingestToken"; agentChanged != want {
			t.Errorf("rotating %s: agent pod template changed = %v, want %v", key, agentChanged, want)
		}
	}
	// The same values render the same checksums (no per-render noise).
	again := render(t, base...)
	if checksum(again, "upgradescope-server", "checksum/secret") != serverSum || checksum(again, "upgradescope-agent", "checksum/token") != agentSum {
		t.Error("a second render of the same values changes a checksum")
	}
	if serverSum == agentSum {
		t.Error("server and agent checksums are equal")
	}

	// An inline agent token has its own checksum.
	a1 := render(t, "agent.serverUrl=https://hub.example.com", "agent.serverToken=t1")
	a2 := render(t, "agent.serverUrl=https://hub.example.com", "agent.serverToken=t2")
	if checksum(a1, "upgradescope-agent", "checksum/token") == checksum(a2, "upgradescope-agent", "checksum/token") {
		t.Error("rotating agent.serverToken leaves the agent pod template as it was")
	}

	// A generated ingest token (none set) is not hashed: helm template and
	// GitOps renderers generate a new one every render and would roll the
	// pods on every sync.
	g := render(t, "server.enabled=true")
	if _, ok := podAnnotation(t, g, "upgradescope-agent", "checksum/token"); ok {
		t.Error("the agent hashes a generated ingest token")
	}
	if a, _ := podAnnotation(t, g, "upgradescope-server", "checksum/secret"); a == "" {
		t.Error("the server has no checksum/secret with generated tokens")
	}

	// An existingSecret's contents are not in the chart: nothing to hash.
	e := render(t, "server.enabled=true", "server.existingSecret=mine", "agent.existingSecret=mine-agent")
	if _, ok := podAnnotation(t, e, "upgradescope-server", "checksum/secret"); ok {
		t.Error("the server hashes values that an existingSecret ignores")
	}
	if _, ok := podAnnotation(t, e, "upgradescope-agent", "checksum/token"); ok {
		t.Error("the agent hashes values that agent.existingSecret ignores")
	}
}

// A value dropped on upgrade must leave the live Secret: stringData is
// merged into data and never removed, so every key is written under data.
func TestSecretKeysAreRemovedWithTheirValues(t *testing.T) {
	name := "upgradescope-server-tokens"
	all := render(t, "server.enabled=true", "server.ingestToken=i", "server.readToken=r", "server.adminToken=a",
		"server.slackWebhook=https://s.example.com", "server.webhook=https://w.example.com", "server.webhookSecret=ws")
	d := secretData(t, all, name)
	for _, k := range []string{"ingestToken", "readToken", "adminToken", "slackWebhook", "webhook", "webhookSecret"} {
		if _, ok := d[k]; !ok {
			t.Errorf("Secret data has no %s", k)
		}
	}
	dropped := secretData(t, render(t, "server.enabled=true", "server.ingestToken=i"), name)
	for _, k := range []string{"readToken", "adminToken", "slackWebhook", "webhook", "webhookSecret"} {
		if _, ok := dropped[k]; ok {
			t.Errorf("removing server.%s leaves %s in the rendered Secret", k, k)
		}
	}
	if _, ok := dropped["ingestToken"]; !ok {
		t.Error("the ingest token is gone")
	}
	// No shared ingest token and nothing else: no key at all.
	if d := secretData(t, render(t, "server.enabled=true", "server.sharedIngestToken=false", "agent.serverToken=t"), name); len(d) != 0 {
		t.Errorf("Secret data = %v, want none", d)
	}
	agent := secretData(t, render(t, "agent.serverUrl=https://hub.example.com", "agent.serverToken=t"), "upgradescope-agent-token")
	if _, ok := agent["serverToken"]; !ok {
		t.Error("agent token Secret has no serverToken under data")
	}
}

func serverArg(t *testing.T, objs []unstructured.Unstructured, prefix string) string {
	t.Helper()
	for _, a := range args(container(t, objs, "upgradescope-server")) {
		if v, ok := strings.CutPrefix(a, prefix); ok {
			return v
		}
	}
	return ""
}

func staleRule(t *testing.T, objs []unstructured.Unstructured) string {
	t.Helper()
	expr, _ := alerts(t, objs)["UpgradescopeClusterStale"]["expr"].(string)
	m := regexp.MustCompile(`> (\d+)$`).FindStringSubmatch(expr)
	if m == nil {
		t.Fatalf("UpgradescopeClusterStale expr = %q, want to end in a number of seconds", expr)
	}
	return m[1]
}

// An unchanged cluster pushes at its next tick after the hourly force-sync,
// so the stale threshold has to follow the agent interval: with the default
// 2h and an interval of 3h, every cluster flagged stale part of every cycle.
func TestStaleThresholdFollowsTheAgentInterval(t *testing.T) {
	for _, tc := range []struct {
		interval  string
		wantFlag  string
		wantAlert string
	}{
		{"10m", "2h", "7200"}, // the default: unchanged output
		{"40m", "2h", "7200"},
		{"1h", "10800s", "10800"},
		{"3h", "32400s", "32400"},
		{"1.5h", "16200s", "16200"},
		{"1h30m", "16200s", "16200"},
	} {
		t.Run(tc.interval, func(t *testing.T) {
			objs := render(t, "server.enabled=true", "agent.interval="+tc.interval, "metrics.prometheusRule.enabled=true")
			if got := serverArg(t, objs, "--stale-after="); got != tc.wantFlag {
				t.Errorf("--stale-after = %q, want %q", got, tc.wantFlag)
			}
			if got := staleRule(t, objs); got != tc.wantAlert {
				t.Errorf("alert threshold = %s seconds, want %s", got, tc.wantAlert)
			}
		})
	}

	// Set explicitly: used as written, and the alert follows it.
	objs := render(t, "server.enabled=true", "server.staleAfter=5h", "metrics.prometheusRule.enabled=true")
	if got := serverArg(t, objs, "--stale-after="); got != "5h" {
		t.Errorf("--stale-after = %q, want 5h", got)
	}
	if got := staleRule(t, objs); got != "18000" {
		t.Errorf("alert threshold = %s, want 18000 (follows server.staleAfter)", got)
	}
	// A separate alert threshold wins for the alert only.
	objs = render(t, "server.enabled=true", "metrics.prometheusRule.enabled=true", "metrics.prometheusRule.clusterStaleAfterSeconds=9000")
	if got := staleRule(t, objs); got != "9000" {
		t.Errorf("alert threshold = %s, want 9000", got)
	}

	// Not above the interval: every cluster would read stale between pushes.
	for _, sets := range [][]string{
		{"server.staleAfter=1h", "agent.interval=1h"},
		{"server.staleAfter=30m", "agent.interval=1h"},
		{"metrics.prometheusRule.enabled=true", "metrics.prometheusRule.clusterStaleAfterSeconds=3600", "agent.interval=1h"},
	} {
		if msg := renderErr(t, append([]string{"server.enabled=true"}, sets...)...); !strings.Contains(msg, "must be above agent.interval") {
			t.Errorf("%v: render error = %q, want one naming agent.interval", sets, msg)
		}
	}
	// A hub with no in-chart agent does not know its agents' intervals.
	if msg := renderErr(t, "server.enabled=true", "agent.enabled=false", "server.staleAfter=30m", "agent.interval=1h"); msg != "" {
		t.Errorf("server-only render failed: %s", msg)
	}
}

// The Service port may be 80 or 443; the non-root pod without capabilities
// cannot bind either, so serve listens on its own container port.
func TestServicePortIsNotTheListenPort(t *testing.T) {
	for _, port := range []string{"80", "443", "8080"} {
		objs := render(t, "server.enabled=true", "server.service.port="+port)
		c := container(t, objs, "upgradescope-server")
		if !slices.Contains(args(c), "--listen=:8080") {
			t.Errorf("service.port=%s: args %v, want --listen=:8080", port, args(c))
		}
		ports, _, _ := unstructured.NestedSlice(c, "ports")
		if cp := fmt.Sprint(ports[0].(map[string]any)["containerPort"]); cp != "8080" {
			t.Errorf("service.port=%s: containerPort %s, want 8080", port, cp)
		}
		svc := find(objs, "Service", "upgradescope-server")
		sp, _, _ := unstructured.NestedSlice(svc.Object, "spec", "ports")
		if got := fmt.Sprint(sp[0].(map[string]any)["port"]); got != port {
			t.Errorf("service.port=%s: Service port %s", port, got)
		}
		if got := sp[0].(map[string]any)["targetPort"]; got != "http" {
			t.Errorf("service.port=%s: targetPort %v, want the named port http", port, got)
		}
		agent := container(t, objs, "upgradescope-agent")
		if want := fmt.Sprintf("--server-url=http://upgradescope-server.upgradescope.svc:%s", port); !slices.Contains(args(agent), want) {
			t.Errorf("service.port=%s: agent args %v, want %s (the agent pushes to the Service)", port, args(agent), want)
		}
	}
	objs := render(t, "server.enabled=true", "server.containerPort=9090", "server.service.port=443")
	c := container(t, objs, "upgradescope-server")
	if !slices.Contains(args(c), "--listen=:9090") {
		t.Errorf("containerPort=9090: args %v", args(c))
	}
	if path, port := probe(t, c, "readinessProbe"); path != "/readyz" || port != "http" {
		t.Errorf("readiness probe %s %s, want /readyz on the named port", path, port)
	}
	if msg := renderErr(t, "server.enabled=true", "server.containerPort=443"); msg == "" {
		t.Error("server.containerPort=443 renders: the non-root pod cannot bind it")
	}
}

// A Service name is a DNS-1035 label: at most 63 characters, which the API
// server enforces and helm template does not.
func TestNamesFitSixtyThreeCharacters(t *testing.T) {
	values := writeValues(t, `
server:
  enabled: true
  replicas: 2
  readToken: r
  teamMap: [{pattern: "a-*", team: a}]
  database: {existingSecret: pg}
  tls: {certManager: {issuerRef: {name: ca}}}
  ingress: {enabled: true, host: u.example.com}
networkPolicy: {enabled: true, serverIngressFrom: [{podSelector: {}}]}
agent:
  extraRegistry:
    thing.yaml: "id: thing"
metrics:
  serviceMonitor: {enabled: true}
  prometheusRule: {enabled: true}
  grafanaDashboard: {enabled: true}
`)
	// 53 characters, Helm's longest release name.
	for _, release := range []string{
		strings.Repeat("a", 53),
		strings.Repeat("a-", 26) + "a", // hyphens: a cut can land on one
		"upgradescope",
	} {
		if release != "upgradescope" && len(release) != 53 {
			t.Fatalf("release %q is %d characters", release, len(release))
		}
		objs := renderRelease(t, release, values)
		for _, o := range objs {
			if n := o.GetName(); len(n) > 63 {
				t.Errorf("%s: %s %q is %d characters", release, o.GetKind(), n, len(n))
			}
		}
		// Every Secret and ConfigMap a pod names exists under that name.
		have := map[string]bool{}
		for _, o := range objs {
			have[o.GetKind()+"/"+o.GetName()] = true
		}
		for _, dep := range []string{"-agent", "-server"} {
			var d *unstructured.Unstructured
			for i := range objs {
				if objs[i].GetKind() == "Deployment" && strings.HasSuffix(objs[i].GetName(), dep) {
					d = &objs[i]
				}
			}
			if d == nil {
				t.Fatalf("%s: no %s Deployment", release, dep)
			}
			raw, _ := d.MarshalJSON()
			for _, m := range regexp.MustCompile(`"(?:secretKeyRef|configMap)":\{[^}]*?"name":"([^"]+)"`).FindAllStringSubmatch(string(raw), -1) {
				if m[1] == "pg" {
					continue
				}
				if !have["Secret/"+m[1]] && !have["ConfigMap/"+m[1]] {
					t.Errorf("%s: %s names %q, which is not rendered", release, d.GetName(), m[1])
				}
			}
		}
		// The ServiceMonitor, the rule and the Ingress name the Service.
		svc := ""
		for _, o := range objs {
			if o.GetKind() == "Service" && strings.HasSuffix(o.GetName(), "-server") {
				svc = o.GetName()
			}
		}
		ing := find(objs, "Ingress", svc)
		if ing == nil {
			t.Fatalf("%s: no Ingress named after Service %q", release, svc)
		}
		rules, _, _ := unstructured.NestedSlice(ing.Object, "spec", "rules")
		paths, _, _ := unstructured.NestedSlice(rules[0].(map[string]any), "http", "paths")
		if name, _, _ := unstructured.NestedString(paths[0].(map[string]any), "backend", "service", "name"); name != svc {
			t.Errorf("%s: Ingress backend %q, Service %q", release, name, svc)
		}
		a := alerts(t, objs)
		expr, _ := a["UpgradescopeClusterStale"]["expr"].(string)
		if !strings.Contains(expr, `job="`+svc+`"`) {
			t.Errorf("%s: stale alert %q does not select the server Service %q", release, expr, svc)
		}
	}
	// A release that fits keeps the names it always had.
	objs := render(t, "server.enabled=true", "metrics.serviceMonitor.enabled=true", "server.teamMap[0].pattern=a-*", "server.teamMap[0].team=a")
	var got []string
	for _, o := range objs {
		if o.GetKind() == "Service" || o.GetKind() == "Secret" || o.GetKind() == "ConfigMap" || o.GetKind() == "PersistentVolumeClaim" {
			got = append(got, o.GetKind()+"/"+o.GetName())
		}
	}
	slices.Sort(got)
	want := []string{
		"ConfigMap/upgradescope-server-team-map", "PersistentVolumeClaim/upgradescope-server-data",
		"Secret/upgradescope-server-tokens", "Service/upgradescope-agent-metrics", "Service/upgradescope-server",
	}
	if !slices.Equal(got, want) {
		t.Errorf("default names = %v, want %v", got, want)
	}
}

// networkPolicy admits the in-chart agent only; behind it, the Ingress
// controller and Prometheus get a 504 or a failed scrape with no error
// anywhere in the chart.
func TestNetworkPolicyGuards(t *testing.T) {
	ingress := []string{"server.enabled=true", "networkPolicy.enabled=true", "server.ingress.enabled=true", "server.ingress.host=h.example.com", "server.readToken=x"}
	if msg := renderErr(t, ingress...); !strings.Contains(msg, "networkPolicy.serverIngressFrom") || !strings.Contains(msg, "Ingress controller") {
		t.Errorf("Ingress with a NetworkPolicy and no peers: render error = %q, want one naming networkPolicy.serverIngressFrom", msg)
	}
	withPeer := append(slices.Clone(ingress), "networkPolicy.serverIngressFrom[0].namespaceSelector.matchLabels.kubernetes\\.io/metadata\\.name=ingress-nginx")
	objs := render(t, withPeer...)
	np := find(objs, "NetworkPolicy", "upgradescope-server")
	if np == nil {
		t.Fatal("no NetworkPolicy")
	}
	from, _, _ := unstructured.NestedSlice(np.Object, "spec", "ingress")
	peers, _ := from[0].(map[string]any)["from"].([]any)
	if len(peers) != 2 {
		t.Errorf("NetworkPolicy peers = %v, want the agent and the ingress controller", peers)
	}

	monitor := []string{"server.enabled=true", "networkPolicy.enabled=true", "metrics.serviceMonitor.enabled=true"}
	if msg := renderErr(t, monitor...); !strings.Contains(msg, "networkPolicy.serverIngressFrom") || !strings.Contains(msg, "Prometheus") {
		t.Errorf("ServiceMonitor with a NetworkPolicy and no peers: render error = %q", msg)
	}
	if msg := renderErr(t, append(slices.Clone(monitor), "networkPolicy.serverIngressFrom[0].namespaceSelector.matchLabels.team=obs")...); msg != "" {
		t.Errorf("ServiceMonitor with a peer: %s", msg)
	}
	// Neither exposure: the agent-only policy still renders.
	if msg := renderErr(t, "server.enabled=true", "networkPolicy.enabled=true"); msg != "" {
		t.Errorf("plain NetworkPolicy: %s", msg)
	}
	// Without the policy nothing is guarded.
	if msg := renderErr(t, "server.enabled=true", "server.ingress.enabled=true", "server.ingress.host=h.example.com", "server.readToken=x"); msg != "" {
		t.Errorf("Ingress without a policy: %s", msg)
	}
}

// replicas above one: a PodDisruptionBudget so a node drain keeps a pod up,
// and a soft spread across nodes.
func TestServerHighAvailability(t *testing.T) {
	ha := []string{"server.enabled=true", "server.replicas=2", "server.database.existingSecret=pg"}
	objs := render(t, ha...)
	pdb := find(objs, "PodDisruptionBudget", "upgradescope-server")
	if pdb == nil {
		t.Fatalf("no PodDisruptionBudget (have %v)", kinds(objs))
	}
	if v, _, _ := unstructured.NestedFieldNoCopy(pdb.Object, "spec", "minAvailable"); fmt.Sprint(v) != "1" {
		t.Errorf("minAvailable = %v, want 1", v)
	}
	sel, _, _ := unstructured.NestedStringMap(pdb.Object, "spec", "selector", "matchLabels")
	if sel["app.kubernetes.io/component"] != "server" || sel["app.kubernetes.io/instance"] != "upgradescope" {
		t.Errorf("PDB selector = %v, want the server pods", sel)
	}
	tsc, _, _ := unstructured.NestedSlice(podTemplate(t, objs, "upgradescope-server"), "spec", "topologySpreadConstraints")
	if len(tsc) != 1 {
		t.Fatalf("topologySpreadConstraints = %v, want the default", tsc)
	}
	c := tsc[0].(map[string]any)
	if c["topologyKey"] != "kubernetes.io/hostname" || c["whenUnsatisfiable"] != "ScheduleAnyway" {
		t.Errorf("default spread = %v, want a soft spread over hostnames", c)
	}
	lsel, _, _ := unstructured.NestedStringMap(c, "labelSelector", "matchLabels")
	if !mapsEqual(lsel, sel) {
		t.Errorf("spread selector %v differs from the PDB's %v", lsel, sel)
	}

	// One replica: nothing that could block a drain.
	one := render(t, "server.enabled=true")
	if find(one, "PodDisruptionBudget", "upgradescope-server") != nil {
		t.Error("a PodDisruptionBudget with one replica blocks every node drain")
	}
	if tsc, _, _ := unstructured.NestedSlice(podTemplate(t, one, "upgradescope-server"), "spec", "topologySpreadConstraints"); len(tsc) != 0 {
		t.Errorf("one replica gets topologySpreadConstraints %v", tsc)
	}

	// Opt-outs and overrides.
	off := render(t, append(slices.Clone(ha), "server.podDisruptionBudget.enabled=false", "server.defaultTopologySpread=false")...)
	if find(off, "PodDisruptionBudget", "upgradescope-server") != nil {
		t.Error("podDisruptionBudget.enabled=false still renders one")
	}
	if tsc, _, _ := unstructured.NestedSlice(podTemplate(t, off, "upgradescope-server"), "spec", "topologySpreadConstraints"); len(tsc) != 0 {
		t.Error("defaultTopologySpread=false still spreads")
	}
	mu := render(t, append(slices.Clone(ha), "server.podDisruptionBudget.maxUnavailable=50%")...)
	pdb = find(mu, "PodDisruptionBudget", "upgradescope-server")
	if v, _, _ := unstructured.NestedString(pdb.Object, "spec", "maxUnavailable"); v != "50%" {
		t.Errorf("maxUnavailable = %q, want 50%%", v)
	}
	if _, ok, _ := unstructured.NestedFieldNoCopy(pdb.Object, "spec", "minAvailable"); ok {
		t.Error("minAvailable rendered next to maxUnavailable")
	}
	own := render(t, append(slices.Clone(ha), "server.topologySpreadConstraints[0].maxSkew=1", "server.topologySpreadConstraints[0].topologyKey=topology.kubernetes.io/zone",
		"server.topologySpreadConstraints[0].whenUnsatisfiable=DoNotSchedule")...)
	tsc, _, _ = unstructured.NestedSlice(podTemplate(t, own, "upgradescope-server"), "spec", "topologySpreadConstraints")
	if len(tsc) != 1 || tsc[0].(map[string]any)["topologyKey"] != "topology.kubernetes.io/zone" {
		t.Errorf("own topologySpreadConstraints = %v, want only the zone constraint", tsc)
	}
	// SQLite with replicas is refused, as before.
	if msg := renderErr(t, "server.enabled=true", "server.replicas=2"); !strings.Contains(msg, "Postgres") {
		t.Errorf("SQLite with 2 replicas: %q", msg)
	}
}

func mapsEqual(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

// renderChartDir renders the chart at dir with the given values files.
func renderChartDir(t *testing.T, dir string, files ...string) ([]unstructured.Unstructured, error) {
	t.Helper()
	a := []string{"template", "upgradescope", dir, "--namespace", "upgradescope"}
	for _, f := range files {
		a = append(a, "-f", f)
	}
	cmd := exec.Command(helmBin(t), a...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("%v: %s", err, stderr.String())
	}
	var objs []unstructured.Unstructured
	dec := utilyaml.NewYAMLOrJSONDecoder(bytes.NewReader(out), 4096)
	for {
		var o unstructured.Unstructured
		if err := dec.Decode(&o.Object); err != nil {
			if errors.Is(err, io.EOF) {
				return objs, nil
			}
			return nil, fmt.Errorf("decode rendered manifests: %w", err)
		}
		if o.Object != nil {
			objs = append(objs, o)
		}
	}
}

// `helm upgrade --reset-then-reuse-values` (Helm 3.14+) renders the NEW
// chart with its own values.yaml as the defaults and the OLD release's user
// values (what `helm get values` prints) on top. `--reuse-values` would
// instead make the old release's defaults the new chart's, which pins the
// old image digest and every default that changed since. This renders the
// current chart as the first does and checks that the new defaults win.
func TestUpgradeWithResetThenReuseValues(t *testing.T) {
	const newDigest = "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	dir := t.TempDir()
	if err := os.CopyFS(dir, os.DirFS(".")); err != nil {
		t.Fatal(err)
	}
	// Stamp the way the release workflow does (hack/chart-release-annotations.sh).
	vals, err := os.ReadFile(filepath.Join(dir, "values.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	stamped := strings.Replace(string(vals), "  digest: \"\"\n", "  digest: \""+newDigest+"\"\n", 1)
	if stamped == string(vals) {
		t.Fatal("values.yaml has no digest line to stamp")
	}
	if err := os.WriteFile(filepath.Join(dir, "values.yaml"), []byte(stamped), 0o600); err != nil {
		t.Fatal(err)
	}
	chartYAML, err := os.ReadFile(filepath.Join(dir, "Chart.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	appVersion := regexp.MustCompile(`(?m)^appVersion: "?([^"\n]+)"?$`).FindSubmatch(chartYAML)
	if appVersion == nil {
		t.Fatal("no appVersion in Chart.yaml")
	}

	for _, old := range []string{"user-values-v0.1.x.yaml", "user-values-v0.2.0-rc.2.yaml"} {
		t.Run(old, func(t *testing.T) {
			objs, err := renderChartDir(t, dir, filepath.Join("testdata", "upgrade", old))
			if err != nil {
				t.Fatalf("the current chart does not render on %s: %v", old, err)
			}
			want := "ghcr.io/abd-ulbasit/upgradescope:" + string(appVersion[1]) + "@" + newDigest
			for _, dep := range []string{"upgradescope-agent", "upgradescope-server"} {
				c := container(t, objs, dep)
				if c["image"] != want {
					t.Errorf("%s image = %v, want %s (the new chart's digest, never an old one)", dep, c["image"], want)
				}
			}
			// Defaults that changed since v0.1.x apply unless the user set them.
			srv := container(t, objs, "upgradescope-server")
			if mem, _, _ := unstructured.NestedString(srv, "resources", "limits", "memory"); mem != "1Gi" {
				t.Errorf("server memory limit = %q, want the new default 1Gi", mem)
			}
			if _, ok := envVar(srv, "SQLITE_TMPDIR"); !ok {
				t.Error("an upgraded SQLite server has no SQLITE_TMPDIR")
			}
		})
	}
}

var fence = regexp.MustCompile("^\\s*(```|~~~)")

// The upgrade commands the docs give must not use --reuse-values, which
// carries the old chart's defaults (its image digest, resource limits,
// security contexts) onto the new chart and fails to render when the new
// chart adds a value. A page may warn against it in prose.
func TestDocsDoNotRecommendReuseValues(t *testing.T) {
	roots := []string{"../../README.md", "README.md", "../../docs"}
	var files []string
	for _, r := range roots {
		err := filepath.WalkDir(r, func(p string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if !d.IsDir() && strings.HasSuffix(p, ".md") {
				files = append(files, p)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	bare := regexp.MustCompile(`--reuse-values`)
	var withReset int
	for _, f := range files {
		fh, err := os.Open(f)
		if err != nil {
			t.Fatal(err)
		}
		sc := bufio.NewScanner(fh)
		sc.Buffer(make([]byte, 1<<20), 1<<20)
		inFence, n := false, 0
		for sc.Scan() {
			n++
			line := sc.Text()
			if fence.MatchString(line) {
				inFence = !inFence
			}
			if strings.Contains(line, "--reset-then-reuse-values") {
				withReset++
			}
			if bare.MatchString(strings.ReplaceAll(line, "--reset-then-reuse-values", "")) && inFence {
				t.Errorf("%s:%d recommends --reuse-values in a command: %s", f, n, strings.TrimSpace(line))
			}
		}
		_ = fh.Close()
	}
	if withReset == 0 {
		t.Error("no doc gives the --reset-then-reuse-values command")
	}
}
