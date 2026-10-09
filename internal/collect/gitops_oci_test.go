package collect

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	clienttesting "k8s.io/client-go/testing"

	"github.com/abd-ulbasit/upgradescope/internal/inventory"
)

// #267: Argo CD reads a Helm chart from an OCI registry as an Application
// source of repoURL oci://..., path ".", and no chart field. The chart is
// the repository's last path element and its version the targetRevision;
// it renders with helm template like any Argo CD chart, so it is the same
// gap.

const ociIngressNginx = "oci://ghcr.io/acme/charts/ingress-nginx"

func helmRelease(t *testing.T) runtime.Object {
	t.Helper()
	return helmSecret(t, helmRev{ns: "kube-system", release: "dns", rev: 1, status: "deployed", chart: "coredns", chartVersion: "1.0.0"})
}

func TestGitOpsArgoNativeOCIChartSingleSource(t *testing.T) {
	app := argoApp("ingress-nginx", map[string]any{
		"destination": inCluster("ingress-nginx"),
		"source":      map[string]any{"repoURL": ociIngressNginx, "path": ".", "targetRevision": "4.11.3"},
	})
	f := newGitOpsFixture(t, []*metav1.APIResourceList{argoServed()}, []runtime.Object{app}, helmRelease(t))
	inv, err := f.helmStep()
	pe := partial(t, err)
	want := []inventory.GitOpsChart{{
		Tool: inventory.GitOpsArgoCD, Name: "ingress-nginx", Namespace: "argocd", Target: "ingress-nginx",
		Chart: "ingress-nginx", Version: "4.11.3", Repo: ociIngressNginx,
	}}
	if !reflect.DeepEqual(inv.GitOpsCharts, want) {
		t.Errorf("gitops charts = %#v\nwant            %#v", inv.GitOpsCharts, want)
	}
	// With another Helm release present, the chart is still a gap: Argo CD
	// leaves no release for it.
	if !pe.incomplete || !reflect.DeepEqual(pe.skipped, []string{"argocd"}) || !strings.Contains(pe.Error(), "1 Argo CD chart(s) read from Applications") {
		t.Errorf("partial = %+v, want a gap skipping argocd for 1 chart read from Applications", pe)
	}
}

func TestGitOpsArgoNativeOCIChartMultiSource(t *testing.T) {
	app := argoApp("platform", map[string]any{
		"destination": inCluster("platform"),
		"sources": []any{
			map[string]any{"repoURL": "https://github.com/acme/config.git", "targetRevision": "main", "ref": "values"},
			map[string]any{"repoURL": ociIngressNginx, "path": ".", "targetRevision": "4.*"},
			map[string]any{"repoURL": "registry-1.docker.io/bitnamicharts", "chart": "redis", "targetRevision": "20.1.0"},
			map[string]any{"repoURL": "oci://registry-1.docker.io/bitnamicharts/nginx", "targetRevision": "15.9.0"}, // path omitted
		},
	})
	f := newGitOpsFixture(t, []*metav1.APIResourceList{argoServed()}, []runtime.Object{app}, helmRelease(t))
	inv, err := f.helmStep()
	pe := partial(t, err)
	want := []inventory.GitOpsChart{
		{Tool: "argocd", Name: "platform", Namespace: "argocd", Target: "platform", Chart: "ingress-nginx", Version: "4.*", Repo: ociIngressNginx},
		{Tool: "argocd", Name: "platform", Namespace: "argocd", Target: "platform", Chart: "redis", Version: "20.1.0", Repo: "registry-1.docker.io/bitnamicharts"},
		{Tool: "argocd", Name: "platform", Namespace: "argocd", Target: "platform", Chart: "nginx", Version: "15.9.0", Repo: "oci://registry-1.docker.io/bitnamicharts/nginx"},
	}
	if !reflect.DeepEqual(inv.GitOpsCharts, want) {
		t.Errorf("gitops charts = %#v\nwant            %#v", inv.GitOpsCharts, want)
	}
	if !strings.Contains(pe.Error(), "3 Argo CD chart(s) read from Applications") {
		t.Errorf("reason %q: want all three charts counted", pe.Error())
	}
}

// Credentials in the repoURL are redacted before the chart name is taken
// from it, and never reach the inventory: a query string would otherwise
// be the last path element.
func TestGitOpsArgoNativeOCIChartRedactsCredentials(t *testing.T) {
	for _, tc := range []struct{ name, repoURL, wantRepo, wantChart string }{
		{"userinfo", "oci://ci:s3cr3t-token@ghcr.io/acme/charts/ingress-nginx", ociIngressNginx, "ingress-nginx"},
		{"query string", "oci://ghcr.io/acme/charts/ingress-nginx?token=s3cr3t-token", ociIngressNginx, "ingress-nginx"},
		{"query string with a slash", "oci://ghcr.io/acme/charts/ingress-nginx?token=s3cr3t/token", ociIngressNginx, "ingress-nginx"},
		{"fragment", "oci://ghcr.io/acme/charts/ingress-nginx#s3cr3t-token", ociIngressNginx, "ingress-nginx"},
		{"trailing slash", ociIngressNginx + "/", ociIngressNginx + "/", "ingress-nginx"},
		{"digest-pinned", ociIngressNginx + "@sha256:" + strings.Repeat("ab", 32), ociIngressNginx + "@sha256:" + strings.Repeat("ab", 32), "ingress-nginx"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			app := argoApp("ingress-nginx", map[string]any{
				"destination": inCluster("ingress-nginx"),
				"source":      map[string]any{"repoURL": tc.repoURL, "path": ".", "targetRevision": "4.11.3"},
			})
			f := newGitOpsFixture(t, []*metav1.APIResourceList{argoServed()}, []runtime.Object{app}, helmRelease(t))
			inv, _ := f.helmStep()
			if len(inv.GitOpsCharts) != 1 {
				t.Fatalf("gitops charts = %#v, want one", inv.GitOpsCharts)
			}
			c := inv.GitOpsCharts[0]
			if c.Chart != tc.wantChart || c.Repo != tc.wantRepo {
				t.Errorf("chart %q repo %q, want %q %q", c.Chart, c.Repo, tc.wantChart, tc.wantRepo)
			}
			if raw := fmt.Sprintf("%+v", inv.GitOpsCharts); strings.Contains(raw, "s3cr3t") {
				t.Errorf("credential in the inventory: %s", raw)
			}
		})
	}
}

// A source that is not a native OCI chart reads as before: no chart field
// and a repoURL that is not oci:// (a Git path), or an oci:// URL with no
// repository path below the registry, names nothing.
func TestGitOpsArgoOCISourceThatNamesNoChartIsNotRead(t *testing.T) {
	for _, repoURL := range []string{
		"https://github.com/acme/config.git",
		"oci://ghcr.io",
		"oci://ghcr.io/",
		"oci://",
		"",
		"oci://u:p@",
		"oci://ghcr.io/" + strings.Repeat("a", maxChartNameBytes+1),
	} {
		app := argoApp("x", map[string]any{
			"destination": inCluster("x"),
			"source":      map[string]any{"repoURL": repoURL, "path": ".", "targetRevision": "1.0.0"},
		})
		f := newGitOpsFixture(t, []*metav1.APIResourceList{argoServed()}, []runtime.Object{app}, helmRelease(t))
		inv, err := f.helmStep()
		if pe := partial(t, err); len(inv.GitOpsCharts) != 0 || pe.incomplete {
			t.Errorf("repoURL %q: charts %#v, partial %+v, want nothing read and no gap", repoURL, inv.GitOpsCharts, pe)
		}
	}
}

// A native OCI chart is found as an add-on, like a chart from any other
// Argo CD source, and an OCI source for another cluster is not counted.
func TestGitOpsArgoNativeOCIChartFeedsAddOnDetection(t *testing.T) {
	app := argoApp("ingress-nginx", map[string]any{
		"destination": inCluster("ingress-nginx"),
		"source":      map[string]any{"repoURL": ociIngressNginx, "path": ".", "targetRevision": "4.11.3"},
	})
	spoke := argoApp("spoke", map[string]any{
		"destination": map[string]any{"server": "https://spoke.example.com", "namespace": "x"},
		"source":      map[string]any{"repoURL": ociIngressNginx, "path": ".", "targetRevision": "4.0.0"},
	})
	f := newGitOpsFixture(t, []*metav1.APIResourceList{argoServed()}, []runtime.Object{app, spoke})
	inv := Collect(context.Background(), f.clients(), loadKB(t), Options{})
	var found []inventory.AddOnInstance
	for _, a := range inv.AddOns {
		if a.ID == "ingress-nginx" {
			found = append(found, a)
		}
	}
	if len(found) != 1 || found[0].ChartVersion != "4.11.3" || !reflect.DeepEqual(found[0].Namespaces, []string{"ingress-nginx"}) {
		t.Fatalf("add-ons = %+v, want one ingress-nginx at chart version 4.11.3 in ingress-nginx", found)
	}
}

// Without any Helm release, the Argo CD presence gap says it already (the
// native OCI chart is counted but not reported a second time).
func TestGitOpsArgoNativeOCIChartWithoutReleasesIsOneGap(t *testing.T) {
	app := argoApp("ingress-nginx", map[string]any{
		"destination": inCluster("ingress-nginx"),
		"source":      map[string]any{"repoURL": ociIngressNginx, "path": ".", "targetRevision": "4.11.3"},
	})
	f := newGitOpsFixture(t, []*metav1.APIResourceList{argoServed()}, []runtime.Object{app})
	_, err := f.helmStep()
	pe := partial(t, err)
	if !pe.incomplete || !reflect.DeepEqual(pe.skipped, []string{"argocd"}) || strings.Count(pe.Error(), "no Helm releases read, but Argo CD is present") != 1 || strings.Contains(pe.Error(), "read from Applications leave no Helm release") {
		t.Errorf("partial = %+v, want one gap skipping argocd", pe)
	}
}

// #267: Flux applies an OCIRepository's ref by digest, else semver, else
// tag. The version recorded follows it: a digest is recorded as the digest
// (no chart version, exactChartVersion refuses it), a semver as the range it
// is, a tag alone as the exact version.
func TestGitOpsFluxOCIRepositoryVersionFollowsFluxPrecedence(t *testing.T) {
	digest := "sha256:" + strings.Repeat("ab", 32)
	for _, tc := range []struct {
		name             string
		ref              map[string]any
		wantVersion      string
		wantChartVersion string // the add-on's, with no release Secret to supply it
	}{
		{"tag alone is the exact version", map[string]any{"tag": "4.11.3"}, "4.11.3", "4.11.3"},
		{"tag with a v is the exact version", map[string]any{"tag": "v4.11.3"}, "v4.11.3", "4.11.3"},
		{"semver beats tag, as a range", map[string]any{"tag": "4.0.0", "semver": ">=4.11.0 <5.0.0"}, ">=4.11.0 <5.0.0", ""},
		{"a semver of one version is exact", map[string]any{"semver": "4.11.3"}, "4.11.3", "4.11.3"},
		{"digest beats tag", map[string]any{"tag": "4.0.0", "digest": digest}, digest, ""},
		{"digest beats semver", map[string]any{"semver": ">=4.11.0 <5.0.0", "digest": digest}, digest, ""},
		{"digest beats both", map[string]any{"tag": "4.0.0", "semver": "4.0.0", "digest": digest}, digest, ""},
		{"no ref at all follows latest: no version", nil, "", ""},
		{"an empty ref: no version", map[string]any{}, "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			spec := map[string]any{"url": ociIngressNginx}
			if tc.ref != nil {
				spec["ref"] = tc.ref
			}
			hr := fluxRelease("v2", "flux-system", "ingress-nginx", map[string]any{
				"targetNamespace": "ingress-nginx",
				"chartRef":        map[string]any{"kind": "OCIRepository", "name": "c"},
			})
			repo := cr("source.toolkit.fluxcd.io/v1", "OCIRepository", "flux-system", "c", spec)
			f := newGitOpsFixture(t, []*metav1.APIResourceList{
				resources("helm.toolkit.fluxcd.io/v2", fluxHelmReleases),
				resources("source.toolkit.fluxcd.io/v1", fluxOCIRepos),
			}, []runtime.Object{hr, repo})
			inv := Collect(context.Background(), f.clients(), loadKB(t), Options{})
			if len(inv.GitOpsCharts) != 1 || inv.GitOpsCharts[0].Version != tc.wantVersion {
				t.Fatalf("gitops charts = %#v, want one at version %q", inv.GitOpsCharts, tc.wantVersion)
			}
			var found []inventory.AddOnInstance
			for _, a := range inv.AddOns {
				if a.ID == "ingress-nginx" {
					found = append(found, a)
				}
			}
			if len(found) != 1 || found[0].ChartVersion != tc.wantChartVersion {
				t.Fatalf("add-ons = %+v, want one ingress-nginx at chart version %q", found, tc.wantChartVersion)
			}
		})
	}
}

// A digest-pinned OCIRepository URL (oci://host/chart@sha256:...) names the
// chart without the digest.
func TestGitOpsOCIChartNameDropsADigest(t *testing.T) {
	for url, want := range map[string]string{
		ociIngressNginx:       "ingress-nginx",
		ociIngressNginx + "/": "ingress-nginx",
		ociIngressNginx + "@sha256:" + strings.Repeat("0", 64): "ingress-nginx",
		"oci://ghcr.io/ingress-nginx":                          "ingress-nginx",
	} {
		if got, ok := ociChartName(url); !ok || got != want {
			t.Errorf("ociChartName(%q) = %q, %v; want %q", url, got, ok, want)
		}
	}
	for _, url := range []string{"", "oci://", "oci://ghcr.io", "oci://ghcr.io/", "https://ghcr.io/acme/chart", "ghcr.io/acme/chart"} {
		if got, ok := ociChartName(url); ok {
			t.Errorf("ociChartName(%q) = %q, true; want no chart", url, got)
		}
	}
}

// #271 review: GitOpsCache remembers a refusal (403) and nothing else. A
// list that failed another way (a timeout, a 5xx, a rate limit) is asked for
// again on the next call, and does not clear an earlier refusal either.
func TestGitOpsCacheNeverRemembersANon403Error(t *testing.T) {
	list := gitopsList{gvr: schema.GroupVersionResource{Group: "source.toolkit.fluxcd.io", Version: "v1", Resource: "ocirepositories"}, namespace: "team-a"}
	for _, tc := range []struct {
		name string
		err  error
	}{
		{"timeout", context.DeadlineExceeded},
		{"canceled", context.Canceled},
		{"500", apierrors.NewInternalError(errors.New("boom"))},
		{"503", apierrors.NewServiceUnavailable("overloaded")},
		{"429", apierrors.NewTooManyRequests("slow down", 1)},
		{"server timeout", apierrors.NewServerTimeout(list.gvr.GroupResource(), "list", 1)},
		{"not found", apierrors.NewNotFound(list.gvr.GroupResource(), "x")},
		{"unauthorized", apierrors.NewUnauthorized("expired")},
		{"transport", errors.New("connection refused")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cache := NewGitOpsCache()
			cache.record(list, tc.err)
			if cache.wasRefused(list) || len(cache.refused) != 0 {
				t.Errorf("a %s was remembered as a refusal: %v", tc.name, cache.refused)
			}
			// An earlier refusal is not cleared by it either: only success
			// forgets one.
			cache.record(list, apierrors.NewForbidden(list.gvr.GroupResource(), "", errors.New("RBAC")))
			cache.record(list, tc.err)
			if !cache.wasRefused(list) {
				t.Errorf("a %s forgot the refusal before it", tc.name)
			}
			cache.record(list, nil)
			if cache.wasRefused(list) {
				t.Error("a successful list did not forget the refusal")
			}
		})
	}
}

// ...and through the collector: an OCIRepository list that times out or
// answers 5xx is asked again on the very next call, however many calls.
func TestGitOpsListFailingOtherwiseIsAskedForEveryCall(t *testing.T) {
	for name, fail := range map[string]error{
		"500": apierrors.NewInternalError(errors.New("boom")),
		"503": apierrors.NewServiceUnavailable("overloaded"),
		"429": apierrors.NewTooManyRequests("slow down", 1),
	} {
		t.Run(name, func(t *testing.T) {
			f := ociFleet(t, 3, "team-a")
			f.dyn.PrependReactor("list", "ocirepositories", func(clienttesting.Action) (bool, runtime.Object, error) {
				return true, nil, fail
			})
			now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
			cache := NewGitOpsCache()
			cache.now = func() time.Time { return now }
			for call := 1; call <= 3; call++ {
				f.dyn.ClearActions()
				var inv inventory.Inventory
				pe := partial(t, collectHelmStep(context.Background(), f.clients(), nil, nil, cache, &inv))
				if lists, gets := ociReads(f); len(lists) != 1 || len(gets) != 0 {
					t.Errorf("call %d: lists %q and %d GETs; want the list asked for again, and no GET", call, lists, len(gets))
				}
				if !pe.incomplete || !strings.Contains(pe.Error(), "not resolved") {
					t.Errorf("call %d: partial %+v, want the chartRefs unresolved", call, pe)
				}
				if len(cache.refused) != 0 {
					t.Errorf("call %d: cache holds %v", call, cache.refused)
				}
				now = now.Add(time.Minute)
			}
		})
	}
}

var _ = metav1.NamespaceAll

// A multi-source Application's `ref` source supplies values files to a
// chart of another source and is no chart itself: an oci:// one (values
// kept in an OCI artifact) is not read as a chart named after the
// repository's last path element. A source that sets both ref and chart
// is a chart.
func TestGitOpsArgoRefSourceIsNotAChart(t *testing.T) {
	app := argoApp("platform", map[string]any{
		"destination": inCluster("platform"),
		"sources": []any{
			map[string]any{"repoURL": "oci://ghcr.io/acme/config/platform-values", "targetRevision": "1.0.0", "ref": "values"},
			map[string]any{"repoURL": ociIngressNginx, "path": ".", "targetRevision": "4.11.3"},
			map[string]any{"repoURL": "registry-1.docker.io/bitnamicharts", "chart": "redis", "targetRevision": "20.1.0", "ref": "both"},
		},
	})
	f := newGitOpsFixture(t, []*metav1.APIResourceList{argoServed()}, []runtime.Object{app}, helmRelease(t))
	inv, _ := f.helmStep()
	var names []string
	for _, c := range inv.GitOpsCharts {
		names = append(names, c.Chart)
	}
	if want := []string{"ingress-nginx", "redis"}; !reflect.DeepEqual(names, want) {
		t.Errorf("charts read = %v, want %v (the ref source supplies values, not a chart)", names, want)
	}
}

// An OCI source whose path is not "." is read as the chart named by the
// repository's last element: Argo CD would read the chart from that path
// inside the artifact, so this can name a chart that is not deployed. That
// is over-reporting (an add-on that is not there, which the user can
// ignore), documented in the GitOps guide; the test pins it so a change is
// deliberate.
func TestGitOpsArgoOCISourceWithAPathIsStillReadAsTheRepositoryChart(t *testing.T) {
	app := argoApp("ingress-nginx", map[string]any{
		"destination": inCluster("ingress-nginx"),
		"source":      map[string]any{"repoURL": ociIngressNginx, "path": "charts/sub", "targetRevision": "4.11.3"},
	})
	f := newGitOpsFixture(t, []*metav1.APIResourceList{argoServed()}, []runtime.Object{app}, helmRelease(t))
	inv, _ := f.helmStep()
	if len(inv.GitOpsCharts) != 1 || inv.GitOpsCharts[0].Chart != "ingress-nginx" {
		t.Errorf("gitops charts = %#v, want the repository's chart", inv.GitOpsCharts)
	}
}
