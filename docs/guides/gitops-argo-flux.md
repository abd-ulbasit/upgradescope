# GitOps with Argo CD and Flux

upgradescope fits a GitOps setup in three places: the chart installs like any
other, the charts your GitOps tool deploys are found without a Helm release,
and the `ClusterReadiness` object can live in Git, so a sync or a promotion
can wait on readiness.

## Installing the chart from Git

Tools that render the chart without cluster access (Argo CD renders with
`helm template`; so does Kustomize's Helm support) cannot look up live
Secrets. Without
`server.ingestToken` or `server.existingSecret`, the chart would generate a
new ingest token on every render, so under GitOps set one of them, or
reference Secrets your secret manager creates (`agent.existingSecret`,
`server.existingSecret`). The [chart README](https://github.com/abd-ulbasit/upgradescope/blob/main/deploy/chart/README.md#install)
explains the lookup.

Helm installs the `ClusterReadiness` CRD from the chart's `crds/` directory
and never upgrades it. The agent keeps the CRD's schema in step with its
own binary at startup (`agent.manageCRD=true`, the default) using
server-side apply, and touches only the fields it owns, so labels and
annotations Argo CD or Flux set on the CRD survive. To manage the CRD from
Git yourself, set `agent.manageCRD=false`: the agent then never writes it,
and the CRD write permissions are dropped from its role. The manifest to
commit is [`deploy/chart/crds/`](https://github.com/abd-ulbasit/upgradescope/tree/main/deploy/chart/crds),
documented field by field in the [CRD reference](../reference/crd.md).

## Charts your GitOps tool deploys

Helm checks (a chart's `kubeVersion`, removed APIs in a release's stored
manifest) read the release records Helm leaves in the cluster. Argo CD
renders charts with `helm template` and leaves no record at all; Flux's
helm-controller keeps releases where the HelmRelease says. A cluster managed
that way has nothing for those checks to read, so upgradescope reads what the
tools themselves declare, and says what it could not assess. This is exactly
what is supported:

| | Read | Not read |
|---|---|---|
| **Helm** | releases in the `secrets` and `configmaps` storage drivers (`HELM_DRIVER=secret`, the default, and `configmap`): chart, versions, `kubeVersion`, stored manifest | the `sql` driver: its releases live in an external database the cluster does not show. Not supported |
| **Argo CD** | `applications.argoproj.io/v1alpha1`: the `chart`, `repoURL` and `targetRevision` of each `spec.sources[]` entry (or of `spec.source`, when `spec.sources` is empty, as Argo CD does) that sets `chart`, and each such entry whose `repoURL` is `oci://...` with no `chart`, which is how Argo CD reads a Helm chart from an OCI registry (`path: .`), unless it sets only `ref` (a multi-source entry that supplies values files to another source's chart): the chart is the last element of the repository path (`oci://ghcr.io/acme/charts/ingress-nginx` is `ingress-nginx`, with the userinfo, query and fragment cut first) and its version the `targetRevision`, for Applications (including those an ApplicationSet generates) whose destination is this cluster (`https://kubernetes.default.svc` or `https://kubernetes.default.svc.cluster.local`, with or without `:443`, or the name `in-cluster`) | sources that render a path in Git (a Helm chart in a repository, Kustomize with `helmCharts`) name no chart, and an `oci://` source with no repository path below the registry host names none either. An `oci://` source that holds plain manifests (a `path` other than `.`) is read as a chart too, since it cannot be told from one here: that over-reports the gap below, the safe direction (pinned by `TestGitOpsArgoOCISourceWithAPathIsStillReadAsTheRepositoryChart`). Applications for other clusters, which are counted in the reason |
| **Flux** | `helmreleases.helm.toolkit.fluxcd.io`: `spec.chart.spec` (`chart`, `version`, `sourceRef`; from a `GitRepository` or `Bucket` the chart is a path, and the last element of it is taken as the chart's name) and a `spec.chartRef` to an `OCIRepository` (`source.toolkit.fluxcd.io`, its `spec.url` and `spec.ref`: the digest, else the semver range, else the tag, the order Flux itself applies; the OCIRepositories are listed, one list of the namespace the chartRefs point into or one cluster-wide, paged at 50, and with a role that may list only some namespaces, per namespace, fetched by name where even that is refused; the agent remembers a refused list across ticks and asks for it again only once an hour, so a role with `get` alone costs those refused lists once an hour, not on every tick, and a list granted later is used within the hour, while `scan` asks once per run). The newest of `v2`, `v2beta2` and `v2beta1` the cluster serves | a `chartRef` to a `HelmChart`, or an `OCIRepository` that cannot be read (both are counted in the reason, with the first read error, and the capability is partial). HelmReleases with a `spec.kubeConfig`, which deploy to other clusters |

What the charts feed is **add-on detection**: a chart the registry knows (for
example `ingress-nginx`) is an install in the namespace the chart deploys
into, found whether or not a pod's image or labels would have found it. A
chart reference carries no application version, so the version comes from the
pods that run it; an add-on retired as a whole (Ingress NGINX) is still
end-of-life without one. The inventory lists the references as `gitopsCharts`,
and the add-on's `source` is `gitops` when nothing stronger found it.

A chart version in these resources is what the author asked for, often a
constraint (`4.*`, `>=4.0.0 <5.0.0`), not what is installed. It is shown as the
add-on's chart version only when it is a single version and no Helm release
records the real one. For a Flux OCIRepository the version follows Flux's own
precedence, **digest over semver over tag**: a `ref.digest` is recorded as the
digest (`sha256:...`), which is no chart version, so a tag beside it is not
reported as one; a `ref.semver` is recorded as the range it is (a range is no
exact version; a semver of one version is), whatever the tag says; a `ref.tag`
alone is the version; no `ref` at all (Flux then follows `latest`) records
none. A Flux cluster, where helm-controller leaves a release
Secret, reports that release's chart version when the release and the chart
source are in the same namespace: the HelmRelease's `targetNamespace` is unset
or its own namespace (the agent matches by namespace and does not read
`spec.storageNamespace`; the release Secret lives in the storage namespace,
which defaults to the HelmRelease's). With a `targetNamespace` elsewhere, the
Secret is in the HelmRelease's namespace and the chart deploys into the
target, so they are two installs of the add-on, as
for any chart with a namespace override: the release's at its app version, the
chart source's with the chart version it asks for, if exact.

Repository URLs are recorded **without userinfo, query string or fragment**. An Application's
`repoURL` and an OCIRepository's `spec.url` can carry userinfo
(`https://user:token@host`) or a token in the query string, and the agent
copies neither: only the scheme, host, port and path are kept, whether or not
the value parses as a URL (a Git address such as `git@github.com:org/repo.git`
is recorded as `github.com:org/repo.git`). The cut does not depend on the URL
parsing: everything up to the last `@` is dropped, so a token or password that
itself contains a `/`, `?` or `#` does not leak either. A URL with a `?` or
`#` before its last `@` cannot be told apart from a query that holds an `@`
and a token (`?user=ci@example.com&token=...`, which Helm, curl and git all
send), so it is recorded as empty. An `@` anywhere else in the URL, which a
chart repository does not use, therefore costs the part before it; an OCI reference pinned by digest keeps its digest. A token
embedded in the URL *path* cannot be told from a path and is recorded: a
Cloudsmith entitlement URL such as `https://dl.cloudsmith.io/<token>/org/repo/helm/charts/`
keeps its token, so give such tokens through userinfo or a Secret-backed
repository instead. The agent does not read the Secrets that hold
repository credentials. The URL as recorded is what the inventory holds,
what is pushed to the server and stored, and what the CLI prints.

An Application's `destination.namespace` and a HelmRelease's `targetNamespace`
are free text to the API server. A chart source whose target is not a valid
namespace name is not counted (the reason says how many), so one such resource
cannot stop the agent from reporting the rest of the cluster.

What stays **not assessed** for these charts is the part that needs a release:
`kubeVersion` and the stored manifest. When the cluster has no Helm release at
all but shows one of the tools, the `helm` capability is *partial* and names it
(`argocd`, `flux`) in its skipped list, so the report shows those checks as not
assessed instead of clean. The tool shows when its CRD is served, or when a
Deployment, StatefulSet or DaemonSet carries its tracking metadata: the
`argocd.argoproj.io/tracking-id` annotation, the `argocd.argoproj.io/instance`
label, or Flux's `helm.toolkit.fluxcd.io/name` label. The workload check is how
an Argo CD running in another cluster shows: it needs a role that can list
those workloads (the chart's can; a restricted CLI user's may not, and the check
then shows nothing and is no gap). Argo CD sets the tracking annotation by
default only from 3.0; with 2.x's default of the `app.kubernetes.io/instance`
label, which Helm sets too and so proves nothing, an Argo CD in another
cluster leaves no marker here until `application.resourceTrackingMethod` is
`annotation` (or `annotation+label`). The CRD check needs no permission. Helm is
an optional capability, so this never turns a verdict to `unknown` by itself.

Every chart read from an Argo CD Application, a native OCI source included, is
that gap too, **whether or not the cluster has other Helm releases**: `helm template` leaves the chart no
release, so its `kubeVersion` and stored-manifest checks cannot have run. This
is the common case, since Argo CD is usually installed with its own Helm chart.
Applications that name no chart (a path in Git) add no gap while releases
exist. Flux is different: helm-controller leaves ordinary release Secrets, so
Flux is a gap only on a cluster with no Helm release at all, and not when its
HelmRelease list is served and empty (Flux used for Kustomizations only), nor
when every HelmRelease deploys elsewhere (a `spec.kubeConfig`, or a target that
is not a namespace name) and so is not counted. When
API discovery itself fails, `helm` is partial and names both tools, because it
is not known whether either is installed.

Because a partial `helm` capability makes the add-on and Helm-release findings
count as unassessed when scans are compared (for deltas and notifications),
a cluster that serves either CRD without `rbac.gitops.*` set shows `helm` as
partial for good. Enable the flag for the tool you run to clear it. This
includes upgrading the chart on a Flux cluster whose releases were read in full
before this version.

The chart references are free text written by whoever can create a HelmRelease
in a namespace they own (or an Application, where Argo CD allows Applications in
any namespace). Such a principal can name `ingress-nginx` and produce an
end-of-life add-on finding for the cluster. The impact is a false finding, not
access; if that matters, restrict who may create these resources.

Reading the custom resources needs permission the agent does not have by
default. Enable what you run:

```yaml
rbac:
  gitops:
    argocd: true   # get, list on applications.argoproj.io
    flux: true     # get, list on helmreleases.helm.toolkit.fluxcd.io and ocirepositories.source.toolkit.fluxcd.io
```

Without them, with the tool installed, the agent reports `helm` as partial,
with the forbidden list as the reason; nothing fails. The charts that were read
count as add-on evidence even where nothing else could be: with the pod list
forbidden and no Helm release or IngressClass readable, `addons` is partial
(not unavailable) and says it was detected from the GitOps chart sources, and
when the Helm release storage itself is unreadable (Secrets and ConfigMaps
forbidden) `helm` is unavailable, its reason saying the chart sources were read
and still feed add-on detection. A CLI scan uses your own
credentials and needs no values. Lists are paged (50 per page), because an
Application's status can be large.

## The ClusterReadiness object in Git

The agent creates its `ClusterReadiness` (named `cluster` by default,
`agent.crName`) and only ever writes its status, plus `spec.targets` when
the chart's `agent.targets` is set. To own the spec from Git, commit the
object and leave `agent.targets` empty:

```yaml
apiVersion: upgradescope.basit.engineer/v1alpha1
kind: ClusterReadiness
metadata:
  name: cluster
spec:
  targets: ["1.38"]
  ignore:
    - key: eol-addon/ingress-nginx
      reason: migrating to Gateway API, tracked in PLAT-123
      expires: "2099-12-31"   # a date inside your migration window
```

If `agent.targets` is set as well, the agent resets `spec.targets` to it on
every tick and the two fight. `spec.ignore` takes the same rules as the
CLI's `.upgradescope.yaml` ([suppressions](suppressions-and-baselines.md#the-agent-specignore)).

The status carries standard `Ready` and `AllTargetsReady` conditions and
`observedGeneration` ([status reference](../observability.md#clusterreadiness-status)),
which is what health checks read. `Ready` is the **first** target's verdict.
`AllTargetsReady` covers every target: `False` when any is blocked (the
message names it, such as `1.38 blocked (3 blockers)`), `Unknown` when none is
blocked but one was not assessed, `True` only when all are ready. For a plan
of several targets, gate on `AllTargetsReady`, not `Ready`: with
`spec.targets: ["1.37","1.38"]`, a ready 1.37 and a blocked 1.38 leave
`Ready` `True`.

### A spec edit is applied at the next tick

The agent reads the spec only inside a tick, so a commit that changes
`spec.targets` or adds a `spec.ignore` rule (often to accept a blocker and
unblock a gate) shows the old verdict until the next tick: up to
`agent.interval` plus 10% jitter, about 11 minutes with the 10m default.
Until then `status.observedGeneration` is lower than `metadata.generation`;
that is how a health check or a script tells an old verdict from a current
one. To wait for the edit to be applied (here generation 5):

```sh
kubectl wait ucr/cluster --for=jsonpath='{.status.observedGeneration}'=5 --timeout=15m
```

or lower `agent.interval` (1m at least) so the lag is short. The Argo CD
health check below reports Progressing while the generations differ, and the
Flux rule below treats the object as in progress. Allow the lag in any
`timeout` of a sync wait or a Kustomization.

## Argo CD

!!! note "Tested in part"
    CI runs this script, read from this page, under gopher-lua (the Lua
    engine Argo CD embeds) over objects the agent's status writer produced:
    ready, blocked, unknown, no target, stale, a spec edited since the last
    tick, and no status
    (`TestArgoHealthScriptAgainstWrittenStatus` in `internal/crd`). It has
    not been run inside a live Argo CD.

Argo CD applies health checks to the resources an Application tracks. To
gate a sync or a promotion on readiness, commit the `ClusterReadiness`
object (with `spec.targets`) to the Application's repository and leave the
chart's `agent.targets` empty, so Git owns the spec and the agent fills the
status. Then add this to `argocd-cm`:

```yaml
data:
  # The script needs the string, math and os (os.time) libraries.
  resource.customizations.useOpenLibs.upgradescope.basit.engineer_ClusterReadiness: "true"
  resource.customizations.health.upgradescope.basit.engineer_ClusterReadiness: |
    -- Treat a status older than this as stale: three default 10m intervals.
    local staleAfterSeconds = 30 * 60

    local hs = { status = "Progressing", message = "Waiting for the upgradescope agent's first evaluation" }
    if obj.status == nil or obj.status.conditions == nil then
      return hs
    end
    if obj.metadata.generation ~= nil and obj.status.observedGeneration ~= nil
        and obj.status.observedGeneration < obj.metadata.generation then
      hs.message = "spec changed; waiting for the agent's next tick"
      return hs
    end

    -- lastEvaluated is RFC 3339 UTC ("2026-10-02T12:00:00Z"). Convert it to
    -- Unix time arithmetically: os.time on a table would read it as the
    -- controller's local time.
    local function unixUTC(y, m, d, h, mi, s)
      if m <= 2 then y = y - 1 end
      local era = math.floor(y / 400)
      local yoe = y - era * 400
      local doy = math.floor((153 * ((m + 9) % 12) + 2) / 5) + d - 1
      local doe = yoe * 365 + math.floor(yoe / 4) - math.floor(yoe / 100) + doy
      return (era * 146097 + doe - 719468) * 86400 + h * 3600 + mi * 60 + s
    end
    local last = obj.status.lastEvaluated
    if last ~= nil then
      local y, mo, d, h, mi, s = string.match(last, "^(%d+)-(%d+)-(%d+)T(%d+):(%d+):(%d+)")
      if y ~= nil then
        local at = unixUTC(tonumber(y), tonumber(mo), tonumber(d), tonumber(h), tonumber(mi), tonumber(s))
        if os.time() - at > staleAfterSeconds then
          hs.status = "Degraded"
          hs.message = "status is stale: last evaluated " .. last .. "; is the upgradescope agent running?"
          return hs
        end
      end
    end

    -- Ready is the first target's verdict. AllTargetsReady covers every
    -- target of a multi-target plan; an agent older than v0.2.0 does not
    -- write it, and then only Ready decides.
    local ready, all
    for _, c in ipairs(obj.status.conditions) do
      if c.type == "Ready" then ready = c end
      if c.type == "AllTargetsReady" then all = c end
    end
    if ready == nil then
      return hs
    end
    hs.message = ready.message
    if ready.status ~= "True" then
      -- False (Blocked) or Unknown (NotAssessed): not safe to upgrade.
      hs.status = "Degraded"
      hs.message = ready.reason .. ": " .. ready.message
      return hs
    end
    if all ~= nil and all.status ~= "True" then
      -- The first target is ready, a later one is blocked or not assessed.
      hs.status = "Degraded"
      hs.message = "AllTargetsReady " .. all.reason .. ": " .. all.message
      return hs
    end
    hs.status = "Healthy"
    return hs
```

| State | Health |
|---|---|
| no status yet | Progressing |
| `observedGeneration` behind `metadata.generation` (spec just edited) | Progressing |
| `lastEvaluated` older than `staleAfterSeconds` | Degraded |
| `Ready=True` (and `AllTargetsReady=True`, where the agent writes it) | Healthy |
| `Ready=False` (Blocked) or `Unknown` (NotAssessed) | Degraded |
| `Ready=True` but `AllTargetsReady` False or Unknown (a later target is blocked or not assessed) | Degraded |

Raise `staleAfterSeconds` if you run the agent with a longer `--interval`.
The agent Deployment's own health in Argo CD follows its readiness probe.

## Flux

!!! warning "Not tested by this project"
    No CI job runs Flux against upgradescope. The rule below follows the
    conditions the agent writes, which are tested (`TestReadyCondition` and
    `TestAllTargetsReadyCondition` in `internal/crd`); the Flux side is
    untested. That its health check honours `observedGeneration` was read in
    Flux's source (`runtime/cel/status_evaluator.go` in fluxcd/pkg, on
    2026-10-11), not run.

Flux's kustomize-controller can wait on custom resources it applies. A
`Kustomization` that applies the `ClusterReadiness` object above can state
its health rule with CEL (`healthCheckExprs`, Flux 2.5 or later):

```yaml
apiVersion: kustomize.toolkit.fluxcd.io/v1
kind: Kustomization
metadata:
  name: upgrade-readiness
  namespace: flux-system
spec:
  interval: 10m
  path: ./clusters/prod/upgradescope
  prune: true
  sourceRef: {kind: GitRepository, name: platform}
  wait: true
  timeout: 15m
  healthCheckExprs:
    - apiVersion: upgradescope.basit.engineer/v1alpha1
      kind: ClusterReadiness
      current: status.conditions.exists(e, e.type == 'AllTargetsReady' && e.status == 'True')
      failed: status.conditions.exists(e, e.type == 'AllTargetsReady' && e.status == 'False')
```

The rule reads `AllTargetsReady`, so a blocked later target of a plan holds
the rollout; for a single target it is the same as `Ready`. Use `Ready` in
both expressions to follow the first target alone. The expressions use
`exists`, not `filter(...).all(...)`, because `all` is true over an empty
list: with no matching condition, they stay in progress instead of passing.

Flux's health check for custom resources compares `status.observedGeneration`
with `metadata.generation` before it evaluates `current` and `failed`: while
the status lags the spec, the object is in progress, whatever the old
conditions say (kstatus does the same). So a spec edit does not pass on the
old verdict; the Kustomization waits for the next tick, up to about 11
minutes with the default interval, and its `timeout` should allow that. An
agent older than v0.2.0 does not write `AllTargetsReady`: neither expression
then matches and the Kustomization stays in progress until its timeout; use
`Ready` there.

`AllTargetsReady=Unknown` (no blocker found, but a required check was not assessed)
matches neither expression, so the Kustomization stays in progress until
its timeout. Downstream Kustomizations can then `dependsOn` this one to
hold a rollout until the cluster reads ready.
