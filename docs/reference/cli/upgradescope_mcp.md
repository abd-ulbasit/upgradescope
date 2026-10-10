## upgradescope mcp

Serve upgrade readiness to AI assistants over MCP (read-only)

### Synopsis

Run a Model Context Protocol server so an AI assistant (Claude Code, Claude
Desktop, any MCP client) can ask about upgrade readiness. Every tool is
read-only; none changes a cluster, the server or a file.

Tools: scan (scan the cluster in the kubeconfig for one or more targets, as
'upgradescope scan --output json' does), list_findings and get_report (the
latest scan, a report file written by 'scan --output json', or an inventory
file judged at a target), registry_lookup (add-on end-of-life and
compatibility from the embedded registry) and, in fleet mode, fleet_summary.
Reports and findings follow the published schema, api/report.schema.json, so
they are the JSON the CLI and the REST API write.

The server speaks MCP on stdin and stdout, which an assistant client starts
as a subprocess. With --http ADDR it serves MCP over streamable HTTP at
http://ADDR/mcp instead, on 127.0.0.1 unless ADDR names another host (which
needs --allow-remote). The HTTP endpoint has no authentication unless
--http-token sets a bearer token every request must carry; without one, any
local user or process that can reach the port can run scans with your
kubeconfig.

The cluster a scan reads is the one --kubeconfig and --context name, else
$KUBECONFIG and the kubeconfig's current context, as for 'scan'; without
--context, the current context is read once at start and kept, so switching
contexts later does not move the server to another cluster; when there is
none at start, scan is off until the server is restarted with --context.
Nothing else
chooses it: an assistant names the target versions, never the cluster, and
no ignore file is looked up. With --server-url, get_report and list_findings
can read a cluster from an upgradescope server and fleet_summary summarises
the fleet, using the server's read token (--read-token, --read-token-file
or $UPGRADESCOPE_READ_TOKEN); a server that requires one rejects calls
without it, and the tool shows that error.

```
upgradescope mcp [flags]
```

### Examples

```
  # What an MCP client starts (see docs/getting-started/mcp.md for its configuration)
  upgradescope mcp

  # The same, on a specific kubeconfig context
  upgradescope mcp --context staging

  # Fleet mode: also answer from an upgradescope server
  UPGRADESCOPE_READ_TOKEN=... upgradescope mcp --server-url https://upgradescope.example.com

  # Streamable HTTP on loopback, with a bearer token
  UPGRADESCOPE_MCP_HTTP_TOKEN=... upgradescope mcp --http 127.0.0.1:8808
```

### Options

```
      --allow-remote               with --http, accept an address that is not loopback
      --context string             kubeconfig context for scan (default: the kubeconfig's current context when the server starts)
  -h, --help                       help for mcp
      --http string                serve MCP over streamable HTTP at http://ADDR/mcp instead of stdio; a bare port or :PORT binds 127.0.0.1
      --http-token string          with --http: a bearer token every request must carry (Authorization: Bearer TOKEN); without it the endpoint has no authentication (visible in process listings: prefer $UPGRADESCOPE_MCP_HTTP_TOKEN or --http-token-file)
      --http-token-file string     read --http-token from this file, e.g. a mounted Secret (surrounding whitespace is trimmed)
      --kubeconfig string          path to kubeconfig for scan (default: standard loading rules)
      --read-token string          with --server-url: the server's read token; omit it for an open read API (visible in process listings: prefer $UPGRADESCOPE_READ_TOKEN or --read-token-file)
      --read-token-file string     read --read-token from this file, e.g. a mounted Secret (surrounding whitespace is trimmed)
      --request-timeout duration   give up on a single API request of a scan after this long (0 = no per-request limit) (default 30s)
      --server-url string          fleet mode: base URL of an upgradescope server, e.g. https://upgradescope.example.com
```

### SEE ALSO

* [upgradescope](upgradescope.md)	 - Continuous Kubernetes upgrade-readiness scanner

