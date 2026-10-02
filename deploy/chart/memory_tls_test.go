package chart

import (
	"bytes"
	"os/exec"
	"slices"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// envVar returns the value of the container's env var name, and whether
// it is set (valueFrom counts as set, with an empty value).
func envVar(c map[string]any, name string) (string, bool) {
	env, _, _ := unstructured.NestedSlice(c, "env")
	for _, e := range env {
		m := e.(map[string]any)
		if m["name"] == name {
			v, _ := m["value"].(string)
			return v, true
		}
	}
	return "", false
}

// renderErr renders the chart and returns helm's error output, "" when it
// renders.
func renderErr(t *testing.T, sets ...string) string {
	t.Helper()
	args := []string{"template", "upgradescope", ".", "--namespace", "upgradescope"}
	for _, s := range sets {
		args = append(args, "--set", s)
	}
	cmd := exec.Command(helmBin(t), args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return stderr.String()
	}
	return ""
}

// The Go runtime does not know the container's memory limit: without
// GOMEMLIMIT the collector lets garbage grow to twice the live heap, and a
// pod whose live heap fits its limit is OOM-killed anyway (#121). Both
// containers get GOMEMLIMIT at 90% of their memory limit, leaving room for
// memory the Go heap does not count.
func TestGoMemLimitFollowsTheContainerLimit(t *testing.T) {
	for _, tc := range []struct {
		name  string
		sets  []string
		agent string // "" = unset
		srv   string
	}{
		{"defaults", nil, "241591910", "483183820"},
		{"binary units", []string{"agent.resources.limits.memory=1Gi", "server.resources.limits.memory=1536Mi"}, "966367641", "1449551462"},
		{"decimal units and plain bytes", []string{"agent.resources.limits.memory=500M", "server.resources.limits.memory=1000000000"}, "450000000", "900000000"},
		{"a fraction", []string{"agent.resources.limits.memory=0.5Gi"}, "483183820", "483183820"},
		{"no limit", []string{"agent.resources.limits=null", "server.resources.limits=null"}, "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			objs := render(t, append([]string{"server.enabled=true", "server.ingestToken=t"}, tc.sets...)...)
			for deployment, want := range map[string]string{"upgradescope-agent": tc.agent, "upgradescope-server": tc.srv} {
				got, set := envVar(container(t, objs, deployment), "GOMEMLIMIT")
				if want == "" && set || want != "" && got != want {
					t.Errorf("%s GOMEMLIMIT = %q (set %v), want %q", deployment, got, set, want)
				}
			}
		})
	}
	// An explicit GOMEMLIMIT in extraEnv wins, without a duplicate entry.
	objs := render(t, "agent.extraEnv[0].name=GOMEMLIMIT", "agent.extraEnv[0].value=100MiB")
	env, _, _ := unstructured.NestedSlice(container(t, objs, "upgradescope-agent"), "env")
	n := 0
	for _, e := range env {
		if e.(map[string]any)["name"] == "GOMEMLIMIT" {
			n++
		}
	}
	if v, _ := envVar(container(t, objs, "upgradescope-agent"), "GOMEMLIMIT"); n != 1 || v != "100MiB" {
		t.Errorf("agent has %d GOMEMLIMIT entries, value %q; want exactly the extraEnv one", n, v)
	}
	if msg := renderErr(t, "agent.resources.limits.memory=lots"); !strings.Contains(msg, "memory") {
		t.Errorf("an unparseable memory limit rendered (stderr %q)", msg)
	}
}

// With server.tls.secretName the in-chart server serves HTTPS with that
// kubernetes.io/tls Secret, its probes and ServiceMonitor use HTTPS, and
// the in-chart agent pushes to an https:// URL, trusting the Secret's
// ca.crt when it has one: a default install pushed the any-cluster
// ingest token over plain HTTP (#126 SE-09).
func TestServerTLSFromSecret(t *testing.T) {
	objs := render(t, "server.enabled=true", "server.ingestToken=t", "server.tls.secretName=srv-tls", "metrics.serviceMonitor.enabled=true")
	srv := container(t, objs, "upgradescope-server")
	for _, want := range []string{"--tls-cert-file=/etc/upgradescope/tls/tls.crt", "--tls-key-file=/etc/upgradescope/tls/tls.key"} {
		if !slices.Contains(args(srv), want) {
			t.Errorf("server args %v lack %s", args(srv), want)
		}
	}
	for _, p := range []string{"livenessProbe", "readinessProbe"} {
		if scheme, _, _ := unstructured.NestedString(srv, p, "httpGet", "scheme"); scheme != "HTTPS" {
			t.Errorf("server %s scheme = %q, want HTTPS", p, scheme)
		}
	}
	vols, _, _ := unstructured.NestedSlice(find(objs, "Deployment", "upgradescope-server").Object, "spec", "template", "spec", "volumes")
	if !hasSecretVolume(vols, "srv-tls") {
		t.Errorf("server volumes %v do not mount Secret srv-tls", vols)
	}
	agent := container(t, objs, "upgradescope-agent")
	if !slices.Contains(args(agent), "--server-url=https://upgradescope-server.upgradescope.svc:8080") {
		t.Errorf("agent args %v: want an https:// server URL", args(agent))
	}
	if dir, _ := envVar(agent, "SSL_CERT_DIR"); dir == "" {
		t.Error("agent does not trust the server Secret's ca.crt (no SSL_CERT_DIR)")
	}
	avols, _, _ := unstructured.NestedSlice(find(objs, "Deployment", "upgradescope-agent").Object, "spec", "template", "spec", "volumes")
	if !hasSecretVolume(avols, "srv-tls") {
		t.Errorf("agent volumes %v do not mount the server Secret's ca.crt", avols)
	}
	ep, _, _ := unstructured.NestedSlice(find(objs, "ServiceMonitor", "upgradescope-server").Object, "spec", "endpoints")
	if scheme, _ := ep[0].(map[string]any)["scheme"].(string); scheme != "https" {
		t.Errorf("server ServiceMonitor scheme = %q, want https", scheme)
	}

	// Without TLS: plain HTTP, as before.
	objs = render(t, "server.enabled=true", "server.ingestToken=t")
	if a := args(container(t, objs, "upgradescope-agent")); !slices.Contains(a, "--server-url=http://upgradescope-server.upgradescope.svc:8080") {
		t.Errorf("agent args %v: want the plain http:// URL without TLS", a)
	}
	if slices.ContainsFunc(args(container(t, objs, "upgradescope-server")), func(a string) bool { return strings.HasPrefix(a, "--tls-") }) {
		t.Error("server gets TLS flags without server.tls")
	}
}

// server.tls.certManager.issuerRef renders a cert-manager Certificate for
// the Service's DNS names into the TLS Secret.
func TestServerTLSFromCertManager(t *testing.T) {
	objs := render(t, "server.enabled=true", "server.ingestToken=t", "server.tls.certManager.issuerRef.name=ca-issuer")
	cert := find(objs, "Certificate", "upgradescope-server")
	if cert == nil {
		t.Fatalf("no Certificate rendered (have %v)", kinds(objs))
	}
	secret, _, _ := unstructured.NestedString(cert.Object, "spec", "secretName")
	issuer, _, _ := unstructured.NestedString(cert.Object, "spec", "issuerRef", "name")
	names, _, _ := unstructured.NestedStringSlice(cert.Object, "spec", "dnsNames")
	if secret != "upgradescope-server-tls" || issuer != "ca-issuer" || !slices.Contains(names, "upgradescope-server.upgradescope.svc") {
		t.Errorf("Certificate secretName %q issuer %q dnsNames %v", secret, issuer, names)
	}
	if !slices.Contains(args(container(t, objs, "upgradescope-server")), "--tls-cert-file=/etc/upgradescope/tls/tls.crt") {
		t.Error("server does not serve the cert-manager certificate")
	}
}

// The shared, any-cluster ingest token is optional: with
// server.sharedIngestToken=false the server gets none (only per-cluster
// tokens push) and the in-chart agent must bring its own.
func TestSharedIngestTokenOptional(t *testing.T) {
	objs := render(t, "server.enabled=true", "server.sharedIngestToken=false", "agent.serverToken=per-cluster")
	if _, set := envVar(container(t, objs, "upgradescope-server"), "UPGRADESCOPE_INGEST_TOKEN"); set {
		t.Error("server gets UPGRADESCOPE_INGEST_TOKEN with server.sharedIngestToken=false")
	}
	if sec := find(objs, "Secret", "upgradescope-server-tokens"); sec != nil {
		if _, ok, _ := unstructured.NestedString(sec.Object, "stringData", "ingestToken"); ok {
			t.Error("chart Secret holds an ingestToken with server.sharedIngestToken=false")
		}
	}
	if msg := renderErr(t, "server.enabled=true", "server.sharedIngestToken=false"); !strings.Contains(msg, "sharedIngestToken") {
		t.Errorf("the agent rendered without a push token (stderr %q)", msg)
	}
	// Default: the shared token, as before.
	objs = render(t, "server.enabled=true", "server.ingestToken=t")
	if _, set := envVar(container(t, objs, "upgradescope-server"), "UPGRADESCOPE_INGEST_TOKEN"); !set {
		t.Error("server lacks UPGRADESCOPE_INGEST_TOKEN by default")
	}
}

func hasSecretVolume(vols []any, secret string) bool {
	for _, v := range vols {
		if name, _, _ := unstructured.NestedString(v.(map[string]any), "secret", "secretName"); name == secret {
			return true
		}
	}
	return false
}
