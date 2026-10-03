package collect

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"path"
	"regexp"
	"slices"
	"strings"
	"unicode"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/validate/content"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/metadata"

	"github.com/abd-ulbasit/upgradescope/internal/inventory"
	"github.com/abd-ulbasit/upgradescope/internal/kb"
)

// GitOps tools deploy charts without a release Secret the Helm collector
// can read: Argo CD renders them with helm template, which creates no
// release at all, and Flux's helm-controller keeps its releases wherever
// the HelmRelease says. Left alone, the helm capability would report
// "available" with nothing in it for such a cluster, and chart-derived
// checks would read as clean (#70). So, beside the release storage, the
// helm step reads what the tools themselves record about their charts
// (Argo CD Application sources, Flux HelmReleases) for add-on matching,
// and when the cluster shows the tools but has no Helm release, says that
// the checks that need a release were not assessed.

// gitopsPageSize bounds a list of custom resources: unlike the metadata
// lists, these carry whole objects, and an Application's status lists
// every resource it manages, so a page of 500 could be tens of MiB.
const gitopsPageSize = 50

// maxChartNameBytes bounds a chart name read from a custom resource: the
// most an RFC 1123 subdomain, which a chart name is a stricter form of.
const maxChartNameBytes = inventory.MaxObjectNameBytes

const (
	argoLabel = "Argo CD"
	fluxLabel = "Flux"

	// The name Argo CD registers the cluster it runs in under.
	argoInClusterName = "in-cluster"

	argoGroup  = "argoproj.io"
	fluxGroup  = "helm.toolkit.fluxcd.io"
	fluxSource = "source.toolkit.fluxcd.io"
)

// Versions of each API the collector reads, newest first. Flux served
// helm.toolkit.fluxcd.io/v2beta1 and v2beta2 until v2 became stable in
// Flux 2.3; the shapes read here are the same in all three.
var (
	argoVersions      = []string{"v1alpha1"}
	fluxVersions      = []string{"v2", "v2beta2", "v2beta1"}
	fluxSourceVersion = []string{"v1", "v1beta2"}
)

// argoInClusterHosts are the hosts of the in-cluster API server URL an
// Application destination may use (https, with or without port 443).
var argoInClusterHosts = []string{"kubernetes.default.svc", "kubernetes.default.svc.cluster.local"}

// workloadMarkerResources are the workloads listed, metadata-only, for
// tracking labels and annotations when no CRD shows the tools.
var workloadMarkerResources = []schema.GroupVersionResource{
	{Group: "apps", Version: "v1", Resource: "deployments"},
	{Group: "apps", Version: "v1", Resource: "statefulsets"},
	{Group: "apps", Version: "v1", Resource: "daemonsets"},
}

// gitopsTool is one GitOps tool the helm step looks for.
type gitopsTool struct {
	id       string // inventory.GitOpsArgoCD, GitOpsFlux
	label    string
	group    string
	versions []string
	resource string
	// tracks reports whether a workload carries the tool's tracking
	// metadata. Argo CD tracks by annotation (argocd.argoproj.io/tracking-id,
	// the default since Argo CD 3.0; in 2.x it has to be switched on) or by
	// the argocd.argoproj.io/instance label; app.kubernetes.io/instance, the
	// 2.x default, is no marker, since Helm sets it too. Flux marks what helm-controller applies with
	// helm.toolkit.fluxcd.io/name; kustomize-controller's labels say nothing
	// about charts.
	tracks func(metav1.PartialObjectMetadata) bool
	// rendersOnly marks a tool that renders charts with helm template and
	// leaves no Helm release at all (Argo CD): each chart read from it is
	// known to have nothing for the release checks to assess. A tool that
	// does leave releases (Flux's helm-controller) is a gap only while the
	// cluster shows no release.
	rendersOnly bool
	// note says what the tool not leaving a Helm release means for checks.
	note string
}

var gitopsTools = []gitopsTool{
	{
		id: inventory.GitOpsArgoCD, label: argoLabel, group: argoGroup, versions: argoVersions, resource: "applications",
		tracks: func(m metav1.PartialObjectMetadata) bool {
			return m.Annotations["argocd.argoproj.io/tracking-id"] != "" || m.Labels["argocd.argoproj.io/instance"] != ""
		},
		rendersOnly: true,
		note:        "it renders charts with helm template and leaves no release object",
	},
	{
		id: inventory.GitOpsFlux, label: fluxLabel, group: fluxGroup, versions: fluxVersions, resource: "helmreleases",
		tracks: func(m metav1.PartialObjectMetadata) bool { return m.Labels["helm.toolkit.fluxcd.io/name"] != "" },
	},
}

// gitopsToolState is what collectGitOps found out about one tool.
type gitopsToolState struct {
	gitopsTool
	gvr     schema.GroupVersionResource // zero when the resource is not served
	marked  bool                        // workloads carry its tracking metadata
	read    int                         // charts recorded
	seen    int                         // custom resources listed, whatever became of them
	foreign int                         // chart sources for other clusters, not recorded
	invalid int                         // chart sources deploying to a name that is no namespace, not recorded
	// unresolved counts Flux chartRefs that could not be resolved to a
	// chart; unresolvedWhy is the first read error behind them, or "".
	unresolved    int
	unresolvedWhy string
	failure       string // why its resources could not be read, or ""
	listed        bool   // its resources were listed without error
}

func (s gitopsToolState) served() bool { return s.gvr != (schema.GroupVersionResource{}) }

// present is what shows the tool is here: its CRD is served, or workloads
// carry its tracking metadata.
func (s gitopsToolState) present() (evidence string, ok bool) {
	switch {
	case s.served():
		return fmt.Sprintf("%s.%s/%s is served", s.resource, s.group, s.gvr.Version), true
	case s.marked:
		return "workloads carry its tracking metadata", true
	}
	return "", false
}

// collectHelmStep is the helm step: the Helm release storage drivers
// (collectHelmWith, which reuses cache when it is not nil), then the charts GitOps tools deploy (collectGitOps). The
// release storage decides the capability's availability, as before; what
// the tools add (their charts, and notes on what is not assessed) only
// ever makes an available capability partial, never unavailable.
func collectHelmStep(ctx context.Context, c Clients, lifecycle []kb.APILifecycleEntry, cache *HelmCache, inv *inventory.Inventory) error {
	err := collectHelmWith(ctx, c.Kube, c.Metadata, lifecycle, cache, inv)
	var pe partialError
	available := errors.As(err, &pe)
	states := collectGitOps(ctx, c, available && len(inv.HelmReleases) == 0, inv)
	if !available {
		// The release storage could not be read at all, so the capability
		// is unavailable; say that the charts GitOps tools declare were
		// read all the same, since add-on detection uses them.
		if n := len(inv.GitOpsCharts); err != nil && n > 0 {
			err = fmt.Errorf("%w; %d GitOps chart source(s) were read and still feed add-on detection", err, n)
		}
		return err
	}
	if len(states) == 0 {
		return err
	}
	return pe.withGitOps(states, len(inv.HelmReleases) == 0)
}

// withGitOps adds what collectGitOps found to a helm capability that read
// its release storage: a count of the charts read, and the gaps. The
// capability is partial, naming each tool in Skipped, when a tool's
// resources could not be read, or when the cluster has no Helm release but
// shows the tool (see collectGitOps). Charts read from a tool that only
// renders (Argo CD) are a gap whether or not the cluster has other
// releases, since each is known to have none.
func (pe partialError) withGitOps(states []gitopsToolState, noReleases bool) error {
	msgs := []string{pe.msg}
	var counts []string
	for _, s := range states {
		if s.read > 0 {
			counts = append(counts, fmt.Sprintf("%d via %s", s.read, s.label))
		}
	}
	if len(counts) > 0 {
		msgs = append(msgs, "GitOps chart sources: "+strings.Join(counts, ", "))
	}
	for _, s := range states {
		skip := false
		if s.foreign > 0 {
			msgs = append(msgs, fmt.Sprintf("%d %s chart source(s) deploy to other clusters, not counted", s.foreign, s.label))
		}
		if s.invalid > 0 {
			msgs = append(msgs, fmt.Sprintf("%d %s chart source(s) deploy to a target that is not a namespace name, not counted", s.invalid, s.label))
		}
		if s.failure != "" {
			msgs = append(msgs, s.failure)
			skip = true
		}
		if s.unresolved > 0 {
			msg := fmt.Sprintf("%d HelmRelease chartRef(s) not resolved to a chart", s.unresolved)
			if s.unresolvedWhy != "" {
				msg += " (" + s.unresolvedWhy + ")"
			}
			msgs = append(msgs, msg)
			skip = true
		}
		// (With no release at all, the presence gap below says it already.)
		if s.rendersOnly && s.read > 0 && !noReleases {
			msgs = append(msgs, fmt.Sprintf("%d %s chart(s) read from Applications leave no Helm release: chart kubeVersion and stored-manifest checks were not assessed for them; %s", s.read, s.label, s.note))
			skip = true
		}
		// A tool whose resources were listed and are none deploys no chart;
		// nor does one whose resources all deploy elsewhere (to other
		// clusters, or to no namespace), which are not counted here.
		nothingDeployed := !s.rendersOnly && s.listed && s.seen-s.foreign-s.invalid == 0
		if evidence, ok := s.present(); ok && noReleases && !nothingDeployed {
			msg := fmt.Sprintf("no Helm releases read, but %s is present (%s): chart kubeVersion and stored-manifest checks were not assessed for the charts it deploys", s.label, evidence)
			if s.note != "" {
				msg += "; " + s.note
			}
			msgs = append(msgs, msg)
			skip = true
		}
		if skip {
			pe.skipped = append(pe.skipped, s.id)
			pe.incomplete = true
		}
	}
	slices.Sort(pe.skipped)
	pe.msg = strings.Join(msgs, "; ")
	return pe
}

// collectGitOps looks for Argo CD and Flux and reads the charts they
// deploy into inv.GitOpsCharts, from Application spec.source and
// spec.sources entries with chart set, and from HelmRelease spec.chart or
// a chartRef to an OCIRepository (Flux's v2 API). A tool is found when its
// CRD is served; with noReleases, also when workloads carry its tracking
// metadata (a tool managing this cluster from another one has no CRD
// here). Only resources that deploy to the scanned cluster count: an
// Application whose destination is another cluster, or a HelmRelease with
// a kubeConfig, is counted in foreign and skipped.
//
// It never fails: a tool whose resources could not be read (forbidden, the
// role the chart gives the agent without rbac.gitops.*) is left with a
// failure that the caller reports as a gap. Workload markers are
// best-effort: a workload list that fails shows nothing, and is no gap
// (a role that cannot list workloads). Resources not served are not
// an error. Nothing is read without a discovery client; without a dynamic
// client the tools are only looked for.
func collectGitOps(ctx context.Context, c Clients, noReleases bool, inv *inventory.Inventory) []gitopsToolState {
	if c.Discovery == nil {
		return nil
	}
	disc := discovery.ToDiscoveryInterfaceWithContext(c.Discovery)
	states := make([]gitopsToolState, len(gitopsTools))
	for i, t := range gitopsTools {
		states[i].gitopsTool = t
	}
	groups, err := disc.ServerGroupsWithContext(ctx)
	if err != nil {
		for i := range states {
			states[i].failure = fmt.Sprintf("API discovery failed, so it is not known whether %s is installed or what it deploys: %v", states[i].label, err)
		}
		return states
	}
	served := map[string][]string{} // group → versions
	for _, g := range groups.Groups {
		for _, v := range g.Versions {
			served[g.Name] = append(served[g.Name], v.Version)
		}
	}
	for i := range states {
		s := &states[i]
		gvr, err := servedResource(ctx, disc, served[s.group], s.group, s.versions, s.resource)
		if err != nil {
			s.failure = fmt.Sprintf("%s chart sources not read: discovery of %s.%s: %v", s.label, s.resource, s.group, err)
			continue
		}
		s.gvr = gvr
	}
	if noReleases && c.Metadata != nil {
		markWorkloads(ctx, c.Metadata, states)
	}
	if c.Dynamic == nil {
		return trimStates(states)
	}
	for i := range states {
		s := &states[i]
		if !s.served() {
			continue
		}
		var charts []inventory.GitOpsChart
		var err error
		switch s.id {
		case inventory.GitOpsArgoCD:
			charts, err = readArgoApplications(ctx, c.Dynamic, s)
		case inventory.GitOpsFlux:
			charts, err = readFluxHelmReleases(ctx, c.Dynamic, disc, served, s)
		}
		if err != nil {
			s.failure = fmt.Sprintf("%s chart sources not read: %v", s.label, err)
			continue
		}
		s.listed = true
		s.read = len(charts)
		inv.GitOpsCharts = append(inv.GitOpsCharts, charts...)
	}
	return trimStates(states)
}

// trimStates keeps the states that have anything to report.
func trimStates(states []gitopsToolState) []gitopsToolState {
	return slices.DeleteFunc(states, func(s gitopsToolState) bool {
		_, present := s.present()
		return !present && s.failure == "" && s.read == 0
	})
}

// servedResource returns the first of versions the apiserver serves group
// at with resource, preferring the order given; the zero value when none.
// versions the group is not listed at are not asked for.
func servedResource(ctx context.Context, disc discovery.DiscoveryInterfaceWithContext, listed []string, group string, versions []string, resource string) (schema.GroupVersionResource, error) {
	for _, v := range versions {
		if !slices.Contains(listed, v) {
			continue
		}
		list, err := disc.ServerResourcesForGroupVersionWithContext(ctx, group+"/"+v)
		if apierrors.IsNotFound(err) {
			continue
		}
		if err != nil {
			return schema.GroupVersionResource{}, err
		}
		if slices.ContainsFunc(list.APIResources, func(r metav1.APIResource) bool { return r.Name == resource }) {
			return schema.GroupVersionResource{Group: group, Version: v, Resource: resource}, nil
		}
	}
	return schema.GroupVersionResource{}, nil
}

// markWorkloads lists workloads metadata-only until every tool without a
// served CRD has been found or the workloads run out.
func markWorkloads(ctx context.Context, meta metadata.Interface, states []gitopsToolState) {
	pending := func() bool {
		return slices.ContainsFunc(states, func(s gitopsToolState) bool { return !s.served() && !s.marked })
	}
	for _, gvr := range workloadMarkerResources {
		if !pending() {
			return
		}
		opts := metav1.ListOptions{Limit: listPageSize}
		for pending() {
			l, err := meta.Resource(gvr).Namespace(metav1.NamespaceAll).List(ctx, opts)
			if err != nil {
				break // best-effort: see collectGitOps
			}
			for _, m := range l.Items {
				for i := range states {
					if !states[i].served() && !states[i].marked && states[i].tracks(m) {
						states[i].marked = true
					}
				}
			}
			if l.Continue == "" {
				break
			}
			opts.Continue = l.Continue
		}
	}
}

// listCustomResources lists one custom resource cluster-wide, paged,
// calling fn for every item.
func listCustomResources(ctx context.Context, dyn dynamic.Interface, gvr schema.GroupVersionResource, fn func(*unstructured.Unstructured)) error {
	opts := metav1.ListOptions{Limit: gitopsPageSize}
	for {
		l, err := dyn.Resource(gvr).Namespace(metav1.NamespaceAll).List(ctx, opts)
		if err != nil {
			return fmt.Errorf("list %s: %w", gvr.Resource, err)
		}
		for i := range l.Items {
			fn(&l.Items[i])
		}
		if l.GetContinue() == "" {
			return nil
		}
		opts.Continue = l.GetContinue()
	}
}

// readArgoApplications reads the chart sources of every Application that
// deploys to the scanned cluster.
func readArgoApplications(ctx context.Context, dyn dynamic.Interface, s *gitopsToolState) ([]inventory.GitOpsChart, error) {
	var out []inventory.GitOpsChart
	err := listCustomResources(ctx, dyn, s.gvr, func(app *unstructured.Unstructured) {
		s.seen++
		spec := mapAt(app.Object, "spec")
		// spec.sources, when set, replaces spec.source: Argo CD ignores a
		// source beside it.
		var sources []map[string]any
		if list, _ := spec["sources"].([]any); len(list) > 0 {
			for _, e := range list {
				if src, ok := e.(map[string]any); ok {
					sources = append(sources, src)
				}
			}
		} else if src := mapAt(spec, "source"); src != nil {
			sources = append(sources, src)
		}
		dest := mapAt(spec, "destination")
		for _, src := range sources {
			chart := stringAt(src, "chart")
			if chart == "" || !plausibleChartName(chart) {
				continue
			}
			if !argoInCluster(dest) {
				s.foreign++
				continue
			}
			target := stringAt(dest, "namespace")
			if !plausibleTarget(target) {
				s.invalid++
				continue
			}
			out = append(out, inventory.GitOpsChart{
				Tool: s.id, Name: app.GetName(), Namespace: app.GetNamespace(), Target: target,
				Chart: chart, Version: stringAt(src, "targetRevision"), Repo: redactRepoURL(stringAt(src, "repoURL")),
			})
		}
	})
	return out, err
}

// argoInCluster reports whether an Application destination is the cluster
// Argo CD runs in: by its API server URL (the service's short or cluster
// DNS name, with or without port 443) or, when it names none, by the name
// Argo CD registers the cluster under.
func argoInCluster(dest map[string]any) bool {
	server := stringAt(dest, "server")
	if server == "" {
		return stringAt(dest, "name") == argoInClusterName
	}
	u, err := url.Parse(server)
	if err != nil || u.Scheme != "https" || u.User != nil || strings.Trim(u.Path, "/") != "" {
		return false
	}
	return slices.Contains(argoInClusterHosts, u.Hostname()) && (u.Port() == "" || u.Port() == "443")
}

// plausibleTarget reports whether the namespace a chart deploys into, read
// from a custom resource, could be one: empty (left to the manifests) or a
// namespace name, by the rule the server holds identifiers to
// (inventory.ValidateIdentifiers). Argo CD's destination.namespace and
// Flux's targetNamespace are free text to the apiserver, so a resource
// anyone with access to one namespace can write must not make the whole
// inventory unacceptable.
func plausibleTarget(ns string) bool { return ns == "" || len(content.IsDNS1123Label(ns)) == 0 }

// readFluxHelmReleases reads the chart of every HelmRelease that deploys
// to the scanned cluster: spec.chart.spec (a HelmRepository, GitRepository
// or Bucket chart), or a chartRef to an OCIRepository, which it resolves.
// A chartRef of another kind, or an OCIRepository that cannot be read, is
// counted in s.unresolved.
func readFluxHelmReleases(ctx context.Context, dyn dynamic.Interface, disc discovery.DiscoveryInterfaceWithContext, served map[string][]string, s *gitopsToolState) ([]inventory.GitOpsChart, error) {
	type ref struct{ namespace, name string }
	var out []inventory.GitOpsChart
	var pending []int     // indexes into out of charts awaiting an OCIRepository
	refs := map[int]ref{} // …and the OCIRepository each awaits
	err := listCustomResources(ctx, dyn, s.gvr, func(hr *unstructured.Unstructured) {
		s.seen++
		spec := mapAt(hr.Object, "spec")
		if mapAt(spec, "kubeConfig") != nil {
			s.foreign++
			return
		}
		target := stringAt(spec, "targetNamespace")
		if target == "" {
			target = hr.GetNamespace()
		}
		if !plausibleTarget(target) {
			s.invalid++
			return
		}
		c := inventory.GitOpsChart{Tool: s.id, Name: hr.GetName(), Namespace: hr.GetNamespace(), Target: target}
		if cs := mapAt(mapAt(spec, "chart"), "spec"); cs != nil {
			c.Chart, c.Version = stringAt(cs, "chart"), stringAt(cs, "version")
			if sr := mapAt(cs, "sourceRef"); sr != nil {
				ns := stringAt(sr, "namespace")
				if ns == "" {
					ns = hr.GetNamespace()
				}
				c.Repo = fmt.Sprintf("%s/%s/%s", stringAt(sr, "kind"), ns, stringAt(sr, "name"))
			}
			// From a GitRepository or Bucket the chart is a path in it
			// ("./charts/ingress-nginx"): its last element names the chart.
			if kind := stringAt(mapAt(cs, "sourceRef"), "kind"); kind != "" && kind != "HelmRepository" {
				c.Chart = path.Base(strings.TrimRight(c.Chart, "/"))
			}
			if plausibleChartName(c.Chart) {
				out = append(out, c)
			}
			return
		}
		cr := mapAt(spec, "chartRef")
		if cr == nil {
			return
		}
		if stringAt(cr, "kind") != "OCIRepository" {
			s.unresolved++
			return
		}
		ns := stringAt(cr, "namespace")
		if ns == "" {
			ns = hr.GetNamespace()
		}
		pending = append(pending, len(out))
		refs[len(out)] = ref{ns, stringAt(cr, "name")}
		out = append(out, c)
	})
	if err != nil {
		return nil, err
	}
	if len(pending) == 0 {
		return out, nil
	}
	gvr, _ := servedResource(ctx, disc, served[fluxSource], fluxSource, fluxSourceVersion, "ocirepositories")
	var resolved []inventory.GitOpsChart
	cache := map[ref]*unstructured.Unstructured{}
	for i, c := range out {
		r, awaiting := refs[i]
		if !awaiting {
			resolved = append(resolved, c)
			continue
		}
		repo, seen := cache[r]
		if !seen && gvr != (schema.GroupVersionResource{}) {
			var err error
			repo, err = dyn.Resource(gvr).Namespace(r.namespace).Get(ctx, r.name, metav1.GetOptions{})
			if err != nil && s.unresolvedWhy == "" {
				s.unresolvedWhy = fmt.Sprintf("get ocirepository %s/%s: %v", r.namespace, r.name, err)
			}
			cache[r] = repo
		}
		// The URL is redacted first: the chart name is its last element,
		// which a query string would otherwise be part of.
		repoURL := ""
		if repo != nil {
			repoURL = redactRepoURL(stringAt(mapAt(repo.Object, "spec"), "url"))
		}
		chart := path.Base(strings.TrimRight(strings.TrimPrefix(repoURL, "oci://"), "/"))
		if repoURL == "" || !plausibleChartName(chart) {
			s.unresolved++
			continue
		}
		ociRef := mapAt(mapAt(repo.Object, "spec"), "ref")
		c.Chart, c.Repo = chart, repoURL
		c.Version = stringAt(ociRef, "tag")
		if c.Version == "" {
			c.Version = stringAt(ociRef, "semver")
		}
		resolved = append(resolved, c)
	}
	return resolved, nil
}

// redactRepoURL returns a chart repository URL read from a custom resource
// without anything that could be a credential: userinfo ("user:token@"),
// the query string and the fragment. The scheme, host (with port) and path
// are kept. The agent never reads repository credentials, and a URL that
// carries one must not be copied into the inventory, which is pushed to
// the server, stored and printed by the CLI.
//
// The cut is textual and does not trust url.Parse. Go ends the authority at
// the first "/", "?" or "#", so a userinfo holding one of them (a base64
// token, a password such as "12/ab") parses successfully with the
// credential as the host, path, query or fragment. Everything up to the
// last "@" after the scheme is therefore dropped first, and only then are
// the query and fragment cut off. When a "?" or "#" comes before that last
// "@", text cannot tell a password holding "?" ("user:443?tok@host") from
// a query holding "@" ("?user=ci@example.com&token=tok", which Helm, curl
// and git all send), so nothing is recorded: the value is returned as the
// empty string. An "@" is never legitimate in a Helm or OCI repository
// URL, so such a value loses more than the credential, never less. A value with
// no scheme, such as an scp-like Git address ("git@host:org/repo.git"), is
// cut the same way. Control characters are removed, and a URL left with no
// host is returned as the empty string.
func redactRepoURL(raw string) string {
	s := strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, strings.TrimSpace(raw))
	scheme, rest, hasScheme := strings.Cut(s, "://")
	if !hasScheme || !validScheme(scheme) {
		// No scheme, or something before the first "://" that cannot be
		// one ("user:pa" in "user:pa://x@host"): that "://" is part of a
		// credential.
		scheme, rest, hasScheme = "", s, false
	}
	// An OCI reference pinned by digest ("ghcr.io/acme/chart@sha256:...")
	// keeps its digest: it is set aside, and only when what precedes it is a
	// registry host and a path, so a credential cannot pass for a digest's
	// prefix (in "user:pw@sha256:..." the digest is a host, and is cut).
	digest := ""
	if m := ociDigestSuffix.FindStringIndex(rest); m != nil {
		before := rest[:m[0]]
		if ociRegistryPath.MatchString(before[strings.LastIndex(before, "@")+1:]) {
			rest, digest = before, rest[m[0]:]
		}
	}
	if at := strings.LastIndex(rest, "@"); at >= 0 {
		if q := strings.IndexAny(rest, "?#"); q >= 0 && q < at {
			return ""
		}
		rest = rest[at+1:]
	}
	if i := strings.IndexAny(rest, "?#"); i >= 0 {
		rest = rest[:i]
	}
	if !hasScheme {
		return rest + digest
	}
	if host, _, _ := strings.Cut(rest, "/"); host == "" {
		return ""
	}
	return scheme + "://" + rest + digest
}

var (
	// ociDigestSuffix is a trailing "@<algorithm>:<hex>" digest.
	ociDigestSuffix = regexp.MustCompile(`@(?:sha256|sha384|sha512):[0-9a-fA-F]{32,}$`)
	// ociRegistryPath is a registry host, an optional numeric port, and a
	// path: what must follow any userinfo for a digest to be kept.
	ociRegistryPath = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9.-]*[A-Za-z0-9])?(?::[0-9]+)?/[^@?#]+$`)
)

// validScheme reports whether s is a URL scheme (RFC 3986: a letter, then
// letters, digits, "+", "-" or ".").
func validScheme(s string) bool {
	for i, r := range s {
		letter := r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z'
		if !letter && (i == 0 || !(r >= '0' && r <= '9' || r == '+' || r == '-' || r == '.')) {
			return false
		}
	}
	return s != ""
}

// plausibleChartName reports whether a chart name read from a custom
// resource could be one: non-empty and no longer than a name may be. The
// resources are written by whoever can create them in one namespace.
func plausibleChartName(s string) bool { return s != "" && len(s) <= maxChartNameBytes }

// mapAt returns m[key] when it is an object, else nil; safe on a nil m.
func mapAt(m map[string]any, key string) map[string]any {
	v, _ := m[key].(map[string]any)
	return v
}

// stringAt returns m[key] when it is a string, else "" (YAML reads a
// version like 1.10 as a number; such a value is not a chart reference
// the registry can use); safe on a nil m.
func stringAt(m map[string]any, key string) string {
	v, _ := m[key].(string)
	return v
}
