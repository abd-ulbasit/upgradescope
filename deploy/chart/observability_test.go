package chart

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"slices"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
)

// render renders the chart with the given --set flags into its objects;
// a "-f=<path>" entry passes a values file instead.
func render(t *testing.T, sets ...string) []unstructured.Unstructured {
	t.Helper()
	args := []string{"template", "upgradescope", ".", "--namespace", "upgradescope"}
	for _, s := range sets {
		if f, ok := strings.CutPrefix(s, "-f="); ok {
			args = append(args, "-f", f)
			continue
		}
		args = append(args, "--set", s)
	}
	cmd := exec.Command(helmBin(t), args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("helm %s: %v\n%s", strings.Join(args, " "), err, stderr.String())
	}
	var objs []unstructured.Unstructured
	dec := utilyaml.NewYAMLOrJSONDecoder(bytes.NewReader(out), 4096)
	for {
		var o unstructured.Unstructured
		if err := dec.Decode(&o.Object); err != nil {
			if errors.Is(err, io.EOF) {
				return objs
			}
			t.Fatalf("decode rendered manifests: %v", err)
		}
		if o.Object != nil {
			objs = append(objs, o)
		}
	}
}

// find returns the rendered object of kind and name, or nil.
func find(objs []unstructured.Unstructured, kind, name string) *unstructured.Unstructured {
	for i := range objs {
		if objs[i].GetKind() == kind && objs[i].GetName() == name {
			return &objs[i]
		}
	}
	return nil
}

func kinds(objs []unstructured.Unstructured) []string {
	var out []string
	for _, o := range objs {
		out = append(out, o.GetKind()+"/"+o.GetName())
	}
	return out
}

// container returns the first container of the named Deployment.
func container(t *testing.T, objs []unstructured.Unstructured, deployment string) map[string]any {
	t.Helper()
	d := find(objs, "Deployment", deployment)
	if d == nil {
		t.Fatalf("Deployment %s not rendered (have %v)", deployment, kinds(objs))
	}
	cs, _, _ := unstructured.NestedSlice(d.Object, "spec", "template", "spec", "containers")
	if len(cs) == 0 {
		t.Fatalf("Deployment %s has no containers", deployment)
	}
	return cs[0].(map[string]any)
}

func probe(t *testing.T, c map[string]any, kind string) (path, port string) {
	t.Helper()
	path, _, _ = unstructured.NestedString(c, kind, "httpGet", "path")
	port, _, _ = unstructured.NestedString(c, kind, "httpGet", "port")
	return path, port
}

func args(c map[string]any) []string {
	raw, _, _ := unstructured.NestedStringSlice(c, "args")
	return raw
}

func TestAgentProbesPortAndLogFlags(t *testing.T) {
	c := container(t, render(t), "upgradescope-agent")
	if p, port := probe(t, c, "livenessProbe"); p != "/healthz" || port != "http" {
		t.Errorf("agent livenessProbe = %s on %s, want /healthz on http", p, port)
	}
	if p, port := probe(t, c, "readinessProbe"); p != "/readyz" || port != "http" {
		t.Errorf("agent readinessProbe = %s on %s, want /readyz on http", p, port)
	}
	ports, _, _ := unstructured.NestedSlice(c, "ports")
	if len(ports) != 1 || ports[0].(map[string]any)["name"] != "http" || fmt.Sprint(ports[0].(map[string]any)["containerPort"]) != "8081" {
		t.Errorf("agent ports = %v, want http 8081", ports)
	}
	for _, want := range []string{"--health-addr=:8081", "--log-format=text", "--log-level=info"} {
		if !slices.Contains(args(c), want) {
			t.Errorf("agent args %v lack %s", args(c), want)
		}
	}

	c = container(t, render(t, "agent.healthPort=9090", "agent.logFormat=json", "agent.logLevel=debug"), "upgradescope-agent")
	for _, want := range []string{"--health-addr=:9090", "--log-format=json", "--log-level=debug"} {
		if !slices.Contains(args(c), want) {
			t.Errorf("agent args %v lack %s", args(c), want)
		}
	}
}

func TestServerReadinessProbeUsesReadyz(t *testing.T) {
	c := container(t, render(t, "server.enabled=true", "server.ingestToken=t"), "upgradescope-server")
	if p, _ := probe(t, c, "readinessProbe"); p != "/readyz" {
		t.Errorf("server readinessProbe path = %s, want /readyz (pings the store)", p)
	}
	if p, _ := probe(t, c, "livenessProbe"); p != "/healthz" {
		t.Errorf("server livenessProbe path = %s, want /healthz (process only)", p)
	}
}

func TestMonitoringObjectsOffByDefault(t *testing.T) {
	for _, o := range render(t, "server.enabled=true", "server.ingestToken=t") {
		switch o.GetKind() {
		case "ServiceMonitor", "PrometheusRule", "ConfigMap":
			t.Errorf("%s/%s rendered by default", o.GetKind(), o.GetName())
		}
		if o.GetName() == "upgradescope-agent-metrics" {
			t.Errorf("agent metrics Service rendered without serviceMonitor.enabled")
		}
	}
}

func TestServiceMonitors(t *testing.T) {
	objs := render(t, "metrics.serviceMonitor.enabled=true", "metrics.serviceMonitor.labels.release=kps",
		"server.enabled=true", "server.ingestToken=t")
	svc := find(objs, "Service", "upgradescope-agent-metrics")
	if svc == nil {
		t.Fatalf("agent metrics Service not rendered (have %v)", kinds(objs))
	}
	if comp, _, _ := unstructured.NestedString(svc.Object, "spec", "selector", "app.kubernetes.io/component"); comp != "agent" {
		t.Errorf("agent metrics Service selects component %q, want agent", comp)
	}
	for name, wantPort := range map[string]string{"upgradescope-agent": "http", "upgradescope-server": "http"} {
		sm := find(objs, "ServiceMonitor", name)
		if sm == nil {
			t.Fatalf("ServiceMonitor %s not rendered (have %v)", name, kinds(objs))
		}
		if sm.GetLabels()["release"] != "kps" {
			t.Errorf("ServiceMonitor %s labels = %v, want release=kps", name, sm.GetLabels())
		}
		eps, _, _ := unstructured.NestedSlice(sm.Object, "spec", "endpoints")
		if len(eps) != 1 {
			t.Fatalf("ServiceMonitor %s endpoints = %v, want 1", name, eps)
		}
		ep := eps[0].(map[string]any)
		if ep["port"] != wantPort || ep["path"] != "/metrics" {
			t.Errorf("ServiceMonitor %s endpoint = %v, want port %s path /metrics", name, ep, wantPort)
		}
		if _, ok := ep["authorization"]; ok {
			t.Errorf("ServiceMonitor %s sends a token without a read token configured", name)
		}
	}

	// A read token protects the server's /metrics: the monitor sends it.
	objs = render(t, "metrics.serviceMonitor.enabled=true", "server.enabled=true", "server.ingestToken=t", "server.readToken=r")
	ep, _, _ := unstructured.NestedSlice(find(objs, "ServiceMonitor", "upgradescope-server").Object, "spec", "endpoints")
	key, _, _ := unstructured.NestedString(ep[0].(map[string]any), "authorization", "credentials", "key")
	name, _, _ := unstructured.NestedString(ep[0].(map[string]any), "authorization", "credentials", "name")
	if key != "readToken" || name != "upgradescope-server-tokens" {
		t.Errorf("server ServiceMonitor credentials = %s/%s, want upgradescope-server-tokens/readToken", name, key)
	}

	// Agent only: no server monitor.
	objs = render(t, "metrics.serviceMonitor.enabled=true")
	if find(objs, "ServiceMonitor", "upgradescope-server") != nil {
		t.Error("server ServiceMonitor rendered without server.enabled")
	}
}

// alerts returns alert name -> rule from the rendered PrometheusRule.
func alerts(t *testing.T, objs []unstructured.Unstructured) map[string]map[string]any {
	t.Helper()
	pr := find(objs, "PrometheusRule", "upgradescope")
	if pr == nil {
		t.Fatalf("PrometheusRule not rendered (have %v)", kinds(objs))
	}
	out := map[string]map[string]any{}
	groups, _, _ := unstructured.NestedSlice(pr.Object, "spec", "groups")
	for _, g := range groups {
		rules, _, _ := unstructured.NestedSlice(g.(map[string]any), "rules")
		for _, r := range rules {
			rule := r.(map[string]any)
			out[rule["alert"].(string)] = rule
		}
	}
	return out
}

func TestPrometheusRule(t *testing.T) {
	got := alerts(t, render(t, "metrics.prometheusRule.enabled=true"))
	for _, name := range []string{"UpgradescopeUpgradeBlocked", "UpgradescopeVerdictUnknown", "UpgradescopeAgentNotTicking"} {
		if _, ok := got[name]; !ok {
			t.Errorf("alert %s missing (have %v)", name, got)
		}
	}
	if _, ok := got["UpgradescopeClusterStale"]; ok {
		t.Error("UpgradescopeClusterStale rendered without server.enabled")
	}
	if f := got["UpgradescopeVerdictUnknown"]["for"]; f != "1h" {
		t.Errorf("UpgradescopeVerdictUnknown for = %v, want 1h", f)
	}
	// Alert templates reach Prometheus unexpanded by Helm.
	if s, _, _ := unstructured.NestedString(got["UpgradescopeUpgradeBlocked"], "annotations", "summary"); !strings.Contains(s, "{{ $labels.target }}") {
		t.Errorf("blocked summary = %q, want a $labels.target template", s)
	}

	got = alerts(t, render(t, "metrics.prometheusRule.enabled=true", "server.enabled=true", "server.ingestToken=t",
		"metrics.prometheusRule.clusterStaleAfterSeconds=900"))
	if expr, _ := got["UpgradescopeClusterStale"]["expr"].(string); !strings.Contains(expr, "> 900") {
		t.Errorf("UpgradescopeClusterStale expr = %q, want the configured threshold", expr)
	}
}

const (
	dashboardFile      = "../grafana/upgradescope-dashboard.json"
	chartDashboardFile = "files/grafana/upgradescope-dashboard.json"
)

func TestGrafanaDashboardConfigMap(t *testing.T) {
	objs := render(t, "metrics.grafanaDashboard.enabled=true")
	cm := find(objs, "ConfigMap", "upgradescope-grafana-dashboard")
	if cm == nil {
		t.Fatalf("dashboard ConfigMap not rendered (have %v)", kinds(objs))
	}
	if cm.GetLabels()["grafana_dashboard"] != "1" {
		t.Errorf("ConfigMap labels = %v, want grafana_dashboard=1 (the Grafana sidecar's default)", cm.GetLabels())
	}
	data, _, _ := unstructured.NestedString(cm.Object, "data", "upgradescope-dashboard.json")
	want, err := os.ReadFile(dashboardFile)
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(data) != strings.TrimSpace(string(want)) {
		t.Error("ConfigMap dashboard differs from deploy/grafana/upgradescope-dashboard.json")
	}
}

// Helm can only read files inside the chart, so the chart carries a copy
// of deploy/grafana/upgradescope-dashboard.json (the one to import by
// hand). Both must stay identical and valid JSON.
func TestGrafanaDashboardCopyInSync(t *testing.T) {
	a, err := os.ReadFile(dashboardFile)
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(chartDashboardFile)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(a, b) {
		t.Fatalf("%s and %s differ: cp %s deploy/chart/%s", dashboardFile, chartDashboardFile, "deploy/grafana/upgradescope-dashboard.json", chartDashboardFile)
	}
	var d map[string]any
	if err := json.Unmarshal(a, &d); err != nil {
		t.Fatalf("dashboard is not JSON: %v", err)
	}
	if d["uid"] != "upgradescope" {
		t.Errorf("dashboard uid = %v, want upgradescope (stable across imports)", d["uid"])
	}
}

var metricRef = regexp.MustCompile(`upgradescope_[a-z_]+`)

// Every metric the dashboard and the alert rules query is one the agent
// or server exports: a renamed metric would otherwise blank a panel or
// silence an alert with no error anywhere.
func TestDashboardAndRulesUseExportedMetrics(t *testing.T) {
	var src []byte
	for _, f := range []string{"../../internal/agent/observe.go", "../../internal/server/metrics.go"} {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		src = append(src, b...)
	}
	exported := map[string]bool{}
	for _, m := range regexp.MustCompile(`"(upgradescope_[a-z_]+)"`).FindAllSubmatch(src, -1) {
		exported[string(m[1])] = true
	}

	dash, err := os.ReadFile(dashboardFile)
	if err != nil {
		t.Fatal(err)
	}
	var rules bytes.Buffer
	for _, r := range alerts(t, render(t, "metrics.prometheusRule.enabled=true", "server.enabled=true", "server.ingestToken=t")) {
		rules.WriteString(r["expr"].(string) + "\n")
	}
	for _, ref := range metricRef.FindAllString(string(dash)+rules.String(), -1) {
		base := ref
		for _, suf := range []string{"_bucket", "_count", "_sum"} {
			base = strings.TrimSuffix(base, suf)
		}
		if !exported[base] && ref != "upgradescope_dashboard" {
			t.Errorf("%s is queried but not exported by the agent or server", ref)
		}
	}
}
