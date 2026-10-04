package main

import (
	"context"
	"fmt"
	"io"
	"math/rand/v2"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
)

// The opt-in GitOps fill (#233): Argo CD Applications and Flux
// HelmReleases (with the OCIRepositories their chartRefs name), the objects
// the agent's GitOps collector (#218, internal/collect/gitops.go) lists when
// rbac.gitops.* is on. The CRDs are not created here: hack/bench/agent.sh
// installs the upstream ones (pinned by version and sha256) before it seeds.
//
// What the objects are, so that a number measured with them is not read as
// more than it is:
//   - Every object is a valid instance of the upstream CRD's schema at the
//     versions agent.sh pins, and every one is read by the collector: all
//     deploy to this cluster, and every chart resolves. A cluster that does
//     not look like this (Applications for other clusters, chartRefs to
//     other kinds) makes the same requests and reads fewer charts.
//   - The status is generated, not copied from a running controller. An
//     Argo CD Application's status lists every object it manages and a real
//     one is often larger than this; a HelmRelease's carries its release
//     history. The sizes below are plausible, not typical: the response
//     bytes of a list scale with them (the per-object size is in the
//     seeder's summary, so a measured number can be rescaled).

var (
	argoApplicationGVR = schema.GroupVersionResource{Group: "argoproj.io", Version: "v1alpha1", Resource: "applications"}
	fluxHelmReleaseGVR = schema.GroupVersionResource{Group: "helm.toolkit.fluxcd.io", Version: "v2", Resource: "helmreleases"}
	fluxOCIRepoGVR     = schema.GroupVersionResource{Group: "source.toolkit.fluxcd.io", Version: "v1", Resource: "ocirepositories"}
)

// gitopsChart is a chart a GitOps resource deploys.
type gitopsChart struct {
	name, repo, version string
}

// addOnCharts are charts the knowledge base has add-on entries for, on
// every addOnChartEvery-th resource: the add-on matcher has work to do.
var addOnCharts = []gitopsChart{
	{"ingress-nginx", "https://kubernetes.github.io/ingress-nginx", "4.11.3"},
	{"cert-manager", "https://charts.jetstack.io", "v1.16.1"},
	{"grafana", "https://grafana.github.io/helm-charts", "8.5.12"},
	{"kube-prometheus-stack", "https://prometheus-community.github.io/helm-charts", "65.1.1"},
	{"external-dns", "https://kubernetes-sigs.github.io/external-dns", "1.15.0"},
}

const addOnChartEvery = 10

// chartOf is the chart of resource i: an add-on chart for one in ten, else
// an in-house service chart from a private repository.
func chartOf(i int) gitopsChart {
	if i%addOnChartEvery == 0 {
		return addOnCharts[(i/addOnChartEvery)%len(addOnCharts)]
	}
	return gitopsChart{fmt.Sprintf("svc-%04d", i), "https://charts.example.internal", fmt.Sprintf("%d.%d.%d", 1+i%4, i%17, i%9)}
}

func argoAppName(i int) string     { return fmt.Sprintf("app-%04d", i) }
func fluxReleaseName(i int) string { return fmt.Sprintf("hr-%04d", i) }

// fluxUsesChartRef reports whether HelmRelease i names its chart through a
// chartRef to an OCIRepository (Flux 2.3 and later), rather than a chart
// source: every second one does.
func fluxUsesChartRef(i int) bool { return i%2 == 1 }

// argoMultiSource reports whether Application i has spec.sources (a chart
// and a Git repository holding its values) rather than spec.source: every
// second one does.
func argoMultiSource(i int) bool { return i%2 == 1 }

// gitopsCounts is what a config seeds, and what the collector should read
// back: every Application and every HelmRelease deploys one chart.
type gitopsCounts struct {
	ArgoApplications, ArgoMultiSource int
	FluxHelmReleases, FluxChartRefs   int // chartRefs are the HelmReleases that name an OCIRepository
	OCIRepositories                   int // one per chartRef
	ExpectedCharts                    int // what the collector reads: one per Application and per HelmRelease
}

func countGitOps(cfg config) gitopsCounts {
	c := gitopsCounts{ArgoApplications: cfg.ArgoApps, FluxHelmReleases: cfg.FluxHelmReleases}
	for i := range cfg.ArgoApps {
		if argoMultiSource(i) {
			c.ArgoMultiSource++
		}
	}
	for i := range cfg.FluxHelmReleases {
		if fluxUsesChartRef(i) {
			c.FluxChartRefs++
		}
	}
	c.OCIRepositories = c.FluxChartRefs
	c.ExpectedCharts = c.ArgoApplications + c.FluxHelmReleases
	return c
}

func gitopsNamespace(i int, cfg config) string { return nsName(i % max(cfg.Namespaces, 1)) }

func gitopsLabels() map[string]any { return map[string]any{benchLabel: "true"} }

// argoApplicationObject is Argo CD Application i: single-source (spec.source)
// or multi-source (spec.sources: the chart, and a Git repository that holds
// its values, which has no chart and so is not read), deploying to this
// cluster by its in-cluster URL. Its status lists the objects it manages,
// as a synced Application's does.
func argoApplicationObject(i int, cfg config) *unstructured.Unstructured {
	ch := chartOf(i)
	ns := gitopsNamespace(i, cfg)
	spec := map[string]any{
		"project":     "default",
		"destination": map[string]any{"server": "https://kubernetes.default.svc", "namespace": ns},
		"syncPolicy":  map[string]any{"automated": map[string]any{"prune": true, "selfHeal": true}},
	}
	chartSource := map[string]any{"repoURL": ch.repo, "chart": ch.name, "targetRevision": ch.version}
	if argoMultiSource(i) {
		chartSource["helm"] = map[string]any{"valueFiles": []any{"$values/envs/prod/" + ch.name + ".yaml"}}
		spec["sources"] = []any{
			chartSource,
			map[string]any{"repoURL": "https://git.example.internal/platform/deploy.git", "targetRevision": "main", "ref": "values"},
		}
	} else {
		spec["source"] = chartSource
	}
	rng := rand.New(rand.NewPCG(cfg.Seed, uint64(i)*7+1))
	managed := 8 + rng.IntN(13) // 8 to 20 objects
	resources := make([]any, 0, managed)
	for r := range managed {
		kind, group := argoManagedKinds[r%len(argoManagedKinds)][0], argoManagedKinds[r%len(argoManagedKinds)][1]
		resources = append(resources, map[string]any{
			"group": group, "kind": kind, "version": "v1", "namespace": ns,
			"name": fmt.Sprintf("%s-%d", argoAppName(i), r), "status": "Synced",
			"health": map[string]any{"status": "Healthy"},
		})
	}
	synced := time.Date(2026, 9, 20, 0, 0, 0, 0, time.UTC).Add(time.Duration(rng.IntN(14*24)) * time.Hour).Format(time.RFC3339)
	revision := ch.version
	status := map[string]any{
		"sync":         map[string]any{"status": "Synced", "revision": revision, "comparedTo": map[string]any{"destination": spec["destination"], "source": chartSource}},
		"health":       map[string]any{"status": "Healthy"},
		"resources":    resources,
		"reconciledAt": synced,
		"summary":      map[string]any{"images": []any{fmt.Sprintf("ghcr.io/example/%s:%s", ch.name, ch.version)}},
		"history": []any{map[string]any{
			"id": int64(0), "revision": revision, "deployedAt": synced, "deployStartedAt": synced,
			"source": chartSource,
		}},
		"operationState": map[string]any{
			"phase": "Succeeded", "message": "successfully synced (all tasks run)",
			"startedAt": synced, "finishedAt": synced,
			"operation": map[string]any{"sync": map[string]any{"revision": revision}},
			"syncResult": map[string]any{
				"revision": revision,
				"source":   chartSource,
				"resources": func() []any {
					out := make([]any, 0, managed)
					for r := range managed {
						k := argoManagedKinds[r%len(argoManagedKinds)]
						out = append(out, map[string]any{
							"group": k[1], "kind": k[0], "version": "v1", "namespace": ns,
							"name": fmt.Sprintf("%s-%d", argoAppName(i), r), "status": "Synced", "syncPhase": "Sync",
							"hookPhase": "Running", "message": k[0] + "/" + fmt.Sprintf("%s-%d", argoAppName(i), r) + " configured",
						})
					}
					return out
				}(),
			},
		},
	}
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "argoproj.io/v1alpha1", "kind": "Application",
		"metadata": map[string]any{"name": argoAppName(i), "namespace": ns, "labels": gitopsLabels()},
		"spec":     spec,
		"status":   status,
	}}
}

var argoManagedKinds = [][2]string{
	{"Deployment", "apps"}, {"Service", ""}, {"ConfigMap", ""}, {"ServiceAccount", ""},
	{"Ingress", "networking.k8s.io"}, {"HorizontalPodAutoscaler", "autoscaling"}, {"Secret", ""},
	{"NetworkPolicy", "networking.k8s.io"}, {"Role", "rbac.authorization.k8s.io"}, {"RoleBinding", "rbac.authorization.k8s.io"},
}

// fluxHelmRelease is Flux HelmRelease i and, when it names its chart through
// a chartRef, the OCIRepository that chartRef points at (one each, in the
// HelmRelease's namespace, as Flux's own guides make them). The status
// carries what helm-controller records after an install.
type fluxHelmRelease struct {
	release *unstructured.Unstructured
	status  map[string]any
	oci     *unstructured.Unstructured // nil unless the HelmRelease uses a chartRef
	ociStat map[string]any
}

func fluxHelmReleaseObjects(i int, cfg config) fluxHelmRelease {
	ch := chartOf(i + 1) // another phase than the Applications, so the same add-on is not always paired
	ns := gitopsNamespace(i, cfg)
	name := fluxReleaseName(i)
	spec := map[string]any{
		"interval":        "10m",
		"releaseName":     name,
		"targetNamespace": ns,
		"install":         map[string]any{"remediation": map[string]any{"retries": int64(3)}},
		"upgrade":         map[string]any{"remediation": map[string]any{"retries": int64(3)}},
		"values":          map[string]any{"replicaCount": int64(2)},
	}
	out := fluxHelmRelease{}
	rng := rand.New(rand.NewPCG(cfg.Seed, uint64(i)*11+3))
	when := time.Date(2026, 9, 20, 0, 0, 0, 0, time.UTC).Add(time.Duration(rng.IntN(14*24)) * time.Hour).Format(time.RFC3339)
	digest := fmt.Sprintf("sha256:%016x%016x%016x%016x", rng.Uint64(), rng.Uint64(), rng.Uint64(), rng.Uint64())
	if fluxUsesChartRef(i) {
		ociName := fmt.Sprintf("oci-%04d", i)
		spec["chartRef"] = map[string]any{"kind": "OCIRepository", "name": ociName}
		out.oci = &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "source.toolkit.fluxcd.io/v1", "kind": "OCIRepository",
			"metadata": map[string]any{"name": ociName, "namespace": ns, "labels": gitopsLabels()},
			"spec": map[string]any{
				"interval": "10m",
				"url":      "oci://ghcr.io/example/charts/" + ch.name,
				"ref":      map[string]any{"tag": ch.version},
			},
		}}
		out.ociStat = map[string]any{
			"conditions": []any{map[string]any{
				"type": "Ready", "status": "True", "reason": "Succeeded", "lastTransitionTime": when,
				"message": fmt.Sprintf("stored artifact for digest '%s@%s'", ch.version, digest), "observedGeneration": int64(1),
			}},
			"artifact": map[string]any{
				"digest": digest, "lastUpdateTime": when, "path": fmt.Sprintf("ocirepository/%s/%s/%s.tar.gz", ns, ociName, digest[7:]),
				"revision": ch.version + "@" + digest, "size": int64(4096 + rng.IntN(60000)),
				"url": fmt.Sprintf("http://source-controller.flux-system.svc.cluster.local./ocirepository/%s/%s/%s.tar.gz", ns, ociName, digest[7:]),
			},
			"observedGeneration": int64(1),
		}
	} else {
		spec["chart"] = map[string]any{"spec": map[string]any{
			"chart": ch.name, "version": ch.version,
			"sourceRef": map[string]any{"kind": "HelmRepository", "name": fmt.Sprintf("repo-%d", i%20), "namespace": "flux-system"},
			"interval":  "10m",
		}}
	}
	out.release = &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "helm.toolkit.fluxcd.io/v2", "kind": "HelmRelease",
		"metadata": map[string]any{"name": name, "namespace": ns, "labels": gitopsLabels()},
		"spec":     spec,
	}}
	history := make([]any, 0, 3)
	for h := range 3 {
		history = append(history, map[string]any{
			"chartName": ch.name, "chartVersion": ch.version, "appVersion": "v" + ch.version, "configDigest": digest, "digest": digest,
			"firstDeployed": when, "lastDeployed": when, "name": name, "namespace": ns, "status": []string{"deployed", "superseded", "superseded"}[h],
			"version": int64(3 - h), "ociDigest": digest,
		})
	}
	out.status = map[string]any{
		"conditions": []any{
			map[string]any{"type": "Ready", "status": "True", "reason": "UpgradeSucceeded", "lastTransitionTime": when, "message": "Helm upgrade succeeded for release " + ns + "/" + name + ".v3 with chart " + ch.name + "@" + ch.version, "observedGeneration": int64(1)},
			map[string]any{"type": "Released", "status": "True", "reason": "UpgradeSucceeded", "lastTransitionTime": when, "message": "Helm upgrade succeeded for release " + ns + "/" + name + ".v3 with chart " + ch.name + "@" + ch.version, "observedGeneration": int64(1)},
		},
		"helmChart":                  ns + "/" + ns + "-" + name,
		"history":                    history,
		"lastAttemptedRevision":      ch.version,
		"lastAttemptedReleaseAction": "upgrade",
		"lastAttemptedConfigDigest":  digest,
		"observedGeneration":         int64(1),
		"storageNamespace":           ns,
	}
	return out
}

// gitopsSummary is what the GitOps fill created.
type gitopsSummary struct {
	ArgoApplications, ArgoMultiSource int
	FluxHelmReleases, FluxChartRefs   int
	OCIRepositories                   int
	// ExpectedCharts is what the collector reads back: one per Application
	// and per HelmRelease.
	ExpectedCharts int
	// Serialized JSON bytes of one object of each kind, averaged over what
	// was created (the size of a list response scales with these).
	ArgoApplicationAvgBytes, FluxHelmReleaseAvgBytes, OCIRepositoryAvgBytes int
	Seconds                                                                 float64
}

// seedGitOps creates the Applications, then the OCIRepositories, then the
// HelmReleases (a chartRef names an OCIRepository that already exists), and
// sets the HelmReleases' and OCIRepositories' status, which is a
// subresource. An existing object is left as it is, so a rerun continues.
func seedGitOps(ctx context.Context, dyn dynamic.Interface, cfg config, workers int, log io.Writer) (gitopsSummary, error) {
	start := time.Now()
	sum := gitopsSummary{}
	counts := countGitOps(cfg)
	if counts.ArgoApplications+counts.FluxHelmReleases == 0 {
		return sum, nil
	}
	phase := func(name string, n int, create func(ctx context.Context, i int) error) error {
		if n == 0 {
			return nil
		}
		t := time.Now()
		if err := parallel(ctx, n, workers, create); err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		fmt.Fprintf(log, "seed: %d %s in %s\n", n, name, time.Since(t).Round(time.Millisecond))
		return nil
	}
	size := func(u *unstructured.Unstructured) int {
		b, _ := u.MarshalJSON()
		return len(b)
	}
	if err := phase("argo applications", cfg.ArgoApps, func(ctx context.Context, i int) error {
		app := argoApplicationObject(i, cfg)
		return retried(ctx, func() error {
			return created(dyn.Resource(argoApplicationGVR).Namespace(app.GetNamespace()).Create(ctx, app, metav1.CreateOptions{}))
		})
	}); err != nil {
		return sum, err
	}
	if cfg.ArgoApps > 0 {
		sum.ArgoApplicationAvgBytes = avgSize(cfg.ArgoApps, func(i int) int { return size(argoApplicationObject(i, cfg)) })
	}
	// OCIRepositories first: each chartRef names one.
	if err := phase("flux ocirepositories", cfg.FluxHelmReleases, func(ctx context.Context, i int) error {
		o := fluxHelmReleaseObjects(i, cfg)
		if o.oci == nil {
			return nil
		}
		return createWithStatus(ctx, dyn.Resource(fluxOCIRepoGVR).Namespace(o.oci.GetNamespace()), o.oci, o.ociStat)
	}); err != nil {
		return sum, err
	}
	if err := phase("flux helmreleases", cfg.FluxHelmReleases, func(ctx context.Context, i int) error {
		o := fluxHelmReleaseObjects(i, cfg)
		return createWithStatus(ctx, dyn.Resource(fluxHelmReleaseGVR).Namespace(o.release.GetNamespace()), o.release, o.status)
	}); err != nil {
		return sum, err
	}
	if cfg.FluxHelmReleases > 0 {
		sum.FluxHelmReleaseAvgBytes = avgSize(cfg.FluxHelmReleases, func(i int) int {
			o := fluxHelmReleaseObjects(i, cfg)
			withStatus := o.release.DeepCopy()
			withStatus.Object["status"] = o.status
			return size(withStatus)
		})
		if counts.OCIRepositories > 0 {
			n, total := 0, 0
			for i := range cfg.FluxHelmReleases {
				if o := fluxHelmReleaseObjects(i, cfg); o.oci != nil {
					withStatus := o.oci.DeepCopy()
					withStatus.Object["status"] = o.ociStat
					total += size(withStatus)
					n++
				}
			}
			sum.OCIRepositoryAvgBytes = total / n
		}
	}
	sum.ArgoApplications, sum.ArgoMultiSource = counts.ArgoApplications, counts.ArgoMultiSource
	sum.FluxHelmReleases, sum.FluxChartRefs, sum.OCIRepositories = counts.FluxHelmReleases, counts.FluxChartRefs, counts.OCIRepositories
	sum.ExpectedCharts = counts.ExpectedCharts
	sum.Seconds = time.Since(start).Seconds()
	return sum, nil
}

// createWithStatus creates obj and then sets its status, which is a
// subresource, retrying what fails transiently. An object that already exists
// is left as it is.
func createWithStatus(ctx context.Context, res dynamic.ResourceInterface, obj *unstructured.Unstructured, status map[string]any) error {
	var got *unstructured.Unstructured
	err := retried(ctx, func() error {
		var err error
		got, err = res.Create(ctx, obj, metav1.CreateOptions{})
		return err
	})
	if err != nil {
		return created(got, err)
	}
	got.Object["status"] = status
	return retried(ctx, func() error {
		_, err := res.UpdateStatus(ctx, got, metav1.UpdateOptions{})
		return err
	})
}

// retryDelay is the pause before a retry, times the attempt number.
var retryDelay = 500 * time.Millisecond

// transient reports whether an error is one a retry can cure: a timeout (the
// lab's etcd answers "request timed out" under KWOK's heartbeats), throttling
// or an unavailable server.
func transient(err error) bool {
	return apierrors.IsTimeout(err) || apierrors.IsServerTimeout(err) || apierrors.IsTooManyRequests(err) ||
		apierrors.IsServiceUnavailable(err) || apierrors.IsInternalError(err) ||
		strings.Contains(err.Error(), "request timed out")
}

// retried runs f, again after a pause when it fails transiently, up to six
// tries in all.
func retried(ctx context.Context, f func() error) error {
	var err error
	for attempt := range 6 {
		if err = f(); err == nil || !transient(err) {
			return err
		}
		select {
		case <-ctx.Done():
			return err
		case <-time.After(time.Duration(attempt+1) * retryDelay):
		}
	}
	return err
}

func avgSize(n int, size func(i int) int) int {
	total := 0
	for i := range n {
		total += size(i)
	}
	return total / n
}
