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
helm upgrade upgradescope oci://ghcr.io/abd-ulbasit/charts/upgradescope \
  --version <new> -n upgradescope --reuse-values
```

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
  --reuse-values --set agent.clusterName=prod-eu
```

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
