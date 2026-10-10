## upgradescope clusters list

List clusters: id, name, UID, last push and staleness

### Synopsis

List the clusters a server (or its database) knows: id, name, cluster UID, the last push
and, when asked through a server, whether the cluster is stale (no push within --stale-after).

```
upgradescope clusters list [flags]
```

### Examples

```
  upgradescope clusters list --server-url https://upgradescope.example.com
  upgradescope clusters list --db upgradescope.db
```

### Options

```
      --db string                path to the SQLite database (parent directory is created) (default "upgradescope.db")
      --db-url string            Postgres URL (postgres://user:pass@host:5432/db); mutually exclusive with --db (visible in process listings: prefer $UPGRADESCOPE_DB_URL or --db-url-file)
      --db-url-file string       read --db-url from this file, e.g. a mounted Secret (surrounding whitespace is trimmed)
  -h, --help                     help for list
      --read-token string        with --server-url: the server's read token (or its admin token); omit for an open read API (visible in process listings: prefer $UPGRADESCOPE_READ_TOKEN or --read-token-file)
      --read-token-file string   read --read-token from this file, e.g. a mounted Secret (surrounding whitespace is trimmed)
      --server-ca-file string    PEM bundle of a private CA that issued the server's certificate, trusted on top of the system roots (for an https server behind a private CA; $SSL_CERT_FILE also adds roots, for the whole process); needs an https server URL. Verification is never skipped
      --server-url string        base URL of a running upgradescope server, e.g. https://upgradescope.example.com (instead of --db/--db-url)
```

### SEE ALSO

* [upgradescope clusters](upgradescope_clusters.md)	 - List, delete and rename the clusters a server knows

