# The dashboard

`upgradescope serve` embeds a web dashboard and serves it at `/`. It reads
the same API as everything else, so it shows what the server has stored:
the fleet score matrix, a per-team rollup, one cluster's findings for any
target, and the add-on registry. It needs no separate deployment, and it
needs the server (`serve`, or the chart with `server.enabled=true`): the
agent alone does not serve it
([Fleet server](../getting-started/fleet.md)).

## Screens and routes

The dashboard routes by the URL hash, so every view has an address you can
paste into a ticket or a chat. The server needs no route of its own for
them.

| Screen | Route | What it shows |
|---|---|---|
| Fleet | `#/` | A clusters by targets matrix of readiness scores, with a summary strip, search, filters and a sort |
| Teams | `#/teams[?target=1.38]` | Each team's worst score, verdict and blockers across the fleet, for one target |
| Cluster | `#/cluster/{id}[?target=1.38]` | One cluster's verdict, score, trend, findings and per-team table for a target |
| Registry | `#/registry` | The add-on EOL and compatibility dataset this server was built with |

### Fleet

Each cell is a stored evaluation: the score, the verdict as a mark and as
text, and the number of blockers. A cell opens the cluster at that target.
An empty cell (`—`) has no stored evaluation; it opens a what-if, which the
server computes on request and does not store. `n/a` means the cluster
already runs that target. A cluster whose agent has stopped pushing carries
a `stale` badge and its last-seen time: its scores describe the cluster as it
was then.

The **summary strip** above the matrix counts the clusters for one target
(the selector lists only the targets the matrix shows; it starts on the one
most clusters can still upgrade to). Every cluster is in exactly one bucket,
so the buckets add up to the number of clusters you can see:

| Bucket | A cluster is in it when |
|---|---|
| Ready | its stored verdict for the target is `ready` |
| Blocked | its stored verdict is `blocked` |
| Unknown | its stored verdict is `unknown`: nothing blocked, but a required check did not run. Never counted as ready |
| Stale | its agent has stopped pushing, whatever the old verdict was |
| n/a | it already runs the target or newer |
| No stored evaluation | none is stored for the target (a what-if would compute it) |

When a cluster fits more than one description, the first row it fits in
this order wins: n/a, stale, no stored evaluation, then the verdict. With a
team-scoped token the server returns only that scope's clusters, so the
counts are for them.

Click a bucket to filter the matrix to it; click it again to clear. The
**toolbar** has a name search (a case-insensitive substring), a quick filter
(Blocked, Unknown, Stale, Has blockers, and the other buckets) and a sort:
worst score first (the default), name, or last seen with the oldest first.
It says "N of M clusters". When nothing matches there is a clear-filters
button. The header row and the cluster column stay in view while you scroll;
at most 100 rows are drawn, and **Show more** adds 100.

A filter looks at the targets the matrix shows, never at a target the
server left out of its columns (the note under the heading says how many).
Once you click a bucket, or pick a target in the summary, the target is
pinned and the filter and the worst-score sort look at that target alone;
until then they look at every shown target.

The toolbar and the summary's target live in the hash, so a reload or a
pasted link restores them:

| Parameter | Values |
|---|---|
| `q` | text matched against cluster names |
| `filter` | `blocked`, `unknown`, `stale`, `has-blockers`, `ready`, `na`, `missing` |
| `sort` | `score` (default), `name`, `seen` |
| `target` | a target the matrix shows, such as `1.37` |

```text
#/?q=prod&filter=blocked&sort=name&target=1.37
```

A value the dashboard does not know is ignored.

### Cluster

The page shows the verdict and score for a target (the cluster's next minor
unless `target` is given; any minor works, and one with no stored
evaluation comes back as a what-if), the trend, and the findings grouped by
severity. The findings filters are in the hash next to the target, and a
single finding can be linked:

| Parameter | Values |
|---|---|
| `target` | a minor version such as `1.37` |
| `category` | a finding category, such as `removed-api` |
| `severity` | `blocker`, `warning` or `info` |
| `team` | a team name, or `(unattributed)` for findings no team owns |
| `q` | text matched against a finding's title, detail, namespaces and team chips |
| `finding` | a finding's key: scrolls to it and highlights it |

```text
#/cluster/3?target=1.37&category=removed-api&q=ingress
#/cluster/3?target=1.37&finding=eol-addon%2Fingress-nginx
```

Each finding has a `#` link whose address is that finding: the target and
`finding=<key>`, with the key URL-encoded. Because routing uses the hash, the
link is a query parameter that scrolls to and highlights the finding, not an
element anchor. Picking another target drops the
filters, because a category that exists for one target may not for the next.

### Fresh data

Each view loads once when you open it and does not poll. The page head has
an "updated HH:MM:SS" stamp and a **Refresh** button. A tab that becomes
visible again refetches when what it shows is more than 60 seconds old.
While a refetch runs, the old data stays on screen with a small spinner; a
failed refresh keeps it and says so next to the button.

## Reading with a token

When the server asks for a read token (`serve --read-token`, or a minted
one), the page shows an Unauthorized message until you set it with the
**set token** button in the header. The token is sent as a bearer header on
every API call. It is kept in `sessionStorage`, so it is gone when the tab
closes, unless you tick **Remember on this device**, which keeps it in this
browser's `localStorage` instead. It is never in both. Saving it with the box
unticked, or clearing it, removes the remembered copy.

[Read access](../operations/auth.md) has the kinds of read token, and
[Tenancy and access control](../operations/tenancy.md#credentials) says what
each credential may do. Neither protects the dashboard's static files, which
hold no data. To put the dashboard behind single sign-on, see
[Putting the dashboard behind SSO](../operations/auth.md#putting-the-dashboard-behind-sso).

### The team-scoped banner

A read token minted for teams (`tokens create --read --teams payments`)
reads only those teams' clusters, findings and scores. The server filters
the data; the dashboard shows a banner under the header that names the
teams ("Showing team payments only: this view does not include other
teams' clusters, findings or scores") so a short list is not mistaken for a
small fleet. A cluster outside the scope answers 404, and the error says it
may belong to another team.

## Serving under a path prefix

The dashboard works behind a reverse proxy at a **path prefix**, for example
`https://ops.example.com/upgradescope/`, when three things hold:

1. **Open the URL with the trailing slash.** Asset and API URLs are
   relative. Without the slash the browser resolves them against the parent
   path (`/assets/...` instead of `/upgradescope/assets/...`), and the page
   comes up blank with its assets answering 404.
2. **The proxy strips the prefix** before it forwards to `serve`. The server
   knows nothing of the prefix: it sees `/`, `/assets/...` and `/api/...`.
3. **The proxy's Host is allowed.** `serve` answers an unknown Host with
   `421`; the name the proxy forwards must be in `--allowed-host` (the chart
   does this for `server.ingress.host`; see
   [The Host check](../operations/auth.md#the-host-check-dns-rebinding)).

Because the URLs are relative, the server serves the page itself only where
they resolve: at `/`, `/index.html` and one extensionless segment such as
`/teams`. The dashboard routes by hash (`#/cluster/3`), so it has no other
path of its own. At a nested path such as `/cluster/3`, `/cluster/3/` or
`/foo/` (a link someone typed or pasted) the server answers `302` with a
relative `Location` (`../#/cluster/3`), which the browser resolves against
the URL it asked for, so the dashboard opens at the root of the prefix on
that route. The redirect is written by hand and relative on purpose: an
absolute one would drop the proxy's prefix. A path with a file extension that
is not a file, and anything under `/assets/`, is a JSON 404.

An nginx location that does this (the trailing slash on `proxy_pass` is what
strips the prefix):

```nginx
location = /upgradescope { return 301 /upgradescope/; }

location /upgradescope/ {
    proxy_pass http://upgradescope-server.upgradescope.svc:8080/;
    proxy_set_header Host $host;
    proxy_set_header X-Forwarded-Proto $scheme;
    # Pass Location headers through untouched: they are relative.
    proxy_redirect off;
}
```

The same with Kubernetes Ingress objects, for a controller that supports the
ingress-nginx rewrite annotation (ingress-nginx itself reached end of life in
March 2026; on another controller use its own strip-prefix feature, for
example a Traefik `StripPrefix` middleware):

```yaml
apiVersion: networking.k8s.io/v1
kind: Ingress
metadata:
  name: upgradescope
  namespace: upgradescope
  annotations:
    nginx.ingress.kubernetes.io/rewrite-target: /$2
spec:
  ingressClassName: nginx
  rules:
    - host: ops.example.com
      http:
        paths:
          - path: /upgradescope(/|$)(.*)
            pathType: ImplementationSpecific
            backend:
              service:
                name: upgradescope-server
                port:
                  number: 8080
```

These snippets are examples; they are not run in CI. What the server does
behind a prefix-stripping proxy (the redirects, and every asset and API URL
resolving on the same prefix) is tested; see DB-05 in the
[claims ledger](../claims.md).

If the dashboard is blank or its assets answer 404, see
[Dashboard blank or assets 404 behind a proxy](../troubleshooting.md#dashboard-blank-or-assets-404-behind-a-proxy).
