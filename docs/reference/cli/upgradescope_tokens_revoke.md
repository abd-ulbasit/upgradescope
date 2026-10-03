## upgradescope tokens revoke

Revoke one ingest token of a cluster by id, or all of them with --all

### Synopsis

Revoke one ingest token by id (see 'tokens list'), or every active token of the cluster with --all.
Zero-downtime rotation: 'tokens create <cluster>', roll the new token out to the agent, then
'tokens revoke <cluster> --id <old id>'. The agent reads its token at startup, so rolling it out
means restarting the agent after updating its Secret (kubectl rollout restart deploy/<release>-agent).

```
upgradescope tokens revoke <cluster> (--id <id> | --all) [flags]
```

### Examples

```
  upgradescope tokens revoke prod-eu --id 3
  upgradescope tokens revoke prod-eu --all
```

### Options

```
      --all                  revoke every active token of the cluster
      --db string            path to the SQLite database (parent directory is created) (default "upgradescope.db")
      --db-url string        Postgres URL (postgres://user:pass@host:5432/db); mutually exclusive with --db (visible in process listings: prefer $UPGRADESCOPE_DB_URL or --db-url-file)
      --db-url-file string   read --db-url from this file, e.g. a mounted Secret (surrounding whitespace is trimmed)
  -h, --help                 help for revoke
      --id int               revoke only the token with this id (from 'tokens list' or 'tokens create')
```

### SEE ALSO

* [upgradescope tokens](upgradescope_tokens.md)	 - Manage per-cluster ingest tokens for agent snapshot pushes

