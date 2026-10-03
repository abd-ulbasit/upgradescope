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
| `get`, `list` on `namespaces`, `nodes`, `pods` | the cluster ID (kube-system UID), team labels, kubelet versions and container runtimes, control-plane pod versions, add-on images and labels | — |
| `get` on non-resource URLs `/version`, `/metrics` | the server version; `apiserver_requested_deprecated_apis` | — |
| `get`, `list` on `secrets` | Helm releases (Helm's default storage driver): a metadata-only list of `owner=helm` Secrets, then one `get` per release | `rbac.helmSecrets=false` |
| `get`, `list` on `configmaps` | Helm releases stored by the configmaps driver, read the same way | `rbac.helmSecrets=false` |
| `get`, `list` on every group/resource the knowledge base flags as deprecated or removed (one rule per API group, from `files/kb-rbac-rules.yaml`) | the API-usage collector's metadata-only lists. Generated from the embedded knowledge base; a test fails when it drifts. Its `networking.k8s.io` rule also covers the add-on collector's `list` of `ingressclasses` (an Ingress NGINX IngressClass is add-on evidence), and its `apiextensions.k8s.io` rule the CRD collector's `list` of `customresourcedefinitions` (CRD versions and `status.storedVersions`); `rbac_test.go` pins both so a knowledge-base change cannot drop them. No custom resource is granted, so the agent cannot check which custom resources use a deprecated or unserved CRD version, and reports `crds` as partial for such CRDs | — |
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
Helm are still found by their container images and pod labels. The verdict does not
require Helm. Releases kept by Helm's SQL driver, and charts that Argo CD
renders with `helm template`, have no release object in the cluster either
way.

`rbac.create=false` lets you bind a role of your own; each collector that
lacks access degrades to "not assessed" with the reason.

## Findings are only as trustworthy as namespace write access

Most of what upgradescope judges comes from objects that whoever can
write to a namespace controls: Helm release Secrets and ConfigMaps (labels
`owner=helm`), pod images and tags, and pod labels such as
`app.kubernetes.io/name` and `app.kubernetes.io/version`. Nothing proves
that a release object was written by Helm or that a label tells the
truth. A tenant with `create` on Secrets (or ConfigMaps) in its own
namespace can forge a release, and one that can create pods can run any
image or label:

- **Fabricate a finding.** A forged release of chart `ingress-nginx`, or a
  pod labelled `app.kubernetes.io/name=ingress-nginx`, raises the
  `eol-addon/ingress-nginx` blocker for that namespace and, through it,
  the cluster's verdict and every gate that reads it.
- **Hide a finding**, only within its own namespace and only narrowly. A
  release's chart `appVersion` stands only for pods on its own release
  line there, so a forged release that claims a newer version of an
  add-on does not hide an older image of it on another line: that image
  is judged at its own version (#165). A patch-level difference on the
  same line, a pod whose image has no version tag, and an add-on whose
  image no matcher recognizes can still be hidden
  ([Add-on registry](../concepts/addon-registry.md#how-an-add-on-is-found)).

The effects stay within what the tenant can write: its forged evidence is
attributed to its own namespace (and team), it cannot change what
upgradescope reads from other namespaces, nodes or the API server
(`/version`, `/metrics`, discovery), and it cannot make the agent write
anything but its own `ClusterReadiness`. Treat findings about add-ons and
Helm releases in a namespace as no more trustworthy than the namespace's
write access; where tenants are not trusted, review a blocker's namespaces
before acting on it, and suppress with a reason what you have verified
to be forged ([Suppressions and baselines](../guides/suppressions-and-baselines.md)).

Forged or not, one Helm release object cannot take the agent down: what
reading it costs is bounded, not only its size. Its payload is decoded
within fixed bounds (4 MiB stored, 16 MiB of JSON once decompressed; real
releases decode to under 7 MiB). A release over them, such as a gzip bomb
that decompresses to hundreds of MiB, is skipped as `release payload too
large`; one that is not a single gzip member, or that decompresses past
its gzip size trailer, is not decodable either. The stored manifest of a release within them is parsed a run of
documents at a time, each run at most 1 MiB and 64Ki YAML nodes, because
parsing amplifies its input: a manifest of tiny objects or of newlines
that fits the cap took 390–564 MiB parsed whole. A single document over 2
MiB or 64Ki nodes (the largest real ones found are kyverno's policies
CRD, 1.4 MiB, and Argo CD's applicationsets CRD, about 48,500 nodes) is
not parsed: the release is recorded
with what its other documents hold, and named. Either way the `helm`
capability becomes partial and names the release; the others are still
read. Measured on the test harness, above the collector's baseline: the
gzip bomb peaks at 17 MiB of heap, also when its size trailer lies or a
second gzip member follows (`TestCollectHelmGzipBombIsBounded`;
its test process at 71 MB resident), and the worst manifests that fit
the cap at up to 47 MiB of live heap (`TestCollectHelmManifestParsingIsBounded`:
all newlines, tiny objects, documents that are not objects, documents at
the size and at the node bound). That test runs at GOGC=10 because the
chart's GOMEMLIMIT keeps the heap near its live size as it nears the
limit; at GOGC=100, uncollected garbage took one CI run to 93 MiB.
Before the bounds, a 951 KB Secret that
decompressed to 700 MiB took a scan to 1.93 GB and OOM-killed the agent at
its 256Mi limit, and a 127 KiB one of tiny ConfigMaps still could (#168).
Releases are read one at a time, so the bound holds however many such
objects there are.

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
  ingest tokens are stored as their sha256 hash and first 8 characters and
  can push only as their own cluster; the server refuses an open read API
  unless the address it actually bound is loopback, or it is told otherwise
  (`--allow-anonymous-read`). The `Bearer` scheme is matched
  case-insensitively.
- **Secrets never in argv.** Every token and the database URL can come from
  an environment variable or a file (`--read-token-file`, ...); the chart
  passes them as environment variables from Secrets, never as arguments.
- **Bounded input.** Snapshot and gate bodies are capped
  (`--max-snapshot-bytes`, `--max-gate-bytes`), gzip included, and their
  memory is bounded by structure, not only bytes: JSON values and YAML
  nodes (aliases at what they expand to) are counted against a budget
  before anything is decoded, buffered bodies share a budget, and one
  request per endpoint decodes at a time, as does one read of a stored
  snapshot (two reads of the whole fleet). A `/gate` answer is bounded
  before it is encoded, within `--max-gate-bytes`. Responses waiting for
  slow clients share one budget. Over a budget is `413`, a body too slow for
  the read timeout `408`, a full queue or response budget `503`
  ([Memory and request limits](../operations.md#memory-and-request-limits),
  which lists what is outside these bounds). Connections have read,
  write and idle timeouts; their number is not capped.
- **Data at rest.** The SQLite database and its `-wal` and `-shm` files
  are created 0600 (an existing one is tightened on open).
- **CSV exports** guard every place a spreadsheet could start a cell
  against formula injection, past leading white space too.
- **TLS** directly (`--tls-cert-file`, `--tls-key-file`, TLS 1.2 minimum),
  from the chart (`server.tls`: a Secret or a cert-manager `Certificate`;
  the in-chart agent then pushes over HTTPS) or at an Ingress. The agent
  warns at startup when it would send its token over plain HTTP to a host
  that is not loopback.
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
