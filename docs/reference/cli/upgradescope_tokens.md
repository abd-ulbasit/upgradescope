## upgradescope tokens

Manage per-cluster ingest tokens for agent snapshot pushes

### Synopsis

Manage per-cluster ingest tokens. Each token authenticates snapshot
pushes for one cluster name only, so a leaked token cannot write another
cluster's history. The server database keeps each token's sha256 hash and
its first 8 characters (which "tokens list" shows), never the token.

### Options

```
  -h, --help   help for tokens
```

### SEE ALSO

* [upgradescope](upgradescope.md)	 - Continuous Kubernetes upgrade-readiness scanner
* [upgradescope tokens create](upgradescope_tokens_create.md)	 - Mint an ingest token bound to one cluster
* [upgradescope tokens list](upgradescope_tokens_list.md)	 - List ingest tokens; never prints a token
* [upgradescope tokens revoke](upgradescope_tokens_revoke.md)	 - Revoke one ingest token of a cluster by id, or all of them with --all

