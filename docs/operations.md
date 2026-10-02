# Operating the server

What it takes to run `upgradescope serve` as a fleet hub for months:
bounded storage, a cluster lifecycle, stale-data signals, notifications a
receiver can trust, auditor exports, and access control. The Helm chart's
fleet-hub install is in [deploy/chart/README.md](../deploy/chart/README.md#fleet-hub-server-only);
metrics and alerts are in [observability.md](observability.md).

## Retention and sizing

Every snapshot whose inventory changed is stored, with one evaluation per
target, and every re-evaluation whose result changed adds an evaluation.
Unchanged pushes are deduplicated, so storage grows with the number of real
inventory changes (node, image and namespace churn included), at most one
per agent tick (144 a day at the default 10m interval).

`serve --retention` (default `90d`; whole days or a Go duration of at least
a day; `0` keeps everything) prunes once at startup and then daily:

- evaluations created before the window, then
- snapshots received before the window that no evaluation refers to any
  more,
- except each cluster's latest snapshot and its evaluations, however old:
  a cluster that has been silent longer than the window keeps its current
  state (and shows as stale).

An older snapshot stays while any of its evaluations is inside the window.
Score history, the dashboard sparkline and exports reach back the window
at most. Each run that deletes rows logs the counts. Measured in
`internal/server/store/sqlite_lifecycle_test.go`: one cluster whose
inventory changed every day for a year (365 snapshots, 730 evaluations
with two targets) plus one cluster silent all year (1 and 1) go from 366
snapshots and 731 evaluations to 92 and 183 under a 90-day window: 91 days
of the busy cluster, and the silent cluster's current state.

Sizing: the October 2026 audit measured about 35 KB per changed snapshot
for a realistic inventory (60 nodes, 250 namespaces, 80 Helm releases),
plus one report per target. Plan for
`clusters × changes per day × retention days × (snapshot + targets × report)`.
SQLite reuses the pages pruning frees, but the file does not shrink; run
`sqlite3 upgradescope.sqlite VACUUM` with the server stopped to return the
space. A large or busy fleet belongs on Postgres (`--db-url`, chart value
`server.database.existingSecret`), which also allows several replicas.

Fleet reads stay small as the fleet grows: `/api/v1/fleet` and the cluster
list read each cluster's latest snapshot id and server version, never its
inventory. `make bench-server` seeds 500 clusters with ~35 KiB inventories
on SQLite and runs 10 concurrent `/fleet` readers; it fails above a 1s p95
or a 512 MiB heap peak. On an arm64 Mac (October 2026): p95 about 0.5s,
peak live heap 15 MiB (it was 1.1 GiB and 1.2s while every request
decoded every inventory).

## What a push is judged as

- **Stored as sent.** A snapshot keeps the inventory bytes the agent pushed,
  `collectedAt` and fields this server does not know included, so a newer
  server can judge them. Deduplication hashes the fields this server knows,
  without `collectedAt`: a push that differs only in unknown fields is a
  duplicate (`200`), and the duplicate's `agentVersion` and `kbVersion` are
  recorded on the latest snapshot.
- **Refused before anything is written** (`422`): a missing or `null`
  inventory, an inventory `schemaVersion` other than 1 (including `{}`), or
  a `serverVersion` that is not a Kubernetes 1.x version.
- **Degraded pushes** (no `serverVersion`: the agent could not read
  `/version`) are judged at the version the cluster last reported, so its
  fleet cells, default target and report stay. The versions capability is
  required, so such a cell is `unknown` at best (`blocked` when the push
  shows a blocker), and the report's `notAssessed` says why. A cluster's
  very first push without a version is judged only at `--targets`.
- **v0.1.x agents** (`agentVersion` 0.1.0, 0.1.1 or earlier, their
  pre-releases and Go pseudo-versions) collected two signals with meanings
  this server no longer judges: api-usage counted every object the
  apiserver *serves* at a deprecated version (APF FlowSchemas became
  removed-API blockers) and their own requests landed in the
  deprecated-calls metric; a Helm-chart-found add-on's version was the
  chart version. Their api-usage and deprecated-calls are reported as not
  assessed, with the reason, and a chart version is kept as evidence only,
  so such a cluster is `unknown` until its agent is upgraded. Builds from
  later source (`dev`, a 0.1.2 pseudo-version or snapshot) are judged
  normally.
- **Outdated verdicts.** A stored verdict depends on the date (EOL windows),
  the KB and the team map. The background pass re-evaluates hourly and just
  after each UTC midnight; until it has, every read of a stored verdict
  (fleet cells, cluster summaries, the report) carries `"outdated": true`
  and starts the next pass early. Reads never recompute.

## Cluster lifecycle

A cluster is registered by its first push, under the agent's
`--cluster-name`, and bound to the cluster UID (the `kube-system`
namespace UID) it pushed. A push under that name from another UID, or with
no UID at all, is refused with 409, so two clusters never interleave in one
history.

Deleting and renaming need the admin token: `serve --admin-token`
(`$UPGRADESCOPE_ADMIN_TOKEN`, `--admin-token-file`; chart
`server.adminToken` or `adminTokenFromSecret`). Without one the server
refuses both (403); the read and ingest tokens get 403 too. The admin token
must differ from both, and it also authorizes reads.

| | API | CLI |
|---|---|---|
| list | `GET /api/v1/clusters` (read token) | `upgradescope clusters list --server URL` |
| delete | `DELETE /api/v1/clusters/{id}` → 204 | `upgradescope clusters delete <name> --server URL` |
| rename | `PATCH /api/v1/clusters/{id}` `{"name": "new"}` → 200 | `upgradescope clusters rename <name> <new-name> --server URL` |

The CLI takes the token from `--admin-token` (`--read-token` for `list`),
the environment variable or a `-file` flag. Without `--server` it works on
the database directly (`--db` or `--db-url`), for example while the server
is stopped.

- **Delete** removes the cluster, its snapshots and evaluations, its queued
  notifications, and the per-cluster ingest tokens minted for its name.
  An agent still pushing under the name registers it again, so stop or
  re-point the agent first. To re-register a rebuilt cluster (new UID,
  same name), delete the old record and mint a new token.
- **Rename** moves the history and re-binds the name's ingest tokens. The
  agent sends its own `--cluster-name` with every push, so change that too
  (chart `agent.clusterName`): until then its pushes are refused when it
  uses a per-cluster token, or register the old name again when it uses
  the shared ingest token.

Every delete and rename is logged by the server.

## Stale clusters

A cluster is **stale** when its agent has not pushed, duplicates included,
within `serve --stale-after` (default `2h`; chart `server.staleAfter`). An
unchanged cluster pushes on its agent's next tick after the hourly
force-sync, about every 70 minutes with the defaults, so a window under
about 80 minutes would flag healthy clusters. A stale cluster's scores
describe it as it was at `lastSeen`.

- `GET /api/v1/clusters`, `GET /api/v1/clusters/{id}` and every row of
  `GET /api/v1/fleet` carry `lastSeen` and `stale`.
- `/metrics` exports `upgradescope_cluster_stale{cluster}` (0 or 1) next to
  `upgradescope_cluster_last_push_age_seconds{cluster}`; the chart's
  `UpgradescopeClusterStale` alert fires on the push age.

## Notifications

With `--slack-webhook` or `--webhook`, the server sends a notification when
an evaluation pass changes a cluster's readiness: a new blocker, all
blockers resolved (became ready), or an add-on entering its end-of-life
window. A cluster's first evaluation of a target is the baseline and sends
nothing; neither does a pass whose verdict is unknown.

Delivery: notifications are committed to an outbox with the evaluations
that produced them and delivered by a background worker, so a push never
waits on a receiver and a restart loses nothing. A failed delivery (an
error, a timeout of 2s, any non-2xx status, **including redirects**, which
are not followed) is retried with exponential backoff from 30s, up to 8
attempts (about an hour), separately per sink. Delivery is at least once:
deduplicate on `deliveryId`.

There is **one notification per cluster per evaluation pass**, grouping all
targets: a change found for several targets with the same title and detail
(an EOL add-on is a blocker for every target) is one entry listing them; a
finding whose wording names the target keeps one entry per target, each in
its own words. Each notification lists at most 5
new blockers and 5 eol-approaching changes; the rest are counted in
`omitted`.

### Webhook payload

The generic webhook's JSON body, its headers, how to verify the
signature, and the JSON Schema it validates against are in the
[webhook reference](reference/webhook.md).

### Slack

Slack gets the same notification as text: one line for a single change,
`[upgradescope] <cluster> → <targets>: <kind>: <title>`, otherwise a
header line and one bullet per change.

## CSV export

`GET /api/v1/clusters/{id}/export?format=csv&target=` exports the current
stored evaluation (the HTML format carries the same verdict with a score
history). Columns:

```
cluster,target,evaluatedAt,kbVersion,verdict,score,severity,category,key,title,detail,remediation,teams,namespaces,citations
```

Every row carries the cluster, target, evaluation time, KB version,
verdict and score. The `severity` column types the rows:

- `summary`: always the first row, so a clean cluster's export is not a
  bare header. The title states the verdict and score; the detail counts
  blockers, warnings and capabilities not assessed.
- `blocker`, `warning`, `info`: one row per finding.
- `not-assessed`: one row per capability the evaluation could not assess
  (`key` is the capability, `detail` the reason). A required one makes the
  verdict `unknown`, never `ready`.

Teams, namespaces and citations are `;`-joined. Cells that a spreadsheet
would run as a formula are prefixed with `'`.

## Access control and tenancy

The server has four credentials, each optional except where noted:

| Credential | Authorizes | Configured with |
|---|---|---|
| read token | every `GET /api/v1/*` endpoint (clusters, reports, findings, history, team rollups, exports, registry), `POST /api/v1/gate`, `/metrics` | `--read-token`; empty = open, refused on a non-loopback `--listen` without `--allow-anonymous-read` |
| per-cluster ingest tokens | `POST /api/v1/snapshots` as one cluster name | `upgradescope tokens create <cluster>` |
| shared ingest token | `POST /api/v1/snapshots` as **any** cluster | `--ingest-token` |
| admin token | deleting and renaming clusters, plus reads | `--admin-token`; empty = both refused |

What the read token does **not** do:

- **It is one fleet-wide secret.** Whoever holds it reads every cluster,
  every team's findings and namespaces, and every export. There are no
  per-team scopes yet, so a team cannot be given a view of only its
  clusters.
- **It is not an identity.** Reads are not attributed to anyone, and
  rotating it means restarting the server and handing the new value to
  every consumer at once.
- **It does not protect the dashboard's static files** (HTML, JS, CSS),
  which hold no data, nor `/healthz` and `/readyz`.
- **It is not encryption.** Without TLS (`--tls-cert-file`, or an Ingress
  that terminates it) it crosses the network in the clear.
- **It lives in the browser.** The dashboard keeps it in `localStorage`;
  the server's Content-Security-Policy forbids inline and third-party
  script, which is what would read it.
- It cannot push snapshots or delete clusters: those need the ingest and
  admin tokens.

### Putting the dashboard behind SSO

For people, put an authenticating proxy in front of the server:
[oauth2-proxy](https://oauth2-proxy.github.io/oauth2-proxy/), an
identity-aware proxy, or your ingress controller's external-auth support.
The server needs no changes. Two patterns:

1. **Proxy in front, read token behind.** Keep `--read-token`. The proxy
   decides who may open the dashboard; the dashboard still asks for the
   read token once (stored in that browser). Machine clients (the CI gate,
   Prometheus, scripts) keep calling the server with the token, directly
   or through a proxy route that skips authentication. Defense in depth,
   and the simplest for automation.
2. **The proxy is the gate.** Run without a read token
   (`--allow-anonymous-read`, chart `server.ingress.allowAnonymousRead=true`
   when the chart's Ingress carries the auth annotations) and make sure
   nothing but the proxy can reach the read API: a ClusterIP Service, a
   NetworkPolicy (`networkPolicy.enabled`, `serverIngressFrom` naming the
   proxy, Prometheus and the agents' sources). Agents authenticate
   `POST /api/v1/snapshots` with their own tokens, so let that route through
   without SSO; CI callers then need the proxy's machine credentials (for
   oauth2-proxy, bearer JWTs from a trusted issuer). Anyone who reaches the
   Service directly reads everything, so this rests on the network policy.

A sketch of pattern 2 with oauth2-proxy in front of the chart's Service
(adapt the provider flags to your identity provider):

```yaml
args:
  - --provider=oidc
  - --oidc-issuer-url=https://sso.example.com
  - --email-domain=example.com
  - --upstream=http://upgradescope-server.upgradescope.svc:8080
  - --http-address=0.0.0.0:4180
  - --reverse-proxy=true
  # Agents push with their own ingest tokens.
  - --skip-auth-route=POST=^/api/v1/snapshots$
env:
  - {name: OAUTH2_PROXY_CLIENT_ID, valueFrom: {secretKeyRef: {name: oauth2-proxy, key: client-id}}}
  - {name: OAUTH2_PROXY_CLIENT_SECRET, valueFrom: {secretKeyRef: {name: oauth2-proxy, key: client-secret}}}
  - {name: OAUTH2_PROXY_COOKIE_SECRET, valueFrom: {secretKeyRef: {name: oauth2-proxy, key: cookie-secret}}}
```

With pattern 1, do not let the proxy overwrite the `Authorization` header
(oauth2-proxy's `--pass-authorization-header` and
`--set-authorization-header` do): the server reads the read token from it.
Keep cluster administration off the proxied path either way: the admin
token belongs to operators and their CLI, not to the dashboard.
