# Acting on readiness

The [`ClusterReadiness`](../reference/crd.md) object publishes the verdict
in the cluster: per target Kubernetes minor, a `score`, `ready`, a `verdict`,
counts of blockers and warnings, and the top findings, plus
`status.lastEvaluated`. The repository ships examples that turn it into
something that pushes back, in [`examples/`](https://github.com/abd-ulbasit/upgradescope/tree/main/examples):

| Example | For | What it does |
|---|---|---|
| [Kyverno policy](https://github.com/abd-ulbasit/upgradescope/tree/main/examples/policies/kyverno) | clusters that run Kyverno | audits (or denies) a workload that asks for an upgrade to a minor that is not ready |
| [Gatekeeper constraint](https://github.com/abd-ulbasit/upgradescope/tree/main/examples/policies/gatekeeper) | clusters that run OPA Gatekeeper | warns on (or denies) the same, with the object replicated through Gatekeeper's sync |
| [Renovate preset](https://github.com/abd-ulbasit/upgradescope/tree/main/examples/renovate) | repositories that Renovate keeps current | groups and prioritizes chart bumps for the add-ons the registry tracks |

They are examples to copy and adapt, not a supported product surface: read the
README next to each before applying it.

## The policies

Both policies match a workload carrying the annotation

```yaml
upgradescope.dev/upgrade-target: "1.37"
```

That annotation is the examples' convention, not something upgradescope
reads. It says "this object starts an upgrade to 1.37" for whatever runs your
upgrades; edit the policy's `kinds` to the objects that carry it in your
cluster. The policy then reads the `ClusterReadiness` (named `cluster` by
default, the chart's `agent.crName`) and reports the request when
`status.targets[]` has an entry for that minor with `ready: false`. `ready` is
false for a `blocked` verdict and for an `unknown` one, the scan could not
clear the target, so both are reported.

### Defaults: warn, and fail open

- **Audit or warn only.** Kyverno's `failureAction` is `Audit` and
  Gatekeeper's `enforcementAction` is `warn`. Nothing is blocked until you
  change that.
- **Fail open when stale.** If `status.lastEvaluated` is more than an hour old
  (or missing), the policy allows the request. The default agent interval is 10
  minutes and the agent withdraws its verdict after two intervals plus 12
  minutes of failed ticks, so an hour tolerates a missed tick or two; if you
  raise the agent interval, keep the limit at least twice the interval. Gating a
  cluster operation on a verdict nobody refreshed is worse than not gating it.
- **Fail open when it cannot tell.** No `ClusterReadiness`, or no entry for the
  requested minor (it was never assessed), also allows. The consequence to
  know: a policy engine that cannot read the object (an unsynced Gatekeeper cache,
  and for Kyverno a missing reader role, which is unverified) may look the same
  as "allowed". Apply the reader role
  and check the policy report or the replicated data before trusting a quiet
  result.

### Reader RBAC

The chart grants `get` and `list` on `clusterreadinesses` to the agent only,
so each policy engine needs its own read access, and each example ships it as a
read-only `ClusterRole` (`get`, `list`, `watch`):

- Kyverno: `rbac.yaml` carries the aggregation labels for its admission and
  background controllers.
- Gatekeeper: `config.yaml` makes it replicate the object, and `rbac.yaml`
  binds the role to `gatekeeper-admin` for installs whose manager role cannot
  already read it.

### What they cover

**In-cluster operations only.** The policies are admission policies: they see
what goes through this cluster's apiserver. A managed-cloud upgrade (a GKE, EKS
or AKS node-pool or control-plane upgrade started through the provider's API)
bypasses the apiserver and is not seen. Whether Kyverno or Gatekeeper can gate
those at all is **unverified**, so do not rely on these policies for them. Gate those upgrades where you start
them, for example with the [CI gate](../getting-started/ci-gate.md).

### How they are tested

CI runs, without a cluster:

- `kyverno test` (Kyverno CLI, pinned and checksum-verified) over five cases:
  ready, not ready, stale, unassessed, and no object;
- `gator verify` over the same cases, with the `ClusterReadiness` as replicated
  data;
- Go tests that decode the fixtures as the agent's own status type, check that
  the policies name the real group, version, resource and default object name,
  and that the reader roles are read-only.

Not exercised: live admission, Kyverno's background scan, Gatekeeper's sync and
audit, and the RBAC wiring. The "fresh" fixtures carry a `lastEvaluated` far in
the future because neither tool lets a test set the clock.

## The Renovate preset

`examples/renovate/upgradescope.json` is a **static** preset: a hand-written
list of the Helm chart names in the add-on registry, with three rules. Minor and
patch bumps are grouped, major bumps wait for approval and are never
automerged, and end-of-life add-ons (today five charts, `ingress-nginx` among them) are prioritized and
carry a note that the bump does not make a retired project supported. It does
not follow your scan or the registry once copied; a Go test keeps the file in
the repository in step with the registry, and Renovate's own
`renovate-config-validator --strict` checks it in CI.
