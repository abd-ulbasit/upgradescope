# Read access: tokens, teams and SSO

Who may read what on `upgradescope serve`: the read API, the dashboard's
data, `POST /api/v1/gate` and `/metrics`. Pushes and cluster
administration have credentials of their own
([Tenancy and access control](tenancy.md#credentials)).

Every read is answered for a **scope**: the whole fleet, or a set of
teams. Teams are the attribution findings already carry (a namespace's
team label, or the server's `--team-map` rule that overrides it), so a
team can be handed a view of its own clusters and findings without the
rest of the fleet. A team name is free text: a `--team-map` team can be
`Platform Team` or `Équipe, Paris`. A read token names one team per
`--teams` flag, taken as written, and the trusted header and the
`X-Upgradescope-Teams` answer header use the
[team list encoding](#the-team-list-encoding). The one name a scope
cannot hold is `*`, a token's scope of the whole fleet: a `--team-map`
team called `*` is read as the team `(*)` (serve logs a warning at
startup and keeps serving), so mint its token with `--teams '(*)'`. A
map that also names a team `(*)` makes the two one team, and the
warning says so: rename one to keep them apart. A namespace label value
cannot be `*`.

## Read credentials

The server checks, in this order:

| Credential | Scope | Configured with |
|---|---|---|
| `--read-token` | the whole fleet | `serve --read-token` (or `$UPGRADESCOPE_READ_TOKEN`, `--read-token-file`) |
| the admin token | the whole fleet | `serve --admin-token` |
| a stored read token | its teams, or the whole fleet for `*` | `upgradescope tokens create --read --teams payments --teams checkout` |
| a trusted proxy's team header | the teams it lists, never the whole fleet | `serve --trust-team-header X-Forwarded-Groups --trusted-proxy-cidr 127.0.0.1/32` |
| nothing | the whole fleet, only while the read API is open | no `--read-token`, no read token ever minted, no trusted header |

All tokens are sent as `Authorization: Bearer <token>`. Once
`--read-token` is set, a read token has been minted or the trusted header
is configured, a request that presents no valid credential gets `401`. `serve` refuses an open read API on an address that is not
loopback unless `--allow-anonymous-read` is set.

## Team-scoped read tokens

```console
$ upgradescope tokens create --read --teams payments --teams checkout
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
with no evaluation yet (no snapshot pushed) is in no team's scope. Only
evaluations written with the server's current `--team-map` count: after
the map changes, a cluster is in no team's scope until the startup pass
(or its next push) re-evaluates it, and the evaluation of a target since
removed from `--targets`, which is never rewritten, never puts it back in
the old team's.

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

**What the caps still decide.** The caps are applied before any scope,
and they do not divide by team. A collector lists at most 100 objects per
API across the whole cluster, in the order a live List returns them
(namespace, then name), and counts the rest in `objectsOmitted`; the
engine lists at most 100 namespaces per finding, in name order. So where
a cap left some of the scope's objects or namespaces at an API unlisted,
which of them are listed depends on how many objects (or namespaces) at
that API sort before them, in any team's namespaces. This is the one way
a scoped answer depends on other teams' evidence, and a scoped caller can
read it. From how many of its own objects at an API are listed, it learns
how many objects at that API other namespaces hold that sort before the
first of its own left unlisted (exactly, when some of its own are listed;
"100 or more", when none is), and from a read's cut finding whether the
API's objects across the cluster number more than 100. It learns nothing
else of them: not their names, namespaces or teams. Concretely:

- **In reads**, a cut finding lists the scope's objects and namespaces
  that made the cut, drops its title's count and says its lists were
  capped whenever the finding's lists were, whoever's objects filled them.
- **In the gate**, the share holds the scope's objects the collector
  listed. A finding lists those and the PR's, counts the scope's others
  in `objectsOmitted`, and its detail follows what is listed: "3
  object(s) use this API" where only the PR's are, the managers that
  wrote the listed ones. So do the CRD-version remediation's note on
  managers, and whether a Helm release's stored-manifest object is
  matched to the live object the scan lists; when it is not, the release
  gets a finding of its own, which counts toward the score and the
  cluster verdict, and with all of these the answer's size changes. The
  gate's verdict, `X-Upgradescope-Verdict` and the status `?fail-on`
  gives judge only what the PR introduces, and do not depend on the
  caps.

Two tests pin this, with payments' two Ingresses unlisted behind web's
hundred in `a-web` against a cluster of payments' two alone: the finding
lists only the PR's Ingress there, and where payments' Helm release
stores the two, the release gets a blocker of its own there and the
share's score drops, while the verdict and status stay the same.

**What stays.** What describes the cluster as a whole: its name, version,
score, verdict, blocker and warning counts, capability gaps, and score
history. These are the whole cluster's, other teams' findings included:
subtracting the scope's own blockers and warnings from the cluster's
counts gives how many the other teams (and no team) have, per target
(reads at `?target=` included). The gate does not show them: its score,
counts and cluster verdict are the share's. Two parts of the cluster's description name workloads and are
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
be probed by its answer: `?cluster=` matches a name among the scope's
clusters only, so a cluster outside it named like an in-scope cluster's
id does not shadow that id. It is absent from `/clusters`, `/fleet` and
`/fleet/teams`. The server looks the scope up first and never looks up a
cluster outside it, so an id that exists but is out of scope costs the
same store query as an unknown one. Cluster ids are sequential and say
nothing but how many clusters were registered; what the 404 keeps from a
scoped caller is the cluster's name and data.

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
| `POST /api/v1/gate` | without `?cluster=`: exactly as fleet-wide, it reads no stored data and every finding is the manifests'. With `?cluster=`: `404` outside the scope. Otherwise the gate runs on the scope's share of the cluster, never on the whole cluster: the API usage, custom resources, Helm releases, GitOps charts and add-ons in the namespaces of the scope's teams, and what describes the cluster as a whole (version, nodes, control plane, CRD definitions, capabilities); never another team's evidence, a namespace no team owns, cluster-scoped usage or the apiserver's deprecated-call rows. Everything in the answer is decided from that share and the manifests: the findings (a finding the PR's objects are in counts, titles and details only the PR's objects and the scope's, even when the PR names another team's namespace, where its objects are attributed to no team), the verdict and `X-Upgradescope-Verdict`, the status `?fail-on` gives, the cluster verdict, the score and the scope's team scores. Of the share's own findings, the scope's; the manifests' unrecognized images only; suppression warnings of the share only. The exception is which of the scope's objects the share lists where the collector's 100-objects-per-API cap left some unlisted, and what follows from that, never the verdict, its header or the status ([what the caps still decide](#what-a-team-scoped-read-sees)). So a CI status depends on who asks ([the gate with a team-scoped token](#the-gate-with-a-team-scoped-token)) |
| `GET /api/v1/registry` | as fleet-wide: the public knowledge base |
| `GET /metrics` | `403`: its per-cluster series name every cluster, so it takes a fleet-wide credential (Prometheus gets `--read-token` or a `*` token) |

Every scoped answer carries `X-Upgradescope-Teams: <team,...>`, and the
dashboard says "Showing teams ... only" while it is set. A fleet-wide
answer carries no such header.

### The gate with a team-scoped token

`POST /api/v1/gate?cluster=` with a team-scoped credential judges the PR
against the scope's share of the cluster (the table above), and nothing
else: two clusters that differ only outside the scope give the same
answer, status, verdict and score included, byte for byte, in every
format, except where the collector's 100-objects-per-API cap, which lists
objects across the cluster in namespace order, left some of the scope's
objects unlisted. Then which of them are listed, `objectsOmitted`, the
finding's detail, and what the engine derives from the listed objects
(Helm releases' findings, and with them the score, the cluster verdict
and the answer's size) depend on how many objects at that API sort before
them ([what the caps still decide](#what-a-team-scoped-read-sees)); the
verdict, `X-Upgradescope-Verdict` and the status never do. The whole
cluster is never evaluated for that request, so a `413` from evaluating
it depends on another team's workloads only through that size.

The price is that **a PR that breaks only another team's workloads
passes a team-scoped gate.** Say a PR changes a CRD that several teams
use so that it stops serving `v1alpha1`, and only web's custom resources
are still at `v1alpha1`: the payments-scoped gate answers `ready` and
`200`, the fleet-wide gate `blocked` and `422`. The answer does not hint
that something outside the scope would fail either. Any such marker,
even a single bit with no count, team, namespace or title, would let a
team probe the rest of the cluster one request per API: post one object
at a removed API, or a CRD that stops serving a version, and read
whether someone else uses it. The score is the share's for the same
reason: were it the whole cluster's, a PR that adds one object at an API
where another team already has a finding would leave it unchanged.

So pick the gate's credential by what the repository can break:

- only a team's own workloads (its namespaces): the team's scoped token
  is enough, and its CI reads nothing of other teams;
- anything shared (CRDs, cluster-scoped objects, add-ons, another team's
  namespaces): a fleet-wide token (`--read-token` or a `*` token), which
  judges the PR against the whole cluster, as the gate did before read
  scopes.

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
upgradescope serve --listen 127.0.0.1:8080 \
  --trust-team-header X-Forwarded-Groups --trusted-proxy-cidr 127.0.0.1/32
```

A read whose TCP peer is in a `--trusted-proxy-cidr` range and that
carries the header reads as the teams it lists, in the
[team list encoding](#the-team-list-encoding), every copy of the header
counted (oauth2-proxy sends one header, the session's groups joined by
commas; other proxies may send one per group). A bearer token, when one is
presented and valid, takes precedence. The peer is the connection's
source address, never `X-Forwarded-For`. From any other address the
header is ignored: a client that reaches the server directly cannot
spoof it, and needs a token. The header always names teams: `*` in it is
a team called `*`, never the whole fleet, so whoever can name a group in
the identity provider cannot grant fleet-wide reads with it. People who
need the fleet use a fleet-wide token.

The proxy's requests carry no `Authorization` header, so a shared cache
in front of the proxy would key a team's answer by its URL alone. In
this mode every read answer is sent with `Vary: <header>, Authorization`,
and every scoped answer, in any mode, with
`Cache-Control: private, no-store`.

The two flags go together, and the mode is off by default.

!!! danger "When header mode is safe"
    The server trusts whatever arrives from a trusted address. Header
    mode is safe only when **both** hold:

    1. **The server is reachable only through the proxy.** Listen on
       loopback (`--listen 127.0.0.1:8080`) with the proxy in the same
       pod, or otherwise make sure nothing but the proxy can open a
       connection from a `--trusted-proxy-cidr` address: no Service,
       NodePort or Ingress that reaches the server's port, and a
       `--trusted-proxy-cidr` that covers the proxy's addresses and
       nothing else. A pod CIDR is not "the proxy": every pod in it could
       send the header.
    2. **The proxy removes every client-supplied copy of the header**
       before it forwards a request, on every route it forwards,
       authenticated or not, and then sets it from the signed-in session
       only. A proxy that passes a client's `X-Forwarded-Groups` on (or
       appends to it) lets that client read any team it names.

    oauth2-proxy v7.15.5, which the example pins, does both halves of
    (2) with `--pass-user-headers=true --skip-auth-strip-headers=true`:
    it deletes the request's copies of each header it injects
    (`X-Forwarded-Groups` among them), whatever their case and whether
    they are written with `-` or `_`, then sets it from the session
    only: one header, the session's groups joined by commas. On routes
    it does not authenticate (`--skip-auth-route`) it strips them too,
    and sets them only from a valid session cookie the request carries
    ([flag reference](https://oauth2-proxy.github.io/oauth2-proxy/7.15.x/configuration/overview);
    in its source, `getRequestHeaders` in
    [`pkg/apis/options/legacy_options.go`](https://github.com/oauth2-proxy/oauth2-proxy/blob/v7.15.5/pkg/apis/options/legacy_options.go)
    sets `PreserveRequestValue` to `!SkipAuthStripHeaders`;
    `newStripHeaders` and `stripNormalizedHeader` in
    [`pkg/middleware/headers.go`](https://github.com/oauth2-proxy/oauth2-proxy/blob/v7.15.5/pkg/middleware/headers.go)
    delete those headers before the injector runs, and
    `injectRequestHeaders` there joins what it injects with
    `flattenHeaders`; `getAuthenticatedSession` in
    [`oauthproxy.go`](https://github.com/oauth2-proxy/oauth2-proxy/blob/v7.15.5/oauthproxy.go)
    returns the request's session on a skip-auth route). With the alpha
    configuration, the same holds only for headers whose
    `preserveRequestValue` is false. Any other proxy must be checked for
    the same: it strips the client's copies, whatever their case, before
    it sets the header. (`X_Forwarded_Groups` is not the same header to
    the server: Go reads header names as written, with `-`, so a client
    cannot slip one past a proxy that strips `X-Forwarded-Groups`.)
    Check yours after signing in:
    `curl -H 'X-Forwarded-Groups: other-team' https://<host>/api/v1/clusters`
    must not list `other-team`'s clusters, and neither may the same
    request to a route the proxy does not authenticate
    (`POST /api/v1/gate?cluster=<other-team's cluster>` must answer 401
    without a token).

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
compared with the teams namespaces are attributed to, exactly, after
decoding. With oauth2-proxy, `--oidc-groups-claim` picks the claim and
`--allowed-group` limits who may sign in at all.

#### The team list encoding

Team names are free text, so the trusted header and the
`X-Upgradescope-Teams` answer header carry them in one defined form: a
comma-separated list in which each team is percent-encoded as a URL path
segment (Go's `url.PathEscape`, JavaScript's `encodeURIComponent` reads
it): a comma is `%2C`, a percent sign `%25`, a space `%20`, and a
non-ASCII character its UTF-8 bytes (`é` is `%C3%A9`). The server trims
the whitespace around each entry, decodes it, and drops an entry that is
not valid percent-encoding (`50%off`): it cannot know which team that
is. A name sent as is reads as itself when it holds no comma and no
percent sign: `Platform Team`, or raw UTF-8 `équipe`, both work, which is
what oauth2-proxy sends for such groups. A group whose name holds a comma
or a percent sign must be sent encoded, or renamed: oauth2-proxy sends
it as is, and `a,b` would read as the teams `a` and `b`.

| Team | In the header |
|---|---|
| `payments` | `payments` |
| `Platform Team` | `Platform Team` or `Platform%20Team` |
| `Équipe, Paris` | `%C3%89quipe%2C%20Paris` |
| two teams, `payments` and `web` | `payments,web`, or two header lines |

`X-Upgradescope-Teams` on a scoped answer is always fully encoded
(`Platform%20Team,payments`), and the dashboard decodes it.

### Example: oauth2-proxy as a sidecar

[`deploy/examples/oauth2-proxy/upgradescope-oauth2-proxy.yaml`](https://github.com/abd-ulbasit/upgradescope/blob/main/deploy/examples/oauth2-proxy/upgradescope-oauth2-proxy.yaml)
runs oauth2-proxy v7.15.5 in the server's pod and holds both conditions
above (its test, `deploy/examples/examples_test.go`, fails if one goes):

- `serve` listens on `127.0.0.1:8080`, so it is reachable only from
  inside the pod, and trusts the header from `127.0.0.1/32` only. The
  one Service, `upgradescope` (port 80), exposes the proxy's port and
  never the server's. Point your Ingress and the OIDC client's redirect
  URL (`https://<host>/oauth2/callback`) at it.
- The proxy pins `--pass-user-headers=true` and
  `--skip-auth-strip-headers=true`, and leaves `Authorization` alone
  (`--pass-basic-auth=false`).

Machines use the same Service, on the routes the proxy does not
authenticate: agents push to `POST /api/v1/snapshots` with their ingest
tokens, CI posts to `POST /api/v1/gate?target=...` and Prometheus scrapes
`GET /metrics` with read tokens, and the kubelet probes `/readyz` and
`/healthz` through it (serve does not listen on the pod's address).
v7.15.5 matches a `--skip-auth-route` against the request's decoded path
without its query (`isAllowedRoute` in `oauthproxy.go`, `GetRequestPath`
in `pkg/requests/util/util.go`), so `POST=^/api/v1/gate$` matches every
gate call, whatever its query. Versions before v7.11.0 (the CVE-2025-54576 fix) match the request
URI, query included: there the same route never matches a gate call, and
CI gets the proxy's sign-in answer instead of the server's. The
example's test checks the routes against the requests machines make.
The proxy strips the group header on those routes too, so each request
reads as the token it presents and nothing else: without one, `401`.

The proxy's sign-in is not what guards the data; the server's token
check is. With `--reverse-proxy`, oauth2-proxy matches the skip-auth
routes against the path in the client's `X-Forwarded-Uri`, from any
client unless `--trusted-proxy-ip` names the proxies allowed to send
`X-Forwarded-*` headers. So `GET /api/v1/clusters` with
`X-Forwarded-Uri: /metrics` gets past the proxy without signing in. It
reaches the server with the group header stripped and reads as the token
it presents: without one, `401`. Set `--trusted-proxy-ip` to your
ingress controller's addresses, and have the ingress overwrite or drop a
client's `X-Forwarded-Uri`, if signing in must be the only way past the
proxy. Operators reach the server itself with `kubectl port-forward` to
port 8080 and a token (the danger box above says what that permission
allows). What reaches
the server over loopback is trusted with the header, which is why the
pod opts out of Istio and Linkerd sidecar injection.

Adapt the provider flags to your identity provider, and create the
`oauth2-proxy` Secret (`client-id`, `client-secret`, `cookie-secret`)
first; the manifest's header lists what it expects.
