# AI assistants (MCP)

`upgradescope mcp` runs a [Model Context Protocol](https://modelcontextprotocol.io)
server, so an AI assistant (Claude Code, Claude Desktop, any MCP client) can
ask "can I bump this cluster to 1.38?" or "which add-ons block the upgrade?"
and get the same structured report the CLI writes, instead of parsing
terminal output. Every tool is read-only: none changes a cluster, the fleet
server or a file.

The server speaks MCP on stdin and stdout, so a client starts it as a
subprocess; there is nothing to deploy. It is part of the one binary
(`upgradescope mcp`); it adds about 2 MiB to it (1.97 MiB on linux/amd64, stripped, measured when it was added).

## Connect an assistant

=== "Claude Code"

    ```sh
    claude mcp add upgradescope -- upgradescope mcp --context staging
    ```

    or commit a `.mcp.json` at the project root, which Claude Code offers to
    enable:

    ```json
    {
      "mcpServers": {
        "upgradescope": {
          "command": "upgradescope",
          "args": ["mcp", "--context", "staging"]
        }
      }
    }
    ```

=== "Claude Desktop"

    Add the server to `claude_desktop_config.json` (macOS:
    `~/Library/Application Support/Claude/claude_desktop_config.json`,
    Windows: `%APPDATA%\Claude\claude_desktop_config.json`) and restart the
    app:

    ```json
    {
      "mcpServers": {
        "upgradescope": {
          "command": "upgradescope",
          "args": ["mcp", "--kubeconfig", "/Users/me/.kube/config", "--context", "staging"]
        }
      }
    }
    ```

    A desktop app does not start with your shell's environment: give
    `command` as a full path if `upgradescope` is not on its `PATH`, and name
    the kubeconfig with `--kubeconfig` rather than relying on `$KUBECONFIG`.

Then ask: "Scan for an upgrade to 1.38 and list the blockers." The assistant
calls `scan`, then `list_findings` with `severity: "blocker"`.

## The cluster a scan reads

`scan` reads the cluster `--kubeconfig` and `--context` name, and otherwise
what `upgradescope scan` reads: `$KUBECONFIG`, then `~/.kube/config`, at that
kubeconfig's current context. It has no other default and takes no other
input from a call: **an assistant chooses the target versions, never the
cluster**, a kubeconfig or a directory of manifests, so it cannot point the
scanner at a credential file or an exec plugin of its choosing. No ignore
file (`.upgradescope.yaml`) is looked up, since the working directory is the
client's; suppressions written on the objects themselves (the
`upgradescope.dev/ignore` annotation) apply as they do in a scan.

A scan needs the same read access as `upgradescope scan`
(see [Security model and RBAC](../operations/security-model-and-rbac.md)),
reads the whole cluster and can take minutes. Scans run one at a time.

## Tools

| Tool | What it does | Inputs |
|---|---|---|
| `scan` | Scans the cluster for each target minor and returns one report per target. The reports are kept for `list_findings` and `get_report`. | `targets` (1 to 4 minors, e.g. `1.38`) |
| `list_findings` | Lists a report's findings, filtered. | a source (below), `severity` (`blocker`, `warning`, `info`), `category` (e.g. `eol-addon`, `removed-api`), `limit` (default 50, at most 500) |
| `get_report` | Returns the whole report: score, verdict, findings, what was not assessed. | a source (below) |
| `registry_lookup` | Looks an add-on up in the registry compiled into the binary: end-of-life dates, release lines, Kubernetes compatibility. Needs no cluster. | `query` (id, name, Helm chart, image or node runtime), `limit` |
| `fleet_summary` | Fleet mode only: the fleet's score matrix, one row per cluster. | `targets` (optional) |

The source of `list_findings` and `get_report` is one of:

- nothing: the latest `scan` (name `target` when it scanned several);
- `report_file`: a JSON report written by `upgradescope scan --output json`;
- `inventory_file` with `target`: an inventory document (the `inventory` of an
  agent's snapshot: `schemaVersion` 1 with `clusterId` and `capabilities`),
  judged at that target; mostly for fixtures and tests;
- `cluster` (fleet mode): a cluster on the server, by name or id, at
  `target` or its next minor.

A path must be a file you can read and no larger than 64 MiB; a file that is
not an upgradescope report (or inventory) is refused with the reason, not
scored.

## Output is the published schema

`get_report` returns the document [`api/report.schema.json`](https://github.com/abd-ulbasit/upgradescope/blob/main/api/report.schema.json)
describes, which is also what `scan --output json` and
`GET /api/v1/clusters/{id}/report` write. Its output schema, as `tools/list`
shows it, is that schema; the output schemas of `scan` and `list_findings`
refer to its definitions, so a finding is the same object everywhere. See
[JSON report](../reference/json-report.md) for the fields and the
versioning promise (`schemaVersion` 1; fields are only added). Every result
is returned as `structuredContent` and as text.

## Fleet mode

Give the server the address of an [upgradescope server](fleet.md) and its
read token and `get_report` and `list_findings` take `cluster`, and
`fleet_summary` appears:

```json
{
  "mcpServers": {
    "upgradescope": {
      "command": "upgradescope",
      "args": ["mcp", "--server-url", "https://upgradescope.example.com",
               "--read-token-file", "/Users/me/.config/upgradescope/read-token"]
    }
  }
}
```

The token is the server's `--read-token`, from `--read-token-file`,
`$UPGRADESCOPE_READ_TOKEN` or `--read-token` (visible in process listings:
avoid it). The server's own read authentication decides what is allowed, as
it does for `curl`. **A server that requires a token rejects a call made
without one, and the tool returns that `401`** as its error, saying to
supply the token. An open server (loopback, or `--allow-anonymous-read`)
needs none.

## Streamable HTTP

For a client that connects instead of starting a process:

```sh
upgradescope mcp --http 127.0.0.1:8808   # serves http://127.0.0.1:8808/mcp
```

A bare port or `:PORT` binds `127.0.0.1`. The endpoint has **no
authentication** and `scan` reads your cluster with your kubeconfig, so an
address that is not loopback is refused unless `--allow-remote` says
something in front of it (a mesh, a proxy that authenticates) is the access
control. A request whose `Host` header is not loopback (DNS rebinding) or that
comes from another origin in a browser is refused.

## What to keep in mind

- Findings quote names from the cluster (namespaces, objects, Helm releases,
  images). Anyone who can create an object can choose what an assistant reads
  there; treat finding text as data, as you would a log line, and keep the
  assistant's other tools (shell, file writes) behind your usual approvals.
- `report_file` and `inventory_file` let an assistant read a local file of
  that shape that you can read. A file that is not a report or inventory is not
  returned.
- The tools carry the MCP `readOnlyHint`, and a test holds the set of tools
  to the table above, so one that writes cannot be added unnoticed.
