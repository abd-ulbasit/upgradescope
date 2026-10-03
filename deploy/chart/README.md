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
marks clusters that stopped pushing stale after `server.staleAfter` (2h).
The docs cover [retention, sizing and backups](https://abd-ulbasit.github.io/upgradescope/operations/retention-and-backup/),
[webhooks](https://abd-ulbasit.github.io/upgradescope/reference/webhook/)
and [putting the dashboard behind an SSO proxy](https://abd-ulbasit.github.io/upgradescope/operations/tenancy/).

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
| `get`/`update`/`patch` on the CRD `clusterreadinesses.upgradescope.dev` only (only with `agent.manageCRD=true`, the default) | Keeping the CRD schema in step with the agent binary by server-side apply |
| `get`/`list`/`create` clusterreadinesses; `update`/`patch` and status `get`/`update`/`patch` on the one named `agent.crName` | The agent's own results object |

`rbac.helmSecrets=false` removes the Secret and ConfigMap rules. The Helm
capability is then not assessed, with the forbidden lists as the reason,
and the report has no Helm chart findings; everything else works. Releases
kept by Helm's sql driver, and charts that GitOps tools render with
`helm template` (Argo CD), have no release object in the cluster, so Helm
chart checks never see them; add-on detection from container images still
does. `rbac.create=false` lets you bind a
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
format the CRD accepts; the schema rejects `v1.37` and `1.37.2`. Changing
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

Tokens and webhook URLs reach the containers only as environment variables
from `secretKeyRef`, never as arguments, and never appear in a Deployment.

| Env var | Secret key | Source |
|---|---|---|
| `UPGRADESCOPE_INGEST_TOKEN` (server) | `ingestToken` | `server.ingestToken`, generated, or `server.existingSecret` (optional key without the agent); none with `server.sharedIngestToken=false` |
| `UPGRADESCOPE_READ_TOKEN` (server) | `readToken` | `server.readToken`, or `server.existingSecret` with `server.readTokenFromSecret=true` |
| `UPGRADESCOPE_ADMIN_TOKEN` (server) | `adminToken` | `server.adminToken`, or `server.existingSecret` with `server.adminTokenFromSecret=true` |
| `UPGRADESCOPE_DB_URL` (server) | `server.database.key` | `server.database.existingSecret` (Postgres) |
| `UPGRADESCOPE_SLACK_WEBHOOK` (server) | `slackWebhook` | `server.slackWebhook`, or optional key of `server.existingSecret` |
| `UPGRADESCOPE_WEBHOOK_URL` (server) | `webhook` | `server.webhook`, or optional key of `server.existingSecret` |
| `UPGRADESCOPE_WEBHOOK_SECRET` (server) | `webhookSecret` | `server.webhookSecret`, or optional key of `server.existingSecret` |
| `UPGRADESCOPE_SERVER_TOKEN` (agent) | `serverToken`, or the server's `ingestToken` | `agent.existingSecret`, `agent.serverToken`, else the in-chart server's Secret |

`server.existingSecret` alone is enough for a combined install: the server
and the in-chart agent both use its `ingestToken` key.

## Read API exposure

With `server.enabled=true` and no read token, the read API is
**unauthenticated** behind the ClusterIP Service. Set `server.readToken`
(or `readTokenFromSecret`) before exposing it via Ingress/LoadBalancer;
`server.ingress.enabled` refuses to render without one unless
`server.ingress.allowAnonymousRead=true` says an authenticating layer
(oauth2-proxy, an identity-aware proxy) fronts it.
`networkPolicy.enabled=true` admits traffic to the server only from this
release's agent and the peers in `networkPolicy.serverIngressFrom`.

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
- Server behind a private CA: mount the CA bundle into the agent and set
  `SSL_CERT_DIR` to its directory; Go adds those certificates to the
  image's system roots (example in `values.yaml`).
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
| `agent.crName` | string | `"cluster"` | Name of the ClusterReadiness object the agent manages: an RFC 1123 subdomain (lowercase letters, digits, - and .; at most 253 bytes). Changing it creates a new object and leaves the old one behind, because the agent has no delete permission: run kubectl delete ucr &lt;old-name&gt;. |
| `agent.enabled` | bool | `true` | Run the agent in this cluster. false = server-only install (a fleet hub that agents in other clusters push to): no agent Deployment, ServiceAccount, RBAC, push-token Secret, metrics Service or agent alerts. The ClusterReadiness CRD in crds/ is still installed unless you pass --skip-crds. |
| `agent.existingSecret` | string | `""` | Name of an existing Secret with key "serverToken" (preferred over an inline token for anything beyond dev). The token reaches the agent as $UPGRADESCOPE_SERVER_TOKEN from a secretKeyRef, never as an argument. |
| `agent.extraArgs` | list | `[]` | Extra `upgradescope agent` flags, e.g. ["--force-sync-every=2h"]. |
| `agent.extraEnv` | list | `[]` | Extra container env, volumes and mounts. Private CA for pushes to a server behind a corporate CA: mount the PEM bundle and point SSL_CERT_DIR at it (Go adds those certificates to the image's roots):   extraVolumes: [{name: ca, configMap: {name: corp-ca}}]   extraVolumeMounts: [{name: ca, mountPath: /etc/upgradescope/ca, readOnly: true}]   extraEnv: [{name: SSL_CERT_DIR, value: /etc/upgradescope/ca}] |
| `agent.extraVolumeMounts` | list | `[]` | — |
| `agent.extraVolumes` | list | `[]` | — |
| `agent.healthPort` | int | `8081` | Port of the agent's /healthz (liveness), /readyz (a tick succeeded within two intervals) and /metrics (Prometheus) listener, named "http" on the pod. See docs/observability.md. |
| `agent.interval` | string | `"10m"` | Evaluation interval: a Go duration of at least 1m, e.g. 10m, 1h, 1m30s, 300s or 1.5h. |
| `agent.logFormat` | string | `"text"` | Log format, text (logfmt) or json, and level: debug, info, warn, error. The agent logs one line at startup and one per tick. |
| `agent.logLevel` | string | `"info"` | — |
| `agent.manageCRD` | bool | `true` | Keep the ClusterReadiness CRD schema in step with the agent binary at startup (agent --manage-crd). Grants get/update/patch on that one CRD only. The CRD itself is installed by the chart's crds/ directory; set false when the CRD is managed elsewhere (e.g. GitOps) to drop the CRD write permissions (get/update/patch on clusterreadinesses.upgradescope.dev). The KB-derived read rule still allows get/list on all CRDs (the KB flags apiextensions.k8s.io/v1beta1), whatever this is set to. |
| `agent.nodeSelector` | object | `{}` | — |
| `agent.podAnnotations` | object | `{}` | — |
| `agent.podLabels` | object | `{}` | app.kubernetes.io/name, instance and component are reserved for the selector and ignored here. |
| `agent.podSecurityContext` | object | `{"runAsGroup":65532,"runAsNonRoot":true,"runAsUser":65532,"seccompProfile":{"type":"RuntimeDefault"}}` | Pod and container security contexts, merged over these defaults. For OpenShift's restricted-v2 SCC, which assigns the UID itself, unset the IDs: podSecurityContext: {runAsUser: null, runAsGroup: null}. |
| `agent.priorityClassName` | string | `""` | — |
| `agent.resources` | object | `{"limits":{"cpu":"200m","memory":"256Mi"},"requests":{"cpu":"50m","memory":"64Mi"}}` | The memory limit also sets GOMEMLIMIT to 90% of it (the Go runtime does not read the container limit itself); set GOMEMLIMIT in extraEnv to override. Without a memory limit, or with one the chart cannot read (e.g. "100m"), the chart sets none; the binary then takes 90% of its cgroup limit itself, when it has one. |
| `agent.securityContext.allowPrivilegeEscalation` | bool | `false` | — |
| `agent.securityContext.capabilities.drop[0]` | string | `"ALL"` | — |
| `agent.securityContext.readOnlyRootFilesystem` | bool | `true` | — |
| `agent.serverToken` | string | `""` | Bearer token for snapshot pushes, stored in a chart Secret. Ignored when existingSecret is set. When both are empty and server.enabled=true, the agent uses the in-chart server's ingest token (key ingestToken of its Secret, generated or server.existingSecret). |
| `agent.serverUrl` | string | `""` | URL of an upgradescope server to push snapshots to. Empty = CRD-only mode. If server.enabled=true and this is empty, it defaults to the in-chart server Service. |
| `agent.targets` | list | `[]` | Target Kubernetes minors to evaluate, MAJOR.MINOR only (the CRD's format), e.g. ["1.37", "1.38"], passed to the agent as --targets. The chart never renders the ClusterReadiness object: the agent creates it and, when targets is non-empty, resets spec.targets to this list on every tick, so this value wins over `kubectl edit`. Empty = the agent leaves spec.targets alone; set them with `kubectl patch ucr cluster --type merge -p '{"spec":{"targets":["1.37"]}}'` or let it default to the next minor above the server version. Emptying this after it was set does NOT clear spec.targets (the CR keeps the last list); to go back to the default next minor, run `kubectl patch ucr cluster --type merge -p '{"spec":{"targets":[]}}'`. |
| `agent.teamLabel` | string | `"team"` | Namespace label used for team attribution. |
| `agent.tolerations` | list | `[]` | — |
| `image.digest` | string | `""` | sha256:&lt;64 hex&gt;. When set and tag is empty, the pods pull repository:&lt;appVersion&gt;@digest, so the image cannot change under the tag. The published chart sets it to the digest of the image released with it. It is not applied when you set tag (it is the appVersion image's digest); to pin another tag, write tag: vX.Y.Z@sha256:&lt;64 hex&gt;. With another repository and an empty tag it still applies: set it to "" unless that repository mirrors the same image. |
| `image.pullPolicy` | string | `"IfNotPresent"` | The default image is pinned by digest in the published chart, so IfNotPresent always runs the released bytes. |
| `image.repository` | string | `"ghcr.io/abd-ulbasit/upgradescope"` | — |
| `image.tag` | string | `""` | Empty = the chart's appVersion, i.e. the release this chart was published with. |
| `imagePullSecrets` | list | `[]` | Pull secrets for a private registry or mirror, set on both pods, e.g. [{name: regcred}]. |
| `metrics` | object | `{"grafanaDashboard":{"annotations":{},"enabled":false,"labels":{"grafana_dashboard":"1"}},"prometheusRule":{"clusterStaleAfterSeconds":7200,"enabled":false,"labels":{}},"serviceMonitor":{"enabled":false,"interval":"1m","labels":{}}}` | Prometheus Operator and Grafana integration, all off by default. The metrics themselves are always served: the agent's on agent.healthPort, the server's on its Service port (behind the read token when one is set). docs/observability.md lists every metric; the alerts are in docs/guides/prometheus-grafana.md. |
| `metrics.grafanaDashboard.annotations` | object | `{}` | e.g. {grafana_folder: Kubernetes} for the sidecar's folder annotation. |
| `metrics.grafanaDashboard.enabled` | bool | `false` | Ship deploy/grafana/upgradescope-dashboard.json as a ConfigMap for the Grafana sidecar, which loads ConfigMaps labeled grafana_dashboard: "1" by default. |
| `metrics.prometheusRule.clusterStaleAfterSeconds` | int | `7200` | A cluster whose agent has not pushed for this long is stale. Agents push on every change and at least hourly (--force-sync-every), so keep it above that. |
| `metrics.prometheusRule.enabled` | bool | `false` | Render a monitoring.coreos.com/v1 PrometheusRule: upgrade blocked, verdict unknown for over 1h, agent not ticking and (with server.enabled) a cluster that stopped pushing. Scrape the metrics with serviceMonitor or your own config: the rules select the jobs serviceMonitor produces, &lt;fullname&gt;-agent-metrics and &lt;fullname&gt;-server (upgradescope-agent-metrics and upgradescope-server for a release named upgradescope). |
| `metrics.serviceMonitor.enabled` | bool | `false` | Render monitoring.coreos.com/v1 ServiceMonitors for the agent (plus a headless Service for its metrics port) and, with server.enabled, the server. Needs the Prometheus Operator CRDs. |
| `metrics.serviceMonitor.labels` | object | `{}` | Labels your Prometheus selects ServiceMonitors by, e.g. {release: kube-prometheus-stack}. |
| `networkPolicy.enabled` | bool | `false` | Render a NetworkPolicy that admits traffic to the server only from this release's agent pods and the peers below (no effect without server.enabled, or on a CNI that does not enforce NetworkPolicy). |
| `networkPolicy.serverIngressFrom` | list | `[]` | Extra NetworkPolicyPeer entries allowed to reach the server, e.g. the ingress controller or the CI runners that call /api/v1/gate. The server's /metrics is on the same port, so list Prometheus here when metrics.serviceMonitor is enabled:   - namespaceSelector: {matchLabels: {kubernetes.io/metadata.name: ingress-nginx}} |
| `rbac.create` | bool | `true` | Create the agent ClusterRole/ClusterRoleBinding. Every rule is listed and explained in templates/rbac.yaml and the chart README: get/list on namespaces, nodes, pods and the API group/resources the embedded KB flags as deprecated; get on /version and /metrics; writes only to the ClusterReadiness CR named agent.crName (and, with agent.manageCRD, to the clusterreadinesses.upgradescope.dev CRD; get/list on CRDs comes from the KB rules either way). No wildcards, no watch, no subresources such as nodes/proxy or pods/log. |
| `rbac.helmSecrets` | bool | `true` | Cluster-wide get/list on Secrets and ConfigMaps, for Helm release detection (releases stored by Helm's secrets and configmaps drivers). RBAC cannot filter Secrets by label or type, so true means the agent can read EVERY Secret and ConfigMap in the cluster. false removes both rules; the Helm capability is then not assessed (the reason is the forbidden lists) and Helm chart findings are missing from the report. Releases in Helm's sql driver, and charts that GitOps tools render with helm template (Argo CD), have no release object in the cluster either way; add-on detection from container images still covers them. |
| `server.adminToken` | string | `""` | Bearer token for cluster administration: deleting and renaming clusters (DELETE/PATCH /api/v1/clusters/{id}, `upgradescope clusters delete` and `rename`, with --server). Empty = both are refused. It must differ from the read and ingest tokens. Stored in the chart Secret; ignored when existingSecret is set (use adminTokenFromSecret). |
| `server.adminTokenFromSecret` | bool | `false` | With existingSecret: enable cluster administration with its adminToken key. |
| `server.affinity` | object | `{}` | — |
| `server.database.existingSecret` | string | `""` | Name of an existing Secret holding a Postgres URL (postgres://user:pass@host:5432/db?sslmode=require). Set = Postgres: the URL reaches serve as $UPGRADESCOPE_DB_URL (never an argument), no PVC is rendered, and replicas may be more than 1. Empty = SQLite on the persistence volume. |
| `server.database.key` | string | `"url"` | Key of the URL in that Secret. |
| `server.enabled` | bool | `false` | Run the upgradescope server in-cluster too (single-cluster combined install). The agent will push to it automatically. |
| `server.existingSecret` | string | `""` | Name of an existing Secret to use instead of the chart's. Keys:   ingestToken    the shared push token; the in-chart agent pushes with                  it. Required with the agent, optional without it (a                  fleet hub can accept per-cluster tokens only)   readToken      read when readTokenFromSecret=true   adminToken     read when adminTokenFromSecret=true   slackWebhook   optional; Slack notifications when present   webhook        optional; generic webhook when present   webhookSecret  optional; signs generic webhook requests when present |
| `server.extraArgs` | list | `[]` | Extra `upgradescope serve` flags, e.g. ["--max-snapshot-bytes=41943040"]. |
| `server.extraEnv` | list | `[]` | — |
| `server.extraVolumeMounts` | list | `[]` | — |
| `server.extraVolumes` | list | `[]` | — |
| `server.ingestToken` | string | `""` | Shared bearer token agents must present on POST /api/v1/snapshots. Empty = the chart generates a random 40-character token into its Secret on first install and keeps it on upgrades (Helm lookup). NOTES.txt prints the command to read it back for agents in other clusters (Secret &lt;fullname&gt;-server-tokens, e.g. upgradescope-server-tokens). Renderers without cluster access (helm template, Argo CD) cannot look the old token up and generate a new one on every render: under GitOps set this or existingSecret. Ignored when existingSecret is set. |
| `server.ingress.allowAnonymousRead` | bool | `false` | The render fails when the Ingress would publish a read API with no read token. Set true only when an authenticating layer (oauth2-proxy through ingress annotations, an identity-aware proxy) fronts it; see docs/operations/tenancy.md. |
| `server.ingress.annotations` | object | `{}` | — |
| `server.ingress.className` | string | `""` | — |
| `server.ingress.enabled` | bool | `false` | Expose the server (API, dashboard, snapshot ingest) through an Ingress. Set a read token first. Agents push to https://&lt;host&gt;. |
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
| `server.podLabels` | object | `{}` | app.kubernetes.io/name, instance and component are reserved for the selector and ignored here. |
| `server.podSecurityContext` | object | `{"fsGroup":65532,"runAsGroup":65532,"runAsNonRoot":true,"runAsUser":65532,"seccompProfile":{"type":"RuntimeDefault"}}` | As agent.podSecurityContext; fsGroup makes the data volume writable. OpenShift restricted-v2: {runAsUser: null, runAsGroup: null, fsGroup: null}. |
| `server.priorityClassName` | string | `""` | — |
| `server.readToken` | string | `""` | Optional bearer token for the read API. EMPTY = READ API IS OPEN — acceptable behind a ClusterIP Service on a private cluster, but set one before exposing the Service in any way. With existingSecret the value itself is not used; a non-empty one acts like readTokenFromSecret=true. |
| `server.readTokenFromSecret` | bool | `false` | With existingSecret: protect the read API with its readToken key. |
| `server.replicas` | int | `1` | Server Pods. More than one needs a shared database (database below): SQLite on a ReadWriteOnce volume has a single writer. |
| `server.resources` | object | `{"limits":{"cpu":"500m","memory":"1Gi"},"requests":{"cpu":"100m","memory":"128Mi"}}` | Sets GOMEMLIMIT as agent.resources does (~921MiB for 1Gi). 1Gi holds one /gate request, one snapshot ingest, one cluster read and the re-evaluation pass at their worst with four server.targets, the most serve takes, and notifications configured, measured on SQLite (~176, ~216, ~133 and ~198 MiB of heap, with every report at most the snapshot cap), two reads of a 500-cluster fleet (~16 MiB each), plus the buffered /gate and snapshot bodies (30 and 40 MiB) and the responses held for their clients (one 40 MiB budget): ~865 MiB of heap, which a limit below about 962Mi does not fit. Each server.targets entry adds about one report of up to the snapshot cap to an ingest and one to the re-evaluation pass (~54 MiB of the sum per extra target at the default 20 MiB cap), so with fewer targets it needs less: with none, about 650 MiB, which 768Mi holds. Open connections (uncapped, ~18 KiB each), kernel socket buffers, larger fleets and snapshots a v0.1 server stored are outside it: see docs/operations.md. |
| `server.retention` | string | `"90d"` | History older than this is pruned at startup and daily, except each cluster's latest snapshot and its evaluations: whole days (90d) or a Go duration (2160h); 0 keeps everything (as "0" or --set ...retention=0). |
| `server.securityContext.allowPrivilegeEscalation` | bool | `false` | — |
| `server.securityContext.capabilities.drop[0]` | string | `"ALL"` | — |
| `server.securityContext.readOnlyRootFilesystem` | bool | `true` | — |
| `server.service.port` | int | `8080` | — |
| `server.service.type` | string | `"ClusterIP"` | — |
| `server.serviceAccount.create` | bool | `true` | The server never calls the Kubernetes API: it gets its own ServiceAccount and its pod mounts no API token. create=false with an empty name runs it as the namespace's default ServiceAccount (still without a mounted token). |
| `server.serviceAccount.name` | string | `""` | — |
| `server.sharedIngestToken` | bool | `true` | Serve the shared ingest token at all. It may push as ANY cluster; false leaves only per-cluster tokens (`upgradescope tokens create &lt;cluster&gt;`, each bound to one cluster name): the chart Secret holds no ingestToken, serve gets none, and the in-chart agent needs its own token (agent.existingSecret or agent.serverToken). |
| `server.slackWebhook` | string | `""` | Optional Slack incoming-webhook URL for finding-delta notifications. Stored in the chart Secret and passed as $UPGRADESCOPE_SLACK_WEBHOOK; it never appears in the Deployment. Ignored when existingSecret is set. |
| `server.staleAfter` | string | `"2h"` | A cluster whose agent has not pushed for this long is marked stale in the API, the dashboard data and /metrics. Agents push at least about every 70m by default. |
| `server.targets` | list | `[]` | Extra targets evaluated on every accepted snapshot, e.g. ["1.37","1.38"]: at most 4 entries (serve --targets takes 4 distinct minors; the chart counts entries, so list each minor once). Each adds about one report of up to the snapshot cap to every push and to the re-evaluation pass, and server.resources is sized for 4. |
| `server.teamMap` | list | `[]` | Namespace→team overrides applied before every evaluation, rendered into a ConfigMap and passed as --team-map; the first matching glob wins, e.g. [{pattern: "payments-*", team: payments}]. |
| `server.tls.caKey` | string | `"ca.crt"` | The key of the CA certificate in that Secret, which the ServiceMonitor and the in-chart agent trust (cert-manager CA and self-signed issuers write ca.crt). Empty for a publicly trusted certificate whose Secret has no CA: they then use their system roots. |
| `server.tls.certManager.issuerRef` | object | `{}` | Or have cert-manager issue one for the Service names into &lt;fullname&gt;-server-https (not the Ingress's &lt;fullname&gt;-server-tls), e.g. {name: cluster-ca, kind: ClusterIssuer}. Needs the cert-manager CRDs. |
| `server.tls.secretName` | string | `""` | HTTPS on the server's own port, from an existing kubernetes.io/tls Secret (tls.crt, tls.key, optional CA). Without it the in-chart agent pushes its bearer token over plain HTTP inside the cluster (the agent logs a warning). The certificate must name the Service (&lt;fullname&gt;-server.&lt;namespace&gt;.svc); the in-chart agent then pushes to https:// and trusts the Secret's caKey, when it has one, on top of its image's roots. Probes and the ServiceMonitor switch to HTTPS. The Service port gets appProtocol https, and server.ingress gets nginx.ingress.kubernetes.io/backend-protocol: HTTPS unless its annotations set it; another controller may need its own annotation. serve reads the certificate at startup, so a renewal takes a pod restart. |
| `server.tolerations` | list | `[]` | — |
| `server.webhook` | string | `""` | Optional generic webhook URL: POSTed one versioned JSON notification per cluster and evaluation pass (schema in docs/reference/webhook.md). Stored like slackWebhook, as $UPGRADESCOPE_WEBHOOK_URL. |
| `server.webhookSecret` | string | `""` | Optional HMAC-SHA256 key: webhook requests then carry X-Upgradescope-Signature: sha256=&lt;hex&gt;. Stored like slackWebhook, as $UPGRADESCOPE_WEBHOOK_SECRET. |
| `serviceAccount.create` | bool | `true` | Create the agent ServiceAccount. Set false to bring your own (set name). |
| `serviceAccount.name` | string | `""` | — |
