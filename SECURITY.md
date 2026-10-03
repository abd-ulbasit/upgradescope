# Security policy

## Supported versions

Only the **latest minor release** receives security fixes. Fixes ship as a
patch release on that minor. Older minors are not patched, so upgrade to get
the fix.

| Version | Supported |
|---|---|
| 0.1.x (latest minor) | yes |
| anything older | no |

The knowledge base is compiled into the binary and the container image. A fix
to detection data (a wrong EOL date or a missed API removal) therefore also
reaches you only through a new release.

## Reporting a vulnerability

**Do not open a public issue, discussion or pull request for a
vulnerability.**

Report it privately through GitHub's private vulnerability reporting:

**[Report a vulnerability](https://github.com/abd-ulbasit/upgradescope/security/advisories/new)**
(the Security tab → "Report a vulnerability").

Please include:

- the affected version (`upgradescope --version`) and mode (`scan`, `agent` or `serve`)
- the deployment: the Helm chart values you changed, the store (SQLite or
  Postgres), and whether `--read-token` is set
- steps to reproduce, or a proof of concept
- the impact as you understand it

## What to expect

This is a single-maintainer project, so these are targets rather than an SLA:

- **Acknowledgement** within 3 business days.
- **Initial assessment** (accepted or not, plus a severity) within 10 business days.
- **Fix or mitigation** for confirmed high or critical issues within 30 days
  of acknowledgement. Lower-severity issues are fixed in the next regular
  release.
- **Disclosure**: once a fixed release is available, we publish a GitHub
  security advisory and credit you, unless you prefer to stay anonymous.
  Please give us the chance to ship the fix before you disclose publicly.

## Scope

In scope:

- **Agent RBAC and in-cluster footprint.** The chart's ClusterRole
  (`deploy/chart/templates/rbac.yaml`, every rule explained in the
  [security model](https://abd-ulbasit.github.io/upgradescope/operations/security-model-and-rbac/))
  grants the agent's ServiceAccount:
  - `get`/`list` on namespaces, nodes and pods, and on each group/resource
    the embedded knowledge base flags as deprecated or removed (generated
    into `files/kb-rbac-rules.yaml`; its `networking.k8s.io` rule also
    covers the `ingressclasses` list that add-on detection reads); no
    wildcards and no `watch`;
  - `get` on the `/version` and `/metrics` endpoints;
  - with `rbac.helmSecrets=true` (the default), cluster-wide `get`/`list` on
    Secrets and ConfigMaps, for Helm release detection. RBAC cannot filter
    them by label or type, so the agent can read **every** Secret and
    ConfigMap; treat its token as privileged. `rbac.helmSecrets=false`
    removes both rules;
  - with `agent.manageCRD=true` (the default), `get`/`update`/`patch` on the
    one CRD `clusterreadinesses.upgradescope.dev` (by `resourceNames`; no
    `create`);
  - `get`/`list`/`create` on `clusterreadinesses`, and `update`/`patch` on
    the object named `agent.crName` and its `status` (by `resourceNames`).

  The agent writes only its own `ClusterReadiness` object and, with
  `manageCRD`, that CRD. Report anything that makes the agent write anything
  else, any way to obtain or abuse its ServiceAccount token, and any leak of
  Secret contents beyond the Helm release metadata the agent needs. The
  breadth of the grants listed above is known, so a report that only
  restates them is not a new finding.
- **Server authentication and authorization.** This covers the ingest bearer
  tokens (the shared `--ingest-token` and per-cluster tokens from
  `upgradescope tokens`), the read token, team-scoped read tokens and the
  trusted-proxy team header (a scoped read that sees another team's
  cluster, finding or team score is in scope), the cluster binding of
  per-cluster tokens, and input handling on `POST /api/v1/snapshots` and
  `POST /api/v1/gate` (size limits, gzip handling, malformed input). It also
  covers the store layer (SQL injection) and the exported HTML reports
  (injection or XSS) and CSV exports (spreadsheet formula injection).

  The server's memory is bounded by the request budgets in
  [docs/operations.md](docs/operations.md#memory-and-request-limits): body
  caps, node budgets counted before anything is decoded (YAML aliases at
  what they expand to, and the stream with its aliases expanded within
  the body cap), a shared budget for buffered bodies, and one request at
  a time per endpoint doing anything whose memory follows the input's
  structure (measuring what YAML aliases expand to, decoding,
  evaluating); the per-cluster reads, which load a stored snapshot and
  may evaluate it, share one more such slot, and the reads of the whole
  fleet (`/clusters`, `/fleet`, `/metrics`), which load no snapshot
  inventory and no stored report, two slots of their own (`/fleet`
  takes at most 16 `?targets=`). A `/gate` answer is bounded, in its
  format, before it is encoded, and one that could be over
  `--max-gate-bytes` is `413` (`?path=` is at most 512 bytes). A push
  whose identifiers are not valid for what they name (the cluster name
  an RFC 1123 subdomain, namespaces RFC 1123 labels, and so on), or that
  carries more than the limits collectors keep to (a string over 16 KiB,
  more than 100 objects per API usage entry, a group/version/kind listed
  twice), is `422` before anything is stored; the free text a collector
  copies whole from the cluster (capability reasons, the ignore
  annotations) is cut to the limits instead. Every report the server
  evaluates, stores or exports is at most `--max-snapshot-bytes`, since a
  report repeats what its inventory names: a push, a what-if or a
  `/gate?cluster=` over it is `413`, and so is an export (HTML writes
  `'` `"` `&` as five bytes). A push is evaluated, and re-evaluated, at
  its default target and every `serve --targets` minor, and each extra
  target adds about one report of up to `--max-snapshot-bytes` to an
  ingest and one to the re-evaluation pass (18-33 MiB each at the
  default 20 MiB, measured), so `serve` refuses more than 4 of them and
  the bounds are measured, and the chart's memory limit is sized, at 4,
  with notifications configured: each evaluation of a target decided
  before reads that earlier report for what changed (its findings' keys
  and severities only).
  Stored reports and JSON responses carry a
  snapshot's strings as long as they were pushed (no HTML or
  line-separator escapes; a push that is not UTF-8 is `422`), and the
  evaluation summaries the fleet reads carry keep a bounded part of what
  each evaluation could not assess. Every read's response and every `/gate` answer is
  built in its slot and waits for its client in one budget of twice
  `--max-snapshot-bytes`; one larger than what is left of that budget
  gets `503`, and one larger than the whole budget (never a `/gate`
  answer, unless `--max-gate-bytes` is over twice `--max-snapshot-bytes`)
  is sent in its slot, whose client gets 20s to take it.
  Any request that makes the server use memory beyond them is in scope,
  with or without credentials, except what is listed as outside them
  below.

  What the budgets leave is known, and documented with its measured
  cost in docs/operations.md. Memory outside them: nothing caps how many
  connections a client opens, and each costs ~10 KiB of heap and an
  8 KiB goroutine stack while open (more while up to 64 KiB of headers
  arrive), until the idle (120s), header (10s), read (60s) or write
  (120s) timeout closes it; the kernel's socket buffers are not in the
  heap figures either, and each connection that does not read can hold
  up to the host's TCP send buffer maximum (4 MiB by default on Linux)
  of the pod's memory until a write deadline closes it; nothing caps how
  many clusters the server holds (a shared ingest token registers one
  per new name), and a fleet read costs more as the fleet grows (up to
  ~16 MiB for 500 clusters evaluated at five targets, 11.4 MiB measured;
  for 2000 clusters with 200-byte names, 100 `/metrics` clients that
  never read peak the heap at 125 MiB, both fleet slots and the held
  responses included); and a snapshot a
  v0.1 server stored before these
  budgets existed is decoded without a node count when `/gate?cluster=`,
  re-evaluation or a what-if read reads it, and, having no stored server
  version, is loaded whole by `/clusters`, `/fleet` and `/metrics` to
  read it. Availability within them: a client that really sends three
  times `--max-gate-bytes` and then stalls makes other `/gate` requests
  `503` until the 60s read timeout cuts it off; one that keeps asking for
  what-if reports keeps other per-cluster reads waiting; and because any
  answer larger than what is left of the response budget gets `503`, a
  client that chooses large answers (a report, a `/gate` answer of up to
  `--max-gate-bytes`, or a fleet read of a large fleet) and does not
  read them can keep that budget full for up to the
  120s write timeout, and again after it, starving per-cluster reads,
  fleet reads (Prometheus scrapes and the dashboard included) and `/gate`
  of every answer that does not fit. All of these need no credentials
  when the read API is open.
- **Supply chain.** This covers release archives and `checksums.txt`, the
  container image, the GitHub Action in `action/` (how it downloads and runs
  the binary), the CI workflows (for example, pull request workflows that can
  be abused), and the knowledge-base refresh pipeline (`tools/gen-kb`,
  `tools/eol-sync`, `kb-refresh.yml`).

Out of scope:

- **Running `serve` without a read credential.** The flag help and the
  chart's `values.yaml` both document that this leaves the read API open. A
  missing token is a configuration choice, not a vulnerability. So is a
  `--trust-team-header` proxy that passes client-supplied copies of the
  header on, or a `--trusted-proxy-cidr` wider than the proxy: the docs
  say both make the header spoofable.
- Findings that are wrong (false positives or false negatives) but have no
  security impact. Open a regular bug report for those.
- Vulnerabilities in dependencies that upgradescope does not reach. Do report
  ones it does reach.
- Denial of service that needs a valid ingest token or cluster-admin access.
