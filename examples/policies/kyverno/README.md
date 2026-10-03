# Kyverno: require a ready upgradescope target

`upgradescope-ready-target.yaml` is a `ClusterPolicy` that reads the
`ClusterReadiness` the upgradescope agent keeps and reports (or, in
`Enforce`, denies) a workload that asks for an upgrade to a Kubernetes minor
the object says is not ready.

## What it gates

A Job, Deployment, StatefulSet or DaemonSet that carries

```yaml
metadata:
  annotations:
    upgradescope.dev/upgrade-target: "1.37"
```

on `CREATE` or `UPDATE`. The annotation is this example's convention, not
something upgradescope reads: it marks "this object starts an upgrade to
1.37" for whatever runs your upgrades (a runbook Job, a pipeline's
Deployment). Edit `match.kinds` to the kinds that carry it in your cluster,
for example a Cluster API `Cluster` or a system-upgrade-controller `Plan`.

The rule fetches `/apis/upgradescope.dev/v1alpha1/clusterreadinesses/cluster`
(`cluster` is the chart's `agent.crName`; change the `urlPath` if you set
another) and violates when `status.targets[]` has an entry for the requested
minor with `ready: false`. `ready` is `false` for a `blocked` verdict **and**
for an `unknown` one (the scan could not clear the target), so both are
reported; the message says which.

## Defaults and fail-open

`failureAction: Audit`: a violation goes to the policy report and the
request goes through. Nothing is blocked until you change it to `Enforce`.

The rule **allows** (fails open) when it cannot tell, because gating a
cluster operation on data nobody refreshed is worse than not gating it:

| Situation | Result |
|---|---|
| `status.lastEvaluated` older than 1 hour, or missing | allowed (stale) |
| no entry for the requested minor in `status.targets` | allowed (never assessed) |
| no ClusterReadiness (404) | allowed (`apiCall.default: {}`), unverified against a live Kyverno |
| the entry says `ready: true` | allowed |

The freshness limit is the `1h` in the last condition. The agent's default
interval is 10 minutes, and it withdraws its verdict after two intervals plus
12 minutes of failed ticks, so an hour tolerates a missed tick or two. If you
raise the agent interval, raise the limit with it: keep it at least twice the
interval, or a healthy agent reads as stale and the policy never denies.

A read that fails for another reason (a 403 from a missing reader role) is
**not** shown to fail open: `apiCall.default` is documented for a missing
object, and nothing here checks against a live Kyverno whether a 403 takes the
default or raises a rule error (which `failurePolicy` then turns into a
rejection or a pass). Apply `rbac.yaml` first and check the policy report
before trusting a quiet result.

## Install

```sh
kubectl apply -f rbac.yaml                      # Kyverno may read ClusterReadiness
kubectl apply -f upgradescope-ready-target.yaml
```

`rbac.yaml` is a read-only `ClusterRole` (`get`, `list`, `watch` on
`clusterreadinesses.upgradescope.dev`) that Kyverno's admission and
background controllers aggregate. The upgradescope chart grants those verbs
to the agent only.

## Limits

- **In-cluster operations only.** An upgrade started through a cloud
  provider's API (GKE, EKS, AKS node-pool or control-plane upgrades) does not
  go through this cluster's apiserver, so no admission policy sees it. Whether
  Kyverno can gate such operations at all is **unverified**; do not rely on
  this policy for them.
- **Checked offline only.** `tests/` runs in CI with the Kyverno CLI (see
  below), not against a cluster. Not exercised: live admission, the RBAC
  aggregation, the background scan of existing objects, and `apiCall.default`
  taking over from a real 404 (the CLI's mock fails a non-2xx answer before
  it reaches the default, so the `missing` case feeds the empty object the
  default stands for).
- Written as a `kyverno.io/v1` `ClusterPolicy`, which Kyverno 1.19's CLI
  already warns is deprecated in favor of its newer policy types; it has not
  been ported to them.
- Moving to `Enforce` makes a request wait on the apiserver read and the
  policy; that is a decision about your admission path, not a default this
  example makes for you.

## Tests

`tests/` has one folder per case, each a `kyverno test` file that answers
the policy's apiCall with a `ClusterReadiness` fixture:

| Case | ClusterReadiness | Result |
|---|---|---|
| `ready` | fresh, 1.36 ready | allowed |
| `not-ready` | fresh, 1.37 not ready | **violation** (1.36 still allowed) |
| `stale` | 1.37 not ready, evaluated in 2020 | allowed (stale) |
| `unassessed` | fresh, no entry for 1.38 | allowed |
| `missing` | empty object | allowed |

"Fresh" is a `lastEvaluated` in the year 2200: the CLI has no clock to set,
so a date that is never in the past stands for "just evaluated".

```sh
make examples-test    # all of the examples' checks; installs the pinned kyverno
kyverno test examples/policies/kyverno/tests    # by hand, with kyverno >= 1.19
```
