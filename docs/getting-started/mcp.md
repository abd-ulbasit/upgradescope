# AI assistants (MCP)

`upgradescope mcp` runs a [Model Context Protocol](https://modelcontextprotocol.io)
server, so an AI assistant (Claude Code, Claude Desktop, any MCP client) can
ask "can I bump this cluster to 1.38?" or "which add-ons block the upgrade?"
and get the same structured report the CLI writes, instead of parsing
terminal output. Every tool is read-only: none changes a cluster, the fleet
server or a file.

The server speaks MCP on stdin and stdout, so a client starts it as a
subprocess; there is nothing to deploy. It is part of the one binary
(`upgradescope mcp`); it adds about 2 MiB to it (2.00 MiB on linux/amd64, stripped, measured
at `67f30be`; MC-08 in the [claims ledger](../claims.md) has the command).

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
kubeconfig's current context. Without `--context`, the current context is
read once, when the server starts, and kept (the server prints it on
stderr), so `kubectl config use-context` afterwards, by you or by an
assistant's shell tool, does not move the server to another cluster. If
there is none to keep at start (no kubeconfig, one that names no current
context, or one that cannot be read), **`scan` is off** for the life of the
server: the start message says so, every `scan` call is refused saying to
restart with `--context`, and a context that becomes current later is never
used. The other tools work. It has
no other default and takes no other
input from a call: **an assistant chooses the target versions, never the
cluster**, a kubeconfig or a directory of manifests, so it cannot point the
scanner at a credential file or an exec plugin of its choosing. No ignore
file (`.upgradescope.yaml`) is looked up, since the working directory is the
client's; suppressions written on the objects themselves (the
`upgradescope.dev/ignore` annotation) apply as they do in a scan.

A scan needs the same read access as `upgradescope scan`
(see [Security model and RBAC](../operations/security-model-and-rbac.md)),
reads the whole cluster and can take minutes. A call with several targets
reads the cluster once and judges it at each. Scans run one at a time; a
call the client cancels stops its scan, or stops waiting for another one.
A client that goes away cancels its calls too: on stdio by closing its
end, and over HTTP by closing the connection that carries the call (its
answer could not be delivered any more), so an abandoned scan gives up the
scan slot at once.

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

A path must name a regular file you can read: a directory, a named pipe, a
device or `/dev/stdin` is refused at once, before it is opened for reading.
On Linux and macOS the file is also opened without waiting, so a named pipe
put in its place between the check and the open is refused too; Windows has
no such open, and the check is the only guard there. A regular file whose
read blocks (one on a stalled NFS or FUSE mount) has no deadline the
operating system honours: the call returns when the client cancels it, and
the read goes on in the background until the file answers. Such a read keeps
its place among the two file reads that run at once until it returns, so
cancelled calls cannot pile up reads behind a stalled mount; while two are
stuck, a further call waits, and gives up when the client cancels it.
A report may be at most 8 MiB and an inventory at most 20 MiB (what a
default server takes from an agent), whatever size the file reports or
grows to while it is read. An inventory is checked as a server checks an
agent's push: one the server would refuse (an identifier that is not valid
for what it names, a `serverVersion` that is not 1.x, a value over the
limits collectors keep to) is refused, and capability reasons over their
limit are cut and judged, as the server cuts them. A file that is not an
upgradescope report, one that does not follow `api/report.schema.json`, or
not an inventory the server would judge, is refused with the reason, not
scored; the reason names the field and the rule, and never quotes the file,
neither a value nor a key.

## Large reports

A result carries its document twice, as `structuredContent` and as the same
JSON in a text block, and an MCP client takes a bounded message: the Go SDK's
client at most 16 MiB. That is why a report is read only up to 8 MiB. A
result that would still be larger than a client takes (a `get_report` of a
report near that bound, a `scan` of a very large cluster) is refused with
the reason and what to ask for instead: `list_findings` with `severity`,
`category` or a smaller `limit`. The reports of such a scan are kept, so
`list_findings` reads them.

Holding a report while it is checked and sent takes several times its size,
so at most two `get_report`, `list_findings` or `fleet_summary` calls run at
once, however many a client sends in parallel; the others wait their turn,
or give up when the client cancels them.

## Output is the published schema

`get_report` returns the document [`api/report.schema.json`](https://github.com/abd-ulbasit/upgradescope/blob/main/api/report.schema.json)
describes, which is also what `scan --output json` and
`GET /api/v1/clusters/{id}/report` write. Its output schema, as `tools/list`
shows it, is that schema; the output schemas of `scan` and `list_findings`
refer to its definitions, so a finding is the same object everywhere. See
[JSON report](../reference/json-report.md) for the fields and the
versioning promise (`schemaVersion` 1; fields are only added). Every result
is returned as `structuredContent` and as text. A result that carries a
report (`scan`, `get_report`, `list_findings`) has two text blocks: a notice
about the cluster's text in it (see [below](#what-to-keep-in-mind)), then
the JSON.

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

The client follows no redirect (a `3xx` is the tool's error), and a read
token sent over plain `http://` to a host that is not loopback gets a
warning on stderr: it crosses the network in the clear.

## Streamable HTTP

For a client that connects instead of starting a process:

```sh
upgradescope mcp --http 127.0.0.1:8808   # serves http://127.0.0.1:8808/mcp
```

A bare port or `:PORT` binds `127.0.0.1`. Without a token the endpoint has
**no authentication**, and loopback is not a boundary between users: **any
local user or process that can reach the port can call `scan`, which reads
your cluster with your kubeconfig**. Give it a bearer token that every
request must carry:

```sh
openssl rand -hex 32 > ~/.config/upgradescope/mcp-token
upgradescope mcp --http 127.0.0.1:8808 --http-token-file ~/.config/upgradescope/mcp-token
claude mcp add --transport http upgradescope http://127.0.0.1:8808/mcp \
  --header "Authorization: Bearer $(cat ~/.config/upgradescope/mcp-token)"
```

The token can also come from `$UPGRADESCOPE_MCP_HTTP_TOKEN` or `--http-token`
(visible in process listings). A request without it, or with another, gets
`401`, and its connection is closed without reading the body it sent.

Whatever the token, a peer cannot hold the server's connections or memory:

- a request's headers must arrive within 10 seconds and the whole request
  within 60 seconds, its headers may take at most 64 KiB (`431` past that)
  and its body at most 4 MiB (`413`); a keep-alive connection idle for 120
  seconds is closed. There is no limit on writing an answer, so a scan that
  takes minutes still returns its result;
- an MCP session that gets no request for 10 minutes is closed (a client
  that comes back gets `404` and opens a new one, as the MCP specification
  has it), and at most 100 sessions are open at once: past that a new one
  is refused with `503` and the reason, and the open ones go on working.

An address that is not loopback is refused unless `--allow-remote` is
given; use it with `--http-token`, or when something in front of the
endpoint (a mesh, a proxy that authenticates) is the access control, and
note the endpoint is plain HTTP, so the token crosses that network in the
clear. A request from another origin in a browser is refused. The MCP SDK
refuses a request whose `Host` header is not loopback (DNS rebinding) only
when it arrives on a loopback address: a request that arrives on the
address `--allow-remote` opens gets no such check.

## What to keep in mind

- A report carries text from the cluster, which whoever can create or
  annotate an object there chooses: the names it read (namespaces, objects,
  Helm releases, images, teams, field managers), which finding titles and
  details quote, and free text copied from objects. The free text is
  `objects[].ignore` and `objects[].ignoreReason` (the
  `upgradescope.dev/ignore` and `upgradescope.dev/ignore-reason`
  annotations, kept up to 16 KiB and listed even when they suppress
  nothing), `suppressed[].reason` (the reason of an annotation that did
  suppress a finding) and `notAssessed[].reason` (what the API server
  answered). In `--files` mode the manifests' authors choose all of it; in a
  `report_file` or from the fleet server, whoever wrote the report.
- So every `scan`, `get_report` and `list_findings` result opens with a
  notice that marks that text as **cluster-supplied, not instructions**, and
  names where the free text is (as JSON pointers, e.g.
  `/findings/0/objects/0/ignoreReason`); the result's `_meta` carries the
  same, structured, under `upgradescope.dev/clusterSupplied`. Each such
  value longer than 2 KiB is cut, ending in `…(cut by upgradescope mcp)`, so
  `get_report` can differ there from the report file it read. The server's
  instructions tell the assistant the same. This helps an assistant tell
  the cluster's words from the tool's; it does not make it immune to them:
  keep the assistant's other tools (shell, file writes) behind your usual
  approvals.
- `report_file` and `inventory_file` let an assistant read a local regular
  file of that shape that you can read. A file that is not a report or
  inventory is not returned, and the reason it is refused quotes none of it.
- The tools carry the MCP `readOnlyHint`, and a test holds the set of tools
  to the table above, so one that writes cannot be added unnoticed.
