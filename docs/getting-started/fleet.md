# Fleet server

`upgradescope serve` collects what every agent sees. It stores each
cluster's inventory snapshots and evaluations (SQLite or Postgres), and
serves a fleet score matrix, per-team rollups, score history, auditor
exports, the CI gate endpoint, change notifications and a web dashboard.
Agents work without it; the server adds history and the cross-cluster view.

## Start a server

On a workstation, for a first look:

```sh
upgradescope serve --ingest-token "$(openssl rand -hex 32)"
# listening on http://127.0.0.1:8080; open it in a browser for the dashboard
```

The defaults are deliberately narrow: it listens on loopback
(`--listen 127.0.0.1:8080`) and keeps SQLite in `upgradescope.db`. On any
other address it refuses to start without a read token, unless
`--allow-anonymous-read` says something else guards it:

```console
$ upgradescope serve --listen :8080
refusing to serve the read API and /api/v1/gate without a token on ":8080": set --read-token, listen on loopback, or pass --allow-anonymous-read to accept open reads
```

In a cluster, the chart runs it next to the agent
(`--set server.enabled=true`) or alone as a fleet hub with Postgres, an
Ingress and TLS; the [chart README](https://github.com/abd-ulbasit/upgradescope/blob/main/deploy/chart/README.md#fleet-hub-server-only)
has the hub recipe.

## Connect agents

Give each cluster its own ingest token. It is printed once, only its hash
is stored, and it can push only as the cluster it names:

```console
$ upgradescope tokens create prod-eu-1 --db upgradescope.db
ee2b2560...
ingest token id 1 (prefix ee2b2560) for cluster "prod-eu-1" created — shown once, only its hash is stored
```

(Against Postgres, pass `--db-url` or set `$UPGRADESCOPE_DB_URL`.) Then, in
that cluster:

```sh
kubectl create namespace upgradescope
kubectl -n upgradescope create secret generic push-token --from-literal=serverToken=<token>
helm install upgradescope deploy/chart -n upgradescope \
  --set agent.clusterName=prod-eu-1 \
  --set agent.serverUrl=https://upgradescope.example.com \
  --set agent.existingSecret=push-token
```

The agent pushes its inventory when it changes and at least hourly
(`--force-sync-every`), retrying transient failures. The server evaluates
each new snapshot against the cluster's next minor and every
`serve --targets` minor, and stores the result. The first push from a
cluster registers it and binds its name to the cluster's UID (the
`kube-system` namespace UID), so two clusters cannot share a name.

## Read the fleet

```sh
curl -s -H "Authorization: Bearer $READ_TOKEN" "$SERVER/api/v1/fleet?targets=1.37,1.38"
curl -s -H "Authorization: Bearer $READ_TOKEN" "$SERVER/api/v1/fleet/teams?target=1.38"
curl -s -H "Authorization: Bearer $READ_TOKEN" "$SERVER/api/v1/clusters/1/report?target=1.38"
```

The fleet matrix serves stored evaluations only: a cell is `null` when a
cluster has no evaluation for that target. Per-cluster reads (report,
findings, teams) fall back to a *what-if*, evaluated on request from the
latest snapshot and not stored, and say which they served
(`source: stored` or `what-if`). Every endpoint is in the
[REST API reference](../reference/api.md).

## Teams

Findings are attributed to teams through a namespace label (`team` by
default; `--team-label` on the agent and `scan`). The server can override
labels with a glob map, first match wins:

```yaml
# serve --team-map teams.yaml (chart: server.teamMap)
- pattern: "payments-*"
  team: payments
- pattern: "kube-*"
  team: platform
```

Team scores apply the score formula to each team's findings. Findings with
no team are grouped as `unattributed`. Teams split the findings; they are
not an access boundary ([Tenancy](../operations/tenancy.md)).

## Next

- Retention, sizing and backups: [Retention and backup](../operations/retention-and-backup.md).
- Tokens, SSO and who can read what: [Tenancy and access control](../operations/tenancy.md).
- Notifications, cluster lifecycle and exports: [Running the server](../operations.md).
