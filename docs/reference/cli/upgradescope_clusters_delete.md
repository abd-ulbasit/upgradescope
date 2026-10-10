## upgradescope clusters delete

Delete a cluster with its history, queued notifications and ingest tokens

### Synopsis

Delete a cluster record with its snapshot and evaluation history, its queued
notifications and the per-cluster ingest tokens minted for its name. Use it to
decommission a cluster, or when a cluster was rebuilt: a cluster name is bound to the
cluster UID it first pushed, and the server answers 409 to a push from another UID.
Delete the old record and the next push registers the new one; an agent that used a
per-cluster token needs a new one ('upgradescope tokens create').
An agent that keeps pushing under the name registers it again.

```
upgradescope clusters delete <cluster> [flags]
```

### Examples

```
  upgradescope clusters delete prod-eu --server-url https://upgradescope.example.com
  upgradescope clusters delete prod-eu --db upgradescope.db
```

### Options

```
      --admin-token string        with --server-url: the server's admin token (serve --admin-token) (visible in process listings: prefer $UPGRADESCOPE_ADMIN_TOKEN or --admin-token-file)
      --admin-token-file string   read --admin-token from this file, e.g. a mounted Secret (surrounding whitespace is trimmed)
      --db string                 path to the SQLite database (parent directory is created) (default "upgradescope.db")
      --db-url string             Postgres URL (postgres://user:pass@host:5432/db); mutually exclusive with --db (visible in process listings: prefer $UPGRADESCOPE_DB_URL or --db-url-file)
      --db-url-file string        read --db-url from this file, e.g. a mounted Secret (surrounding whitespace is trimmed)
  -h, --help                      help for delete
      --server-ca-file string     PEM bundle of a private CA that issued the server's certificate, trusted on top of the system roots (for an https server behind a private CA; $SSL_CERT_FILE also adds roots, for the whole process); needs an https server URL. Verification is never skipped
      --server-url string         base URL of a running upgradescope server, e.g. https://upgradescope.example.com (instead of --db/--db-url)
```

### SEE ALSO

* [upgradescope clusters](upgradescope_clusters.md)	 - List, delete and rename the clusters a server knows

