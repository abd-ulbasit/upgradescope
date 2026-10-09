package chart

import (
	neturl "net/url"
	"slices"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/abd-ulbasit/upgradescope/internal/server"
)

// allowedHosts is the server container's --allowed-host list, each entry
// checked as serve checks it.
func allowedHosts(t *testing.T, c map[string]any) []string {
	t.Helper()
	var out []string
	for _, a := range args(c) {
		v, ok := strings.CutPrefix(a, "--allowed-host=")
		if !ok {
			continue
		}
		for _, h := range strings.Split(v, ",") {
			n, err := server.ParseAllowedHost(h)
			if err != nil {
				t.Errorf("--allowed-host entry %q: serve refuses it: %v", h, err)
			}
			out = append(out, n)
		}
	}
	return out
}

// With the Host guard on (a loopback --listen, or --trust-team-header
// through server.extraArgs), serve answers only for the hosts it is told
// about (#240). The chart tells it the Service's DNS names, which the
// in-chart agent pushes to, the Ingress host and server.allowedHosts, so
// turning the guard on breaks none of them. The probes and the
// ServiceMonitor send the pod's IP, the address their request arrives on,
// which the guard always answers for: they set no Host header.
func TestServerAllowedHosts(t *testing.T) {
	objs := render(t, "server.enabled=true", "server.ingestToken=t", "server.readToken=r",
		"server.ingress.enabled=true", "server.ingress.host=Upgradescope.Example.com",
		"server.allowedHosts[0]=upgradescope.internal.example", "server.allowedHosts[1]=10.96.0.10",
		"metrics.serviceMonitor.enabled=true")
	c := container(t, objs, "upgradescope-server")
	got := allowedHosts(t, c)
	for _, want := range []string{
		"upgradescope-server",
		"upgradescope-server.upgradescope",
		"upgradescope-server.upgradescope.svc",
		"upgradescope-server.upgradescope.svc.cluster.local",
		"upgradescope.example.com",
		"upgradescope.internal.example",
		"10.96.0.10",
	} {
		if !slices.Contains(got, want) {
			t.Errorf("--allowed-host %v lacks %s", got, want)
		}
	}
	// The in-chart agent's push names a host the server answers for.
	agent := container(t, objs, "upgradescope-agent")
	var serverURL string
	for _, a := range args(agent) {
		if v, ok := strings.CutPrefix(a, "--server-url="); ok {
			serverURL = v
		}
	}
	u, err := neturl.Parse(serverURL)
	if err != nil || !slices.Contains(got, u.Hostname()) {
		t.Errorf("the agent pushes to %q (%v), whose host is not in --allowed-host %v", serverURL, err, got)
	}
	// The Ingress routes its host to the server.
	ing := find(objs, "Ingress", "upgradescope-server")
	if ing == nil {
		t.Fatal("no Ingress rendered")
	}
	rules, _, _ := unstructured.NestedSlice(ing.Object, "spec", "rules")
	host, _, _ := unstructured.NestedString(rules[0].(map[string]any), "host")
	if n, _ := server.ParseAllowedHost(host); !slices.Contains(got, n) {
		t.Errorf("the Ingress host %q is not in --allowed-host %v", host, got)
	}
	for _, kind := range []string{"livenessProbe", "readinessProbe"} {
		if h, found, _ := unstructured.NestedSlice(c, kind, "httpGet", "httpHeaders"); found {
			t.Errorf("%s sets headers %v: a Host header would have to be allowed too", kind, h)
		}
	}

	// Without an Ingress, the Service names alone; nothing is allowed twice.
	got = allowedHosts(t, container(t, render(t, "server.enabled=true", "server.ingestToken=t", "server.readToken=r"), "upgradescope-server"))
	if len(got) != 4 || !slices.Contains(got, "upgradescope-server.upgradescope.svc") {
		t.Errorf("--allowed-host without an Ingress = %v, want the four Service names", got)
	}
}

// server.allowedHosts takes names and addresses, never a port or scheme:
// the schema refuses one at render time rather than serve at startup.
func TestServerAllowedHostsSchema(t *testing.T) {
	for _, bad := range []string{"https://upgradescope.example.com", "upgradescope.example.com:443", "a/b", "*.example.com"} {
		if msg := renderErr(t, "server.enabled=true", "server.ingestToken=t", "server.readToken=r", "server.allowedHosts[0]="+bad); msg == "" {
			t.Errorf("server.allowedHosts %q rendered", bad)
		}
	}
}
