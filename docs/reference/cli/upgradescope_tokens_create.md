## upgradescope tokens create

Mint an ingest token bound to one cluster

### Synopsis

Mint an ingest token bound to one cluster. The plaintext token is printed
once, to stdout. The server stores its sha256 hash and its first 8
characters (which "tokens list" shows), never the token. Give it to that
cluster's agent (--server-token-file, or the chart's agent.existingSecret).

```
upgradescope tokens create <cluster> [flags]
```

### Examples

```
  upgradescope tokens create prod-eu --db upgradescope.db
  upgradescope tokens create prod-eu --db-url-file /secrets/db-url > prod-eu.token
```

### Options

```
      --db string            path to the SQLite database (parent directory is created) (default "upgradescope.db")
      --db-url string        Postgres URL (postgres://user:pass@host:5432/db); mutually exclusive with --db (visible in process listings: prefer $UPGRADESCOPE_DB_URL or --db-url-file)
      --db-url-file string   read --db-url from this file, e.g. a mounted Secret (surrounding whitespace is trimmed)
  -h, --help                 help for create
```

### SEE ALSO

* [upgradescope tokens](upgradescope_tokens.md)	 - Manage per-cluster ingest tokens for agent snapshot pushes

