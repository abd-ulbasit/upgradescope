# Read access: tokens, teams and SSO

Who may read what on `upgradescope serve`: the read API, the dashboard's
data, `POST /api/v1/gate` and `/metrics`. Pushes and cluster
administration have credentials of their own
([Tenancy and access control](tenancy.md#credentials)).

Every read is answered for a **scope**: the whole fleet, or a set of
teams. Teams are the attribution findings already carry (a namespace's
team label, or the server's `--team-map` rule that overrides it), so a
team can be handed a view of its own clusters and findings without the
rest of the fleet.

## Read credentials

The server checks, in this order:

| Credential | Scope | Configured with |
|---|---|---|
| `--read-token` | the whole fleet | `serve --read-token` (or `$UPGRADESCOPE_READ_TOKEN`, `--read-token-file`) |
| the admin token | the whole fleet | `serve --admin-token` |
| a stored read token | its teams, or the whole fleet for `*` | `upgradescope tokens create --read --teams payments,checkout` |
| a trusted proxy's team header | the teams it lists, never the whole fleet | `serve --trust-team-header X-Forwarded-Groups --trusted-proxy-cidr 127.0.0.1/32` |
| nothing | the whole fleet, only while the read API is open | no `--read-token`, no read token ever minted, no trusted header |

All tokens are sent as `Authorization: Bearer <token>`. Once
`--read-token` is set, a read token has been minted or the trusted header
is configured, a request that presents no valid credential gets `401`. `serve` refuses an open read API on an address that is not
loopback unless `--allow-anonymous-read` is set.

## Team-scoped read tokens

```console
$ upgradescope tokens create --read --teams payments,checkout
3f9a1c0e...  (the token, on stdout alone)
read token id 1 (prefix 3f9a1c0e) for teams checkout,payments created — shown once: ...
$ upgradescope tokens create --read --teams '*'
$ upgradescope tokens list --read
ID  TEAMS              PREFIX    CREATED               REVOKED
1   checkout,payments  3f9a1c0e  2026-10-04T09:12:44Z  -
2   *                  b71d02aa  2026-10-04T09:13:02Z  -
$ upgradescope tokens revoke --read --id 1
```

Read tokens are 64 random hex characters, printed once. The database
keeps each one's sha256 hash and its first 8 characters (which
`tokens list --read` prints), never the token, exactly as for ingest
tokens. They live in a table of their own: a read token never pushes, and
an ingest token never reads. The commands take the same `--db` or
`--db-url` as `serve`, and the server looks a token up on every request,
so one minted or revoked takes effect at once, without a restart.

- **Minting the first read token closes the read API.** A server that ran
  open (on loopback, or with `--allow-anonymous-read`) needs a credential
  from then on. Rows are only ever revoked, never deleted, so revoking the
  last one does not open the read API again.
- **`--read-token` keeps working** as a fleet-wide token, answering exactly
  what it answered before scoped tokens existed (a regression test
  compares the bytes of every read endpoint).
- **A revoked token is refused** (`401`) from the next request on.

### What a team-scoped read sees

**Which clusters.** A cluster is in the scope of teams *T* when one of its
current evaluations (each target's newest evaluation of the cluster's
latest snapshot) names a team of *T*. An evaluation names the teams its
inventory attributes a namespace to, after `--team-map`: the same
namespace→team mapping the engine uses to give each finding its teams. So
a cluster is in scope as soon as one of the team's namespaces is in it:
every workload in that namespace, and every finding about them, is that
team's. A team's cluster with no finding at all is in scope too. A cluster
with no evaluation yet (no snapshot pushed) is in no team's scope.

**Which findings and teams.** Of an in-scope cluster's report, a scoped
read keeps the findings (and suppressed findings) attributed to one of
its teams, and the team scores of its teams. Findings no team owns, such
as a cluster-wide add-on in a namespace without a team, are left out:
they are not the scope's to see. A team's score is still computed from
the whole report, so the team's verdict is the one the fleet-wide view
shows.

**What stays.** What describes the cluster as a whole: its name, version,
score, verdict, blocker and warning counts, capability gaps, and score
history.

**Everything else is hidden.** A cluster outside the scope answers `404`
with the same body as a cluster id that does not exist, on every
per-cluster endpoint and for `/api/v1/gate?cluster=`, so neither its name
nor its id can be probed. It is absent from `/clusters`, `/fleet` and
`/fleet/teams`.

| Endpoint | A team-scoped read |
|---|---|
| `GET /api/v1/clusters` | in-scope clusters only |
| `GET /api/v1/clusters/{id}` | `404` outside the scope |
| `GET /api/v1/clusters/{id}/report` (stored or what-if) | `404` outside the scope; the scope's findings and team scores |
| `GET /api/v1/clusters/{id}/findings` | `404` outside the scope; the scope's findings |
| `GET /api/v1/clusters/{id}/teams` | `404` outside the scope; the scope's teams |
| `GET /api/v1/clusters/{id}/history` | `404` outside the scope; the cluster's scores |
| `GET /api/v1/clusters/{id}/export` (CSV, HTML) | `404` outside the scope; the scope's findings and teams |
| `GET /api/v1/fleet` | in-scope clusters only |
| `GET /api/v1/fleet/teams` | in-scope clusters, the scope's teams |
| `POST /api/v1/gate` | without `?cluster=`: as fleet-wide, it reads no stored data. With `?cluster=`: `404` outside the scope; of the cluster's findings, the scope's. The manifests' own findings and the verdict are unchanged |
| `GET /api/v1/registry` | as fleet-wide: the public knowledge base |
| `GET /metrics` | `403`: its per-cluster series name every cluster, so it takes a fleet-wide credential (Prometheus gets `--read-token` or a `*` token) |

Every scoped answer carries `X-Upgradescope-Teams: <team,...>`, and the
dashboard says "Showing teams ... only" while it is set. A fleet-wide
answer carries no such header.

## Putting the dashboard behind SSO

For people, put an authenticating proxy in front of the server:
[oauth2-proxy](https://oauth2-proxy.github.io/oauth2-proxy/), an
identity-aware proxy, or your ingress controller's external-auth support.
Three patterns, from simplest to most integrated:

1. **Proxy in front, tokens behind.** The proxy decides who may open the
   dashboard; the dashboard still asks for a read token once (stored in
   that browser), fleet-wide or scoped to the person's teams. Machine
   clients (the CI gate, Prometheus, scripts) call the server with their
   tokens, directly or through a proxy route that skips authentication.
   No server flag changes.
2. **The proxy is the gate.** Run without any read credential
   (`--allow-anonymous-read`, chart `server.ingress.allowAnonymousRead=true`
   when the chart's Ingress carries the auth annotations) and make sure
   nothing but the proxy can reach the read API: a ClusterIP Service, a
   NetworkPolicy (`networkPolicy.enabled`, `serverIngressFrom` naming the
   proxy, Prometheus and the agents' sources). Everyone the proxy lets in
   reads the whole fleet, and anyone who reaches the Service directly
   reads it too, so this rests on the network policy.
3. **The proxy names the teams** (below): the proxy's group header sets
   each person's scope, and the server trusts it only from the proxy.

With any of them, do not let the proxy overwrite the `Authorization`
header (oauth2-proxy's `--pass-authorization-header` and
`--set-authorization-header` do): the server reads tokens from it. Keep
cluster administration off the proxied path: the admin token belongs to
operators and their CLI, not to the dashboard.

### Trusted team header (`--trust-team-header`)

```
upgradescope serve --trust-team-header X-Forwarded-Groups --trusted-proxy-cidr 127.0.0.1/32
```

A read whose TCP peer is in a `--trusted-proxy-cidr` range and that
carries the header reads as the teams it lists, comma separated, every
copy of the header counted (oauth2-proxy sends one per group). A bearer
token, when one is presented and valid, takes precedence. The peer is the
connection's source address, never `X-Forwarded-For`. From any other
address the header is ignored: a client that reaches the server directly
cannot spoof it, and needs a token. The header always names teams: `*` in
it is a team called `*`, never the whole fleet, so whoever can name a
group in the identity provider cannot grant fleet-wide reads with it.
People who need the fleet use a fleet-wide token, which the dashboard
sends through the proxy.

The two flags go together, and the mode is off by default.

!!! danger "Only safe when the proxy strips the header"
    The server trusts whatever the proxy forwards. If a client can send
    its own `X-Forwarded-Groups` through the proxy and the proxy passes it
    on (or appends to it), that client reads any team it names. Use this
    mode only with a proxy that removes client-supplied copies of the
    header before setting it, on every route it forwards to the read API,
    and with `--trusted-proxy-cidr` covering the proxy's addresses and
    nothing else. A pod CIDR is not "the proxy": every pod in it could
    send the header. oauth2-proxy v7 removes client-supplied copies of
    the headers it sets unless told to preserve them
    (`preserveRequestValue`); check yours with
    `curl -H 'X-Forwarded-Groups: other-team' https://<host>/api/v1/clusters`
    after signing in, which must not list `other-team`'s clusters.

Map identity-provider groups to team names: the header's values are
compared with the teams namespaces are attributed to, exactly. With
oauth2-proxy, `--oidc-groups-claim` picks the claim and `--allowed-group`
limits who may sign in at all.

### Example: oauth2-proxy as a sidecar

[`deploy/examples/oauth2-proxy/upgradescope-oauth2-proxy.yaml`](https://github.com/abd-ulbasit/upgradescope/blob/main/deploy/examples/oauth2-proxy/upgradescope-oauth2-proxy.yaml)
runs oauth2-proxy in the server's pod. It reaches the server on
`127.0.0.1`, the only address the server trusts the header from
(`--trusted-proxy-cidr=127.0.0.1/32`), so no other pod, node or client
can set a scope, whatever the network allows. Two Services:

- `upgradescope` (port 80) is the proxy, for people: point your Ingress
  and the OIDC client's redirect URL (`https://<host>/oauth2/callback`)
  at it.
- `upgradescope-api` (port 8080) is the server itself, for machines:
  agents push there with their ingest tokens, CI and Prometheus read with
  fleet-wide or scoped tokens. Every read there needs a token.

The proxy skips authentication on `POST /api/v1/snapshots` only, so agents
can push through it too; the server never takes the team header on a push.
Adapt the provider flags to your identity provider, and create the
`oauth2-proxy` Secret (`client-id`, `client-secret`, `cookie-secret`)
first; the manifest's header lists what it expects.
