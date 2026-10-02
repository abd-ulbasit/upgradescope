# Observability

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

## Agent logs

The agent writes a startup line (version, KB version and horizon, interval,
tick deadline, server URL or CRD-only, health address) and exactly one line
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

A tick **fails** when the agent could not evaluate and write the
`ClusterReadiness` status, including when it has no target to evaluate (no
`spec.targets` and an unknown or unparseable server version; the CR then
shows `Ready=Unknown` with the reason in `status.notAssessed`). A failed push does not fail the tick: the CR
status was written, and the agent's local result never depends on the
server. Push failures are logged at WARN and counted separately.

Each tick runs under a deadline of half the interval, at most 5 minutes, so a
wedged API call cannot stop the loop. A stop (SIGTERM) that lands mid-tick
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
| `upgradescope_kb_info` | gauge | `kb_version`, `max_known_k8s` | always 1; the embedded knowledge base |

Score, verdict, findings and capability gauges describe the last
**successful** tick. A failed tick leaves them as they were, and
`upgradescope_agent_last_success_timestamp_seconds` shows how old they are.
A target removed from `spec.targets` drops its series at the next tick.

Go runtime and process metrics (`go_*`, `process_*`) are included.

### Server (`/metrics` on the API port)

| Metric | Type | Labels | Meaning |
|---|---|---|---|
| `upgradescope_http_requests_total` | counter | `route`, `code` | requests by ServeMux pattern (`GET /api/v1/clusters/{id}`), `dashboard` for the SPA, `unmatched` for an unknown API or reserved path |
| `upgradescope_http_request_duration_seconds` | histogram | `route` | latency |
| `upgradescope_ingest_total` | counter | `result` | snapshot pushes: `accepted`, `duplicate`, `unauthorized`, `forbidden`, `conflict`, `invalid`, `too_large`, `error` |
| `upgradescope_cluster_score` | gauge | `cluster`, `target` | score of the cluster's current evaluation |
| `upgradescope_cluster_verdict` | gauge | `cluster`, `target`, `verdict` | 1 for the current verdict, 0 for the others |
| `upgradescope_cluster_blockers` | gauge | `cluster`, `target` | blocker findings |
| `upgradescope_cluster_last_push_age_seconds` | gauge | `cluster` | seconds since the cluster's agent last pushed (duplicates count) |
| `upgradescope_cluster_stale` | gauge | `cluster` | 1 when the agent has not pushed within `serve --stale-after` (default 2h), else 0 |

Per-cluster gauges are read from the database at scrape time, for each
cluster's default target and every applicable `--targets` minor: the same
set the cluster page shows. Each scrape reads every cluster's latest
snapshot, which is fine for fleets of hundreds of clusters at a 1m scrape
interval. If the database cannot be read the scrape fails, so Prometheus
reports `up == 0` for the server.

With a read token (`--read-token`, chart `server.readToken` or
`server.readTokenFromSecret`), `/metrics` needs it like the rest of the
read API. The chart's ServiceMonitor sends it from the server's Secret.

`/healthz`, `/readyz`, `/livez`, `/metrics`, any path below them, and
everything under `/api/` never fall through to the dashboard: an
unregistered one answers a JSON 404, so a probe or scrape pointed at the
wrong path fails loudly instead of receiving `index.html`.

## Scraping with the Prometheus Operator

```yaml
metrics:
  serviceMonitor:
    enabled: true
    interval: 1m
    labels:
      release: kube-prometheus-stack   # whatever your Prometheus selects on
```

This renders a ServiceMonitor for the agent, plus a headless
`<fullname>-agent-metrics` Service for it to select, and, with
`server.enabled`, one for the server. Without the operator, scrape the
agent pod's port `http` (8081) and the server Service's port `http` at
`/metrics`.

With `networkPolicy.enabled`, the server only admits the agent and
`networkPolicy.serverIngressFrom`; add your Prometheus there, since
`/metrics` shares the API port. The chart renders no NetworkPolicy for the
agent.

## Alerts

`metrics.prometheusRule.enabled: true` renders a PrometheusRule (use
`metrics.prometheusRule.labels` for your rule selector). The rules select
the jobs the chart's ServiceMonitors produce: `<fullname>-agent-metrics` and
`<fullname>-server` (`upgradescope-agent-metrics` and `upgradescope-server`
for a release named `upgradescope`).

| Alert | Expression (simplified) | For | Severity |
|---|---|---|---|
| `UpgradescopeUpgradeBlocked` | `upgradescope_readiness_verdict{verdict="blocked"} == 1` | 15m | warning |
| `UpgradescopeVerdictUnknown` | `upgradescope_readiness_verdict{verdict="unknown"} == 1` | 1h | info |
| `UpgradescopeAgentNotTicking` | `time() - upgradescope_agent_last_success_timestamp_seconds > 2 * upgradescope_agent_interval_seconds + 300 or absent(up{job="…-agent-metrics"} == 1)` | 5m | warning |
| `UpgradescopeClusterStale` (server only) | `upgradescope_cluster_last_push_age_seconds > 7200` | 10m | warning |

Agents push on every inventory change and at least hourly
(`--force-sync-every`), so keep `metrics.prometheusRule.clusterStaleAfterSeconds`
(default 7200) above an hour.

For a hub Prometheus that only scrapes the server, the fleet versions of
the first two alerts read:

```promql
upgradescope_cluster_verdict{verdict="blocked"} == 1
upgradescope_cluster_verdict{verdict="unknown"} == 1
```

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

## Argo CD health check

Argo CD applies health checks to the resources an Application tracks. To
gate a sync or a promotion on readiness, commit the `ClusterReadiness`
object (with `spec.targets`) to the Application's repository and leave the
chart's `agent.targets` empty, so Git owns the spec and the agent fills the
status. Then add this to `argocd-cm`:

```yaml
data:
  # The script needs the string, math and os (os.time) libraries.
  resource.customizations.useOpenLibs.upgradescope.dev_ClusterReadiness: "true"
  resource.customizations.health.upgradescope.dev_ClusterReadiness: |
    -- Treat a status older than this as stale: three default 10m intervals.
    local staleAfterSeconds = 30 * 60

    local hs = { status = "Progressing", message = "Waiting for the upgradescope agent's first evaluation" }
    if obj.status == nil or obj.status.conditions == nil then
      return hs
    end
    if obj.metadata.generation ~= nil and obj.status.observedGeneration ~= nil
        and obj.status.observedGeneration < obj.metadata.generation then
      hs.message = "spec changed; waiting for the agent's next tick"
      return hs
    end

    -- lastEvaluated is RFC 3339 UTC ("2026-10-02T12:00:00Z"). Convert it to
    -- Unix time arithmetically: os.time on a table would read it as the
    -- controller's local time.
    local function unixUTC(y, m, d, h, mi, s)
      if m <= 2 then y = y - 1 end
      local era = math.floor(y / 400)
      local yoe = y - era * 400
      local doy = math.floor((153 * ((m + 9) % 12) + 2) / 5) + d - 1
      local doe = yoe * 365 + math.floor(yoe / 4) - math.floor(yoe / 100) + doy
      return (era * 146097 + doe - 719468) * 86400 + h * 3600 + mi * 60 + s
    end
    local last = obj.status.lastEvaluated
    if last ~= nil then
      local y, mo, d, h, mi, s = string.match(last, "^(%d+)-(%d+)-(%d+)T(%d+):(%d+):(%d+)")
      if y ~= nil then
        local at = unixUTC(tonumber(y), tonumber(mo), tonumber(d), tonumber(h), tonumber(mi), tonumber(s))
        if os.time() - at > staleAfterSeconds then
          hs.status = "Degraded"
          hs.message = "status is stale: last evaluated " .. last .. "; is the upgradescope agent running?"
          return hs
        end
      end
    end

    for _, c in ipairs(obj.status.conditions) do
      if c.type == "Ready" then
        hs.message = c.message
        if c.status == "True" then
          hs.status = "Healthy"
        else
          -- False (Blocked) or Unknown (NotAssessed): not safe to upgrade.
          hs.status = "Degraded"
          hs.message = c.reason .. ": " .. c.message
        end
        return hs
      end
    end
    return hs
```

| State | Health |
|---|---|
| no status yet | Progressing |
| `observedGeneration` behind `metadata.generation` (spec just edited) | Progressing |
| `lastEvaluated` older than `staleAfterSeconds` | Degraded |
| `Ready=True` | Healthy |
| `Ready=False` (Blocked) or `Unknown` (NotAssessed) | Degraded |

Raise `staleAfterSeconds` if you run the agent with a longer `--interval`.
The agent Deployment's own health in Argo CD follows its readiness probe.

## Grafana

`deploy/grafana/upgradescope-dashboard.json` has an agent row (score,
verdict and findings per target, capabilities, time since the last
successful tick, tick duration, failed ticks and pushes) and a server row
(clusters table, time since each cluster's last push, push results, API
latency and errors). Import it with Dashboards → New → Import → upload the
file, then pick your Prometheus in its "Data source" variable.

With the Grafana sidecar (kube-prometheus-stack enables it), let the chart
ship it:

```yaml
metrics:
  grafanaDashboard:
    enabled: true
    labels:
      grafana_dashboard: "1"       # the sidecar's default label
    annotations:
      grafana_folder: Kubernetes   # optional, if your sidecar uses folders
```
