# GitOps with Argo CD and Flux

upgradescope fits a GitOps setup in two places: the chart installs like any
other, and the `ClusterReadiness` object can live in Git, so a sync or a
promotion can wait on readiness.

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
    The `Ready` condition and `lastEvaluated` this script reads are
    tested in `internal/crd`; the Lua script itself is not run by CI.

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
