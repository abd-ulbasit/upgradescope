# Upgrade

Upgrading upgradescope is also how its **knowledge base** moves: the API
lifecycle data and the add-on registry are compiled into the binary, so new
removals and end-of-life dates reach you only with a new release
([Knowledge base](../concepts/knowledge-base.md)). Expect verdicts to change
with an upgrade; that is usually the point of it.

Before upgrading, read the release's section of the
[changelog](../changelog.md). Before 1.0, a minor release may change flags,
API responses or the CRD schema, and every such change is listed under
**Changed** ([compatibility policy](../compatibility-policy.md)).

## The CLI and CI

Replace the binary through the channel you installed it with
([Install](install.md)). In CI, bump the pinned version (the Action's
`@vX.Y.Z` and `version`, or the `VERSION` your job downloads) in a pull
request of its own, so a change in verdicts shows up as the effect of the
upgrade and not of an unrelated change. A baseline written by an older
release stays usable: finding keys are stable across releases.

## The chart

```sh
helm get values upgradescope -n upgradescope > values-before.yaml
helm upgrade upgradescope oci://ghcr.io/abd-ulbasit/charts/upgradescope \
  --version <new> -n upgradescope --reset-then-reuse-values
helm get values upgradescope -n upgradescope | diff values-before.yaml -
```

- **Use `--reset-then-reuse-values`, which needs Helm 3.14 or later; do
  not use `--reuse-values`.** The new chart's `values.yaml` supplies the
  defaults and your own settings (what `helm get values` prints) are
  laid over them. `--reuse-values` does it the other way round: it makes
  the old release's *defaults* the new chart's, so every default that
  changed in between stays as it was. That includes the image digest the
  published chart pins (the pods keep running the old image under the new
  chart, and with it the old knowledge base), the server's memory limit
  and the security contexts, and a value the new chart adds can make the
  render fail with a nil-pointer error. Plain `helm upgrade` with no flag
  forgets your settings, so pass `-f values.yaml` with them instead if you
  keep a values file. The two `helm get values` calls show that only your
  own settings carried over.
- **After changing a token, a webhook value or an `existingSecret`,
  restart the pods yourself.** The tokens and webhook URLs reach the
  containers as environment variables, which are read once at start.
  `helm upgrade --set server.readToken=<new>` changes the Secret and leaves
  the running pods on the old value, and the old token keeps working until
  they restart:

  ```sh
  kubectl -n <ns> rollout restart deploy/<fullname>-server deploy/<fullname>-agent
  ```

  (`<fullname>` is the release name plus `-upgradescope`, cut to 63
  characters, or the release name alone when it contains `upgradescope`;
  the chart NOTES print the command with your Deployments' names). The
  same goes for the contents of a Secret you name (`server.existingSecret`,
  `agent.existingSecret`), and for a rotation you make with `kubectl`
  rather than Helm. The chart does not restart the pods for you because it
  cannot do so safely: a checksum of a token in a pod annotation would be
  readable by everyone who can get pods or Deployments, a wider set than
  those who can read Secrets, and a short token can be tested against it
  offline. A follow-up will make `serve` and the agent re-read mounted
  token files when they change, so that no restart is needed. Every key of
  the chart's Secrets is written under `data`, so from this chart version
  on, a value you remove on upgrade is removed from the Secret too (after
  the restart, the pods run without it). The upgrade to this version is the
  exception. Earlier charts wrote `readToken`, `adminToken`, `slackWebhook`,
  `webhook`, `webhookSecret` and the agent's `serverToken` as `stringData`,
  which the API server turned into `data`; Helm works out deletions from the
  previous manifest, which listed `stringData.<key>` and never `data.<key>`,
  so a value you remove in that same upgrade keeps its key in the Secret.
  The key is inert (the pods read a key only when its value is set), and
  you can clear it once with `kubectl -n <ns> patch secret
  <fullname>-server-tokens --type=json -p
  '[{"op":"remove","path":"/data/<key>"}]'` (the agent's Secret is
  `<fullname>-agent-token`), checking
  with `kubectl get secret ... -o jsonpath='{.data}'`. Or remove the value in
  a later upgrade.
- **SQLite needs a writable `/tmp`.** The server's root filesystem is
  read-only, and SQLite spills a large delete (the daily retention prune,
  `clusters delete`) into a temp file. The chart mounts an emptyDir at
  `/tmp` (`server.tmp.sizeLimit`, 1Gi) and sets `SQLITE_TMPDIR` to it.
  If you already mount a volume of your own at `/tmp`
  (`server.extraVolumeMounts`), the chart adds none and SQLite uses yours,
  which must be writable and large enough. Chart versions before this one
  had no such directory, and a prune of
  more than a few tens of MB failed with `disk I/O error (6410)`: if
  you ran one, the first prune after the upgrade deletes the backlog in
  batches of at most 5,000 rows, each a statement of its own and not part of
  one long transaction. Measured on a 60 MB backlog, that form needed no
  temp directory where the same delete inside a transaction failed (SQLite
  journals the statements of a transaction in a temp file; the delete of a
  whole cluster, which is one transaction, still needs the directory). The
  temp space one full batch needs was not measured, so if it still fails,
  `upgradescope_retention_prune_failures_total`
  counts it and `server.tmp.sizeLimit` is the knob
  ([Retention and backup](retention-and-backup.md#when-the-prune-fails)).
- **The stale threshold follows `agent.interval`.** With `server.staleAfter`
  unset, the server marks a cluster stale after the larger of 2h and three
  agent intervals, where the chart used to fix 2h; the
  `UpgradescopeClusterStale` alert follows it
  (`metrics.prometheusRule.clusterStaleAfterSeconds` is 0, meaning the
  same threshold). A `server.staleAfter` or `clusterStaleAfterSeconds`
  that is not above `agent.interval` now fails the render, and the install
  notes warn about one below the larger of `agent.interval` and 1h, plus
  one interval, which healthy clusters exceed between pushes. With the
  default 10m interval nothing changes. Clusters in other regions that
  push to this server have intervals the chart cannot see: keep the
  threshold above the longest of them.
- **Only Service names are cut to fit 63 characters.** A Service name is a
  DNS-1035 label, which the API server refuses past 63 characters, so the
  server Service and the agent metrics Service shorten the release's
  fullname (the release name plus `-upgradescope`, unless the release name
  already contains it) when it would pass 63 with `-server` or
  `-agent-metrics`. A Service name that fits is unchanged, and one that did
  not fit never installed. Every other resource keeps the name it had
  (the data PVC, the token Secret, the ConfigMaps, the Deployments,
  whatever their length), so a long release name loses neither its SQLite
  history nor its generated ingest token on upgrade.
- **The listen port is its own value.** `server.containerPort` (8080) is
  what serve binds; `server.service.port` is only the Service's, so it can
  be 80 or 443. Before, the pod listened on `server.service.port`. If you
  had set that to something other than 8080, the pod now listens on 8080
  and the Service still answers on your port, but a NetworkPolicy, sidecar
  or `kubectl port-forward pod/...` of your own that names the pod's old
  port must follow it (set `server.containerPort` to the old value to keep
  it, if it is 1024 or above).
- **`networkPolicy.enabled` with an Ingress or a ServiceMonitor** needs
  `networkPolicy.serverIngressFrom` now: the policy admits only the
  in-chart agent, and the render fails rather than cut off the Ingress
  controller or Prometheus.
- **More than one server replica** gets a PodDisruptionBudget
  (`server.podDisruptionBudget`) and a soft spread across nodes
  (`server.defaultTopologySpread`, or your own
  `server.topologySpreadConstraints`).

- **The CRD.** Helm installs `crds/` on first install only and never
  upgrades it. The agent brings the `ClusterReadiness` CRD up to its own
  schema at startup by server-side apply (`agent.manageCRD=true`, the
  default). A startup check that fails, or runs out its 30 seconds (a
  transient apiserver fault, a role that may not patch the CRD), no longer
  waits for a pod restart: every tick tries again until one succeeds, and
  meanwhile each tick line carries `crdError` and the object's
  `status.notAssessed` leads with a note, as an older schema prunes the
  status fields it lacks. A CRD that is not installed at all still stops
  the agent at startup
  ([Troubleshooting](../troubleshooting.md#the-agent)). With `agent.manageCRD=false`, apply the new
  `deploy/chart/crds/` yourself before the agent starts, or the agent may
  write status fields the old schema prunes.
- **The server's database** migrates forward automatically at startup
  (SQLite and Postgres alike). Back it up first
  ([Retention and backup](retention-and-backup.md)); an older server
  cannot read a newer schema.
- **Team-scoped reads (0.2.0).** The migration that adds read tokens also
  records, with every evaluation, the teams it names. Evaluations written
  before it have none, so the first start re-evaluates every stored
  evaluation once, in its startup pass: about the cost of one knowledge
  base change across the fleet. Until the pass reaches a cluster, its
  evaluations read `outdated: true` and no team-scoped token sees it;
  fleet-wide reads still see every cluster. `serve` decides whether its
  read API is open only after opening (and so migrating) the database,
  since minted read tokens live there: a first start that refuses an open
  read API on an exposed address has already migrated it. A `--team-map`
  keeps loading as before: a team is free text (`Platform Team` is a
  team), and a token names one per `--teams` flag. The one change is a
  team called `*`, the scope of a fleet-wide read token: it is now read
  as the team `(*)`, and `serve` logs a warning at startup naming the
  rule. Its findings, team scores and `/fleet/teams` row say `(*)` from
  the first re-evaluation on; rename the team in the map if that matters
  to you ([Read access](auth.md)).
- **Agents and servers** can be upgraded in either order (from v0.1.x,
  upgrade the server first when a cluster name is invalid, and run both
  `clusters list` and `clusters rename` from the upgraded binary:
  [Cluster names](#from-v01x)). The push
  protocol is versioned (`schemaVersion` 1), a newer server stores fields
  an older agent does not send and keeps fields a newer agent sends that it
  does not know, and a server re-judges every stored snapshot with its own
  knowledge base. Snapshots from v0.1.x agents are judged with their
  differences named: their API-usage signal meant something else, so those
  clusters read `unknown` until their agents are upgraded
  ([Running the server](../operations.md#what-a-push-is-judged-as)).
  One exception to the either-order rule: upgrade the server before the
  agents of clusters whose agent role cannot list pods. A newer agent
  then reports its add-ons as partial (read from Helm releases and
  IngressClasses only), a gap only a newer server keeps required; an older
  server reads that cluster `ready` where it read `unknown` before. A
  newer agent's `apiAuthorshipUnknown` objects are kept by an older server
  but not reported as findings until it is upgraded.
- **Agent settings refused at startup.** An agent now refuses to start,
  instead of failing every push as if the server were down, with
  `--force-sync-every` 0 or negative (0 used to mean the 1h default), a
  `--server-url` that is not an `http://` or `https://` URL with a host
  (`fleet.example.com` alone, `https://:8080`), a server token with
  whitespace or a control character inside it (surrounding whitespace, such
  as a Secret's trailing newline, is trimmed from the flag, the environment
  and the file alike), or `--cluster-name ""`. The chart renders none of
  these from its own values (it omits an empty `agent.clusterName`, and its
  schema requires an http(s) `agent.serverUrl` with a host), so check
  `agent.extraArgs` and the token Secret before you upgrade: an agent
  given one of them crash-loops with `invalid --<flag>` in its log. A
  `--force-sync-every` below `--interval` is still accepted and still
  means a push on every tick, and so now does one equal to it (which used
  to force-sync only on the ticks the jitter brought late, about half of
  them); the agent logs a warning with the period in effect ([Troubleshooting](../troubleshooting.md#the-agent)).
- **The agent's tick reserve.** Collection now ends 30 seconds before the
  tick deadline (half of it when the deadline is under a minute), keeping
  that reserve for the status write, its error marker and the push: at
  the default 10-minute interval collection gets 4m30s of the 5-minute
  deadline. A check that used to finish in its last half minute now ends
  as not assessed with a step-deadline reason
  ([Observability](../observability.md)).
- **Ingest tokens** generated by the chart are kept across upgrades (Helm
  looks up the live Secret). Under GitOps renderers that cannot look it up,
  set them explicitly ([GitOps](../guides/gitops-argo-flux.md)).

## From v0.1.x

v0.2.0 changes scanning and gating substantially. Among the changes the
changelog lists: a verdict of `unknown` (a required check could not run)
fails the gate unless `--allow-incomplete`, where v0.1.1 reported such
scans as ready and exited 0; an unreadable cluster, or a `--files` scan with
no objects, exits 1 instead of scoring 100; deprecated-API detection judges
who writes an API instead of what is served, which removes false blockers;
and the server's gate endpoint fails on blockers by default. Read the
**Changed** section before you upgrade a CI gate, and expect clusters that
read `ready` under v0.1.x to read differently, in both directions.

**Cluster names.** A v0.1 server registered clusters under any name, and
v0.1 agents sent `--cluster-name` (chart `agent.clusterName`) unchecked.
The server now refuses every push whose cluster name is not a lowercase
RFC 1123 subdomain of at most 253 bytes (`Prod_EU`, `prod eu`) with
`422`, and an upgraded agent refuses to start with one. v0.1.x has no
`clusters rename` command and no rename endpoint, so upgrade the server
(and the CLI you run the rename with) first. Then, before you upgrade
the agents, run `upgradescope clusters list` from the upgraded binary, and for each name that is
not valid, rename the cluster (its history and per-cluster tokens move
with it), then set the agent's name to match:

```sh
upgradescope clusters rename Prod_EU prod-eu --server https://upgradescope.example.com
helm upgrade upgradescope oci://ghcr.io/abd-ulbasit/charts/upgradescope -n upgradescope \
  --reset-then-reuse-values --set agent.clusterName=prod-eu
```

`--reset-then-reuse-values` carries over only what you set and takes every
other value from the new chart, which is what a jump from v0.1.x needs:
the v0.1.x chart's own defaults (`image.tag: dev`, a 512Mi memory limit, no
security contexts) are not carried forward, and `--reuse-values` would
carry them ([The chart](#the-chart)).

Until an agent's name is changed its pushes are refused; nothing stored
is lost, only the cycles it could not push
([Cluster lifecycle](../operations.md#cluster-lifecycle)). When one
release runs both the server and the agent, the `helm upgrade` that
upgrades the server upgrades the agent too, and the new agent refuses to
start with the invalid name, its pod failing, until you have
renamed the cluster and the second `helm upgrade` above sets
`agent.clusterName`.

The server bounds its memory by the input's structure
([Memory and request limits](../operations.md#memory-and-request-limits)):
a `/gate` stream of more than about 4.4 MiB of typical kubectl YAML, or a
document that YAML aliases expand past 4 MiB, gets 413 where v0.1.x decoded
it, so split such streams. Reads that load a stored snapshot run one at a
time and can get 503 with `Retry-After` under load. Snapshots a v0.1
server stored are decoded without a node count until their clusters push
again. On its first start,
`serve` tightens an existing SQLite database and its `-wal` and `-shm`
files to 0600, and its migration copies what each stored evaluation could
not assess out of the report into a column of its own, reading every
stored report once, so that first start takes longer on a large
database. The chart's server memory limit is 1Gi, up from 512Mi, which
the worst case measured on SQLite no longer fit; if you set
`server.resources` yourself, see
[Memory and request limits](../operations.md#memory-and-request-limits).
The chart's agent CPU limit is 1, up from 200m, because a first tick on
1,000 Helm releases did not finish at 200m. For scheduling it costs
nothing (the request, 50m, is what is reserved), but a ResourceQuota on
`limits.cpu` counts it (800m more per agent pod) and a LimitRange whose max
CPU is below 1 rejects it, so a `helm upgrade` on default values can fail
pod admission in such a namespace: give the quota the room, or set
`agent.resources.limits.cpu` yourself (see
[Sizing the agent](install.md#sizing-the-agent)).
`serve --targets` (chart `server.targets`) takes at most 4 minors now,
the count that worst case is measured at: a server started with more
refuses to start, so trim the list before you upgrade.
