# Tenancy and access control

One server holds one fleet. Reads can be scoped to teams: a read token
minted for some teams, or an authenticating proxy's team header, reads
only the clusters those teams own a namespace in and, of those, only their
findings and team scores, a finding that spans teams cut to their
namespaces and objects ([Read access](auth.md)). Teams are the
attribution findings already carry (a namespace label, or the server's
`--team-map`). Team scoping limits what a read shows; it is not
isolation. The server has no users or roles, one database holds every
team's data, and whoever operates the server or the database sees all of
it. Tenants that must not share an operator, a server process or a
database therefore need separate servers, each with its own database and
tokens. In the cluster, the agent's footprint is described in
[Security model and RBAC](security-model-and-rbac.md).

## Credentials

The server has five credentials, each optional except where noted:

| Credential | Authorizes | Configured with |
|---|---|---|
| read token | every `GET /api/v1/*` endpoint (clusters, reports, findings, history, team rollups, exports, registry), `POST /api/v1/gate`, `/metrics`, for the whole fleet | `--read-token`; with none of the read credentials the read API is open, which `serve` refuses unless the address it binds is loopback or `--allow-anonymous-read` is set |
| team-scoped read tokens | the same reads, for some teams (or `*`, the fleet); `/metrics` takes a fleet-wide one only | `upgradescope tokens create --read --teams payments --teams checkout` (one team per flag); see [Read access](auth.md) |
| per-cluster ingest tokens | `POST /api/v1/snapshots` as one cluster name | `upgradescope tokens create <cluster>` |
| shared ingest token | `POST /api/v1/snapshots` as **any** cluster | `--ingest-token` |
| admin token | deleting and renaming clusters, plus reads | `--admin-token`; empty = both refused |

Per-cluster tokens are 64 random hex characters, printed once by
`tokens create`. The server stores each one's sha256 hash and its first 8
characters, which `tokens list` prints to tell them apart; never the
token. `tokens revoke <cluster> --id <id>` revokes one and `--all` every
active one of a cluster. To rotate without a gap: `tokens create`, put
the new token in the agent's Secret (the chart mounts it as a file the
agent re-reads, so no restart: the agent's next push after the kubelet has
synced the Secret, up to about 60 to 90 seconds at its default settings
plus a 5-second check, uses it; a token the agent got from `--server-token`
or `$UPGRADESCOPE_SERVER_TOKEN` is read once at start and needs
`kubectl rollout restart deploy/<fullname>-agent`, where `<fullname>` is the
release name plus `-upgradescope`, cut to 63 characters, or the release name
alone when it contains `upgradescope`), then
revoke the old id.

Tokens are bearer secrets: anyone who sees one in transit can replay it.
Serve HTTPS (`--tls-cert-file`, the chart's `server.tls`, or an Ingress
that terminates TLS) wherever pushes or reads cross a network you do not
trust. The agent logs a warning at startup when it would send its token
over plain `http://` to a host that is not loopback.

The server's SQLite database, and its `-wal` and `-shm` files, are
created readable by their owner only (0600), and a directory `serve`
creates for them 0700; an existing database is tightened on open.

What read tokens do **not** do:

- **They are not identities.** Reads are not attributed to anyone. A
  scoped token reads as its teams; who holds it is up to you.
- **They do not protect the dashboard's static files** (HTML, JS, CSS),
  which hold no data, nor `/healthz` and `/readyz`.
- **They are not encryption.** Without TLS (`--tls-cert-file`, the chart's
  `server.tls`, or an Ingress that terminates it) they cross the network in
  the clear.
- **They live in the browser.** The dashboard keeps one in `sessionStorage`
  (or `localStorage` when remembered); the server's Content-Security-Policy
  forbids inline and third-party script, which is what would read it.
- They cannot push snapshots or delete clusters: those need the ingest and
  admin tokens.

Rotating `--read-token` means handing the new value to every consumer at
once. With the chart, change the value
(`helm upgrade --set server.readToken=<new>`, or the contents of your
`server.existingSecret`): the server mounts it as a file and re-reads it, so
there is no restart, and the new token works once the kubelet has synced the
Secret (up to about 60 to 90 seconds at its default settings) plus a
5-second check; until then the old token still works
([Upgrade](upgrade.md#the-chart)). A `--read-token` given as a flag or
`$UPGRADESCOPE_READ_TOKEN` is read once at start and needs a restart
(`kubectl rollout restart deploy/<fullname>-server`, `<fullname>` as above).
Stored read tokens rotate one consumer at
a time (`tokens create --read`, hand it out, `tokens revoke --read --id <id>`).
For people, put an authenticating proxy in front of the server, which can
also set each person's team scope:
[Putting the dashboard behind SSO](auth.md#putting-the-dashboard-behind-sso).
