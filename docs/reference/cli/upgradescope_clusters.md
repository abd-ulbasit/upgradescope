## upgradescope clusters

List, delete and rename the clusters a server knows

### Synopsis

List, delete and rename the clusters an upgradescope server knows.
With --server-url, the commands call the server's API: list needs the read token (or the
admin token), delete and rename the admin token (serve --admin-token). A server whose
https certificate a private CA issued needs --server-ca-file; a token sent over plain http
to a host that is not loopback crosses the network in the clear, and the command warns.
Without --server-url they open the server's database directly (--db or --db-url), e.g.
while it is stopped; one of the two is required. (--server is the former name of
--server-url and still works.)

### Options

```
  -h, --help   help for clusters
```

### SEE ALSO

* [upgradescope](upgradescope.md)	 - Continuous Kubernetes upgrade-readiness scanner
* [upgradescope clusters delete](upgradescope_clusters_delete.md)	 - Delete a cluster with its history, queued notifications and ingest tokens
* [upgradescope clusters list](upgradescope_clusters_list.md)	 - List clusters: id, name, UID, last push and staleness
* [upgradescope clusters rename](upgradescope_clusters_rename.md)	 - Rename a cluster; its history and ingest tokens move with it

