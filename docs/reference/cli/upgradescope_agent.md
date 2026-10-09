## upgradescope agent

Run the in-cluster continuous upgrade-readiness agent

### Synopsis

Continuously collect the cluster's inventory, evaluate upgrade readiness
every --interval, write the result to the status of the ClusterReadiness
object, and (with --server-url) push snapshots to an upgradescope server.

The Helm chart (deploy/chart) runs it in the cluster with read-only RBAC.

```
upgradescope agent [flags]
```

### Examples

```
  # In the cluster, CRD status only (what the Helm chart runs)
  upgradescope agent

  # Also push snapshots to a fleet server
  upgradescope agent --server-url https://upgradescope.example.com \
    --server-token-file /var/run/secrets/upgradescope/token

  # Try it from a laptop against a kubeconfig context
  upgradescope agent --context kind-dev --interval 1m --cr-name dev
```

### Options

```
      --cluster-name string         cluster label sent to the server, an RFC 1123 subdomain of at most 253 bytes (default: cluster UID)
      --context string              kubeconfig context to use
      --cr-name string              ClusterReadiness object name, an RFC 1123 subdomain of at most 253 bytes (changing it leaves the old object behind: kubectl delete ucr <old-name>) (default "cluster")
      --force-sync-every duration   push a snapshot even if unchanged after this long; must be positive, and a value at or below --interval means every tick (default 1h0m0s)
      --health-addr string          listen address for /healthz, /readyz and /metrics (empty = disabled) (default ":8081")
  -h, --help                        help for agent
      --interval duration           evaluation interval (minimum 1m) (default 10m0s)
      --kubeconfig string           path to kubeconfig (default: in-cluster config, then standard loading rules)
      --log-format string           log format: text (logfmt) or json (default "text")
      --log-level string            log level: debug, info, warn or error (default "info")
      --manage-crd                  keep the ClusterReadiness CRD schema in step with this binary at startup, a failed check retried every tick until it succeeds (needs get/patch on that CRD); false = never touch the CRD (default true)
      --registry-dir string         extra add-on registry entries: one <id>.yaml file or a directory of them, in the schema of registry/CONTRIBUTING.md and validated like the embedded entries; an entry with an embedded id replaces it
      --request-timeout duration    give up on a single API request after this long (0 = no per-request limit) (default 30s)
      --server-ca-file string       PEM bundle of CA certificates trusted for an https --server-url, on top of the system roots (a server behind a private CA); read at startup
      --server-token string         bearer token for snapshot pushes (required with --server-url) (visible in process listings: prefer $UPGRADESCOPE_SERVER_TOKEN or --server-token-file)
      --server-token-file string    read --server-token from this file, e.g. a mounted Secret (surrounding whitespace is trimmed)
      --server-url string           upgradescope server base URL (empty = CRD-only mode)
      --targets strings             target minors, CSV, e.g. 1.37,1.38, at most 8 distinct minors (the ClusterReadiness spec.targets cap; more is refused at start); when set, the ClusterReadiness spec.targets is reconciled to them every tick (overriding kubectl edits)
      --team-label string           namespace label used for team attribution (default "team")
```

### SEE ALSO

* [upgradescope](upgradescope.md)	 - Continuous Kubernetes upgrade-readiness scanner

