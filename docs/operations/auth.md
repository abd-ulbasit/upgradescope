# Read access: tokens, teams and SSO

Who may read what on `upgradescope serve`: the read API, the dashboard's
data, `POST /api/v1/gate` and `/metrics`. Pushes and cluster
administration have credentials of their own
([Tenancy and access control](tenancy.md#credentials)).

Every read is answered for a **scope**: the whole fleet, or a set of
teams. Teams are the attribution findings already carry (a namespace's
team label, or the server's `--team-map` rule that overrides it), so a
team can be handed a view of its own clusters and findings without the
rest of the fleet. A scope lists its teams comma separated, and `*` in a
token's scope is the whole fleet, so `--team-map` refuses a team name
that holds a comma or whitespace or is `*`; a namespace label value
cannot hold either.

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
  compares the bytes of every read endpoint and the gate with the answers
  the server gave before read scopes were added).
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

**A finding that spans teams is cut to the scope.** The engine reports
one finding per API, add-on or release line, over every namespace it is
used in: one `extensions/v1beta1` Ingress finding covers payments' and
web's Ingresses alike. A payments-scoped read gets that finding with
payments' teams, payments' namespaces (by the same namespace→team
mapping, after `--team-map`) and the objects in them only. A namespace
no team is attributed counts as another team's and is cut too. Its title
counts payments' objects ("(1 object in scope)") when the finding listed
every object, and otherwise drops the count. Its detail, which the engine
writes about everything the finding covers (each namespace's count, the
managers writing the objects, the add-on installs), is replaced by a
sentence that names the namespaces kept and says the rest is not shown.
The engine caps a finding's lists (100 namespaces, 100 objects) before
anyone reads it, and what the cap dropped cannot be divided by team, so
a cut finding counts none omitted and says more of the scope's may be
affected than it lists. A finding whose namespace list was capped is
always cut, even when every namespace it lists is the scope's: the ones
it does not list may be another team's or no team's, and its count and
"and N more" would say how much. Its key, severity, remediation and citations are
unchanged. A suppressed finding is cut the same way, and one whose
accepted objects are all another team's is left out. A finding wholly the
scope's is served exactly as the fleet-wide view serves it.

**What stays.** What describes the cluster as a whole: its name, version,
score, verdict, blocker and warning counts, capability gaps, and score
history. Two parts of the cluster's description name workloads and are
not shown:

- **Unrecognized images** (`unrecognizedImages`): image repositories no
  add-on matcher claims, collected cluster-wide and attributed to no
  namespace, so a scoped read gets none.
- **The helm capability's own words**: when the Helm collector could not
  read some releases, its reason and skipped list name them as
  `namespace/name`. A scoped read gets the gap (capability, required,
  partial) with the reason replaced and the skipped list withheld, in
  reports, exports, cluster summaries and the cluster's capability map.
  The other collectors' gaps name APIs, components and resources, and
  are shown whole.

**Everything else is hidden.** A cluster outside the scope answers `404`
with the same body as a cluster id that does not exist, on every
per-cluster endpoint and for `/api/v1/gate?cluster=`, so its name cannot
be probed by its answer. It is absent from `/clusters`, `/fleet` and
`/fleet/teams`. The answer is the same, the time it takes is not quite:
an id that exists but is out of scope costs one more store query than an
unknown one, so a patient caller could tell the two apart by timing.
Cluster ids are sequential and say nothing but how many clusters were
registered; what the 404 keeps from a scoped caller is the cluster's
name and data.

| Endpoint | A team-scoped read |
|---|---|
| `GET /api/v1/clusters` | in-scope clusters only, the helm gap's reason and skipped list withheld |
| `GET /api/v1/clusters/{id}` | `404` outside the scope; the helm capability's reason and skipped list withheld |
| `GET /api/v1/clusters/{id}/report` (stored or what-if) | `404` outside the scope; the scope's findings, cut to it, and team scores; no unrecognized images |
| `GET /api/v1/clusters/{id}/findings` | `404` outside the scope; the scope's findings, cut to it |
| `GET /api/v1/clusters/{id}/teams` | `404` outside the scope; the scope's teams |
| `GET /api/v1/clusters/{id}/history` | `404` outside the scope; the cluster's scores |
| `GET /api/v1/clusters/{id}/export` (CSV, HTML) | `404` outside the scope; the scope's findings, cut to it, and teams |
| `GET /api/v1/fleet` | in-scope clusters only |
| `GET /api/v1/fleet/teams` | in-scope clusters, the scope's teams |
| `POST /api/v1/gate` | without `?cluster=`: exactly as fleet-wide, it reads no stored data and every finding is the manifests'. With `?cluster=`: `404` outside the scope. The findings are evaluated from the scope's share of the cluster and the manifests alone: the API usage, custom resources, Helm releases, GitOps charts and add-ons in the namespaces of the scope's teams, never another team's, a namespace no team owns, cluster-scoped usage or the apiserver's deprecated-call rows. So a finding the PR's objects are in counts, titles and details only the PR's objects and the scope's, even when the PR names another team's namespace (its objects there are attributed to no team). Of the cluster's own findings, the scope's; the scope's team scores; the manifests' unrecognized images only; suppression warnings of the share only. The gate's verdict, which judges only what the manifests introduce, and the cluster verdict and score are the whole cluster's, so a CI status does not depend on who asks |
| `GET /api/v1/registry` | as fleet-wide: the public knowledge base |
| `GET /metrics` | `403`: its per-cluster series name every cluster, so it takes a fleet-wide credential (Prometheus gets `--read-token` or a `*` token) |

Every scoped answer carries `X-Upgradescope-Teams: <team,...>`, and the
dashboard says "Showing teams ... only" while it is set. A fleet-wide
answer carries no such header.

Every read that presents a bearer other than `--read-token` or the admin
token looks it up in the store (an indexed query on its hash), and while
no read token has been minted an open read API lists them once per read,
before the read and fleet concurrency limits apply. Requests with random
bearers therefore each cost a store query; put the server behind a proxy
that rate-limits unauthenticated clients if that matters to you.

### With the Helm chart

The chart has no values for the trusted header yet: add
`--trust-team-header` and `--trusted-proxy-cidr` with your own manifest
(the [example](#example-oauth2-proxy-as-a-sidecar)) or a post-renderer.
Read tokens minted with `tokens create --read` work with the chart as it
is (`kubectl exec deploy/<release>-server -- /upgradescope tokens create
--read --teams payments --db /data/upgradescope.sqlite`). Its Ingress
guard knows only `server.readToken` and `server.ingress.allowAnonymousRead`:
a deployment that relies on minted tokens alone sets
`server.ingress.allowAnonymousRead=true`, which passes
`--allow-anonymous-read`. That is safe once the first read token is
minted (the read API is closed from then on, for good) and open until
then, so mint one before enabling the Ingress.

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
header: the server reads tokens from it. oauth2-proxy sets it on the
upstream request only when told to: `--pass-authorization-header` (the
ID token as a bearer), or `--pass-basic-auth` (on by default) together
with a `--basic-auth-password`; in `auth_request` setups,
`--set-authorization-header` and `--set-basic-auth` put it on the auth
response, which the ingress may copy upstream. The example sets
`--pass-basic-auth=false` and none of the others, and its test fails if
one appears. To check yours, send a fleet-wide token through the proxy
after signing in (`curl -H 'Authorization: Bearer <token>' --cookie ...
https://<host>/metrics` must answer 200, which no team scope does). Keep
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

The proxy's requests carry no `Authorization` header, so a shared cache
in front of the proxy would key a team's answer by its URL alone. In
this mode every read answer is sent with `Vary: <header>, Authorization`,
and every scoped answer, in any mode, with
`Cache-Control: private, no-store`.

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

    **Everything that arrives from a trusted address is the proxy.** With
    `--trusted-proxy-cidr=127.0.0.1/32` that is every connection made
    over the pod's loopback, not only the sidecar's:

    - `kubectl port-forward` to the server's pod delivers its connections
      from `127.0.0.1`, so anyone allowed `pods/portforward` in the
      server's namespace can send the header to port 8080 and read any
      team they can name (never the whole fleet: `*` is a team). Treat
      that permission as read access to every team, or keep it to the
      people who already have it.
    - A service-mesh sidecar (Istio, Linkerd and others) that delivers
      inbound traffic to the application over localhost makes every
      client, inside or outside the cluster, `127.0.0.1`: the header
      would then be trusted from anyone. Keep the server's pod out of the
      mesh (the example sets `sidecar.istio.io/inject: "false"` and
      `linkerd.io/inject: disabled`).

Map identity-provider groups to team names: the header's values are
compared with the teams namespaces are attributed to, exactly. With
oauth2-proxy, `--oidc-groups-claim` picks the claim and `--allowed-group`
limits who may sign in at all.

### Example: oauth2-proxy as a sidecar

[`deploy/examples/oauth2-proxy/upgradescope-oauth2-proxy.yaml`](https://github.com/abd-ulbasit/upgradescope/blob/main/deploy/examples/oauth2-proxy/upgradescope-oauth2-proxy.yaml)
runs oauth2-proxy in the server's pod. It reaches the server on
`127.0.0.1`, the only address the server trusts the header from
(`--trusted-proxy-cidr=127.0.0.1/32`), so no other pod, node or client
that reaches the server over the network can set a scope, whatever the
network allows. What reaches it over loopback can, which is why the
danger box above names port-forwarding and mesh sidecars, and why the
pod opts out of Istio and Linkerd sidecar injection. Two Services:

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
