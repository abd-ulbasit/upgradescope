package chart

import (
	"bytes"
	"encoding/base64"
	"os"
	"os/exec"
	"path/filepath"
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
		if f, ok := strings.CutPrefix(s, "-f="); ok {
			args = append(args, "-f", f)
			continue
		}
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
	// A values file gives a plain byte count as a YAML number, which helm
	// reads as a float64 (5.36870912e+08), unlike --set's int64.
	valuesFile := filepath.Join(t.TempDir(), "values.yaml")
	if err := os.WriteFile(valuesFile, []byte("agent: {resources: {limits: {memory: 536870912}}}\nserver: {resources: {limits: {memory: 1073741824}}}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name  string
		sets  []string
		agent string // "" = unset
		srv   string
	}{
		{"defaults", nil, "241591910", "966367641"},
		{"binary units", []string{"agent.resources.limits.memory=1Gi", "server.resources.limits.memory=1536Mi"}, "966367641", "1449551462"},
		{"decimal units and plain bytes", []string{"agent.resources.limits.memory=500M", "server.resources.limits.memory=1000000000"}, "450000000", "900000000"},
		{"a fraction", []string{"agent.resources.limits.memory=0.5Gi"}, "483183820", "966367641"},
		{"plain bytes from a values file", []string{"-f=" + valuesFile}, "483183820", "966367641"},
		{"an exponent and P units", []string{"agent.resources.limits.memory=5e8", "server.resources.limits.memory=1Pi"}, "450000000", "1013309916158361"},
		{"no limit", []string{"agent.resources.limits=null", "server.resources.limits=null"}, "", ""},
		// A quantity the chart cannot read is left to the binary, which
		// reads the limit from its cgroup when GOMEMLIMIT is unset.
		{"unreadable", []string{"agent.resources.limits.memory=lots", "server.resources.limits.memory=100m"}, "", ""},
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
	if secret != "upgradescope-server-https" || issuer != "ca-issuer" || !slices.Contains(names, "upgradescope-server.upgradescope.svc") {
		t.Errorf("Certificate secretName %q issuer %q dnsNames %v", secret, issuer, names)
	}
	if !slices.Contains(args(container(t, objs, "upgradescope-server")), "--tls-cert-file=/etc/upgradescope/tls/tls.crt") {
		t.Error("server does not serve the cert-manager certificate")
	}
}

// The Ingress's TLS Secret (by default <fullname>-server-tls, which
// operators fill through cert-manager's ingress annotations) and the
// server's own certificate are different certificates: the public host's
// and the Service's, from different issuers. Sharing one Secret made two
// Certificates own it, or the Ingress serve the cluster-CA certificate.
func TestServerTLSSecretIsNotTheIngressSecret(t *testing.T) {
	objs := render(t, "server.enabled=true", "server.ingestToken=t", "server.readToken=r",
		"server.ingress.enabled=true", "server.ingress.host=upgradescope.example.com",
		"server.tls.certManager.issuerRef.name=ca-issuer")
	cert := find(objs, "Certificate", "upgradescope-server")
	ing := find(objs, "Ingress", "upgradescope-server")
	if cert == nil || ing == nil {
		t.Fatalf("want a Certificate and an Ingress, have %v", kinds(objs))
	}
	certSecret, _, _ := unstructured.NestedString(cert.Object, "spec", "secretName")
	tls, _, _ := unstructured.NestedSlice(ing.Object, "spec", "tls")
	ingSecret, _ := tls[0].(map[string]any)["secretName"].(string)
	if certSecret == "" || certSecret == ingSecret {
		t.Errorf("Certificate secretName %q, Ingress tls secretName %q: want two different Secrets", certSecret, ingSecret)
	}
	vols, _, _ := unstructured.NestedSlice(find(objs, "Deployment", "upgradescope-server").Object, "spec", "template", "spec", "volumes")
	if !hasSecretVolume(vols, certSecret) || hasSecretVolume(vols, ingSecret) {
		t.Errorf("server volumes %v: want the Certificate's Secret %q, not the Ingress's %q", vols, certSecret, ingSecret)
	}
	if names, _, _ := unstructured.NestedStringSlice(cert.Object, "spec", "dnsNames"); slices.Contains(names, "upgradescope.example.com") {
		t.Errorf("Certificate dnsNames %v name the Ingress host, which the Ingress's own certificate serves", names)
	}
}

// The shared, any-cluster ingest token is optional: with
// server.sharedIngestToken=false the server gets none (only per-cluster
// tokens push) and the in-chart agent must bring its own.
func TestSharedIngestTokenOptional(t *testing.T) {
	objs := render(t, "server.enabled=true", "server.sharedIngestToken=false", "agent.serverToken=per-cluster")
	if slices.ContainsFunc(containerArgs(t, objs, "upgradescope-server"), func(a string) bool { return strings.HasPrefix(a, "--ingest-token-file=") }) {
		t.Error("server gets --ingest-token-file with server.sharedIngestToken=false")
	}
	if sec := find(objs, "Secret", "upgradescope-server-tokens"); sec != nil {
		for _, field := range []string{"stringData", "data"} {
			if _, ok, _ := unstructured.NestedString(sec.Object, field, "ingestToken"); ok {
				t.Errorf("chart Secret holds an ingestToken in %s with server.sharedIngestToken=false", field)
			}
		}
	}
	if msg := renderErr(t, "server.enabled=true", "server.sharedIngestToken=false"); !strings.Contains(msg, "sharedIngestToken") {
		t.Errorf("the agent rendered without a push token (stderr %q)", msg)
	}
	// Default: the shared token, as before.
	objs = render(t, "server.enabled=true", "server.ingestToken=t")
	if !slices.Contains(containerArgs(t, objs, "upgradescope-server"), "--ingest-token-file=/etc/upgradescope/secret-files/ingestToken") {
		t.Error("server lacks --ingest-token-file by default")
	}
	// The shared token is rendered under data, not stringData: the API
	// server turns stringData into data and keeps no record of it, so a
	// later upgrade that drops the key from stringData left the stored
	// data.ingestToken behind (the token stayed readable, and flipping the
	// toggle back reused it). A data key the chart stops rendering is
	// removed by the upgrade's patch.
	sec := find(objs, "Secret", "upgradescope-server-tokens")
	if _, ok, _ := unstructured.NestedString(sec.Object, "stringData", "ingestToken"); ok {
		t.Error("the shared ingest token is rendered under stringData, so switching sharedIngestToken off later leaves it in the Secret")
	}
	if got, _, _ := unstructured.NestedString(sec.Object, "data", "ingestToken"); got != base64.StdEncoding.EncodeToString([]byte("t")) {
		t.Errorf("Secret data.ingestToken = %q, want the base64 of server.ingestToken", got)
	}
}

// Behind an Ingress, a server that serves HTTPS needs the controller told
// so: the Ingress gets ingress-nginx's backend-protocol annotation (the
// operator's own value wins) and the Service port appProtocol https,
// which other controllers read. The ServiceMonitor and the agent trust
// server.tls.caKey of the Secret; with it empty (a publicly trusted
// certificate, whose Secret has no CA) they use their system roots, and
// the scrape does not break on a missing key.
func TestServerTLSBehindIngressAndPublicCA(t *testing.T) {
	base := []string{"server.enabled=true", "server.ingestToken=t", "server.readToken=r", "server.tls.secretName=srv-tls",
		"server.ingress.enabled=true", "server.ingress.host=upgradescope.example.com", "metrics.serviceMonitor.enabled=true"}
	const backendProtocol = "nginx.ingress.kubernetes.io/backend-protocol"
	objs := render(t, base...)
	if v, _, _ := unstructured.NestedString(find(objs, "Ingress", "upgradescope-server").Object, "metadata", "annotations", backendProtocol); v != "HTTPS" {
		t.Errorf("Ingress %s = %q, want HTTPS", backendProtocol, v)
	}
	ports, _, _ := unstructured.NestedSlice(find(objs, "Service", "upgradescope-server").Object, "spec", "ports")
	if p, _ := ports[0].(map[string]any)["appProtocol"].(string); p != "https" {
		t.Errorf("Service port appProtocol = %q, want https", p)
	}
	ep, _, _ := unstructured.NestedSlice(find(objs, "ServiceMonitor", "upgradescope-server").Object, "spec", "endpoints")
	if key, _, _ := unstructured.NestedString(ep[0].(map[string]any), "tlsConfig", "ca", "secret", "key"); key != "ca.crt" {
		t.Errorf("server ServiceMonitor trusts CA key %q, want ca.crt by default", key)
	}

	objs = render(t, append(base, `server.ingress.annotations.nginx\.ingress\.kubernetes\.io/backend-protocol=GRPCS`)...)
	if v, _, _ := unstructured.NestedString(find(objs, "Ingress", "upgradescope-server").Object, "metadata", "annotations", backendProtocol); v != "GRPCS" {
		t.Errorf("Ingress %s = %q, want the operator's GRPCS", backendProtocol, v)
	}

	objs = render(t, append(base, "server.tls.caKey=")...)
	ep, _, _ = unstructured.NestedSlice(find(objs, "ServiceMonitor", "upgradescope-server").Object, "spec", "endpoints")
	if _, has, _ := unstructured.NestedMap(ep[0].(map[string]any), "tlsConfig", "ca"); has {
		t.Error("server ServiceMonitor names a CA key with server.tls.caKey empty")
	}
	if scheme, _ := ep[0].(map[string]any)["scheme"].(string); scheme != "https" {
		t.Errorf("server ServiceMonitor scheme = %q, want https", scheme)
	}
	agent := container(t, objs, "upgradescope-agent")
	avols, _, _ := unstructured.NestedSlice(find(objs, "Deployment", "upgradescope-agent").Object, "spec", "template", "spec", "volumes")
	if _, set := envVar(agent, "SSL_CERT_DIR"); set || hasSecretVolume(avols, "srv-tls") {
		t.Error("agent mounts the server Secret's CA with server.tls.caKey empty; it should use its image's roots")
	}
	if !slices.Contains(args(agent), "--server-url=https://upgradescope-server.upgradescope.svc:8080") {
		t.Errorf("agent args %v: want the https:// server URL", args(agent))
	}

	// Without server TLS, the Ingress speaks HTTP to the Service.
	objs = render(t, "server.enabled=true", "server.ingestToken=t", "server.readToken=r",
		"server.ingress.enabled=true", "server.ingress.host=upgradescope.example.com")
	if _, has, _ := unstructured.NestedString(find(objs, "Ingress", "upgradescope-server").Object, "metadata", "annotations", backendProtocol); has {
		t.Errorf("Ingress has %s without server TLS", backendProtocol)
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

// TLS terminated at an Ingress (#65): off by default; enabled, it serves
// the host from a TLS Secret through the named class.
func TestIngressTerminatesTLS(t *testing.T) {
	if ing := find(render(t, "server.enabled=true", "server.ingestToken=t"), "Ingress", "upgradescope-server"); ing != nil {
		t.Fatal("an Ingress renders by default")
	}
	objs := render(t, "server.enabled=true", "server.ingestToken=t", "server.readToken=r",
		"server.ingress.enabled=true", "server.ingress.className=nginx", "server.ingress.host=upgradescope.example.com")
	ing := find(objs, "Ingress", "upgradescope-server")
	if ing == nil {
		t.Fatalf("no Ingress (have %v)", kinds(objs))
	}
	class, _, _ := unstructured.NestedString(ing.Object, "spec", "ingressClassName")
	tls, _, _ := unstructured.NestedSlice(ing.Object, "spec", "tls")
	if class != "nginx" || len(tls) != 1 {
		t.Fatalf("Ingress class %q, tls %v: want nginx and one TLS block", class, tls)
	}
	hosts, _, _ := unstructured.NestedStringSlice(tls[0].(map[string]any), "hosts")
	if secret, _ := tls[0].(map[string]any)["secretName"].(string); secret != "upgradescope-server-tls" || !slices.Equal(hosts, []string{"upgradescope.example.com"}) {
		t.Errorf("Ingress TLS secretName %q hosts %v", secret, hosts)
	}
}

// serve re-reads its key pair when the files change, which only happens
// when the kubelet can update the mounted Secret: a subPath mount never
// sees an update, so a renewal would need a restart again.
func TestServerTLSMountFollowsSecretUpdates(t *testing.T) {
	objs := render(t, "server.enabled=true", "server.ingestToken=t", "server.tls.secretName=srv-tls")
	mounts, _, _ := unstructured.NestedSlice(container(t, objs, "upgradescope-server"), "volumeMounts")
	found := false
	for _, m := range mounts {
		m := m.(map[string]any)
		if m["mountPath"] != "/etc/upgradescope/tls" {
			continue
		}
		found = true
		if _, sub := m["subPath"]; sub || m["readOnly"] != true {
			t.Errorf("TLS mount %v: want a read-only directory mount without subPath", m)
		}
	}
	if !found {
		t.Fatalf("server mounts %v: no /etc/upgradescope/tls", mounts)
	}
}

// agent.serverCA trusts a private CA for pushes: one key of a ConfigMap
// or a Secret, mounted read-only and passed as --server-ca-file. It
// replaces the in-chart server's own CA, needs a server to push to, and
// takes a ConfigMap or a Secret, not both.
func TestAgentServerCA(t *testing.T) {
	const file = "--server-ca-file=/etc/upgradescope/server-ca/ca.crt"
	for _, tc := range []struct {
		name, source, key string // source: configMap or secret
		sets              []string
	}{
		{"configMap, default key", "configMap", "ca.crt", []string{"agent.serverCA.configMap=corp-ca"}},
		{"secret, own key", "secret", "bundle.pem", []string{"agent.serverCA.secret=corp-ca", "agent.serverCA.key=bundle.pem"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			objs := render(t, append([]string{"agent.serverUrl=https://hub.internal", "agent.serverToken=t"}, tc.sets...)...)
			agent := container(t, objs, "upgradescope-agent")
			if !slices.Contains(args(agent), file) {
				t.Errorf("agent args %v lack %s", args(agent), file)
			}
			if _, set := envVar(agent, "SSL_CERT_DIR"); set {
				t.Error("agent sets SSL_CERT_DIR alongside --server-ca-file")
			}
			vols, _, _ := unstructured.NestedSlice(find(objs, "Deployment", "upgradescope-agent").Object, "spec", "template", "spec", "volumes")
			if !hasCAVolume(vols, tc.source, "corp-ca", tc.key) {
				t.Errorf("agent volumes %v: want %s corp-ca, key %s at ca.crt", vols, tc.source, tc.key)
			}
		})
	}

	// With the in-chart server's TLS, agent.serverCA wins over its Secret's CA.
	objs := render(t, "server.enabled=true", "server.ingestToken=t", "server.tls.secretName=srv-tls", "agent.serverCA.configMap=corp-ca")
	agent := container(t, objs, "upgradescope-agent")
	vols, _, _ := unstructured.NestedSlice(find(objs, "Deployment", "upgradescope-agent").Object, "spec", "template", "spec", "volumes")
	if _, set := envVar(agent, "SSL_CERT_DIR"); set || hasSecretVolume(vols, "srv-tls") || !slices.Contains(args(agent), file) {
		t.Errorf("agent args %v, volumes %v: want only agent.serverCA trusted", args(agent), vols)
	}
	// No serverCA: no flag.
	objs = render(t, "agent.serverUrl=https://hub.internal", "agent.serverToken=t")
	if slices.ContainsFunc(args(container(t, objs, "upgradescope-agent")), func(a string) bool { return strings.HasPrefix(a, "--server-ca-file") }) {
		t.Error("agent gets --server-ca-file without agent.serverCA")
	}

	for name, sets := range map[string][]string{
		"both sources": {"agent.serverUrl=https://hub.internal", "agent.serverToken=t", "agent.serverCA.configMap=a", "agent.serverCA.secret=b"},
		"no server":    {"agent.serverCA.configMap=corp-ca"},
		"no key":       {"agent.serverUrl=https://hub.internal", "agent.serverToken=t", "agent.serverCA.configMap=corp-ca", "agent.serverCA.key="},
		// The agent refuses --server-ca-file with an http URL; the render
		// says so first, for an http serverUrl or a plain-HTTP in-chart server.
		"http serverUrl":        {"agent.serverUrl=http://hub.internal", "agent.serverToken=t", "agent.serverCA.configMap=corp-ca"},
		"plain in-chart server": {"server.enabled=true", "server.ingestToken=t", "agent.serverCA.configMap=corp-ca"},
	} {
		if msg := renderErr(t, sets...); !strings.Contains(msg, "agent.serverCA") {
			t.Errorf("%s: want a render error naming agent.serverCA, got %q", name, msg)
		}
	}
}

// hasCAVolume reports whether vols holds a volume from the source
// (configMap or secret) name that projects key to ca.crt.
func hasCAVolume(vols []any, source, name, key string) bool {
	nameField := map[string]string{"configMap": "name", "secret": "secretName"}[source]
	for _, v := range vols {
		src, ok, _ := unstructured.NestedMap(v.(map[string]any), source)
		if !ok || src[nameField] != name {
			continue
		}
		items, _, _ := unstructured.NestedSlice(src, "items")
		for _, it := range items {
			if it := it.(map[string]any); it["key"] == key && it["path"] == "ca.crt" {
				return true
			}
		}
	}
	return false
}
