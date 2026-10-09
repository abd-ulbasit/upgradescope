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
| `get`, `list` on `applications.argoproj.io`; `get`, `list` on `helmreleases.helm.toolkit.fluxcd.io`; `get` on `ocirepositories.source.toolkit.fluxcd.io` | the charts GitOps tools deploy: chart, version and repository of each Application source and HelmRelease, and the OCIRepository a HelmRelease's `chartRef` names, for add-on detection. Only these resources: no AppProjects, ApplicationSets or the Secrets that hold repository credentials. A repository URL is recorded without userinfo, query string or fragment, so a credential there in a `repoURL` or an OCIRepository `url` never reaches the inventory, the server or the CLI's output. A token embedded in the URL path (Cloudsmith's `dl.cloudsmith.io/<token>/...`) cannot be told from a path and is recorded: use userinfo or a Secret-backed repository for such tokens. Not granted by default | `rbac.gitops.argocd` and `rbac.gitops.flux` are off unless you set them |
| `get`, `list` on every group/resource the knowledge base flags as deprecated or removed (one rule per API group, from `files/kb-rbac-rules.yaml`) | the API-usage collector's metadata-only lists. Generated from the embedded knowledge base; a test fails when it drifts. Its `networking.k8s.io` rule also covers the add-on collector's `list` of `ingressclasses` (an Ingress NGINX IngressClass is add-on evidence), and its `apiextensions.k8s.io` rule the CRD collector's `list` of `customresourcedefinitions` (CRD versions and `status.storedVersions`); `rbac_test.go` pins both so a knowledge-base change cannot drop them. No custom resource is granted, so the agent cannot check which custom resources use a deprecated or unserved CRD version, and reports `crds` as partial for such CRDs | — |
| `get`, `update`, `patch` on `customresourcedefinitions`, `resourceNames: [clusterreadinesses.upgradescope.dev]` | keeping its own CRD's schema in step with the binary (server-side apply). No `create`: the chart's `crds/` installs it | `agent.manageCRD=false` |
| `get`, `list`, `create` on `clusterreadinesses` | creating its object when it is missing (`create` cannot be limited by name) | — |
| `update`, `patch` on `clusterreadinesses`, and `get`, `update`, `patch` on `clusterreadinesses/status`, `resourceNames: [cluster]` | writing its own object (`spec.targets`, and the `upgradescope.dev/status-error` annotation when a status write fails) and its status (`agent.crName`) | — |

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
require Helm. Releases kept by Helm's SQL driver have no object in the cluster,
and charts that Argo CD renders with `helm template` leave no release
object either; the [GitOps page](../guides/gitops-argo-flux.md#charts-your-gitops-tool-deploys)
says what the agent reads of those, and how to grant it.

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
the cap at 32–46 MiB of live heap on a GitHub-hosted runner, CI run
37160469086 (`TestCollectHelmManifestParsingIsBounded`: all newlines, tiny
objects, documents that are not objects, documents at the size and at the
node bound; the test enforces 64 MiB). Under the chart's GOMEMLIMIT the
runtime sheds garbage as the heap nears the limit, so live heap is what
must fit: the test reads the heap objects after collections forced back
to back at GOGC=10, and runs in `make test-heap`, CI's test-heap job, not
in a plain `go test`, since a busy machine inflates the reading. Read as
a heap that holds garbage, at the default GOGC=100, the same cases peaked
at 24–93 MiB. The run bounds, 1 MiB and 64Ki YAML nodes, are held by
`TestSplitManifestRunsHoldTheBounds`, which reads no heap figure (#213).
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
  ingest tokens and read tokens are stored as their sha256 hash and first 8
  characters; an ingest token can push only as its own cluster, and a read
  token minted for teams reads only their clusters and findings, a finding
  that spans teams cut to their namespaces and objects (any other cluster
  answers 404, as an unknown one does), never `/metrics`
  ([Read access](auth.md)). The server refuses an open read API unless the
  address it actually bound is loopback, or it is told otherwise
  (`--allow-anonymous-read`). The `Bearer` scheme is matched
  case-insensitively. The read, ingest and admin tokens must all differ.
  A per-cluster token is checked again when its push commits, with the
  cluster the push looked up: a push in flight when its token is revoked,
  or its cluster deleted or renamed, stores nothing (`401`, `409`).
- **Host check (DNS rebinding).** On a loopback listener, or with a
  trusted team header, a request whose Host is not `localhost`, a
  loopback address, the address it arrived on, the `--listen` host (not
  `0.0.0.0` or `::`) or an `--allowed-host` name gets `421` before any route or credential is
  looked at: a web page that rebinds its own name to `127.0.0.1` cannot
  read an open loopback read API, or ride a `kubectl port-forward` into
  header mode ([The Host check](auth.md#the-host-check-dns-rebinding)).
- **Trusted team header, off by default.** With `--trust-team-header` and
  `--trusted-proxy-cidr`, a read whose TCP peer is in those ranges takes its
  team scope from that header; from anywhere else the header is ignored. It
  is only as safe as the proxy: it must strip what clients send in that
  header, and the ranges must hold the proxy and nothing else. Every
  connection over loopback counts as the proxy when 127.0.0.1 is trusted:
  `kubectl port-forward` and a mesh sidecar that delivers traffic over
  localhost included
  ([Trusted team header](auth.md#trusted-team-header-trust-team-header)).
  Group names are part of that boundary: the header is a comma-separated
  list oauth2-proxy does not encode, so whoever can name an identity
  provider group `interns,payments` reads payments; filter the groups
  claim at the identity provider or connector.
- **Secrets never in argv.** Every token and the database URL can come from
  an environment variable or a file (`--read-token-file`, ...); the chart
  passes them as environment variables from Secrets, never as arguments.
  Either source is trimmed of surrounding whitespace (a Secret's trailing
  newline), and a value of whitespace only is refused.
- **Webhook URLs are secrets too.** A Slack webhook's path is its
  credential. `serve` refuses to start unless `--slack-webhook` and
  `--webhook` are absolute http(s) URLs, naming the flag and never the
  value, and a failed delivery is logged, and stored in the outbox's
  `last_error`, with the URL's scheme and host only
  (`https://hooks.slack.com/…`).
- **Admin commands never follow redirects.** `clusters delete|rename
  --server` treat any 3xx as an error naming its status and Location:
  Go would follow a 301 or 302 with a GET, which the admin token reads,
  and report a delete that never happened.
- **Rotating a secret needs a restart.** The chart passes tokens and
  webhook URLs as environment variables read once at start. After you change
  `server.ingestToken`, `readToken`, `adminToken`, a webhook value,
  `agent.serverToken` or the contents of a `server.existingSecret` or
  `agent.existingSecret`, run `kubectl -n <ns> rollout restart
  deploy/<release>-server deploy/<release>-agent`: until then the pods keep
  the old value and the old token still works, which matters when you are
  rotating because a token leaked. The chart puts no hash of a secret in
  pod or Deployment metadata to trigger the restart, since everyone who can
  get pods or Deployments could read it, and a short token could be tested
  against it offline ([Upgrade](upgrade.md#the-chart)). A follow-up will make
  `serve` and the agent re-read mounted token files, so that no restart is
  needed.
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
- **TLS** directly (`--tls-cert-file`, `--tls-key-file`, TLS 1.2 minimum,
  Go's default cipher suites), from the chart (`server.tls`: a Secret or a
  cert-manager `Certificate`; the in-chart agent then pushes over HTTPS)
  or at an Ingress (`server.ingress`). `serve` re-reads the key pair when
  its files change, so a renewal needs no restart. Over plain HTTP, agent
  bearer tokens and full cluster inventories cross the network in
  cleartext: the agent warns at startup when it would send its token over
  plain HTTP to a host that is not loopback. For a server behind a
  private CA, the agent's `--server-ca-file` (chart: `agent.serverCA`)
  adds that CA to the system roots; nothing skips certificate
  verification ([Exposing the server to remote agents](../getting-started/fleet.md#exposing-the-server-to-remote-agents)).
- **Browser hardening.** Every response the server's handler writes (the
  dashboard, its assets, the API, exports and their errors, `/healthz`)
  carries a Content-Security-Policy that forbids inline and third-party
  script, plus `nosniff`, `no-referrer` and `DENY` framing. The few
  responses Go's `net/http` writes before routing a request (a 400 for a
  malformed request or `Host`, a 431 for oversized headers, a 501 for an
  unknown `Transfer-Encoding`) carry none of them; their bodies are fixed
  plain text.
- **Network.** `networkPolicy.enabled=true` admits traffic to the server
  only from this release's agent and the peers you list.

## Supply chain

Releases are built by CI from the tagged commit, reproducibly. From v0.2.0,
archives (through a signed `checksums.txt`), the image and the chart are
signed keylessly with cosign, archives carry SBOMs and build provenance, and
the GitHub Action installs only a release archive that matches its
`checksums.txt` ([Install](install.md#verify-a-download)).
