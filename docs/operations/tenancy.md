# Tenancy and access control

upgradescope is single-tenant. One server holds one fleet, and everyone
with read access sees all of it: there are no users, roles or per-team
scopes. Teams are an attribution (a namespace label, or the server's
`--team-map`) that splits findings and scores, not an access boundary.
Separate tenants need separate servers, each with its own database and
tokens. In the cluster, the agent's footprint is described in
[Security model and RBAC](security-model-and-rbac.md).

## Credentials

The server has four credentials, each optional except where noted:

| Credential | Authorizes | Configured with |
|---|---|---|
| read token | every `GET /api/v1/*` endpoint (clusters, reports, findings, history, team rollups, exports, registry), `POST /api/v1/gate`, `/metrics` | `--read-token`; empty = open, which `serve` refuses unless the address it binds is loopback or `--allow-anonymous-read` is set |
| per-cluster ingest tokens | `POST /api/v1/snapshots` as one cluster name | `upgradescope tokens create <cluster>` |
| shared ingest token | `POST /api/v1/snapshots` as **any** cluster | `--ingest-token` |
| admin token | deleting and renaming clusters, plus reads | `--admin-token`; empty = both refused |

Per-cluster tokens are 64 random hex characters, printed once by
`tokens create`. The server stores each one's sha256 hash and its first 8
characters, which `tokens list` prints to tell them apart; never the
token. `tokens revoke <cluster> --id <id>` revokes one and `--all` every
active one of a cluster. To rotate without a gap: `tokens create`, put
the new token in the agent's Secret, restart the agent (it reads the
token at startup: `kubectl rollout restart deploy/<release>-agent`), then
revoke the old id.

Tokens are bearer secrets: anyone who sees one in transit can replay it.
Serve HTTPS (`--tls-cert-file`, the chart's `server.tls`, or an Ingress
that terminates TLS) wherever pushes or reads cross a network you do not
trust. The agent logs a warning at startup when it would send its token
over plain `http://` to a host that is not loopback.

The server's SQLite database, and its `-wal` and `-shm` files, are
created readable by their owner only (0600), and a directory `serve`
creates for them 0700; an existing database is tightened on open.

What the read token does **not** do:

- **It is one fleet-wide secret.** Whoever holds it reads every cluster,
  every team's findings and namespaces, and every export. There are no
  per-team scopes yet, so a team cannot be given a view of only its
  clusters.
- **It is not an identity.** Reads are not attributed to anyone, and
  rotating it means restarting the server and handing the new value to
  every consumer at once.
- **It does not protect the dashboard's static files** (HTML, JS, CSS),
  which hold no data, nor `/healthz` and `/readyz`.
- **It is not encryption.** Without TLS (`--tls-cert-file`, the chart's
  `server.tls`, or an Ingress that terminates it) it crosses the network in
  the clear.
- **It lives in the browser.** The dashboard keeps it in `localStorage`;
  the server's Content-Security-Policy forbids inline and third-party
  script, which is what would read it.
- It cannot push snapshots or delete clusters: those need the ingest and
  admin tokens.

### Putting the dashboard behind SSO

For people, put an authenticating proxy in front of the server:
[oauth2-proxy](https://oauth2-proxy.github.io/oauth2-proxy/), an
identity-aware proxy, or your ingress controller's external-auth support.
The server needs no changes. Two patterns:

1. **Proxy in front, read token behind.** Keep `--read-token`. The proxy
   decides who may open the dashboard; the dashboard still asks for the
   read token once (stored in that browser). Machine clients (the CI gate,
   Prometheus, scripts) keep calling the server with the token, directly
   or through a proxy route that skips authentication. Defense in depth,
   and the simplest for automation.
2. **The proxy is the gate.** Run without a read token
   (`--allow-anonymous-read`, chart `server.ingress.allowAnonymousRead=true`
   when the chart's Ingress carries the auth annotations) and make sure
   nothing but the proxy can reach the read API: a ClusterIP Service, a
   NetworkPolicy (`networkPolicy.enabled`, `serverIngressFrom` naming the
   proxy, Prometheus and the agents' sources). Agents authenticate
   `POST /api/v1/snapshots` with their own tokens, so let that route through
   without SSO; CI callers then need the proxy's machine credentials (for
   oauth2-proxy, bearer JWTs from a trusted issuer). Anyone who reaches the
   Service directly reads everything, so this rests on the network policy.

A sketch of pattern 2 with oauth2-proxy in front of the chart's Service
(adapt the provider flags to your identity provider):

```yaml
args:
  - --provider=oidc
  - --oidc-issuer-url=https://sso.example.com
  - --email-domain=example.com
  - --upstream=http://upgradescope-server.upgradescope.svc:8080
  - --http-address=0.0.0.0:4180
  - --reverse-proxy=true
  # Agents push with their own ingest tokens.
  - --skip-auth-route=POST=^/api/v1/snapshots$
env:
  - {name: OAUTH2_PROXY_CLIENT_ID, valueFrom: {secretKeyRef: {name: oauth2-proxy, key: client-id}}}
  - {name: OAUTH2_PROXY_CLIENT_SECRET, valueFrom: {secretKeyRef: {name: oauth2-proxy, key: client-secret}}}
  - {name: OAUTH2_PROXY_COOKIE_SECRET, valueFrom: {secretKeyRef: {name: oauth2-proxy, key: cookie-secret}}}
```

With pattern 1, do not let the proxy overwrite the `Authorization` header
(oauth2-proxy's `--pass-authorization-header` and
`--set-authorization-header` do): the server reads the read token from it.
Keep cluster administration off the proxied path either way: the admin
token belongs to operators and their CLI, not to the dashboard.
