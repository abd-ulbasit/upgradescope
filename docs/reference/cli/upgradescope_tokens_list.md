## upgradescope tokens list

List ingest tokens, or read tokens with --read; never prints a token

### Synopsis

List ingest tokens: id, cluster, prefix, created and revoked times; or with --read the read tokens:
id, teams ('*' = the whole fleet), prefix, created and revoked times. A token itself is never shown.

```
upgradescope tokens list [flags]
```

### Examples

```
  upgradescope tokens list --db upgradescope.db
  upgradescope tokens list --cluster prod-eu
  upgradescope tokens list --read
```

### Options

```
      --cluster string       only list this cluster's tokens
      --db string            path to the SQLite database (parent directory is created) (default "upgradescope.db")
      --db-url string        Postgres URL (postgres://user:pass@host:5432/db); mutually exclusive with --db (visible in process listings: prefer $UPGRADESCOPE_DB_URL or --db-url-file)
      --db-url-file string   read --db-url from this file, e.g. a mounted Secret (surrounding whitespace is trimmed)
  -h, --help                 help for list
      --read                 list the read tokens instead of the ingest tokens
```

### SEE ALSO

* [upgradescope tokens](upgradescope_tokens.md)	 - Manage per-cluster ingest tokens and team-scoped read tokens

