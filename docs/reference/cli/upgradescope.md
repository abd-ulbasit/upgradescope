## upgradescope

Continuous Kubernetes upgrade-readiness scanner

### Synopsis

upgradescope finds what blocks a Kubernetes upgrade before you start it:
deprecated and removed APIs (stored objects and live callers), end-of-life
add-ons, version skew and chart compatibility, rolled up into a readiness
score and verdict.

Run it once with 'scan', continuously in the cluster with 'agent', and across
a fleet with 'serve'.

```
upgradescope [flags]
```

### Examples

```
  # Is the current kubeconfig context's cluster ready for Kubernetes 1.37?
  upgradescope scan --target 1.37

  # Gate a pull request on rendered manifests
  helm template ./chart --output-dir rendered
  upgradescope scan --files rendered --target 1.37 --output sarif > upgradescope.sarif
```

### Options

```
  -h, --help   help for upgradescope
```

### SEE ALSO

* [upgradescope agent](upgradescope_agent.md)	 - Run the in-cluster continuous upgrade-readiness agent
* [upgradescope clusters](upgradescope_clusters.md)	 - List, delete and rename the clusters a server knows
* [upgradescope completion](upgradescope_completion.md)	 - Generate the autocompletion script for the specified shell
* [upgradescope mcp](upgradescope_mcp.md)	 - Serve upgrade readiness to AI assistants over MCP (read-only)
* [upgradescope scan](upgradescope_scan.md)	 - Scan a cluster (or rendered manifests) for upgrade readiness
* [upgradescope serve](upgradescope_serve.md)	 - Run the upgradescope server: snapshot ingest, REST API, history, notifications
* [upgradescope tokens](upgradescope_tokens.md)	 - Manage per-cluster ingest tokens and team-scoped read tokens
* [upgradescope version](upgradescope_version.md)	 - Print the version, build and knowledge base details

