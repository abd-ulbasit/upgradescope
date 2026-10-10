# upgradescope Helm chart

Installs the upgradescope agent (continuous upgrade-readiness scanning,
results in a `ClusterReadiness` custom resource) and, optionally, the
upgradescope server (snapshot history, what-if API, notifications,
dashboard).

Full documentation: https://abd-ulbasit.github.io/upgradescope/

## Install

From v0.2.0 the chart is published, signed with cosign, as an OCI artifact:

    helm install upgradescope oci://ghcr.io/abd-ulbasit/charts/upgradescope \
      --version 0.2.0 -n upgradescope --create-namespace

Before that release, or to run unreleased changes, install from a clone
of the repository with `deploy/chart` in place of the OCI reference. The
examples below use the clone path. A clone's chart defaults to the image
of the release it was cut for; to run unreleased changes, also set
`image.repository` and `image.tag` to an image built from the clone
(`make docker-build`).

Agent only (CRD-only mode — no server, results via `kubectl`):

    helm install upgradescope deploy/chart -n upgradescope --create-namespace
    kubectl get clusterreadiness cluster

Agent + server in one cluster:

    helm install upgradescope deploy/chart -n upgradescope --create-namespace \
      --set server.enabled=true

With no `server.ingestToken`, the chart generates a random token into the
`<fullname>-server-tokens` Secret, keeps it on `helm upgrade` (it looks the
live Secret up), and points the in-chart agent at it. The install notes
print the command that reads it back for agents in other clusters. Tools
that render without cluster access (`helm template`, Argo CD) cannot look
it up and would generate a new token on every render, so there set
`server.ingestToken` or `server.existingSecret`.

Reach the server, which has a ClusterIP Service only:

    kubectl -n upgradescope port-forward svc/upgradescope-server 8080:8080
    SERVER=http://127.0.0.1:8080
    curl $SERVER/api/v1/clusters

Agent pushing to an existing server elsewhere:

    helm install upgradescope deploy/chart -n upgradescope --create-namespace \
      --set agent.serverUrl=https://uscope.example.com \
      --set agent.existingSecret=my-push-token   # key: serverToken

## Fleet hub (server only)

A hub cluster runs the server alone; every other cluster runs the agent
and pushes to it. Create the Secrets first (the chart only references
them):

    kubectl create namespace upgradescope
    # Postgres URL; the server reads it as $UPGRADESCOPE_DB_URL, never argv.
    kubectl -n upgradescope create secret generic upgradescope-db \
      --from-literal=url='postgres://upgradescope:...@pg.example.internal:5432/upgradescope?sslmode=require'
    # Read token (dashboard, API, CI gate) and admin token (cluster delete
    # and rename); add webhookSecret to sign webhooks.
    kubectl -n upgradescope create secret generic upgradescope-hub \
      --from-literal=readToken="$(openssl rand -hex 32)" \
      --from-literal=adminToken="$(openssl rand -hex 32)"

    helm install upgradescope deploy/chart -n upgradescope --skip-crds -f hub.yaml

with `hub.yaml`:

    agent:
      enabled: false          # no agent, ServiceAccount, RBAC or push-token Secret here
    server:
      enabled: true
      existingSecret: upgradescope-hub
      readTokenFromSecret: true
      adminTokenFromSecret: true
      database:
        existingSecret: upgradescope-db
        key: url
      replicas: 2             # more than 1 only with Postgres
      teamMap:
        - {pattern: "payments-*", team: payments}
      ingress:
        enabled: true
        className: nginx
        host: upgradescope.example.com
        annotations: {cert-manager.io/cluster-issuer: letsencrypt}
        tls: {enabled: true}  # Secret upgradescope-server-tls unless tls.secretName

`--skip-crds` leaves out the `ClusterReadiness` CRD, which only agents use.
Without `database.existingSecret` the hub keeps SQLite on the PVC (one
replica). With `agent.enabled=false` and `existingSecret`, the shared
`ingestToken` key is optional: agents can use per-cluster tokens only, and
each one can push only as its own cluster.

Mint one token per cluster in the hub (printed once; the hub stores its
sha256 hash and first 8 characters), then install each agent with it:

    kubectl -n upgradescope exec deploy/upgradescope-server -- \
      /upgradescope tokens create prod-eu-1                       # Postgres
    kubectl -n upgradescope exec deploy/upgradescope-server -- \
      /upgradescope tokens create prod-eu-1 --db /data/upgradescope.sqlite   # SQLite

    # in cluster prod-eu-1
    kubectl -n upgradescope create secret generic push-token --from-literal=serverToken=<token>
    helm install upgradescope deploy/chart -n upgradescope --create-namespace \
      --set agent.clusterName=prod-eu-1 \
      --set agent.serverUrl=https://upgradescope.example.com \
      --set agent.existingSecret=push-token

`tokens list` and `tokens revoke` work the same way. Decommission a
cluster (its history and tokens go too) with the admin token:

    UPGRADESCOPE_ADMIN_TOKEN=... upgradescope clusters delete prod-eu-1 \
      --server https://upgradescope.example.com

The server prunes history older than `server.retention` (90 days) and
marks clusters that stopped pushing stale after `server.staleAfter` (by
default the larger of 2h and three `agent.interval`s, so 2h at the default 10m).
The docs cover [retention, sizing and backups](https://abd-ulbasit.github.io/upgradescope/operations/retention-and-backup/),
[webhooks](https://abd-ulbasit.github.io/upgradescope/reference/webhook/)
and [team-scoped read tokens and putting the dashboard behind an SSO proxy](https://abd-ulbasit.github.io/upgradescope/operations/auth/).

## Exposing the server to remote agents

Every push carries the cluster's ingest token and its full inventory.
**Over plain `http://`, both cross the network in cleartext**: anyone on
the path can read the inventory and replay the token to push as that
cluster (the shared ingest token, as any cluster). Give remote agents an
`https://` URL, with TLS in one of two places:

- **At an Ingress**, as in the hub recipe above: `server.ingress.enabled`,
  `server.ingress.host` and `server.ingress.className`, with
  `server.ingress.tls` on by default (Secret `<fullname>-server-tls`
  unless `server.ingress.tls.secretName`, e.g. filled by cert-manager's
  `cert-manager.io/cluster-issuer` annotation). Without a read token the
  chart closes the read API (reads are 401 until you mint a token), and
  the Ingress refuses to render when `server.allowAnonymousRead` opens it
  without `server.ingress.allowAnonymousRead`.
- **On the server itself**: `server.tls.secretName` (a `kubernetes.io/tls`
  Secret, mounted read-only and passed as `--tls-cert-file` and
  `--tls-key-file`) or `server.tls.certManager.issuerRef`. Probes, the
  Service port's `appProtocol` and the ServiceMonitor switch to HTTPS, and
  `serve` re-reads the pair when the kubelet updates the Secret, so a
  renewal needs no restart. Expose the Service yourself (LoadBalancer, a
  TLS-passthrough Ingress) or combine it with the Ingress above for TLS
  on both hops.

An agent whose server's certificate a private CA issued needs that CA:

    helm install upgradescope deploy/chart -n upgradescope \
      --set agent.serverUrl=https://upgradescope.internal.example \
      --set agent.existingSecret=push-token \
      --set agent.serverCA.configMap=corp-ca   # key ca.crt; or agent.serverCA.secret

The agent gets it as `--server-ca-file` and trusts it on top of its
image's system roots; nothing skips verification. An agent pushing to an
`http://` URL on a host other than loopback logs a warning at startup.

## RBAC: what the agent can do, and why

The agent ClusterRole (`templates/rbac.yaml`) lists every resource it
covers. It has no wildcard groups, resources or verbs, so it never covers
subresources such as `nodes/proxy` (kubelet exec), `pods/log` or
`pods/exec`. Reads are `get`/`list` only; the agent polls and never
watches.

| Rule | Why |
|---|---|
| `get`/`list` namespaces, nodes, pods | Cluster ID (kube-system UID), team labels, kubelet versions, control-plane pods, add-on images |
| `get` `/version`, `/metrics` | Server version; `apiserver_requested_deprecated_apis` (whether any client still calls deprecated APIs, not which) |
| `get`/`list` on each group/resource the KB flags as deprecated or removed | Counting objects still stored at deprecated APIs. Generated into `files/kb-rbac-rules.yaml`; `rbac_test.go` fails when it drifts from the embedded KB |
| `get`/`list` Secrets and ConfigMaps (only with `rbac.helmSecrets=true`, the default) | Helm release detection lists the objects labelled `owner=helm` (Helm's secrets and configmaps storage drivers) metadata-only, then reads one per release. RBAC cannot filter by label or type, so **this lets the agent read every Secret and ConfigMap in the cluster** |
| `get`/`list` Argo CD Applications, Flux HelmReleases and Flux OCIRepositories (only with `rbac.gitops.argocd` and `rbac.gitops.flux`, both off by default) | The charts GitOps tools deploy: chart, version and repository from each Application source and HelmRelease, for add-on detection. Only those resources: no AppProjects, ApplicationSets or the Secrets that hold repository credentials |
| `get`/`update`/`patch` on the CRD `clusterreadinesses.upgradescope.dev` only (only with `agent.manageCRD=true`, the default) | Keeping the CRD schema in step with the agent binary by server-side apply |
| `get`/`list`/`create` clusterreadinesses; `update`/`patch` and status `get`/`update`/`patch` on the one named `agent.crName` | The agent's own results object |

`rbac.helmSecrets=false` removes the Secret and ConfigMap rules. The Helm
capability is then not assessed, with the forbidden lists as the reason,
and the report has no Helm chart findings; everything else works. Releases
kept by Helm's sql driver have no object in the cluster, and charts that Argo CD
renders with `helm template` leave no release, so the Helm chart checks never
see them. With `rbac.gitops.argocd` or `rbac.gitops.flux` the agent reads the
charts those tools declare, so add-ons they deploy are found by chart (what is
read, and what is still not assessed, is on the
[GitOps page](https://abd-ulbasit.github.io/upgradescope/guides/gitops-argo-flux/#charts-your-gitops-tool-deploys));
add-on detection from container images covers the rest. `rbac.create=false` lets you bind a
role of your own; collectors without access degrade the same way.

The agent cannot create CRDs: `crds/` installs the `ClusterReadiness` CRD.
With `agent.manageCRD=true` (the default), a missing CRD makes the agent
exit at startup with an error that says so. With `agent.manageCRD=false`
the agent never reads or writes the CRD itself, so a missing CRD shows up
as a "CRD not installed" error on every tick instead.

`agent.manageCRD=false` drops the CRD write permissions (`get`/`update`/
`patch` on `clusterreadinesses.upgradescope.dev`), e.g. when GitOps manages
the CRD. It does not remove all CRD access: the KB-derived read rule still
grants `get`/`list` on every CRD, because the KB flags
`apiextensions.k8s.io/v1beta1` and the api-usage collector counts CRDs.
With `agent.manageCRD=true`, the agent applies only the fields it owns, so
labels and annotations that Argo CD, Flux or `kubectl apply` set on the
CRD survive restarts.

`deploy/chart/rbac_test.go` renders this role and checks it with the
upstream RBAC rule matcher: every call the collectors make is allowed, and
`nodes/proxy`, `pods/log`, `pods/exec`, other CRDs, other ClusterReadiness
objects, `watch`, and Secrets with the toggle off are denied.

## Target versions

The chart never renders the `ClusterReadiness` object; the agent creates
it. `agent.targets` is passed to the agent as `--targets`, and while it is
non-empty the agent resets `spec.targets` to it every tick, so the Helm
value wins over `kubectl edit`. Values are `MAJOR.MINOR` (`1.37`), the
format the CRD accepts, and at most 8 of them (the CRD's `spec.targets`
cap); the schema rejects `v1.37`, `1.37.2` and a ninth entry. Changing
it with `helm upgrade` touches only the agent Deployment. With
`agent.targets` empty, set targets on the object directly:

    kubectl patch ucr cluster --type merge -p '{"spec":{"targets":["1.37"]}}'

Emptying `agent.targets` after it was set leaves `spec.targets` at the last
list (an empty flag means "leave the object alone"). To return to the
default next minor, clear it with the same command and `"targets":[]`.

Upgrading from a chart version that rendered the object (agent.targets
was set): Helm deletes it as no longer part of the release, and the agent
recreates it with the same targets on a later tick. That can take up to
`agent.interval` (10m by default) when the new agent pod ticks before Helm
deletes the old object.

## Secrets

Tokens, webhook URLs and the webhook key reach the containers as files
mounted from the Secrets (never with `subPath`, which would not update), by
the `*-file` flags, never as arguments, and never appear in a Deployment.
Only the Postgres URL is still an environment variable.

| File flag | Secret key | Source |
|---|---|---|
| `--ingest-token-file` (server) | `ingestToken` | `server.ingestToken`, generated, or `server.existingSecret` (optional key without the agent); none with `server.sharedIngestToken=false` |
| `--read-token-file` (server) | `readToken` | `server.readToken`, or `server.existingSecret` with `server.readTokenFromSecret=true` |
| `--admin-token-file` (server) | `adminToken` | `server.adminToken`, or `server.existingSecret` with `server.adminTokenFromSecret=true` |
| `UPGRADESCOPE_DB_URL` (server, an environment variable) | `server.database.key` | `server.database.existingSecret` (Postgres) |
| `--slack-webhook-file` (server) | `slackWebhook` | `server.slackWebhook`, or optional key of `server.existingSecret` |
| `--webhook-file` (server) | `webhook` | `server.webhook`, or optional key of `server.existingSecret` |
| `--webhook-secret-file` (server) | `webhookSecret` | `server.webhookSecret`, or optional key of `server.existingSecret` |
| `--server-token-file` (agent) | `serverToken`, or the server's `ingestToken` | `agent.existingSecret`, `agent.serverToken`, else the in-chart server's Secret |

`server.existingSecret` alone is enough for a combined install: the server
and the in-chart agent both use its `ingestToken` key. The keys of an
`existingSecret` that may be absent are mounted from a volume that tolerates
a missing key, and `serve` is told so (`--optional-secret-file`).

**Rotating needs no restart.** `serve` and the agent re-read a file when it
changes, checked at most every 5 seconds and only when a request or a push
needs the value. After you change a token, a webhook value
(`helm upgrade --set server.readToken=<new>`) or the contents of a Secret
you named (`server.existingSecret`, `agent.existingSecret`), the new value is
in use once the kubelet has synced the mounted Secret (up to about 60 to 90
seconds at its default settings, from the Kubernetes documentation, not
measured here) plus that check; until then the old value still works. A new
file that is empty or unreadable keeps the old value, and the pod logs an
error naming the file. With `server.replicas` above 1, each replica's kubelet syncs the Secret on
its own schedule, so for up to that window an agent push can get a 401 from
one replica and a 200 from another. A `UPGRADESCOPE_*_TOKEN` in
`server.extraEnv` or `agent.extraEnv` no longer overrides the chart's file (a
file wins over the environment): set the value in the Secret. A restart is
still needed for what is not a mounted
file: the Postgres URL, anything passed in `server.extraEnv`,
`agent.extraEnv`, `server.extraArgs` or `agent.extraArgs`, and a Slack or
webhook URL that was not set when the server started:

```sh
kubectl -n <ns> rollout restart deploy/<fullname>-server deploy/<fullname>-agent
```

(`<fullname>` is the release name plus `-upgradescope`, cut to 63
characters, or the release name alone when it contains `upgradescope`;
the install NOTES print the command with your names).
No pod annotation carries a checksum of a secret, because anyone who can
get pods or Deployments could read it, and test a short token against it
offline. Every key is written under the chart-managed Secret's `data`, so
from this chart version on, removing a value removes its key. The upgrade to this version is the one
exception: a Secret written by an earlier chart wrote `readToken`,
`adminToken`, `slackWebhook`, `webhook`, `webhookSecret` and the agent's
`serverToken` as `stringData`, which the API server turned into `data`, and
Helm cannot remove a `data` key that its previous manifest never listed. So a
value you remove in the same upgrade keeps its key in the Secret. The key is
inert (the pods read it only when the value is set), but to clear it run
`kubectl -n <ns> patch secret <fullname>-server-tokens --type=json -p
'[{"op":"remove","path":"/data/<key>"}]'` (the agent's is
`<fullname>-agent-token`) and check with `kubectl get secret ... -o
jsonpath='{.data}'`; or remove the value in a later upgrade.

## Read API exposure

With `server.enabled=true` and no `server.readToken` (or
`readTokenFromSecret`), the chart passes `--require-read-credential`: the
read API is **closed**, and every read, the dashboard's data and
`/api/v1/gate` included, returns 401 until you mint a read token with
`kubectl exec` (the install notes print the command for your database).
Read tokens live in the database, so a lost PVC, a restored older backup or
`persistence.enabled=false` (on every pod restart) forgets them: the flag
keeps the API closed regardless, and you mint one again. The probes work
without one; `metrics.serviceMonitor` needs `server.readToken` (the render
fails without it) because the ServiceMonitor can send only that token.

`server.allowAnonymousRead=true` opts into an **open** read API instead
(`--allow-anonymous-read`): open while no read credential exists, and open
again whenever the database is lost or restored, to everyone who can reach
the server, so use it only when an authenticating layer in front is the
access control. `server.ingress.allowAnonymousRead=true` is the same
opt-in (it also says an authenticating layer fronts the Ingress, which the
render requires when the read API is open); to rely on minted tokens, set
neither. An open read API answers an anonymous request only when its Host
is the Service's name, `server.ingress.host`, an entry of
`server.allowedHosts`, `localhost` or the pod's address, and a request that
presents a bearer nothing knows gets 401.
`networkPolicy.enabled=true` admits traffic to the server only from this
release's agent and the peers in `networkPolicy.serverIngressFrom`; with
`server.ingress.enabled` or `metrics.serviceMonitor.enabled` the render
fails unless that list names the Ingress controller or Prometheus, which
the policy would otherwise cut off.

The server also serves the web dashboard at `/` (images built from the
repo Dockerfile embed it). Static assets are unauthenticated; every API
call the dashboard makes carries the read token you set in its header
settings, so the read token still protects all data.

## Production settings

- Private registry: `imagePullSecrets: [{name: regcred}]`.
- Scheduling, per component (`agent.*`, `server.*`): `nodeSelector`,
  `tolerations`, `affinity`, `priorityClassName`, `podAnnotations`,
  `podLabels`, `resources`.
- Extra flags, env and mounts: `extraArgs`, `extraEnv`, `extraVolumes`,
  `extraVolumeMounts`.
- Server behind a private CA: `agent.serverCA.configMap` or
  `agent.serverCA.secret` (and `agent.serverCA.key`, default `ca.crt`)
  names the CA's PEM bundle; the agent gets it as `--server-ca-file` and
  trusts it on top of the image's system roots.
- HTTPS for the in-chart server: `server.tls.secretName` (an existing
  `kubernetes.io/tls` Secret) or `server.tls.certManager.issuerRef` (the
  chart renders a cert-manager `Certificate` for the Service names into
  `<fullname>-server-https`, apart from the Ingress's `-server-tls`). The
  in-chart agent then pushes to `https://` and, like the ServiceMonitor,
  trusts the Secret's `ca.crt` (`server.tls.caKey`; empty for a publicly
  trusted certificate whose Secret has no CA); probes and the
  ServiceMonitor use HTTPS, the Service port gets `appProtocol: https`,
  and `server.ingress` gets ingress-nginx's `backend-protocol: HTTPS`
  annotation unless you set it. Without it the agent sends its bearer
  token over plain HTTP inside the cluster and logs a warning saying so.
- `server.sharedIngestToken=false` drops the shared, any-cluster ingest
  token: only per-cluster tokens push, and the in-chart agent needs its own
  (`agent.existingSecret` or `agent.serverToken`).
- Memory: both containers get `GOMEMLIMIT` at 90% of their memory limit
  (the Go runtime does not read the limit itself), from a byte count or a
  quantity up to `E`/`Ei`; `extraEnv` can set it
  instead. The server's 1Gi holds its worst case, measured on SQLite:
  one `/gate` request, one snapshot ingest, one cluster read and the
  re-evaluation pass at their node budgets, with four `server.targets`,
  the most `serve` takes, and notifications configured, two reads of a
  500-cluster fleet, plus their
  buffered bodies and the responses held for their clients. Open
  connections, kernel socket buffers and larger fleets are not in that
  figure
  ([memory and request limits](https://abd-ulbasit.github.io/upgradescope/operations/#memory-and-request-limits)).
  Below about 962Mi, that worst case (~865 MiB of heap) no longer fits
  under `GOMEMLIMIT`. Each `server.targets` entry adds about one report
  of up to the snapshot cap (`--max-snapshot-bytes`, 20 MiB) to an
  ingest and one to the re-evaluation pass, about 54 MiB of the sum per
  extra target, so with fewer targets it needs less: with none, about
  650 MiB, which 768Mi holds.
- OpenShift `restricted-v2`: unset the fixed IDs so the SCC can assign
  them, e.g. `agent.podSecurityContext: {runAsUser: null, runAsGroup: null}`
  and `server.podSecurityContext: {runAsUser: null, runAsGroup: null, fsGroup: null}`.
  `runAsNonRoot` and the `RuntimeDefault` seccomp profile stay.
- The server runs under its own ServiceAccount and mounts no API token.
- Ports: serve listens on `server.containerPort` (8080) in the pod, which
  has no capability to bind a port below 1024. `server.service.port` is
  the Service's own and may be 80 or 443 (`targetPort: http`).
- SQLite needs scratch space: the root filesystem is read-only, and a large
  delete (the daily retention prune, `clusters delete`) spills into a temp
  file. The chart mounts an emptyDir at `/tmp` (`server.tmp.sizeLimit`,
  1Gi, the default PVC size; raise it with the PVC) and sets
  `SQLITE_TMPDIR=/tmp`. A volume of your own mounted at `/tmp`
  (`server.extraVolumeMounts`) takes its place: the chart then adds none,
  and yours must be writable and as large. Postgres servers get neither.
- High availability: with `server.replicas` above 1 (Postgres only) the chart
  renders a PodDisruptionBudget (`server.podDisruptionBudget`, `minAvailable: 1`)
  and a soft topology spread over nodes (`server.defaultTopologySpread`, or
  your own `server.topologySpreadConstraints`, rendered as written).
- Names: only a Service name is cut to 63 characters (a Service name is a
  DNS-1035 label), by shortening the fullname for that name. Every other
  resource keeps `<fullname><suffix>` however long it is, so upgrading a
  release with a long name keeps its PVC and token Secret.
- Upgrading: use `helm upgrade --reset-then-reuse-values` (Helm 3.14+), not
  `--reuse-values`, which keeps the old chart's defaults, the image digest
  among them
  ([Upgrade](https://abd-ulbasit.github.io/upgradescope/operations/upgrade/#the-chart)).
- `values.schema.json` rejects unknown keys, intervals under one minute
  (`agent.interval` takes any Go duration of at least `1m`: `10m`, `90s`,
  `1.5h`), malformed targets (`agent.targets` must be `MAJOR.MINOR`) and
  more than 4 `server.targets` entries at render time (`serve` counts
  distinct minors, so list each once).

## Uninstall

    helm -n upgradescope uninstall upgradescope

Helm leaves CRDs in place by design (`crds/` semantics), and the
`ClusterReadiness` object the agent created stays too. Full removal:

    kubectl delete crd clusterreadinesses.upgradescope.dev

That deletes the CRD and any `ClusterReadiness` objects. Everything else
(Deployments, RBAC, Secrets, Service, PVC) is removed by `helm uninstall`;
the server PVC is deleted with the release because it is chart-managed.

## Values

Generated from the comments in `values.yaml` (`make helm-docs`).

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `agent.affinity` | object | `{}` | — |
| `agent.clusterName` | string | `""` | Human-readable cluster name sent to the server (default: cluster UID). |
| `agent.crName` | string | `"cluster"` | Name of the ClusterReadiness object the agent manages: an RFC 1123 subdomain (dot-separated labels of lowercase letters, digits and -, each starting and ending with a letter or digit; at most 253 bytes). The schema refuses any other name at helm install, as the agent does at start. Changing it creates a new object and leaves the old one behind, because the agent has no delete permission: run kubectl delete ucr &lt;old-name&gt;. |
| `agent.enabled` | bool | `true` | Run the agent in this cluster. false = server-only install (a fleet hub that agents in other clusters push to): no agent Deployment, ServiceAccount, RBAC, push-token Secret, metrics Service or agent alerts. The ClusterReadiness CRD in crds/ is still installed unless you pass --skip-crds. |
| `agent.existingSecret` | string | `""` | Name of an existing Secret with key "serverToken" (preferred over an inline token for anything beyond dev). Mounted as a file, re-read on change: see serverToken. |
| `agent.extraArgs` | list | `[]` | Extra `upgradescope agent` flags, e.g. ["--force-sync-every=2h"]. |
| `agent.extraEnv` | list | `[]` | Extra container env, volumes and mounts. |
| `agent.extraRegistry` | object | `{}` | Extra add-on registry entries, to cover add-ons the built-in registry does not know: a map of `&lt;id&gt;.yaml` file name to the entry's YAML (the schema of registry/CONTRIBUTING.md, every claim cited). Rendered into a ConfigMap and passed as --registry-dir; an entry with a built-in id replaces it, and an invalid entry stops the agent at start, naming the file. The ConfigMap is mounted as the volume `extra-registry`, a name to avoid in extraVolumes. Only the agent uses it: a server that judges what the agent pushes needs the same entries (server.extraArgs and server.extraVolumes). The entries change the knowledge-base version, so a server without them reads every cluster of this agent as unknown (a registry mismatch the agent's upgrade does not clear). |
| `agent.extraVolumeMounts` | list | `[]` | — |
| `agent.extraVolumes` | list | `[]` | — |
| `agent.healthPort` | int | `8081` | Port of the agent's /healthz (liveness), /readyz (a tick succeeded within two intervals) and /metrics (Prometheus) listener, named "http" on the pod. See docs/observability.md. |
| `agent.interval` | string | `"10m"` | Evaluation interval: a Go duration of at least 1m, e.g. 10m, 1h, 1m30s, 300s or 1.5h. |
| `agent.logFormat` | string | `"text"` | Log format, text (logfmt) or json, and level: debug, info, warn, error. The agent logs one line at startup and one per tick. |
| `agent.logLevel` | string | `"info"` | — |
| `agent.manageCRD` | bool | `true` | manage-crd). Grants get/update/patch on that one CRD only. The CRD itself is installed by the chart's crds/ directory; set false when the CRD is managed elsewhere (e.g. GitOps) to drop the CRD write permissions (get/update/patch on clusterreadinesses.upgradescope.dev). The KB-derived read rule still allows get/list on all CRDs (the KB flags apiextensions.k8s.io/v1beta1), whatever this is set to. |
| `agent.nodeSelector` | object | `{}` | — |
| `agent.podAnnotations` | object | `{}` | — |
| `agent.podLabels` | object | `{}` | app.kubernetes.io/name, instance and component are reserved for the selector and ignored here. |
| `agent.podSecurityContext` | object | `{"runAsGroup":65532,"runAsNonRoot":true,"runAsUser":65532,"seccompProfile":{"type":"RuntimeDefault"}}` | Pod and container security contexts, merged over these defaults. For OpenShift's restricted-v2 SCC, which assigns the UID itself, unset the IDs: podSecurityContext: {runAsUser: null, runAsGroup: null}. |
| `agent.priorityClassName` | string | `""` | — |
| `agent.resources` | object | `{"limits":{"cpu":"1","memory":"256Mi"},"requests":{"cpu":"50m","memory":"64Mi"}}` | The memory limit also sets GOMEMLIMIT to 90% of it (the Go runtime does not read the container limit itself); set GOMEMLIMIT in extraEnv to override. Without a memory limit, or with one the chart cannot read (e.g. "100m"), the chart sets none; the binary then takes 90% of its cgroup limit itself, when it has one. The CPU limit is a cgroup quota, and a tick is CPU-bound: the first tick after a start reads every Helm release once (the whole first tick took about 23 CPU-seconds at 1,000 releases) and must finish that step in its deadline, at most 54 s at the default interval (59 s before the tick reserve; the GitOps reads, with rbac.gitops.* on, share it). Measured, with the 59 s, against 2,000 fake nodes and 1,001 releases (1,000 and the chart's own; docs/operations/scale.md): at 200m the first tick stopped at that deadline with 132 releases unread and a steady tick took 21 s; at 500m the first tick took 48 s, its Helm step within the 59 s; at 1 CPU the tick took 35 s and a steady tick 7 s. So the limit is 1 CPU. For scheduling it costs nothing while the agent idles (the request is what the scheduler reserves), but a ResourceQuota on limits.cpu still counts the limit, and a LimitRange whose max CPU is below 1 rejects it: in such a namespace set limits.cpu yourself (it was 200m through v0.2.0-rc.2, so 800m more quota). The Helm step took about 28 s of those 35 s and now has at most 54 s at the default interval (the tick reserve), so by that arithmetic (computed, not measured) 1 CPU holds to about 1,900 releases, about 1,600 with rbac.gitops.* on; give a larger cluster more, or a longer agent.interval. |
| `agent.securityContext.allowPrivilegeEscalation` | bool | `false` | — |
| `agent.securityContext.capabilities.drop[0]` | string | `"ALL"` | — |
| `agent.securityContext.readOnlyRootFilesystem` | bool | `true` | — |
| `agent.serverCA.configMap` | string | `""` | A server behind a private CA: name a ConfigMap (configMap) or a Secret (secret), not both, whose key holds the CA's PEM bundle. It is mounted read-only and passed as --server-ca-file: pushes trust it on top of the image's system roots (read at agent startup). Set, it replaces the in-chart server's server.tls.caKey. Needs an https server to push to (an https agent.serverUrl, or server.enabled with server.tls). |
| `agent.serverCA.key` | string | `"ca.crt"` | The key of the PEM bundle in that ConfigMap or Secret. |
| `agent.serverCA.secret` | string | `""` | Or a Secret holding the CA bundle (see configMap). |
| `agent.serverToken` | string | `""` | Bearer token for snapshot pushes, stored in a chart Secret. Ignored when existingSecret is set. When both are empty and server.enabled=true, the agent uses the in-chart server's ingest token (key ingestToken of its Secret, generated or server.existingSecret). The token reaches the agent as a file mounted from the Secret (--server-token-file), never as an argument or an environment variable, and the agent re-reads it when it changes, so rotating it needs no restart: the new token is used by the first push after the kubelet has synced the mounted Secret (up to about 60 to 90s at its default settings) plus a 5s check. A push in between may get a 401 and is retried at the next tick. A new file that is empty or unreadable keeps the old token (the agent logs an error naming the file). |
| `agent.serverUrl` | string | `""` | URL of an upgradescope server to push snapshots to. Empty = CRD-only mode. If server.enabled=true and this is empty, it defaults to the in-chart server Service. |
| `agent.targets` | list | `[]` | Target Kubernetes minors to evaluate, MAJOR.MINOR only (the CRD's format), e.g. ["1.37", "1.38"], at most 8 entries (the CRD's spec.targets cap, which the agent also enforces on distinct minors; the chart counts entries, so list each minor once), passed to the agent as --targets. The chart never renders the ClusterReadiness object: the agent creates it and, when targets is non-empty, resets spec.targets to this list on every tick, so this value wins over `kubectl edit`. Empty = the agent leaves spec.targets alone; set them with `kubectl patch ucr cluster --type merge -p '{"spec":{"targets":["1.37"]}}'` or let it default to the next minor above the server version. Emptying this after it was set does NOT clear spec.targets (the CR keeps the last list); to go back to the default next minor, run `kubectl patch ucr cluster --type merge -p '{"spec":{"targets":[]}}'`. |
| `agent.teamLabel` | string | `"team"` | Namespace label used for team attribution. |
| `agent.tolerations` | list | `[]` | — |
| `clusterDomain` | string | `"cluster.local"` | The cluster's DNS domain (the kubelet's clusterDomain): the server Service's fully qualified name, &lt;fullname&gt;-server.&lt;namespace&gt;.svc.&lt;this&gt; (the Service name cut to 63 characters for a long release), is passed to the server as an --allowed-host and named in the cert-manager Certificate. Set it when your cluster does not use cluster.local. |
| `image.digest` | string | `""` | sha256:&lt;64 hex&gt;. When set and tag is empty, the pods pull repository:&lt;appVersion&gt;@digest, so the image cannot change under the tag. The published chart sets it to the digest of the image released with it. It is not applied when you set tag (it is the appVersion image's digest); to pin another tag, write tag: vX.Y.Z@sha256:&lt;64 hex&gt;. With another repository and an empty tag it still applies: set it to "" unless that repository mirrors the same image. |
| `image.pullPolicy` | string | `"IfNotPresent"` | The default image is pinned by digest in the published chart, so IfNotPresent always runs the released bytes. |
| `image.repository` | string | `"ghcr.io/abd-ulbasit/upgradescope"` | — |
| `image.tag` | string | `""` | Empty = the chart's appVersion, i.e. the release this chart was published with. |
| `imagePullSecrets` | list | `[]` | Pull secrets for a private registry or mirror, set on both pods, e.g. [{name: regcred}]. |
| `metrics` | object | `{"grafanaDashboard":{"annotations":{},"enabled":false,"labels":{"grafana_dashboard":"1"}},"prometheusRule":{"clusterStaleAfterSeconds":0,"enabled":false,"labels":{}},"serviceMonitor":{"enabled":false,"interval":"1m","labels":{}}}` | Prometheus Operator and Grafana integration, all off by default. The metrics themselves are always served: the agent's on agent.healthPort, the server's on its Service port (behind the read token when one is set). docs/observability.md lists every metric; the alerts are in docs/guides/prometheus-grafana.md. |
| `metrics.grafanaDashboard.annotations` | object | `{}` | e.g. {grafana_folder: Kubernetes} for the sidecar's folder annotation. |
| `metrics.grafanaDashboard.enabled` | bool | `false` | Ship deploy/grafana/upgradescope-dashboard.json as a ConfigMap for the Grafana sidecar, which loads ConfigMaps labeled grafana_dashboard: "1" by default. |
| `metrics.prometheusRule.clusterStaleAfterSeconds` | int | `0` | A cluster whose agent has not pushed for this long is stale. 0 = the server's own threshold (server.staleAfter, by default the larger of 2h and three agent intervals), so the alert and the stale flag agree. Set it only for a different alert threshold: agents push on every change and at least hourly (--force-sync-every) and at most once per interval, so keep it above max(agent.interval, 1h) plus one interval (4200 at the default 10m; the install notes warn below that). The render fails when it is not above agent.interval. |
| `metrics.prometheusRule.enabled` | bool | `false` | Render a monitoring.coreos.com/v1 PrometheusRule: upgrade blocked, verdict unknown for over 1h, agent not ticking and (with server.enabled) a cluster that stopped pushing and, unless server.retention is 0, no retention prune completed for 2 days (UpgradescopeRetentionStale). Scrape the metrics with serviceMonitor or your own config: the rules select the jobs serviceMonitor produces, &lt;fullname&gt;-agent-metrics and &lt;fullname&gt;-server (shortened to fit 63 characters for long release names; upgradescope-agent-metrics and upgradescope-server for a release named upgradescope). |
| `metrics.serviceMonitor.enabled` | bool | `false` | Render monitoring.coreos.com/v1 ServiceMonitors for the agent (plus a headless Service for its metrics port) and, with server.enabled, the server. Needs the Prometheus Operator CRDs. |
| `metrics.serviceMonitor.labels` | object | `{}` | Labels your Prometheus selects ServiceMonitors by, e.g. {release: kube-prometheus-stack}. |
| `networkPolicy.enabled` | bool | `false` | Render a NetworkPolicy that admits traffic to the server only from this release's agent pods and the peers below (no effect without server.enabled, or on a CNI that does not enforce NetworkPolicy). With server.ingress.enabled or metrics.serviceMonitor.enabled the render fails unless serverIngressFrom lists the ingress controller or Prometheus: the policy would otherwise cut them off. |
| `networkPolicy.serverIngressFrom` | list | `[]` | Extra NetworkPolicyPeer entries allowed to reach the server, e.g. the ingress controller or the CI runners that call /api/v1/gate. The server's /metrics is on the same port, so list Prometheus here when metrics.serviceMonitor is enabled:   - namespaceSelector: {matchLabels: {kubernetes.io/metadata.name: ingress-nginx}} |
| `rbac.create` | bool | `true` | Create the agent ClusterRole/ClusterRoleBinding. Every rule is listed and explained in templates/rbac.yaml and the chart README: get/list on namespaces, nodes, pods and the API group/resources the embedded KB flags as deprecated; get on /version and /metrics; writes only to the ClusterReadiness CR named agent.crName (and, with agent.manageCRD, to the clusterreadinesses.upgradescope.dev CRD; get/list on CRDs comes from the KB rules either way). No wildcards, no watch, no subresources such as nodes/proxy or pods/log. |
| `rbac.gitops.argocd` | bool | `false` | Get/list on Argo CD Applications (argoproj.io/applications, nothing else of that group), to read the chart, repoURL and targetRevision of each chart source: add-ons Argo CD deploys (ingress-nginx, say) are then found by their chart, with no Helm release. Off, with Argo CD installed: the Helm capability is reported partial, naming the forbidden list. Chart kubeVersion and stored-manifest checks stay unavailable for Argo CD either way (helm template leaves no release). |
| `rbac.gitops.flux` | bool | `false` | Get/list on Flux HelmReleases (helm.toolkit.fluxcd.io/helmreleases) and OCIRepositories (source.toolkit.fluxcd.io/ocirepositories, for a HelmRelease chartRef), to read the chart each HelmRelease deploys. Off, with Flux installed: the Helm capability is reported partial, naming the forbidden list. |
| `rbac.helmSecrets` | bool | `true` | Cluster-wide get/list on Secrets and ConfigMaps, for Helm release detection (releases stored by Helm's secrets and configmaps drivers). RBAC cannot filter Secrets by label or type, so true means the agent can read EVERY Secret and ConfigMap in the cluster. false removes both rules; the Helm capability is then not assessed (the reason is the forbidden lists) and Helm chart findings are missing from the report. Releases in Helm's sql driver have no object in the cluster, and charts that Argo CD renders with helm template have no release object either; add-on detection from container images still covers them, and rbac.gitops reads the charts GitOps tools declare. |
| `server.adminToken` | string | `""` | Bearer token for cluster administration: deleting and renaming clusters (DELETE/PATCH /api/v1/clusters/{id}, `upgradescope clusters delete` and `rename`, with --server). Empty = both are refused. It must differ from the read and ingest tokens. Stored in the chart Secret; ignored when existingSecret is set (use adminTokenFromSecret). |
| `server.adminTokenFromSecret` | bool | `false` | With existingSecret: enable cluster administration with its adminToken key. |
| `server.affinity` | object | `{}` | — |
| `server.allowAnonymousRead` | bool | `false` | Open the read API when readToken is empty (the chart then passes serve --allow-anonymous-read): reads need no credential while none exists, and a lost or restored database, or persistence.enabled=false on every pod restart, makes the API open again for everyone who can reach the server. Leave false and mint read tokens unless an authenticating layer in front is the access control. An open read API answers a request that names a host (Host header) outside the Service names, server.ingress.host and server.allowedHosts with 421, and a request that presents an unknown bearer with 401. With a readToken set this has no effect. See docs/operations/auth.md. |
| `server.allowedHosts` | list | `[]` | Extra host names (or IPs) requests may name in their Host header, passed as --allowed-host with the Service's DNS names and server.ingress.host, which the chart always passes. serve checks the Host when it trusts a proxy's team header (--trust-team-header in extraArgs) or listens on loopback, and answers any other name 421 (DNS rebinding); list here any other name clients use, such as a Gateway's host. Names only, any port matches; probes and scrapes of the pod's IP need nothing. |
| `server.containerPort` | int | `8080` | The port serve listens on in the pod, named "http". It is not the Service port: the pod runs as a non-root user with no capabilities, which cannot bind a port below 1024 on runtimes that keep net.ipv4.ip_unprivileged_port_start at 1024 (containerd 1.x, CRI-O), so this stays 1024 or above while service.port may be 80 or 443. |
| `server.database.existingSecret` | string | `""` | Name of an existing Secret holding a Postgres URL (postgres://user:pass@host:5432/db?sslmode=require). Set = Postgres: the URL reaches serve as $UPGRADESCOPE_DB_URL (never an argument), no PVC is rendered, and replicas may be more than 1. Empty = SQLite on the persistence volume. |
| `server.database.key` | string | `"url"` | Key of the URL in that Secret. |
| `server.defaultTopologySpread` | bool | `true` | Spread the server pods across nodes (soft: topologySpreadConstraints, kubernetes.io/hostname, ScheduleAnyway, so a one-node cluster still schedules them all) when replicas is above 1. Ignored when topologySpreadConstraints is set. |
| `server.enabled` | bool | `false` | Run the upgradescope server in-cluster too (single-cluster combined install). The agent will push to it automatically. |
| `server.existingSecret` | string | `""` | Name of an existing Secret to use instead of the chart's. Keys:   ingestToken    the shared push token; the in-chart agent pushes with                  it. Required with the agent, optional without it (a                  fleet hub can accept per-cluster tokens only)   readToken      read when readTokenFromSecret=true   adminToken     read when adminTokenFromSecret=true   slackWebhook   optional; Slack notifications when present   webhook        optional; generic webhook when present   webhookSecret  optional; signs generic webhook requests when present Each key is mounted as a file (without subPath) and re-read when the Secret changes, so editing it rotates the value without a restart (see ingestToken for the latency). A key that may be absent and is added later is picked up for the ingest token and the webhook key. Deleting a key is not how you revoke a token: only the ingestToken key of a hub with no in-chart agent (agent.enabled=false) is optional, and deleting it stops that token working. With the in-chart agent the key is required, and deleting it does NOT revoke the token: the kubelet leaves a volume whose required item is missing as it was, so the old token stays in service and the other rotations in that Secret stop arriving until the key is back. Rotate the value instead. Emptying a key, or deleting another optional one, keeps the old value. |
| `server.extraArgs` | list | `[]` | allow-anonymous-read, on either set false when it is the one the chart passes, and on both set true; serve refuses the pair. The other flag set false is accepted. |
| `server.extraEnv` | list | `[]` | — |
| `server.extraVolumeMounts` | list | `[]` | — |
| `server.extraVolumes` | list | `[]` | — |
| `server.ingestToken` | string | `""` | Shared bearer token agents must present on POST /api/v1/snapshots. Empty = the chart generates a random 40-character token into its Secret on first install and keeps it on upgrades (Helm lookup). NOTES.txt prints the command to read it back for agents in other clusters (Secret &lt;fullname&gt;-server-tokens, e.g. upgradescope-server-tokens). Renderers without cluster access (helm template, Argo CD) cannot look the old token up and generate a new one on every render: under GitOps set this or existingSecret. Ignored when existingSecret is set. This and the other tokens, webhook URLs and the webhook key of the server (and the agent's push token) reach the pods as files mounted from the Secret, never as arguments or environment variables, and serve re-reads a file when it changes. Rotating one (helm upgrade, or editing the Secret you named) needs no restart: it takes effect once the kubelet has synced the mounted Secret (up to about 60 to 90s at its default settings) plus a 5s check, and until then the old value still works. An empty or unreadable new file keeps the old value (serve logs an error naming the file). Not rotated this way: the Postgres URL (server.database), and anything passed in extraEnv or extraArgs; those need `kubectl rollout restart` (the install notes print it). A Slack or webhook URL is used by the server only if it was set when the server started; one added later needs a restart, one changed does not. With replicas above 1, each replica's kubelet syncs the Secret on its own schedule, so for up to that window an agent push can get a 401 from one replica and a 200 from another. A UPGRADESCOPE_*_TOKEN in extraEnv no longer overrides the file the chart passes (a file wins over the environment): set the value in the Secret. |
| `server.ingress.allowAnonymousRead` | bool | `false` | Opts into an OPEN read API through the Ingress, as server.allowAnonymousRead does (it is the same opt-in, kept from before that value existed): set true only when an authenticating layer (oauth2-proxy through ingress annotations, an identity-aware proxy) fronts it. Do not set it to rely on minted read tokens: that leaves the read API open whenever the database is lost or restored. The render fails when server.allowAnonymousRead opens the read API and this is false. See docs/operations/auth.md. |
| `server.ingress.annotations` | object | `{}` | — |
| `server.ingress.className` | string | `""` | — |
| `server.ingress.enabled` | bool | `false` | Expose the server (API, dashboard, snapshot ingest) through an Ingress. Set a read token (or mint one: the read API is closed until then). Agents push to https://&lt;host&gt;. |
| `server.ingress.host` | string | `""` | — |
| `server.ingress.path` | string | `"/"` | — |
| `server.ingress.pathType` | string | `"Prefix"` | — |
| `server.ingress.tls.enabled` | bool | `true` | TLS for host, from a kubernetes.io/tls Secret (e.g. one cert-manager issues via annotations). Default name: &lt;fullname&gt;-server-tls. |
| `server.ingress.tls.secretName` | string | `""` | — |
| `server.nodeSelector` | object | `{}` | — |
| `server.persistence.enabled` | bool | `true` | PVC for the SQLite database (unused with database.existingSecret). false = emptyDir (history lost on pod restart — demo only). |
| `server.persistence.size` | string | `"1Gi"` | — |
| `server.persistence.storageClass` | string | `""` | — |
| `server.podAnnotations` | object | `{}` | — |
| `server.podDisruptionBudget.enabled` | bool | `true` | Render a PodDisruptionBudget for the server when replicas is above 1 (with one replica minAvailable 1 would block every node drain). |
| `server.podDisruptionBudget.maxUnavailable` | string | `""` | Set instead of minAvailable (a count, 0 included, or a percentage such as 50%); when set, minAvailable is not rendered. With both empty the render fails. |
| `server.podDisruptionBudget.minAvailable` | int | `1` | Pods that must stay up during a voluntary disruption (a node drain): a count or a percentage such as 50%. |
| `server.podLabels` | object | `{}` | app.kubernetes.io/name, instance and component are reserved for the selector and ignored here. |
| `server.podSecurityContext` | object | `{"fsGroup":65532,"runAsGroup":65532,"runAsNonRoot":true,"runAsUser":65532,"seccompProfile":{"type":"RuntimeDefault"}}` | As agent.podSecurityContext; fsGroup makes the data volume writable. OpenShift restricted-v2: {runAsUser: null, runAsGroup: null, fsGroup: null}. |
| `server.priorityClassName` | string | `""` | — |
| `server.readToken` | string | `""` | Optional fleet-wide bearer token for the read API. EMPTY = the chart closes the read API (serve --require-read-credential): every read is 401 until you mint a read token with `kubectl exec ... upgradescope tokens create --read --teams '*'` (the install notes print the command), and a lost or restored database cannot reopen it. Opt into an OPEN read API with allowAnonymousRead. With existingSecret the value itself is not used; a non-empty one acts like readTokenFromSecret=true. |
| `server.readTokenFromSecret` | bool | `false` | With existingSecret: protect the read API with its readToken key. |
| `server.replicas` | int | `1` | Server Pods. More than one needs a shared database (database below): SQLite on a ReadWriteOnce volume has a single writer. With more than one the chart also renders a PodDisruptionBudget (podDisruptionBudget) and spreads the pods across nodes (defaultTopologySpread). |
| `server.resources` | object | `{"limits":{"cpu":"500m","memory":"1Gi"},"requests":{"cpu":"100m","memory":"128Mi"}}` | Sets GOMEMLIMIT as agent.resources does (~921MiB for 1Gi). 1Gi holds one /gate request, one snapshot ingest, one cluster read and the re-evaluation pass at their worst with four server.targets, the most serve takes, and notifications configured, measured on SQLite (~176, ~216, ~133 and ~198 MiB of heap, with every report at most the snapshot cap), two reads of a 500-cluster fleet (~16 MiB each), plus the buffered /gate and snapshot bodies (30 and 40 MiB) and the responses held for their clients (one 40 MiB budget): ~865 MiB of heap, which a limit below about 962Mi does not fit. Each server.targets entry adds about one report of up to the snapshot cap to an ingest and one to the re-evaluation pass (~54 MiB of the sum per extra target at the default 20 MiB cap), so with fewer targets it needs less: with none, about 650 MiB, which 768Mi holds. Open connections (uncapped, ~18 KiB each), kernel socket buffers, larger fleets and snapshots a v0.1 server stored are outside it: see docs/operations.md. |
| `server.retention` | string | `"90d"` | History older than this is pruned at startup and daily, except each cluster's latest snapshot and its evaluations: whole days (90d) or a Go duration (2160h); 0 keeps everything (as "0" or --set ...retention=0). A prune that fails is counted (upgradescope_retention_prune_failures_total) and, with metrics.prometheusRule, alerted on; /readyz is not affected. |
| `server.securityContext.allowPrivilegeEscalation` | bool | `false` | — |
| `server.securityContext.capabilities.drop[0]` | string | `"ALL"` | — |
| `server.securityContext.readOnlyRootFilesystem` | bool | `true` | — |
| `server.service.port` | int | `8080` | The Service's port (targetPort: the container's "http"), any value. Agents in this release push to it. |
| `server.service.type` | string | `"ClusterIP"` | — |
| `server.serviceAccount.create` | bool | `true` | The server never calls the Kubernetes API: it gets its own ServiceAccount and its pod mounts no API token. create=false with an empty name runs it as the namespace's default ServiceAccount (still without a mounted token). |
| `server.serviceAccount.name` | string | `""` | — |
| `server.sharedIngestToken` | bool | `true` | Serve the shared ingest token at all. It may push as ANY cluster; false leaves only per-cluster tokens (`upgradescope tokens create &lt;cluster&gt;`, each bound to one cluster name): the chart Secret holds no ingestToken, serve gets none, and the in-chart agent needs its own token (agent.existingSecret or agent.serverToken). A Secret written by chart v0.2.0-rc.2 or earlier keeps its ingestToken key after you switch this off (those versions wrote it as stringData, which the API server turns into data and never removes); the server no longer accepts that token, but remove the key once with `kubectl -n &lt;namespace&gt; patch secret &lt;fullname&gt;-server-tokens --type=json -p '[{"op":"remove","path":"/data/ingestToken"}]'`. Later versions remove it on upgrade; check with `kubectl get secret ... -o jsonpath='{.data}'`. |
| `server.slackWebhook` | string | `""` | Optional Slack incoming-webhook URL for finding-delta notifications. Stored in the chart Secret and mounted as a file (--slack-webhook-file); it never appears in the Deployment. Ignored when existingSecret is set. |
| `server.staleAfter` | string | `""` | A cluster whose agent has not pushed for this long is marked stale in the API, the dashboard data and /metrics. Empty = the larger of 2h and three times agent.interval (2h for any interval up to 40m): an unchanged cluster pushes at its next tick after the hourly force-sync, so the gap between pushes is about max(interval, 1h) plus a tick. A value you set must be above agent.interval or the render fails, and the install notes warn when it is below max(interval, 1h) plus a tick; it also covers clusters in other regions that push to this server, whose own intervals the chart cannot see, so keep it above the longest of them. The UpgradescopeClusterStale alert follows it (metrics.prometheusRule). |
| `server.targets` | list | `[]` | Extra targets evaluated on every accepted snapshot, e.g. ["1.37","1.38"]: at most 4 entries (serve --targets takes 4 distinct minors; the chart counts entries, so list each minor once). Each adds about one report of up to the snapshot cap to every push and to the re-evaluation pass, and server.resources is sized for 4. |
| `server.teamMap` | list | `[]` | Namespace→team overrides applied before every evaluation, rendered into a ConfigMap and passed as --team-map; the first matching glob wins, e.g. [{pattern: "payments-*", team: payments}]. |
| `server.tls.caKey` | string | `"ca.crt"` | The key of the CA certificate in that Secret, which the ServiceMonitor and the in-chart agent trust (cert-manager CA and self-signed issuers write ca.crt). Empty for a publicly trusted certificate whose Secret has no CA: they then use their system roots. |
| `server.tls.certManager.issuerRef` | object | `{}` | Or have cert-manager issue one for the Service names into &lt;fullname&gt;-server-https (not the Ingress's &lt;fullname&gt;-server-tls), e.g. {name: cluster-ca, kind: ClusterIssuer}. Needs the cert-manager CRDs. |
| `server.tls.secretName` | string | `""` | HTTPS on the server's own port, from an existing kubernetes.io/tls Secret (tls.crt, tls.key, optional CA). Without it the in-chart agent pushes its bearer token over plain HTTP inside the cluster (the agent logs a warning). The certificate must name the Service (&lt;server Service name&gt;.&lt;namespace&gt;.svc: &lt;fullname&gt;-server, or the shortened name when the release name is long); the in-chart agent then pushes to https:// and trusts the Secret's caKey, when it has one, on top of its image's roots. Probes and the ServiceMonitor switch to HTTPS. The Service port gets appProtocol https, and server.ingress gets nginx.ingress.kubernetes.io/backend-protocol: HTTPS unless its annotations set it; another controller may need its own annotation. serve re-reads the pair when the kubelet updates the mounted Secret (usually within a minute or two of a renewal), so a renewal needs no restart. |
| `server.tmp.sizeLimit` | string | `"1Gi"` | SQLite only (unused with database.existingSecret). The root filesystem is read-only, and SQLite spills a large delete (the daily retention prune, `clusters delete`) into a temp file. The chart mounts an emptyDir at /tmp and sets SQLITE_TMPDIR to it. Measured on a 60 MB backlog, a delete worked with a temp directory and failed with "disk I/O error (6410)" without one (docs/operations/retention-and-backup.md); it should need no more than the database's size (reasoning, not measured), so the default matches the default persistence.size: raise it with that. An emptyDir that outgrows its limit evicts the pod. It is node ephemeral storage, gone with the pod. The volume is named sqlite-tmp. A volume of your own mounted at /tmp (extraVolumeMounts) replaces it: the chart then adds no temp volume, ignores this limit and keeps SQLITE_TMPDIR=/tmp, so yours must be writable and as large. Without one, an extraVolumes entry named sqlite-tmp fails the render (two volumes of one name). |
| `server.tolerations` | list | `[]` | — |
| `server.topologySpreadConstraints` | list | `[]` | Your own topologySpreadConstraints for the server pods, rendered as written (set labelSelector yourself; the pods carry app.kubernetes.io/name: upgradescope, app.kubernetes.io/instance: &lt;release&gt; and app.kubernetes.io/component: server). Replaces the default spread. |
| `server.webhook` | string | `""` | Optional generic webhook URL: POSTed one versioned JSON notification per cluster and evaluation pass (schema in docs/reference/webhook.md). Stored like slackWebhook (--webhook-file). |
| `server.webhookSecret` | string | `""` | Optional HMAC-SHA256 key: webhook requests then carry X-Upgradescope-Signature: sha256=&lt;hex&gt;. Stored like slackWebhook (--webhook-secret-file). |
| `serviceAccount.create` | bool | `true` | Create the agent ServiceAccount. Set false to bring your own (set name). |
| `serviceAccount.name` | string | `""` | — |
