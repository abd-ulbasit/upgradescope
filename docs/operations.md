# Running the server

What it takes to run `upgradescope serve` as a fleet hub for months: how a
push is judged, a cluster lifecycle, stale-data signals, notifications a
receiver can trust and auditor exports. The Helm chart's fleet-hub install
is in the [chart README](https://github.com/abd-ulbasit/upgradescope/blob/main/deploy/chart/README.md#fleet-hub-server-only).
Elsewhere:

- storage, pruning and backups: [Retention and backup](operations/retention-and-backup.md);
- tokens, SSO and who can read what: [Tenancy and access control](operations/tenancy.md);
- metrics, probes and logs: [Metrics, logs and probes](observability.md);
- every endpoint: the [REST API reference](reference/api.md).

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
window. Each pass is compared with the target's last evaluation that had
a decided verdict (ready or blocked). A pass whose verdict is unknown sends
nothing and is not a baseline either: what it could not see is not news.

A new cluster's first evaluation of a target is the baseline and sends
nothing, and so does the first evaluation of a target added to
`--targets` later, so that restarting the server with a new target does
not notify every cluster at once.

**After a cluster upgrade.** When a cluster upgrades, its default target
moves up one minor: a cluster on 1.35 is judged against 1.36; after its
upgrade to 1.36, against 1.37. The new default target's first decided
evaluation is compared with the previous default target's last decided one
(the nearest lower target that has one, up to 3 minors below). So:

- a blocker that the new target adds is a `new-blocker`. For example, a
  cluster that still calls `networking.k8s.io/v1beta1` ServiceCIDR, removed
  in 1.37, is notified when it reaches 1.36;
- a blocker the cluster already had at the old target (an EOL add-on, an
  API removed long ago) is not announced again. There is no separate
  "target changed" event;
- if the old target was blocked and the new one is ready, the cluster gets
  `became-ready`.

The same comparison applies when a newer server or knowledge base first
decides a default target that it could not judge before (a cluster on the
newest minor, whose next minor was past the knowledge base's horizon and
so `unknown`). Every cluster on that minor then gets its first decided
evaluation in the same pass. Each one that has a stored decided
evaluation of a lower target (within three minors below) is compared with
it and notified once of the blockers that are new to it (at most one
notification per cluster, its changes capped as usual). A cluster without
one, because it was first seen on the newest minor or because its
lower-target evaluations were pruned, has nothing to compare with, and
its first decided evaluation is a silent baseline.

This differs from a target added to `--targets`, which stays silent: adding
a target asks a new question about a cluster that has not changed, and
every blocker it finds was already there. A default target that was
unknown until a knowledge-base update is the cluster's real next upgrade,
and the update is the first time anyone could say what blocks it, so those
blockers are genuinely new to each cluster and are announced, once.

Retention edge: the old target's evaluation is the baseline only while it
is stored. If a new default target stays `unknown` for longer than
`--retention` after the upgrade (no knowledge base for it that long), the
old target's evaluations are pruned (see [Retention and
backup](operations/retention-and-backup.md)), and the first decided
evaluation of the new target is then a silent baseline.

Delivery: notifications are committed to an outbox with the evaluations
that produced them and delivered by a background worker, so a push never
waits on a receiver and a restart does not lose queued messages (unless the
server was down so long that they have passed the 8 hour limit below, when
they are dropped unsent). A failed delivery (an error, a timeout of 2s, any
non-2xx status, **including redirects**, which are not followed) is retried with exponential backoff from 30s, up to 8
attempts (about an hour), separately per sink. A receiver that answers
`429` or `503` with a `Retry-After` header (seconds or an HTTP date) is
left alone for that delay, capped at an hour: the sink is not called for
that message or for any other message queued for it (which are put back
without counting an attempt), so a burst after a fleet-wide pass does not
hammer a rate-limited receiver or use up its messages' attempts. The wait
replaces a shorter backoff, so the attempts of one message may span several
hours. Because a held sink is called only once per hold, the messages queued
behind it would otherwise drain one per hold: so a message is **given up
(logged, not sent) once it has been queued for 8 hours**, whatever its
attempts, and that holds for every queued message. A receiver limited for
good therefore loses notifications older than 8 hours, not the newest. The
hold is kept in memory: a restart, or another replica, forgets it and finds
out with the next call. Delivery is **at least once**:
the same notification may arrive more than once, so deduplicate on
`deliveryId`, which is the same on every retry and for every sink.

There is **one notification per cluster per evaluation pass**, grouping all
targets: a change found for several targets with the same title and detail
(an EOL add-on is a blocker for every target) is one entry listing them; a
finding whose wording names the target keeps one entry per target, each in
its own words. Each notification lists at most 5
new blockers and 5 eol-approaching changes; the rest are counted in
`omitted`.

### Webhook payload

The generic webhook sends a versioned JSON body, `schemaVersion` 1, with
lowercase keys: `deliveryId`, `type` (`readiness.changed`), `timestamp`,
`cluster`, the verdict of every changed target in `targets`, the
`changes` and, when the per-kind cap dropped some, `omitted`. Servers
before schemaVersion 1 sent one PascalCase event per change and target
(`Cluster`, `Target`, `Kind`, `Title`, `Detail`); receivers written for
that shape must be updated.

With `--webhook-secret` (`$UPGRADESCOPE_WEBHOOK_SECRET`,
`--webhook-secret-file`), every delivery carries
`X-Upgradescope-Signature: sha256=<hex HMAC-SHA256(secret, raw body)>`.

Every field, the headers, how to verify the signature, and the JSON
Schema the body validates against are in the
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
