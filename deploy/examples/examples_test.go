// Package examples holds Go tests for the example manifests under
// deploy/examples. The package has no non-test code.
package examples

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"os"
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

// The oauth2-proxy example (docs/operations/auth.md): serve trusts the
// team header from the sidecar on 127.0.0.1 only, its arguments parse as
// serve's, the proxy sets the groups header and leaves Authorization alone,
// and people reach the proxy, not serve.
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

	for _, want := range []string{"--upstream=http://127.0.0.1:8080/", "--pass-user-headers=true", "--skip-auth-route=POST=^/api/v1/snapshots$"} {
		if !slices.Contains(proxy.Args, want) {
			t.Errorf("oauth2-proxy args lack %s", want)
		}
	}
	for _, a := range proxy.Args {
		// The server reads read tokens from Authorization; a proxy that
		// overwrites it, or skips authentication on a read, defeats it.
		if strings.HasPrefix(a, "--pass-authorization-header") || strings.HasPrefix(a, "--set-authorization-header") ||
			strings.HasPrefix(a, "--skip-auth-regex") || strings.HasPrefix(a, "--skip-auth-preflight") ||
			(strings.HasPrefix(a, "--skip-auth-route") && a != "--skip-auth-route=POST=^/api/v1/snapshots$") {
			t.Errorf("oauth2-proxy arg %s", a)
		}
	}

	// People reach the proxy; serve's own port is a Service of its own.
	ports := map[string]string{}
	for _, s := range services {
		for _, p := range s.Spec.Ports {
			ports[s.Name] = p.TargetPort.String()
		}
	}
	if ports["upgradescope"] != "proxy" || ports["upgradescope-api"] != "api" {
		t.Errorf("Service target ports = %v, want upgradescope→proxy and upgradescope-api→api", ports)
	}
}
