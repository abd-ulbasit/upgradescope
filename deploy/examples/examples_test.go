// Package examples holds Go tests for the example manifests under
// deploy/examples. The package has no non-test code.
package examples

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	"sigs.k8s.io/yaml"

	"github.com/abd-ulbasit/upgradescope/internal/cli"
)

// decodeAll splits a multi-document manifest and decodes each document
// strictly into the typed object its kind names.
func decodeAll(t *testing.T, path string) (deploys []appsv1.Deployment, services []corev1.Service) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	r := utilyaml.NewYAMLReader(bufio.NewReader(bytes.NewReader(raw)))
	for {
		doc, err := r.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if len(bytes.TrimSpace(doc)) == 0 {
			continue
		}
		var meta struct{ Kind string }
		if err := yaml.Unmarshal(doc, &meta); err != nil {
			t.Fatal(err)
		}
		switch meta.Kind {
		case "Deployment":
			var d appsv1.Deployment
			if err := yaml.UnmarshalStrict(doc, &d); err != nil {
				t.Fatalf("Deployment: %v", err)
			}
			deploys = append(deploys, d)
		case "Service":
			var s corev1.Service
			if err := yaml.UnmarshalStrict(doc, &s); err != nil {
				t.Fatalf("Service: %v", err)
			}
			services = append(services, s)
		case "PersistentVolumeClaim":
			var p corev1.PersistentVolumeClaim
			if err := yaml.UnmarshalStrict(doc, &p); err != nil {
				t.Fatalf("PersistentVolumeClaim: %v", err)
			}
		default:
			t.Fatalf("unexpected kind %q", meta.Kind)
		}
	}
	return deploys, services
}

func container(t *testing.T, d appsv1.Deployment, name string) corev1.Container {
	t.Helper()
	for _, c := range d.Spec.Template.Spec.Containers {
		if c.Name == name {
			return c
		}
	}
	t.Fatalf("no container %q", name)
	return corev1.Container{}
}

// The oauth2-proxy example (docs/operations/auth.md) holds what makes the
// trusted header safe: serve trusts it from the sidecar on 127.0.0.1
// only, (1) serve listens on loopback and every Service exposes the
// proxy alone, and (2) the proxy strips a client's copies of the header
// it sets. Its arguments parse as serve's, and the proxy leaves
// Authorization alone.
func TestOAuth2ProxyExample(t *testing.T) {
	deploys, services := decodeAll(t, "oauth2-proxy/upgradescope-oauth2-proxy.yaml")
	if len(deploys) != 1 {
		t.Fatalf("%d Deployments, want 1", len(deploys))
	}
	d := deploys[0]
	server, proxy := container(t, d, "server"), container(t, d, "oauth2-proxy")

	cmd, rest, err := cli.Root().Find(server.Args)
	if err == nil {
		err = cmd.ParseFlags(rest)
	}
	if err == nil {
		err = cmd.ValidateFlagGroups()
	}
	if err != nil || cmd.Name() != "serve" {
		t.Fatalf("server args %v do not parse as serve: %v", server.Args, err)
	}
	if got := cmd.Flags().Lookup("trust-team-header").Value.String(); got != "X-Forwarded-Groups" {
		t.Errorf("--trust-team-header = %q, want X-Forwarded-Groups", got)
	}
	if got := cmd.Flags().Lookup("trusted-proxy-cidr").Value.String(); got != "[127.0.0.1/32]" {
		t.Errorf("--trusted-proxy-cidr = %s, want only the sidecar's 127.0.0.1/32", got)
	}

	// Everything that reaches serve over loopback is trusted with the
	// header: a service-mesh sidecar that delivers inbound traffic over
	// localhost would make every client the proxy.
	for k, want := range map[string]string{"sidecar.istio.io/inject": "false", "linkerd.io/inject": "disabled"} {
		if got := d.Spec.Template.Annotations[k]; got != want {
			t.Errorf("pod annotation %s = %q, want %q: no mesh sidecar in the trusted pod", k, got, want)
		}
	}

	// (1) serve is reachable only through the proxy: it listens on
	// loopback, the proxy forwards there, and no Service exposes any port
	// but the proxy's.
	if got := cmd.Flags().Lookup("listen").Value.String(); !strings.HasPrefix(got, "127.0.0.1:") {
		t.Errorf("serve --listen = %q, want 127.0.0.1:<port>: reachable only through the sidecar", got)
	} else if want := "--upstream=http://" + got + "/"; !slices.Contains(proxy.Args, want) {
		t.Errorf("oauth2-proxy args lack %s", want)
	}
	if len(server.Ports) != 0 {
		t.Errorf("server container declares ports %v: serve listens on loopback only", server.Ports)
	}
	proxyPorts := map[string]bool{}
	for _, p := range proxy.Ports {
		proxyPorts[p.Name], proxyPorts[fmt.Sprint(p.ContainerPort)] = true, true
	}
	if len(services) == 0 {
		t.Error("no Service")
	}
	for _, s := range services {
		for _, p := range s.Spec.Ports {
			if !proxyPorts[p.TargetPort.String()] {
				t.Errorf("Service %s targets port %s, which is not the proxy's: only the proxy may be reachable", s.Name, p.TargetPort.String())
			}
		}
	}

	// (2) the proxy strips what a client sends in the header it sets.
	// oauth2-proxy v7.15.5 removes a client's copies of the headers it
	// injects (X-Forwarded-Groups with --pass-user-headers), whatever
	// their case or underscores, on every route when
	// --skip-auth-strip-headers is true, which this pins (it is the
	// default, so a later default change cannot undo it), on the version
	// that behaviour was verified against.
	if !strings.HasSuffix(proxy.Image, ":"+oauth2ProxyVersion) {
		t.Errorf("oauth2-proxy image %s, want the pinned, verified %s", proxy.Image, oauth2ProxyVersion)
	}
	for _, want := range []string{"--pass-user-headers=true", "--skip-auth-strip-headers=true", "--pass-basic-auth=false"} {
		if !slices.Contains(proxy.Args, want) {
			t.Errorf("oauth2-proxy args lack %s", want)
		}
	}
	// Machines go through the proxy on routes it does not authenticate,
	// where it strips the header too, so they read as their tokens.
	skips := []string{"--skip-auth-route=POST=^/api/v1/snapshots$", "--skip-auth-route=POST=^/api/v1/gate$",
		"--skip-auth-route=GET=^/metrics$", "--skip-auth-route=GET=^/readyz$", "--skip-auth-route=GET=^/healthz$"}
	for _, a := range proxy.Args {
		// The server reads read tokens from Authorization; a proxy that
		// overwrites it, keeps a client's copy of the groups header, or
		// skips authentication on a dashboard read, defeats it.
		if strings.HasPrefix(a, "--pass-authorization-header") || strings.HasPrefix(a, "--basic-auth-password") || strings.HasPrefix(a, "--set-authorization-header") ||
			strings.HasPrefix(a, "--skip-auth-regex") || strings.HasPrefix(a, "--skip-auth-preflight") || strings.HasPrefix(a, "--alpha-config") ||
			strings.HasPrefix(a, "--skip-auth-strip-headers") && a != "--skip-auth-strip-headers=true" ||
			(strings.HasPrefix(a, "--skip-auth-route") && !slices.Contains(skips, a)) {
			t.Errorf("oauth2-proxy arg %s", a)
		}
	}
	// The routes match the requests machines make, query string and all,
	// and no read a person makes through the dashboard.
	for _, r := range []struct {
		method, uri string
		skip        bool
	}{
		{"POST", "/api/v1/gate?target=1.35&cluster=x&fail-on=warning&format=sarif", true},
		{"POST", "/api/v1/gate?target=1.35", true},
		{"POST", "/api/v1/snapshots", true},
		{"GET", "/metrics", true},
		{"GET", "/readyz", true},
		{"GET", "/healthz", true},
		{"GET", "/", false},
		{"GET", "/api/v1/clusters", false},
		{"GET", "/api/v1/clusters/1/report?target=1.35", false},
		{"GET", "/api/v1/fleet/teams?target=1.35", false},
		{"GET", "/metrics/x", false},
		{"GET", "/api/v1/gate?target=1.35", false},
		{"POST", "/api/v1/gate/x?target=1.35", false},
	} {
		if got := skipsAuth(t, skips, r.method, r.uri); got != r.skip {
			t.Errorf("%s %s: the proxy skips authentication = %v, want %v", r.method, r.uri, got, r.skip)
		}
	}
}

// oauth2ProxyVersion is the oauth2-proxy release the example pins and
// whose behaviour the docs and skipsAuth describe.
const oauth2ProxyVersion = "v7.15.5"

// skipsAuth reports whether oauth2-proxy v7.15.5 skips authentication
// for a request with routes (--skip-auth-route=METHOD=regex), as its
// isAllowedRoute decides (oauthproxy.go): the method must be the
// route's, and the regex must match the decoded path of the request
// target, without its query (requestutil.GetRequestPath). Before
// v7.11.0 (CVE-2025-54576) the regex was matched against the request
// URI, query included, so ^/api/v1/gate$ never matched a gate call.
func skipsAuth(t *testing.T, routes []string, method, uri string) bool {
	t.Helper()
	u, err := url.ParseRequestURI(uri)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range routes {
		m, re, ok := strings.Cut(strings.TrimPrefix(r, "--skip-auth-route="), "=")
		if !ok {
			t.Fatalf("route %s is not METHOD=regex", r)
		}
		if m == method && regexp.MustCompile(re).MatchString(u.Path) {
			return true
		}
	}
	return false
}

// serve checks the Host of every request in trusted-header mode (#240).
// oauth2-proxy v7.15.5 forwards the client's Host unless
// --pass-host-header=false, which makes it send the upstream's,
// 127.0.0.1:<port>, a loopback address serve always answers for: the
// browser's Ingress host, the Service name agents use and the pod IP the
// kubelet probes all reach serve as that, and no --allowed-host is needed.
func TestOAuth2ProxyExampleSendsTheUpstreamHost(t *testing.T) {
	deploys, _ := decodeAll(t, "oauth2-proxy/upgradescope-oauth2-proxy.yaml")
	proxy := container(t, deploys[0], "oauth2-proxy")
	if !slices.Contains(proxy.Args, "--pass-host-header=false") {
		t.Errorf("oauth2-proxy args lack --pass-host-header=false: serve would get the client's Host and answer it 421")
	}
	for _, a := range proxy.Args {
		if v, ok := strings.CutPrefix(a, "--upstream="); ok {
			u, err := url.Parse(v)
			if err != nil || u.Hostname() != "127.0.0.1" {
				t.Errorf("--upstream %q: want http://127.0.0.1:<port>/, a Host serve answers for", v)
			}
		}
	}
}
