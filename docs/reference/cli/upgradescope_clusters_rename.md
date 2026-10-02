## upgradescope clusters rename

Rename a cluster; its history and ingest tokens move with it

### Synopsis

Rename a cluster; its history and its per-cluster ingest tokens move to the new name.
The agent sends its own --cluster-name with every push, so change that too (chart value
agent.clusterName). Until then its pushes are refused when it uses a per-cluster token
(now bound to the new name), or register the old name again when it uses the shared one.

```
upgradescope clusters rename <cluster> <new-name> [flags]
```

### Examples

```
  upgradescope clusters rename prod-eu prod-eu-1 --server https://upgradescope.example.com
```

### Options

```
      --admin-token string        with --server: the server's admin token (serve --admin-token) (visible in process listings: prefer $UPGRADESCOPE_ADMIN_TOKEN or --admin-token-file)
      --admin-token-file string   read --admin-token from this file, e.g. a mounted Secret (surrounding whitespace is trimmed)
      --db string                 path to the SQLite database (parent directory is created) (default "upgradescope.db")
      --db-url string             Postgres URL (postgres://user:pass@host:5432/db); mutually exclusive with --db (visible in process listings: prefer $UPGRADESCOPE_DB_URL or --db-url-file)
      --db-url-file string        read --db-url from this file, e.g. a mounted Secret (surrounding whitespace is trimmed)
  -h, --help                      help for rename
      --server string             base URL of a running upgradescope server, e.g. https://upgradescope.example.com (instead of --db/--db-url)
```

### SEE ALSO

* [upgradescope clusters](upgradescope_clusters.md)	 - List, delete and rename the clusters a server knows

