package collect

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/abd-ulbasit/upgradescope/internal/inventory"
)

// A repository URL can carry a credential (https://user:token@host, a
// token in the query string). It must not reach the inventory, which is
// pushed to the server, stored, and printed by the CLI.
func TestRedactRepoURL(t *testing.T) {
	for _, tc := range []struct{ name, in, want string }{
		{"plain https", "https://kubernetes.github.io/ingress-nginx", "https://kubernetes.github.io/ingress-nginx"},
		{"plain oci keeps the trailing slash", "oci://ghcr.io/acme/charts/ingress-nginx/", "oci://ghcr.io/acme/charts/ingress-nginx/"},
		{"https with user and password", "https://user:s3cret@charts.example.com/repo", "https://charts.example.com/repo"},
		{"https with a token as the user", "https://ghp_s3cret@charts.example.com/repo", "https://charts.example.com/repo"},
		{"https with a token in the query", "https://charts.example.com/repo?token=s3cret&x=1", "https://charts.example.com/repo"},
		{"https with a fragment", "https://charts.example.com/repo#s3cret", "https://charts.example.com/repo"},
		{"all of it, with a port", "https://u:s3cret@charts.example.com:8443/repo?token=s3cret#s3cret", "https://charts.example.com:8443/repo"},
		{"oci with userinfo", "oci://user:s3cret@ghcr.io/acme/charts/ingress-nginx", "oci://ghcr.io/acme/charts/ingress-nginx"},
		{"scp-like git URL", "git@github.com:acme/charts.git", "github.com:acme/charts.git"},
		{"scp-like with a token", "user:s3cret@github.com:acme/charts.git", "github.com:acme/charts.git"},
		{"ssh with userinfo", "ssh://git:s3cret@github.com/acme/charts.git", "ssh://github.com/acme/charts.git"},
		{"unparseable with userinfo", "https://user:s3cret@host/%zz", "https://host/%zz"},
		{"unparseable password with a slash", "https://user:pa/ss@host:port/x", "https://host:port/x"},
		{"unparseable with a query", "https://host/%zz?token=s3cret", "https://host/%zz"},
		{"an invalid string is kept without userinfo or query", "not a url?token=s3cret", "not a url"},
		{"an invalid string with userinfo", "%%%://u:s3cret@x y", "x y"},
		{"empty", "", ""},
		{"empty host is dropped", "https://u:s3cret@/x", ""},
		{"control characters are dropped", "https://host/a\nb\tc", "https://host/abc"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := redactRepoURL(tc.in)
			if got != tc.want {
				t.Errorf("redactRepoURL(%q) = %q, want %q", tc.in, got, tc.want)
			}
			if strings.Contains(got, "s3cret") {
				t.Errorf("redactRepoURL(%q) = %q still holds the secret", tc.in, got)
			}
		})
	}
}

// Go's URL parser ends the authority at the first "/", "?" or "#", so a
// userinfo holding one of them parses "successfully" with the credential
// as the host, path, query or fragment. None of it may survive.
func TestRedactRepoURLCredentialWithDelimiter(t *testing.T) {
	for _, tc := range []struct{ name, in, want string }{
		{"token with a slash as the user", "https://AbC1/s3cret+gh@charts.example.com/repo", "https://charts.example.com/repo"},
		{"password with a slash after digits", "https://user:123/s3cret@host/x", "https://host/x"},
		{"oci password with a slash after digits", "oci://x:12/s3cret@ghcr.io/acme/chart", "oci://ghcr.io/acme/chart"},
		{"password with a hash", "https://user:1234#s3cret@host/x", "https://host/x"},
		{"password with a question mark", "https://user:443?s3cret@host/x", "https://host/x"},
		{"several at signs", "https://a@b:s3cret@host/x", "https://host/x"},
		{"scheme is part of the credential", "user:s3cret://x@host/repo", "host/repo"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := redactRepoURL(tc.in)
			if got != tc.want {
				t.Errorf("redactRepoURL(%q) = %q, want %q", tc.in, got, tc.want)
			}
			if strings.Contains(got, "s3cret") {
				t.Errorf("redactRepoURL(%q) = %q still holds the secret", tc.in, got)
			}
		})
	}
}

// End to end: the credentials in an Application's repoURL and an
// OCIRepository's url are in neither the inventory nor its JSON, and the
// chart name read from the OCI URL is not polluted by the query.
func TestGitOpsRepoCredentialsNeverReachTheInventory(t *testing.T) {
	app := argoApp("ingress-nginx", map[string]any{
		"destination": inCluster("ingress-nginx"),
		"source": map[string]any{
			"repoURL": "https://user:s3cret@kubernetes.github.io/ingress-nginx?token=s3cret", "chart": "ingress-nginx", "targetRevision": "4.11.3",
		},
	})
	hr := fluxRelease("v2", "flux-system", "cert-manager", map[string]any{
		"chartRef": map[string]any{"kind": "OCIRepository", "name": "cm"},
	})
	repo := cr("source.toolkit.fluxcd.io/v1", "OCIRepository", "flux-system", "cm", map[string]any{
		"url": "oci://x:12/s3cret@ghcr.io/acme/charts/cert-manager?token=s3cret", "ref": map[string]any{"tag": "1.15.3"},
	})
	f := newGitOpsFixture(t, []*metav1.APIResourceList{
		argoServed(),
		resources("helm.toolkit.fluxcd.io/v2", fluxHelmReleases),
		resources("source.toolkit.fluxcd.io/v1", fluxOCIRepos),
	}, []runtime.Object{app, hr, repo})
	inv := Collect(context.Background(), f.clients(), loadKB(t), Options{})
	want := map[string]inventory.GitOpsChart{
		"ingress-nginx": {Tool: "argocd", Name: "ingress-nginx", Namespace: "argocd", Target: "ingress-nginx", Chart: "ingress-nginx", Version: "4.11.3", Repo: "https://kubernetes.github.io/ingress-nginx"},
		"cert-manager":  {Tool: "flux", Name: "cert-manager", Namespace: "flux-system", Target: "flux-system", Chart: "cert-manager", Version: "1.15.3", Repo: "oci://ghcr.io/acme/charts/cert-manager"},
	}
	if len(inv.GitOpsCharts) != len(want) {
		t.Fatalf("gitops charts = %#v, want %d", inv.GitOpsCharts, len(want))
	}
	for _, c := range inv.GitOpsCharts {
		if !reflect.DeepEqual(c, want[c.Name]) {
			t.Errorf("chart = %#v\nwant   %#v", c, want[c.Name])
		}
	}
	out, err := json.Marshal(inv)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), "s3cret") {
		t.Errorf("the inventory JSON holds a repository credential: %s", out)
	}
}
