# Security model and RBAC

upgradescope reads clusters and writes reports. This page lists exactly what
each component can reach, and the trade-offs you choose with the chart's
values. To report a vulnerability, see
[SECURITY.md](https://github.com/abd-ulbasit/upgradescope/blob/main/SECURITY.md).

## The agent's ClusterRole

Rendered from `deploy/chart/templates/rbac.yaml` (shown for a release named
`upgradescope` with the default values). Every rule names its resources:
there are no wildcard groups, resources or verbs, so no subresource
(`nodes/proxy`, `pods/log`, `pods/exec`) is ever covered. Reads are `get` and
`list` only; the agent polls and never watches.

| Rule | Why | Removed by |
|---|---|---|
| `get`, `list` on `namespaces`, `nodes`, `pods` | the cluster ID (kube-system UID), team labels, kubelet versions and container runtimes, control-plane pod versions, add-on images | — |
| `get` on non-resource URLs `/version`, `/metrics` | the server version; `apiserver_requested_deprecated_apis` | — |
| `get`, `list` on `secrets` | Helm releases (Helm's default storage driver): a metadata-only list of `owner=helm` Secrets, then one `get` per release | `rbac.helmSecrets=false` |
| `get`, `list` on `configmaps` | Helm releases stored by the configmaps driver, read the same way | `rbac.helmSecrets=false` |
| `get`, `list` on every group/resource the knowledge base flags as deprecated or removed (one rule per API group, from `files/kb-rbac-rules.yaml`) | the API-usage collector's metadata-only lists. Generated from the embedded knowledge base; a test fails when it drifts | — |
| `get`, `update`, `patch` on `customresourcedefinitions`, `resourceNames: [clusterreadinesses.upgradescope.dev]` | keeping its own CRD's schema in step with the binary (server-side apply). No `create`: the chart's `crds/` installs it | `agent.manageCRD=false` |
| `get`, `list`, `create` on `clusterreadinesses` | creating its object when it is missing (`create` cannot be limited by name) | — |
| `update`, `patch` on `clusterreadinesses`, and `get`, `update`, `patch` on `clusterreadinesses/status`, `resourceNames: [cluster]` | writing its own object and its status (`agent.crName`) | — |

`deploy/chart/rbac_test.go` renders this role and checks it with the
upstream RBAC rule matcher: every call the collectors make is allowed, and
`nodes/proxy`, `pods/log`, `pods/exec`, other CRDs, other `ClusterReadiness`
objects, `watch`, and Secrets with `rbac.helmSecrets=false` are denied. The
kind end-to-end test reads the API server's audit log of a real install and
fails if the agent writes anything but its object and CRD, or reads Secrets
other than through Helm's label selector.

### The `rbac.helmSecrets` trade-off

RBAC cannot grant "Secrets with label `owner=helm`": a `list` on Secrets is
all Secrets. With the default `rbac.helmSecrets=true`, the agent's token can
therefore read **every Secret and ConfigMap in the cluster**, though the
agent itself only lists Helm's release objects (metadata only) and gets one
per release. Treat that token as privileged.

With `rbac.helmSecrets=false`, the Helm capability is reported as not
assessed (with the forbidden error as the reason) and the report has no Helm
chart findings: no chart `kubeVersion` check and no removed APIs in stored
release manifests. Everything else still works, and add-ons installed by
Helm are still found by their container images. The verdict does not
require Helm. Releases kept by Helm's SQL driver, and charts that Argo CD
renders with `helm template`, have no release object in the cluster either
way.

`rbac.create=false` lets you bind a role of your own; each collector that
lacks access degrades to "not assessed" with the reason.

## What the agent writes

Only its own `ClusterReadiness` object (`agent.crName`, default `cluster`),
that object's status, and, with `agent.manageCRD=true`, the fields it owns
in the `ClusterReadiness` CRD. No webhooks, finalizers, owner references or
annotations, and nothing in the cluster changes because of a finding. The
pod runs as UID 65532, non-root, with a read-only root filesystem, no
privilege escalation, all capabilities dropped and the `RuntimeDefault`
seccomp profile.

A `scan` from a laptop needs the same reads (and makes no writes at all,
which the end-to-end audit log check also verifies); what your identity
cannot read is reported as not assessed.

## The server

- **No Kubernetes access.** The server runs under its own ServiceAccount
  and its pod mounts no API token.
- **Bearer tokens** for reads, pushes and administration, each optional
  except where noted ([Tenancy and access control](tenancy.md)). Per-cluster
  ingest tokens are stored as hashes and can push only as their own cluster;
  the server refuses an open read API on a non-loopback address unless told
  otherwise (`--allow-anonymous-read`).
- **Secrets never in argv.** Every token and the database URL can come from
  an environment variable or a file (`--read-token-file`, ...); the chart
  passes them as environment variables from Secrets, never as arguments.
- **Bounded input.** Snapshot and gate bodies are capped
  (`--max-snapshot-bytes`, `--max-gate-bytes`), gzip included; connections
  have read, write and idle timeouts.
- **TLS** directly (`--tls-cert-file`, `--tls-key-file`, TLS 1.2 minimum) or
  at an Ingress.
- **Browser hardening.** Every response carries a Content-Security-Policy
  that forbids inline and third-party script, plus `nosniff`,
  `no-referrer` and `DENY` framing.
- **Network.** `networkPolicy.enabled=true` admits traffic to the server
  only from this release's agent and the peers you list.

## Supply chain

Releases are built by CI from the tagged commit, reproducibly. From v0.2.0,
archives (through a signed `checksums.txt`), the image and the chart are
signed keylessly with cosign, archives carry SBOMs and build provenance, and
the GitHub Action installs only a release archive that matches its
`checksums.txt` ([Install](install.md#verify-a-download)).
