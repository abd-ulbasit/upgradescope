# Metrics, logs and probes

The agent and the server plug into the usual Kubernetes tooling: logs you
can query, probes, Prometheus metrics, alert rules, a Grafana dashboard, a
standard `Ready` condition on the `ClusterReadiness` object, and an Argo CD
health check. Everything in the chart that needs the Prometheus Operator or
Grafana is off by default.

| | Agent | Server |
|---|---|---|
| Liveness | `GET /healthz` on `--health-addr` (default `:8081`): the process is up | `GET /healthz`: the process is up (never touches the database) |
| Readiness | `GET /readyz`: a tick succeeded within 2 × interval + tick deadline; 503 with the reason otherwise | `GET /readyz`: the database answers a ping within 2s; 503 otherwise |
| Metrics | `GET /metrics` on `--health-addr` | `GET /metrics` on the API port, behind the read token when one is set |
| Logs | one line at startup, one per tick (`--log-format text\|json`, `--log-level`) | one line per notable event (unchanged) |

`--health-addr` defaults to `:8081` on all interfaces, which is what a pod
needs. Running `upgradescope agent` on a workstation, prefer
`--health-addr 127.0.0.1:8081`, or another port if something already holds
8081 (kubebuilder operators use it for their probes by default; a taken port
stops the agent at startup with `health listener: address already in use`).
`--health-addr ""` turns the listener off.

## Profiling the agent

`--pprof-addr 127.0.0.1:6060` serves Go's profiler (`/debug/pprof/`) on a
listener of its own, off by default and **loopback only**: a profile shows
the process's heap and goroutine stacks, so the agent refuses a port with no
host, a wildcard (`:6060`, `0.0.0.0`) or a routable address at startup. In a
pod, set it through the chart (`agent.extraArgs: ["--pprof-addr=127.0.0.1:6060"]`)
and reach it with `kubectl port-forward`, which enters the pod's own network
namespace:

```sh
kubectl -n upgradescope port-forward deploy/upgradescope-agent 6060:6060 &
go tool pprof -top "http://127.0.0.1:6060/debug/pprof/profile?seconds=60"
```

The profile covers the seconds it is asked for, so start it just before a
tick is due (the agent's log says when the last one ended; the next is due
0.9 to 1.1 intervals after) and let it run past the tick's end. It samples
the process's CPU time, so under a CPU quota it shows CPU-seconds, not the
wall time a throttled tick spends waiting for its quota.
`docs/operations/scale.md` ("The tick after a partial Helm step") reads one.

## Agent logs

The agent writes a startup line (version, KB version and horizon, interval,
tick deadline and tick reserve, server URL or CRD-only, health address, profiler address) and exactly one line
per tick. `--log-format=json` makes every line a JSON object;
`--log-level` is `debug`, `info`, `warn` or `error`. Chart values:
`agent.logFormat`, `agent.logLevel`.

```
level=INFO msg="tick complete" duration=2.41s push=ok consecutiveFailures=0 capabilities.addons=true capabilities.api-usage=true capabilities.deprecated-calls=false capabilities.helm=true capabilities.versions=true targets.1.36.verdict=blocked targets.1.36.score=72 targets.1.36.blockers=2
```

```json
{"time":"2026-10-02T12:00:02Z","level":"INFO","msg":"tick complete","duration":"2.41s","push":"ok","consecutiveFailures":0,"capabilities":{"addons":true,"api-usage":true,"deprecated-calls":false,"helm":true,"versions":true},"targets":{"1.36":{"verdict":"blocked","score":72,"blockers":2}}}
```

| Field | Meaning |
|---|---|
| `msg` | `tick complete` (INFO, or WARN when only the push failed) or `tick failed` (ERROR, with `err`); `agent stopping` (INFO) on a graceful stop |
| `duration` | wall time of the tick |
| `push` | `off` (CRD-only), `unchanged` (same inventory, hourly force-sync not due), `ok`, `failed` (with `pushError`) |
| `consecutiveFailures` | failed ticks in a row; 0 after a success |
| `capabilities.<name>` | whether that collector could read what it needs; `status.notAssessed` on the CR says why not |
| `targets.<minor>.verdict/score/blockers` | the evaluation per target |
| `crdError` | the ClusterReadiness CRD could not be brought up to date (WARN); retried every tick until it succeeds, see below |

A tick **fails** when the agent could not evaluate and write the
`ClusterReadiness` status, including when it has no target to evaluate (no
`spec.targets` and an unknown or unparseable server version; the CR then
shows `Ready=Unknown` with the reason in `status.notAssessed`). A failed push does not fail the tick: the CR
status was written, and the agent's local result never depends on the
server. Push failures are logged at WARN and counted separately.

A tick also fails, and writes no status, when the agent could not read
the `ClusterReadiness` spec (or decode it), or could not set
`spec.targets` to `--targets`: a status for targets it did not read would
carry the current `observedGeneration` and pass for current. The object
keeps its last status, marked with the `upgradescope.basit.engineer/status-error`
annotation, and the next tick reads the spec again.

Each tick runs under a deadline of half the interval, at most 5 minutes, so a
wedged API call cannot stop the loop. Collection gets that deadline minus
a **reserve** of 30 seconds, or half the deadline when that is under a
minute (an `--interval` under 2m: at the 1m minimum the deadline is 30s,
so collection gets 15s). The reserve is the time of the work after
collection, each part on its own slice, so a collection that runs out its
time still leaves the status written, or the object marked as stale: the
`ClusterReadiness` calls (the CRD check, the spec read, the status write)
must end by half the reserve before the deadline, and a CRD check the
tick retries (the one at startup having failed) by three quarters of it,
so that a hung check, or the wait for a deleted CRD it creates again to be
Established, leaves the spec read and the status write at least a
quarter; the
`upgradescope.basit.engineer/status-error` marker then gets a quarter of the reserve
of its own; the push runs until the deadline, so it has at least a quarter
of the reserve. Within a tick, each API request is
given up after `--request-timeout` (default 30s; in the chart, set it
through `agent.extraArgs`), and each collector step gets its own share of
the collection's time, so a stalled step leaves only its capability not
assessed, its reason naming the step deadline. A stop (SIGTERM) that lands mid-tick
cancels the tick's calls; that tick is not counted or logged as failed, and
the agent logs `agent stopping` with `interruptedTick=true`.

## Probes

The chart sets them for you:

- **Agent**: liveness `/healthz`, readiness `/readyz` on port `http`
  (`agent.healthPort`, default 8081). The pod is NotReady until its first
  tick completes, and again whenever no tick has succeeded for
  2 × interval + the tick deadline (25 minutes at the default 10m
  interval). Liveness does not depend on ticks: the tick deadline
  already guarantees a live process ticks again, so restarting it would
  only lose its state.
- **Server**: liveness `/healthz`, readiness `/readyz`. A database outage
  takes the pod out of its Service without restarting it.

`helm install --wait` waits for the agent's first tick. It usually takes
seconds; the tick deadline caps it at 5 minutes.

## Metrics

Label values come from small fixed sets: targets, verdicts, severities,
categories, capabilities, route patterns, and on the server cluster names
(one per cluster in the fleet). No object, namespace or finding names.

### Agent (`:8081/metrics`)

| Metric | Type | Labels | Meaning |
|---|---|---|---|
| `upgradescope_agent_tick_duration_seconds` | histogram | | tick wall time |
| `upgradescope_agent_tick_errors_total` | counter | | failed ticks (status may be missing or stale) |
| `upgradescope_agent_push_errors_total` | counter | | snapshot pushes that failed (transient errors are retried first) |
| `upgradescope_agent_last_success_timestamp_seconds` | gauge | | Unix time of the last successful tick; 0 before the first |
| `upgradescope_agent_interval_seconds` | gauge | | the configured `--interval` |
| `upgradescope_readiness_score` | gauge | `target` | score 0-100 |
| `upgradescope_readiness_verdict` | gauge | `target`, `verdict` | 1 for the current verdict (`ready`, `blocked`, `unknown`), 0 for the other two |
| `upgradescope_findings` | gauge | `target`, `severity`, `category` | finding count; combinations with no findings are absent |
| `upgradescope_capability_available` | gauge | `capability` | 1 when the collector capability was available |
| `upgradescope_capability_partial` | gauge | `capability` | 1 when the capability was available but could not read everything it covers (`status.notAssessed` marks it partial) |
| `upgradescope_addon_evidence_age_seconds` | gauge | | 0 when the last successful tick listed every pod; otherwise the age of the full pod pass it reused for add-on detection (`--pod-pass-every`, `--pod-pass-max-age`; also `status.addOnEvidenceAgeSeconds`) |
| `upgradescope_kb_info` | gauge | `kb_version`, `max_known_k8s` | always 1; the embedded knowledge base |

Score, verdict, findings and capability gauges describe the last
**successful** tick. A failed tick leaves them as they were, and
`upgradescope_agent_last_success_timestamp_seconds` shows how old they are,
until the verdict is too old to stand as current: at least 3 ticks in a row
have failed **and** the last success is more than `2 × interval + 12m` old.
The agent then stops exporting those gauges (the ClusterReadiness it could
not update carries the `upgradescope.basit.engineer/status-error` annotation). The
first successful tick brings them back. An alert on `blocked` or `unknown`
therefore resolves while the agent is failing; the
`UpgradescopeAgentNotTicking` alert is the one that fires. It fires
`2 × interval + 10m` after the last success (its condition plus its `for`),
and the gauges are withdrawn 2m after that, for scrape and rule-evaluation
lag, so a firing `UpgradescopeUpgradeBlocked` never resolves with nothing
else firing. The age bound is what matters below a 10m interval (at 1m the
gauges go at about 14m, not at the third failed tick); from 10m up the 3
failed ticks come about as late, so at the default 10m interval the gauges
go at the third or fourth failed tick.
A target removed from `spec.targets` drops its series at the next tick.

Go runtime and process metrics (`go_*`, `process_*`) are included.

### Server (`/metrics` on the API port)

| Metric | Type | Labels | Meaning |
|---|---|---|---|
| `upgradescope_http_requests_total` | counter | `route`, `code` | requests by ServeMux pattern (`GET /api/v1/clusters/{id}`), `dashboard` for the SPA, `unmatched` for an unknown API or reserved path, `host-refused` (code `421`) for one the [Host check](operations/auth.md#the-host-check-dns-rebinding) refused before any route |
| `upgradescope_http_request_duration_seconds` | histogram | `route` | latency |
| `upgradescope_ingest_total` | counter | `result` | snapshot pushes: `accepted`, `duplicate`, `unauthorized`, `forbidden`, `conflict`, `invalid`, `too_large`, `error` |
| `upgradescope_cluster_score` | gauge | `cluster`, `target` | score of the cluster's current evaluation |
| `upgradescope_cluster_verdict` | gauge | `cluster`, `target`, `verdict` | 1 for the current verdict, 0 for the others |
| `upgradescope_cluster_blockers` | gauge | `cluster`, `target` | blocker findings |
| `upgradescope_cluster_last_push_age_seconds` | gauge | `cluster` | seconds since the cluster's agent last pushed (duplicates count) |
| `upgradescope_cluster_stale` | gauge | `cluster` | 1 when the agent has not pushed within `serve --stale-after` (default 2h), else 0 |
| `upgradescope_retention_prune_failures_total` | counter | `store` | retention prunes that failed, by `sqlite` or `postgres`; at 0 from startup (the process's own count: a restart resets it) |
| `upgradescope_retention_last_success_timestamp_seconds` | gauge | | Unix time of the last prune that completed; absent until the first one does |
| `upgradescope_retention_rows_deleted_total` | counter | `table` | rows retention deleted, `snapshots` or `evaluations`, including those a failed prune had already committed; at 0 from startup |

The three `upgradescope_retention_*` series exist only with a
`--retention` window (the default is 90d; with `--retention=0` there is no
prune and none of them is exported). The server prunes at startup and then
daily, in batches of at most 5,000 rows a transaction. A prune counts as
successful only when it ran to the end: the gauge is set then and only
then, so a prune that fails partway adds one to the failure counter, leaves
the gauge at the last complete prune and keeps the rows its committed
batches deleted (`upgradescope_retention_rows_deleted_total` counts them);
the next run resumes there. A prune cut short by the server stopping is
neither. The chart's `UpgradescopeRetentionStale` alert has three arms,
for three cadences of restart: it fires when the gauge is more than 2 days
old (a server that has stayed up more than 2 days since a prune last
completed, whose daily prunes fail); when the gauge is absent and the
oldest server process is more than 2 days old (a first prune that never
completes); or when the gauge is absent and `upgradescope_retention_prune_failures_total`
is above 0 (a prune failed in a process that has completed none, however
young it is: a server that restarts every few hours with a startup prune
that keeps failing). The third arm reads the counter's value, not its
increase, because the startup prune fails before Prometheus first scrapes
the new process: the series is first seen at 1 and `increase()` over it is
0. A restart resets both series, so a process that has not failed, or a
restart after a prune succeeded, stays quiet. One transient startup failure
(a locked database, say) keeps arm 3 firing until the next prune completes,
up to the daily retention interval, because the server does not retry
sooner. Not covered: a process that
restarts before it is scraped even once (the pod's restarts are the
signal), and, with several Postgres replicas, a replica that fails while
another has completed a prune, since `absent()` looks at the whole job
(the first arm still judges each replica's own gauge). Retention failing never fails `/readyz`: a server that
cannot prune still ingests and serves, and restarting it would not help,
so the metric and the log line (`server: retention: ... failed`) are the
signals ([Retention and backup](operations/retention-and-backup.md#when-the-prune-fails)).

Per-cluster gauges are read from the database at scrape time, for each
cluster's default target and every applicable `--targets` minor: the same
set the cluster page shows. Each scrape reads every cluster's snapshot
head and its evaluations' summary columns, never an inventory or a
report: for 500 clusters the response is ~740 KB and the scrape adds
~5 MiB to the heap, fine at a 1m scrape interval. If the database cannot
be read the scrape fails, so Prometheus reports `up == 0` for the
server. So it does when the scrape gets `503`: scrapes share two slots
with the cluster list and the fleet matrix, and wait up to 5s for one
(within Prometheus' default 10s `scrape_timeout`; a shorter
`scrape_timeout` can still end a scrape first, as a timeout rather than
a `503`), and their response waits for Prometheus in the budget the
other reads share ([memory and request limits](operations.md#memory-and-request-limits)).

With a read token (`--read-token`, chart `server.readToken` or
`server.readTokenFromSecret`), `/metrics` needs it like the rest of the
read API. The chart's ServiceMonitor sends it from the server's Secret.
Its series name every cluster, so a team-scoped read token gets `403`
there: scrape with `--read-token` or a read token minted for `*`.

A request the Host check refuses is counted as `route="host-refused"`,
`code="421"` (never by its Host or path, so the labels stay a fixed set)
and logged at most once a minute: the line names the Host, quoted and
escaped to ASCII, the client's address, and how many refusals since the
previous line went unlogged. A rising count is a DNS-rebinding page or,
more often, a client reaching the server under a name it was not given
with `--allowed-host`.

`/healthz`, `/readyz`, `/livez`, `/metrics`, any path below them, and
everything under `/api/` never fall through to the dashboard: an
unregistered one answers a JSON 404, so a probe or scrape pointed at the
wrong path fails loudly instead of receiving `index.html`.

## ClusterReadiness status

The agent writes a standard condition and `observedGeneration`:

```yaml
status:
  observedGeneration: 3
  lastEvaluated: "2026-10-02T12:00:02Z"
  conditions:
    - type: Ready
      status: "False"            # True | False | Unknown
      reason: Blocked            # Ready | Blocked | NotAssessed
      message: "1.36: 2 blocker(s) (score 72)"
      observedGeneration: 3
      lastTransitionTime: "2026-10-01T09:10:00Z"
  targets: [...]
```

`Ready` follows the **first** target's verdict: `True` for ready, `False`
(`Blocked`) for at least one blocker, `Unknown` (`NotAssessed`) when no
blockers were found but a required check was not assessed, or no target
could be evaluated. `lastTransitionTime` changes only when the status
does, so it reads as "blocked since".

`kubectl get ucr` shows `LastEvaluated`; more than about two intervals
old means the agent is not ticking. In scripts and pipelines:

```sh
kubectl wait clusterreadiness/cluster --for=condition=Ready --timeout=15m
```

Scraping with the Prometheus Operator, the alert rules and the Grafana
dashboard: [Prometheus and Grafana](guides/prometheus-grafana.md). The
Argo CD health check: [GitOps with Argo CD and Flux](guides/gitops-argo-flux.md).
