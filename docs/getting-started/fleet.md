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
(`--listen 127.0.0.1:8080`) and keeps SQLite in `upgradescope.db`. That
file and its `-wal` and `-shm` files hold every cluster's inventory and the
token hashes; `serve` creates them readable by its user only (0600), in
a directory it creates 0700, and tightens an existing database on open.
On any other address it refuses to start without a read credential
(`--read-token`, a read token minted with `tokens create --read`, or a
trusted proxy's team header), unless `--allow-anonymous-read` says
something else guards it:

```console
$ upgradescope serve --listen :8080
server: refusing to serve the read API and /api/v1/gate without a read token on [::]:8080 (from ":8080"), which is not a loopback address: set a read token (--read-token) or mint one (...), trust an authenticating proxy's team header (--trust-team-header), listen on loopback, or allow anonymous reads (--allow-anonymous-read)
```

Without a read credential, the read API, the dashboard's data and
`/api/v1/gate` answer anyone who can reach the address. On loopback that is
every user and process on the machine, though not a web page: on loopback
`serve` answers only requests whose Host is `localhost` or a loopback
address (or an `--allowed-host`), and any other `421`, so a page that
rebinds its own name to `127.0.0.1` reads nothing
([The Host check](../operations/auth.md#the-host-check-dns-rebinding)).
The first command above is meant
for a single-user workstation. Add `--read-token` (and send it as
`Authorization: Bearer <token>`) wherever that is not acceptable.

In a cluster, the chart runs it next to the agent
(`--set server.enabled=true`; with no `server.readToken` it runs with
`--require-read-credential`, so reads are `401` until you mint a read
token, which the install notes show how to do) or alone as a fleet hub with Postgres, an
Ingress and TLS; the [chart README](https://github.com/abd-ulbasit/upgradescope/blob/main/deploy/chart/README.md#fleet-hub-server-only)
has the hub recipe.

## Connect agents

Give each cluster its own ingest token. It is printed once, the server
keeps its sha256 hash and first 8 characters (never the token), and it can
push only as the cluster it names:

```console
$ upgradescope tokens create prod-eu-1 --db upgradescope.db
ee2b2560...
ingest token id 1 (prefix ee2b2560) for cluster "prod-eu-1" created — shown once: the server stores its sha256 hash and its first 8 characters, never the token
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

A chart from a clone defaults to the image of the release it was cut for.
To run unreleased changes, add `--set image.repository=... --set image.tag=...`
naming an image you build and push yourself ([In-cluster](in-cluster.md)
shows how).

The agent pushes its inventory when it changes and at least hourly
(`--force-sync-every`), retrying transient failures. The server evaluates
each new snapshot against the cluster's next minor and every
`serve --targets` minor, and stores the result. The first push from a
cluster registers it and binds its name to the cluster's UID (the
`kube-system` namespace UID), so two clusters cannot share a name.

## Exposing the server to remote agents

Every push carries the cluster's ingest token (`Authorization: Bearer`)
and its full inventory: namespaces, workloads, images, Helm releases and
API callers. **Over plain `http://`, both cross the network in
cleartext**: anyone on the path can read the inventory and replay the
token to push as that cluster (the shared `--ingest-token`, as any
cluster). Serve agents in other clusters over HTTPS only, in one of two
ways:

- **Terminate TLS at an Ingress.** With the chart, set
  `server.ingress.enabled=true`, `server.ingress.host` and a read token;
  `server.ingress.tls` is on by default and serves the host from the
  Secret `<fullname>-server-tls` (`upgradescope-server-tls` for a release
  named upgradescope), which cert-manager can fill through an annotation:

    ```yaml
    agent:
      enabled: false                     # a hub; the chart README has the full recipe
    server:
      enabled: true
      existingSecret: upgradescope-hub   # with a readToken key
      readTokenFromSecret: true
      ingress:
        enabled: true
        className: nginx
        host: upgradescope.example.com
        annotations: {cert-manager.io/cluster-issuer: letsencrypt}
    ```

- **Serve TLS directly.** `upgradescope serve --tls-cert-file tls.crt
  --tls-key-file tls.key` (both or neither) speaks HTTPS with TLS 1.2 at
  the oldest. It re-reads the pair when either file changes, so a
  renewal (cert-manager rewriting a mounted Secret) needs no restart.
  With the chart, `server.tls.secretName` names a `kubernetes.io/tls`
  Secret, or `server.tls.certManager.issuerRef` has cert-manager issue
  one; probes, the Service port and the ServiceMonitor follow.

Agents then use `--server-url https://...`. A server whose certificate a
private CA issued needs that CA on the agent: `--server-ca-file ca.pem`
(chart: `agent.serverCA.configMap` or `agent.serverCA.secret`, key
`agent.serverCA.key`) adds the PEM bundle to the system roots. There is
no option to skip verification. An agent started with an `http://`
server URL to a host other than loopback logs a warning saying its token
travels unencrypted; that includes in-cluster `*.svc` URLs, since pod
traffic is not encrypted either unless your CNI or mesh does it.

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
default; `--team-label` on the agent and `scan`). Team attribution needs a
cluster: `scan --files` (and a gate without `?cluster=`) reads manifests, not
the cluster's Namespace objects, so every finding of a files scan is
unattributed and `--team-label` is refused with `--files`. To attribute a PR's
findings to teams, gate it against a stored cluster
(`/api/v1/gate?cluster=`), which uses that cluster's namespace labels. The
server can override labels with a glob map, first match wins:

```yaml
# serve --team-map teams.yaml (chart: server.teamMap)
- pattern: "payments-*"
  team: payments
- pattern: "kube-*"
  team: platform
```

Team scores apply the score formula to each team's findings. Findings with
no team are grouped as `(unattributed)` (a name no label value can take, and
`--team-map` refuses it as a team name, so it never merges with a team called
`unattributed`). A team's verdict is
`blocked` by a blocker of its own or an unattributed one (kubelet skew, an object in an
unlabelled namespace), which cannot be ruled out as the team's; otherwise
it is `unknown` when the cluster's report has a required not-assessed gap,
which may hide any team's blocker; otherwise `ready`. Another team's
blocker does not lower it. The fleet-wide team rollup (`GET /api/v1/fleet/teams`
and the dashboard's Teams page) carries each team's worst verdict across the
clusters, `blocked` over `unknown` over `ready`, because a team's score alone
cannot say whether it is blocked by a blocker no team owns. A read token minted for a team
(`upgradescope tokens create --read --teams payments`) reads only that
team's clusters, findings and scores ([Read access](../operations/auth.md)).

## Next

- Retention, sizing and backups: [Retention and backup](../operations/retention-and-backup.md).
- Tokens, SSO and who can read what: [Tenancy and access control](../operations/tenancy.md)
  and [Read access: tokens, teams and SSO](../operations/auth.md).
- Notifications, cluster lifecycle and exports: [Running the server](../operations.md).
