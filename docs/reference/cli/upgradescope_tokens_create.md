## upgradescope tokens create

Mint an ingest token bound to one cluster, or a read token scoped to teams

### Synopsis

Mint an ingest token bound to one cluster, or with --read a read token
scoped to the teams --teams lists ('*' alone: the whole fleet). The
plaintext token is printed once, to stdout. The server stores its sha256
hash and its first 8 characters (which "tokens list" shows), never the
token. Give an ingest token to that cluster's agent (--server-token-file,
or the chart's agent.existingSecret), and a read token to the team's
dashboard users or CI.

```
upgradescope tokens create (<cluster> | --read --teams <team,...|*>) [flags]
```

### Examples

```
  upgradescope tokens create prod-eu --db upgradescope.db
  upgradescope tokens create prod-eu --db-url-file /secrets/db-url > prod-eu.token
  upgradescope tokens create --read --teams payments,checkout > payments.token
  upgradescope tokens create --read --teams '*' > fleet.token
```

### Options

```
      --db string            path to the SQLite database (parent directory is created) (default "upgradescope.db")
      --db-url string        Postgres URL (postgres://user:pass@host:5432/db); mutually exclusive with --db (visible in process listings: prefer $UPGRADESCOPE_DB_URL or --db-url-file)
      --db-url-file string   read --db-url from this file, e.g. a mounted Secret (surrounding whitespace is trimmed)
  -h, --help                 help for create
      --read                 mint a read token for the read API, dashboard, /api/v1/gate and /metrics instead of an ingest token; needs --teams
      --teams strings        with --read: the teams the token reads, comma separated, or '*' alone for the whole fleet
```

### SEE ALSO

* [upgradescope tokens](upgradescope_tokens.md)	 - Manage per-cluster ingest tokens and team-scoped read tokens

