## upgradescope clusters

List, delete and rename the clusters a server knows

### Synopsis

List, delete and rename the clusters an upgradescope server knows.
With --server, the commands call the server's API: list needs the read token (or the
admin token), delete and rename the admin token (serve --admin-token). Without --server
they open the server's database directly (--db or --db-url), e.g. while it is stopped;
one of the two is required.

### Options

```
  -h, --help   help for clusters
```

### SEE ALSO

* [upgradescope](upgradescope.md)	 - Continuous Kubernetes upgrade-readiness scanner
* [upgradescope clusters delete](upgradescope_clusters_delete.md)	 - Delete a cluster with its history, queued notifications and ingest tokens
* [upgradescope clusters list](upgradescope_clusters_list.md)	 - List clusters: id, name, UID, last push and staleness
* [upgradescope clusters rename](upgradescope_clusters_rename.md)	 - Rename a cluster; its history and ingest tokens move with it

