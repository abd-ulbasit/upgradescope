# Prometheus and Grafana

Both components always serve Prometheus metrics: the agent on its health
port (`agent.healthPort`, default 8081), the server on its API port,
behind the read token when one is set. Every metric, with its labels, is in
the [metrics reference](../observability.md#metrics). This page covers the
chart's Prometheus Operator objects, the alert rules and the Grafana
dashboard, all off by default.

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
