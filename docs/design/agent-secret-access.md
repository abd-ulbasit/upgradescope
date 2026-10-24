# Design note: the agent's access to Secrets

Status: proposed (October 2026). Nothing here is implemented yet; the
[security model](../operations/security-model-and-rbac.md#the-rbachelmsecrets-trade-off)
describes what the chart does today.

## Problem

The chart's default, `rbac.helmSecrets: true`, gives the agent's
ServiceAccount `get` and `list` on every Secret and ConfigMap in the
cluster, because Helm keeps its release records in Secrets and RBAC cannot
narrow a rule by label or type. The agent itself only lists Helm's records
(`owner=helm`, metadata first) and gets one per release, but its token can
read anything: service-account tokens, cloud credentials, TLS keys, and
every other Helm release's values, secrets passed as chart values included
(Helm stores the values and the rendered manifests of every revision).

Nothing contains a compromised agent either. `networkPolicy.enabled` is off
by default, and when on it limits ingress to the server only; the agent has
no egress policy. A malicious image or dependency in the agent (the binary
links client-go and a few dozen other modules) could therefore read every
Secret in the cluster and send it anywhere.

What the read buys: the Helm capability, that is the chart `kubeVersion`
check and removed APIs found in stored release manifests. Add-ons installed
by Helm are still detected without it, by their images and labels, and the
verdict does not require Helm.

## Options

1. **Keep the default** (today). Helm findings work out of the box; the
   blast radius is every Secret in the cluster. Documented, and
   `rbac.helmSecrets=false` or `rbac.helmSecretsNamespaces` narrow it.
2. **Default `rbac.helmSecrets` to `false`.** No Secret or ConfigMap read
   unless asked for. A fresh install reports the `helm` capability as not
   assessed, with the forbidden list as the reason, and has no chart
   `kubeVersion` or stored-manifest findings until the operator opts in.
   A behaviour change, so it needs a minor release and a **Breaking** entry.
3. **Default `rbac.helmSecretsNamespaces` to a list** (say `kube-system`,
   `ingress-nginx`, `cert-manager`). Every listed namespace must exist at
   install time, so a default list breaks installs, and `kube-system`, the
   most sensitive namespace, would be on it. Rejected.
4. **A separate Helm reader.** A second Deployment with its own
   ServiceAccount and a minimal image that decodes release records and
   hands the agent only chart names, versions and manifests. The agent's
   larger dependency tree loses the Secret read, but the reader still holds
   it, the chart gains a component, and the agent must trust the reader's
   answers. More code than the risk reduction is worth for a project in
   maintenance mode.
5. **An egress NetworkPolicy for the agent** (alongside any of the above).
   Allow only DNS, the Kubernetes API server and the configured
   `agent.serverUrl`. The API server is usually not a pod, so the policy
   needs its address as a value (`networkPolicy.agent.apiServerCIDRs`);
   the CNI must enforce egress; and a compromised agent could still push
   what it read to the configured server inside a snapshot. It narrows
   exfiltration to places the operator already trusts, which is the point.

## Recommendation

- **Option 2 in the next minor release (0.4.0):** `rbac.helmSecrets`
  defaults to `false`. The install notes (`NOTES.txt`) say that Helm
  findings are off and print the two ways to turn them on
  (`rbac.helmSecrets=true`, or `rbac.helmSecretsNamespaces` with the
  namespaces that hold add-on releases), and the chart README's install
  examples show them. The secure choice should be the one a user gets
  without reading the security page; the cost is a capability the verdict
  does not depend on.
- **Option 5 as an opt-in in the same release,** on by default whenever
  `networkPolicy.enabled=true`, with `apiServerCIDRs` required in that case
  (the chart fails to render without it rather than rendering a policy that
  blocks the API server). Tested like the server's policy: a render matrix
  and kubeconform, plus a kind e2e run with a CNI that enforces egress.
- Keep `rbac.helmSecretsNamespaces` and document again that a Role there
  still reads every Secret of its namespace.

## Migration

- Users who set `rbac.helmSecrets` explicitly see no change.
- Users on the default lose the Secret and ConfigMap rules on `helm
  upgrade`: the ClusterRole is re-rendered without them. The next report
  shows `helm` as not assessed, naming `rbac.helmSecrets`; to keep the
  findings, upgrade with `--set rbac.helmSecrets=true` (or the namespace
  list). CHANGELOG.md lists this under **Changed** with a **Breaking**
  marker, and `hack/check-breaking.sh` requires the `!` in the commit
  subject.
- The claims ledger rows that describe the default (RB-01 and the
  security model's role table) and `deploy/chart/rbac_test.go`'s default
  case change with it.
- Until then, the recommended hardened settings are on the
  [security model page](../operations/security-model-and-rbac.md),
  and inline chart values for tokens are discouraged: see
  `server.existingSecret` in the [Helm values](../reference/helm-values.md).
