## upgradescope tokens

Manage per-cluster ingest tokens and team-scoped read tokens

### Synopsis

Manage the tokens the server keeps in its database.

Ingest tokens (the default) authenticate snapshot pushes for one cluster
name only, so a leaked token cannot write another cluster's history.

Read tokens (--read) authenticate the read API, the dashboard's data,
/api/v1/gate and /metrics, each for a set of teams (--teams a,b) or for the
whole fleet (--teams '*'). A team-scoped token reads only the clusters its
teams own a namespace in, and of those only its teams' findings and team
scores: any other cluster answers 404, as an unknown one does, and is left
out of every list and rollup. /metrics takes a fleet-wide token only. Once
one read token has been minted, the read API needs a credential.

The server database keeps each token's sha256 hash and its first 8
characters (which "tokens list" shows), never the token. The server reads
the tokens from the database on every request: one minted or revoked
here takes effect without a restart.

### Options

```
  -h, --help   help for tokens
```

### SEE ALSO

* [upgradescope](upgradescope.md)	 - Continuous Kubernetes upgrade-readiness scanner
* [upgradescope tokens create](upgradescope_tokens_create.md)	 - Mint an ingest token bound to one cluster, or a read token scoped to teams
* [upgradescope tokens list](upgradescope_tokens_list.md)	 - List ingest tokens, or read tokens with --read; never prints a token
* [upgradescope tokens revoke](upgradescope_tokens_revoke.md)	 - Revoke ingest tokens of a cluster (--id or --all), or a read token (--read --id)

