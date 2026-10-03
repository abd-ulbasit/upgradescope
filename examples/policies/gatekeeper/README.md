# Gatekeeper: require a ready upgradescope target

`template.yaml` (a `ConstraintTemplate`) and `constraint.yaml` (its
`Constraint`) warn on, or, once you change `enforcementAction`, deny, a
workload that asks for an upgrade to a Kubernetes minor the
`ClusterReadiness` says is not ready. `config.yaml` makes Gatekeeper replicate
the `ClusterReadiness` so the template can read it.

## What it gates

A Deployment, StatefulSet, DaemonSet or Job that carries

```yaml
metadata:
  annotations:
    upgradescope.dev/upgrade-target: "1.37"
```

on admission. The annotation is this example's convention, not something
upgradescope reads: it marks "this object starts an upgrade to 1.37" for
whatever runs your upgrades. A Constraint cannot select on annotations, so
`constraint.yaml` selects the kinds and the Rego checks the annotation. Edit
`match.kinds` for the kinds that carry it in your cluster.

The template reads
`data.inventory.cluster["upgradescope.dev/v1alpha1"].ClusterReadiness[<crName>]`
(`parameters.crName`, default `cluster`, the chart's `agent.crName`) and
reports a violation when `status.targets[]` has an entry for the requested
minor with `ready: false`. `ready` is `false` for a `blocked` verdict **and**
for an `unknown` one (the scan could not clear the target); the message says
which.

## Defaults and fail-open

`enforcementAction: warn`: the request goes through and the message is shown
to whoever applied it and in audit results. Nothing is denied until you change
it to `deny`.

The template **allows** (fails open) when it cannot tell, because gating a
cluster operation on data nobody refreshed is worse than not gating it:

| Situation | Result |
|---|---|
| `status.lastEvaluated` older than `parameters.maxAgeSeconds` (default 3600), missing or unparsable | allowed (stale) |
| no entry for the requested minor in `status.targets` | allowed (never assessed) |
| no ClusterReadiness in the replicated data (not synced yet, agent not installed, another `crName`) | allowed |
| the entry says `ready: true` | allowed |

The agent's default interval is 10 minutes, and it withdraws its verdict
after two intervals plus 12 minutes of failed ticks, so an hour tolerates a
missed tick or two. Because an empty cache also fails open, **a sync that does
not work looks like "allowed"**: apply `config.yaml` (and `rbac.yaml` if your
install needs it) and check that the object is replicated before trusting a
quiet result.

## Install

```sh
kubectl apply -f config.yaml       # sync ClusterReadiness (merge into an existing Config)
kubectl apply -f rbac.yaml         # only if Gatekeeper's manager cannot already read it
kubectl apply -f template.yaml
kubectl apply -f constraint.yaml   # after the template's CRD is established
```

Gatekeeper reads one `Config` named `config` in its namespace; if you have
one, add the `syncOnly` entry to it instead of applying a second.
`rbac.yaml` binds a read-only role (`get`, `list`, `watch` on
`clusterreadinesses.upgradescope.dev`) to `gatekeeper-admin`; Gatekeeper's
stock manifests already let its manager read every kind, so it matters for
hardened installs only. The upgradescope chart grants those verbs to the
agent only.

## Limits

- **In-cluster operations only.** An upgrade started through a cloud
  provider's API (GKE, EKS, AKS node-pool or control-plane upgrades) does not
  go through this cluster's apiserver, so no admission policy sees it. Whether
  Gatekeeper can gate such operations at all is **unverified**; do not rely on
  this policy for them.
- **Checked offline only.** `tests/` runs in CI with `gator verify`, with
  the ClusterReadiness supplied as test inventory, not against a cluster. Not
  exercised: the sync (`config.yaml`) populating the cache, the webhook, and
  audit.
- Replicated data is eventually consistent: a verdict can lag the agent by a
  moment.
- Written in Rego v0 syntax, which every Gatekeeper release accepts.

## Tests

`tests/suite.yaml` is a `gator verify` suite over the template and constraint:

| Case | ClusterReadiness (inventory) | Result |
|---|---|---|
| ready target | fresh, 1.36 ready | no violation |
| not-ready target | fresh, 1.37 not ready | **1 violation** |
| stale | 1.37 not ready, evaluated in 2020 | no violation (stale) |
| unassessed target | fresh, no entry for 1.38 | no violation |
| no ClusterReadiness | none | no violation |
| no annotation | fresh | no violation |

"Fresh" is a `lastEvaluated` in the year 2200: gator has no clock to set, so
a date that is never in the past stands for "just evaluated". (Not later: OPA
holds time as nanoseconds in an int64, which ends in 2262.)

```sh
make examples-test    # all of the examples' checks; installs the pinned gator
gator verify examples/policies/gatekeeper/tests/suite.yaml    # by hand
```
