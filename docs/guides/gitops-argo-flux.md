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
| **Argo CD** | `applications.argoproj.io/v1alpha1`: the `chart`, `repoURL` and `targetRevision` of each `spec.sources[]` entry (or of `spec.source`, when `spec.sources` is empty, as Argo CD does) that sets `chart`, for Applications (including those an ApplicationSet generates) whose destination is this cluster (`https://kubernetes.default.svc` or `https://kubernetes.default.svc.cluster.local`, with or without `:443`, or the name `in-cluster`) | sources that render a path in Git (a Helm chart in a repository, Kustomize with `helmCharts`) name no chart. Applications for other clusters, which are counted in the reason |
| **Flux** | `helmreleases.helm.toolkit.fluxcd.io`: `spec.chart.spec` (`chart`, `version`, `sourceRef`; from a `GitRepository` or `Bucket` the chart is a path, and the last element of it is taken as the chart's name) and a `spec.chartRef` to an `OCIRepository` (`source.toolkit.fluxcd.io`, its `spec.url` and `spec.ref.tag` or `semver`). The newest of `v2`, `v2beta2` and `v2beta1` the cluster serves | a `chartRef` to a `HelmChart`, or an `OCIRepository` that cannot be read (both are counted in the reason, with the first read error, and the capability is partial). HelmReleases with a `spec.kubeConfig`, which deploy to other clusters |

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
records the real one; a Flux cluster, where helm-controller leaves a release
Secret, reports that release's chart version.

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

Every chart read from an Argo CD Application is that gap too, **whether or not
the cluster has other Helm releases**: `helm template` leaves the chart no
release, so its `kubeVersion` and stored-manifest checks cannot have run. This
is the common case, since Argo CD is usually installed with its own Helm chart.
Applications that name no chart (a path in Git) add no gap while releases
exist. Flux is different: helm-controller leaves ordinary release Secrets, so
Flux is a gap only on a cluster with no Helm release at all, and not when its
HelmRelease list is served and empty (Flux used for Kustomizations only). When
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
    flux: true     # get, list on helmreleases.helm.toolkit.fluxcd.io, get on ocirepositories.source.toolkit.fluxcd.io
```

Without them, with the tool installed, the agent reports `helm` as partial,
with the forbidden list as the reason; nothing fails. A CLI scan uses your own
credentials and needs no values. Lists are paged (50 per page), because an
Application's status can be large.

## The ClusterReadiness object in Git

The agent creates its `ClusterReadiness` (named `cluster` by default,
`agent.crName`) and only ever writes its status, plus `spec.targets` when
the chart's `agent.targets` is set. To own the spec from Git, commit the
object and leave `agent.targets` empty:

```yaml
apiVersion: upgradescope.dev/v1alpha1
kind: ClusterReadiness
metadata:
  name: cluster
spec:
  targets: ["1.38"]
  ignore:
    - key: eol-addon/ingress-nginx
      reason: migrating to Gateway API, tracked in PLAT-123
      expires: "2026-12-31"
```

If `agent.targets` is set as well, the agent resets `spec.targets` to it on
every tick and the two fight. `spec.ignore` takes the same rules as the
CLI's `.upgradescope.yaml` ([suppressions](suppressions-and-baselines.md#the-agent-specignore)).

The status carries a standard `Ready` condition and `observedGeneration`
([status reference](../observability.md#clusterreadiness-status)), which is
what health checks read.

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
  resource.customizations.useOpenLibs.upgradescope.dev_ClusterReadiness: "true"
  resource.customizations.health.upgradescope.dev_ClusterReadiness: |
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

    for _, c in ipairs(obj.status.conditions) do
      if c.type == "Ready" then
        hs.message = c.message
        if c.status == "True" then
          hs.status = "Healthy"
        else
          -- False (Blocked) or Unknown (NotAssessed): not safe to upgrade.
          hs.status = "Degraded"
          hs.message = c.reason .. ": " .. c.message
        end
        return hs
      end
    end
    return hs
```

| State | Health |
|---|---|
| no status yet | Progressing |
| `observedGeneration` behind `metadata.generation` (spec just edited) | Progressing |
| `lastEvaluated` older than `staleAfterSeconds` | Degraded |
| `Ready=True` | Healthy |
| `Ready=False` (Blocked) or `Unknown` (NotAssessed) | Degraded |

Raise `staleAfterSeconds` if you run the agent with a longer `--interval`.
The agent Deployment's own health in Argo CD follows its readiness probe.

## Flux

!!! warning "Not tested by this project"
    No CI job runs Flux against upgradescope. The rule below follows the
    `Ready` condition the agent writes, which is tested
    (`TestReadyCondition` in `internal/crd`); the Flux side is untested.

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
    - apiVersion: upgradescope.dev/v1alpha1
      kind: ClusterReadiness
      current: status.conditions.filter(e, e.type == 'Ready').all(e, e.status == 'True')
      failed: status.conditions.filter(e, e.type == 'Ready').all(e, e.status == 'False')
```

`Ready=Unknown` (no blocker found, but a required check was not assessed)
matches neither expression, so the Kustomization stays in progress until
its timeout. Downstream Kustomizations can then `dependsOn` this one to
hold a rollout until the cluster reads ready.
