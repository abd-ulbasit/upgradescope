## upgradescope serve

Run the upgradescope server: snapshot ingest, REST API, history, notifications

### Synopsis

Run the upgradescope server. It accepts snapshots that agents push,
evaluates them against every target, keeps the history, serves the REST API
and the dashboard at /, and sends Slack or webhook notifications when a
cluster's readiness changes.

It listens on loopback by default. On any other address, the read API needs
--read-token, or an explicit --allow-anonymous-read.

```
upgradescope serve [flags]
```

### Examples

```
  # Local dashboard at http://127.0.0.1:8080/, SQLite in ./upgradescope.db
  upgradescope serve

  # Fleet server: all interfaces, Postgres, tokens from mounted Secrets
  upgradescope serve --listen :8080 \
    --db-url-file /secrets/db-url --read-token-file /secrets/read-token \
    --targets 1.37,1.38
```

### Options

```
      --admin-token string           bearer token for cluster administration: DELETE and PATCH (rename) /api/v1/clusters/{id}, 'upgradescope clusters delete|rename --server'; it also reads (empty = administration refused) (visible in process listings: prefer $UPGRADESCOPE_ADMIN_TOKEN or --admin-token-file)
      --admin-token-file string      read --admin-token from this file, e.g. a mounted Secret (surrounding whitespace is trimmed)
      --allow-anonymous-read         serve the read API and /api/v1/gate without a read token on a non-loopback --listen address
      --db string                    path to the SQLite database (parent directory is created) (default "upgradescope.db")
      --db-url string                Postgres URL (postgres://user:pass@host:5432/db); mutually exclusive with --db (visible in process listings: prefer $UPGRADESCOPE_DB_URL or --db-url-file)
      --db-url-file string           read --db-url from this file, e.g. a mounted Secret (surrounding whitespace is trimmed)
  -h, --help                         help for serve
      --ingest-token string          optional shared bearer token that may push snapshots as ANY cluster; omit it to accept only per-cluster tokens from 'upgradescope tokens create' (serve warns at startup, not later, when both are in use) (visible in process listings: prefer $UPGRADESCOPE_INGEST_TOKEN or --ingest-token-file)
      --ingest-token-file string     read --ingest-token from this file, e.g. a mounted Secret (surrounding whitespace is trimmed)
      --listen string                address to listen on (loopback by default; use :8080 for all interfaces) (default "127.0.0.1:8080")
      --max-gate-bytes int           largest accepted /api/v1/gate manifest stream, in bytes (default 10485760)
      --max-snapshot-bytes int       largest accepted snapshot push body, in bytes (also applied after gzip decompression) (default 20971520)
      --read-token string            bearer token for the read API and /api/v1/gate (empty = OPEN read access; refused on non-loopback --listen without --allow-anonymous-read) (visible in process listings: prefer $UPGRADESCOPE_READ_TOKEN or --read-token-file)
      --read-token-file string       read --read-token from this file, e.g. a mounted Secret (surrounding whitespace is trimmed)
      --retention string             prune snapshots and evaluations older than this, in days (90d) or a Go duration (2160h), at startup and daily; each cluster's latest snapshot and its evaluations are always kept; 0 keeps everything (default "90d")
      --slack-webhook string         Slack incoming-webhook URL for delta notifications (visible in process listings: prefer $UPGRADESCOPE_SLACK_WEBHOOK or --slack-webhook-file)
      --slack-webhook-file string    read --slack-webhook from this file, e.g. a mounted Secret (surrounding whitespace is trimmed)
      --stale-after duration         mark a cluster stale (API, dashboard data, /metrics) when its agent has not pushed for this long; agents push at least about every 70m by default (default 2h0m0s)
      --targets string               extra target versions evaluated on every snapshot, CSV, e.g. 1.37,1.38
      --team-map string              YAML file of {pattern, team} namespace globs overriding team labels (first match wins)
      --tls-cert-file string         PEM certificate (chain) to serve HTTPS directly; requires --tls-key-file (read at startup)
      --tls-key-file string          PEM private key for --tls-cert-file
      --webhook string               generic webhook URL: POSTed one versioned JSON notification per cluster and evaluation pass (schema in api/webhook.schema.json) (visible in process listings: prefer $UPGRADESCOPE_WEBHOOK_URL or --webhook-file)
      --webhook-file string          read --webhook from this file, e.g. a mounted Secret (surrounding whitespace is trimmed)
      --webhook-secret string        sign generic webhook requests: X-Upgradescope-Signature: sha256=<hex HMAC-SHA256 of the body with this key> (visible in process listings: prefer $UPGRADESCOPE_WEBHOOK_SECRET or --webhook-secret-file)
      --webhook-secret-file string   read --webhook-secret from this file, e.g. a mounted Secret (surrounding whitespace is trimmed)
```

### SEE ALSO

* [upgradescope](upgradescope.md)	 - Continuous Kubernetes upgrade-readiness scanner

