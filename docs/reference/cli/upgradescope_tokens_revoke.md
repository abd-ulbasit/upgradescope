## upgradescope tokens revoke

Revoke ingest tokens of a cluster (--id or --all), or a read token (--read --id)

### Synopsis

Revoke one ingest token by id (see 'tokens list'), or every active token of the cluster with --all.
Zero-downtime rotation: 'tokens create <cluster>', roll the new token out to the agent, then
'tokens revoke <cluster> --id <old id>'. The agent reads its token at startup, so rolling it out
means restarting the agent after updating its Secret (kubectl rollout restart deploy/<release>-agent).

With --read, revoke the read token --id names (see 'tokens list --read'). The server refuses it
from its next request on. Revoking the last read token does not open the read API again.

```
upgradescope tokens revoke (<cluster> (--id <id> | --all) | --read --id <id>) [flags]
```

### Examples

```
  upgradescope tokens revoke prod-eu --id 3
  upgradescope tokens revoke prod-eu --all
  upgradescope tokens revoke --read --id 2
```

### Options

```
      --all                  revoke every active token of the cluster
      --db string            path to the SQLite database (parent directory is created) (default "upgradescope.db")
      --db-url string        Postgres URL (postgres://user:pass@host:5432/db); mutually exclusive with --db (visible in process listings: prefer $UPGRADESCOPE_DB_URL or --db-url-file)
      --db-url-file string   read --db-url from this file, e.g. a mounted Secret (surrounding whitespace is trimmed)
  -h, --help                 help for revoke
      --id int               revoke only the token with this id (from 'tokens list' or 'tokens create')
      --read                 revoke the read token --id names instead of an ingest token
```

### SEE ALSO

* [upgradescope tokens](upgradescope_tokens.md)	 - Manage per-cluster ingest tokens and team-scoped read tokens

