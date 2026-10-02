# In-cluster agent

The agent runs the same evaluation as `scan`, in the cluster, on a timer,
and keeps the answer in a `ClusterReadiness` object: `kubectl get ucr`
shows it, alerts and GitOps health checks read it, and nothing outside the
cluster is needed.

## Install

From v0.2.0 the chart is published as a signed OCI artifact:

```sh
helm install upgradescope oci://ghcr.io/abd-ulbasit/charts/upgradescope \
  --version 0.2.0 -n upgradescope --create-namespace
```

Before that release, install from a clone: `helm install upgradescope
deploy/chart -n upgradescope --create-namespace`. Either way the release
installs the agent Deployment, its ServiceAccount and read-only ClusterRole,
and the `ClusterReadiness` CRD. [Install](../operations/install.md) lists
the options, [Helm values](../reference/helm-values.md) every value.

## Read the result

```sh
kubectl get ucr
```

The columns come from the CRD: `TARGET` (the first target), `SCORE`,
`READY` (the verdict: `ready`, `blocked` or `unknown`), `LASTEVALUATED` and
`AGE`. For example:

```text
NAME      TARGET   SCORE   READY     LASTEVALUATED   AGE
cluster   1.37     75      blocked   4m              2d
```

`kubectl get ucr cluster -o yaml` has the full status: per target the score,
verdict, counts by severity and category, the top 20 findings, and what
was not assessed. The complete list of findings is in `upgradescope scan`
and on the server.

The agent ticks once at startup and then every `agent.interval` (10m by
default, 1m at least) with ±10% jitter. Its pod is Ready once the first
tick has written the status, so `helm install --wait` waits for a first
answer. To block a script or pipeline on readiness:

```sh
kubectl wait clusterreadiness/cluster --for=condition=Ready --timeout=15m
```

## Choose the targets

With no targets, the agent evaluates the next minor above the cluster's
version. To evaluate others, set them in the chart (the agent then resets
`spec.targets` to this list on every tick):

```sh
helm upgrade upgradescope deploy/chart -n upgradescope --reuse-values \
  --set-json 'agent.targets=["1.37","1.38"]'
```

or leave `agent.targets` empty and set them on the object, for example from
Git ([GitOps](../guides/gitops-argo-flux.md)):

```sh
kubectl patch ucr cluster --type merge -p '{"spec":{"targets":["1.37","1.38"]}}'
```

A target past the knowledge base's horizon reads `unknown`, with a
`kb-coverage` gap: the knowledge base cannot know what that release removes.
The default target becomes such a target once the cluster runs the horizon
minor itself, until a release with a newer knowledge base is installed.

## Accept known findings

`spec.ignore` takes the same rules as the CLI's `.upgradescope.yaml`, with a
required reason and an optional expiry; suppressed findings are counted in
the status but leave the score and verdict.
[Suppressions and baselines](../guides/suppressions-and-baselines.md#the-agent-specignore)
has the format.

## What the agent can touch

It reads with `get` and `list` only, never `watch`, and writes only its own
`ClusterReadiness` object, that object's status, and (with
`agent.manageCRD=true`, the default) the `clusterreadinesses.upgradescope.dev`
CRD. Reading Helm releases needs get/list on Secrets and ConfigMaps
cluster-wide, which RBAC cannot narrow; `rbac.helmSecrets=false` removes that
grant at the cost of Helm findings.
[Security model and RBAC](../operations/security-model-and-rbac.md) lists
every rule.

## Next

- Push to a server for history, a fleet view and a dashboard: [Fleet server](fleet.md).
- Alert on it: [Prometheus and Grafana](../guides/prometheus-grafana.md).
- EKS, GKE, AKS, k3s, RKE2, OpenShift: [Managed clusters](../guides/managed-clusters.md).
