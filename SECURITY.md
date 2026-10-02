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
  `upgradescope tokens`), the read token, the cluster binding of per-cluster
  tokens, and input handling on `POST /api/v1/snapshots` and
  `POST /api/v1/gate` (size limits, gzip handling, malformed input). It also
  covers the store layer (SQL injection) and the exported HTML reports
  (injection or XSS) and CSV exports (spreadsheet formula injection).

  The server's memory is bounded by the request budgets in
  [docs/operations.md](docs/operations.md#memory-and-request-limits): body
  caps, node budgets counted before anything is decoded (YAML aliases at
  what they expand to), a shared budget for buffered bodies, and one
  request at a time per endpoint doing anything whose memory follows the
  input's structure (measuring what YAML aliases expand to, decoding,
  evaluating). Any request that makes the server use memory beyond them is
  in scope, with or without credentials. What the budgets leave is known:
  a client that really sends three times `--max-gate-bytes` and then
  stalls makes other `/gate` requests `503` until the 60s read timeout cuts
  it off, without credentials when the read API is open; and a snapshot a
  v0.1 server stored before these budgets existed is decoded without a
  node count when `/gate?cluster=` or re-evaluation reads it.
- **Supply chain.** This covers release archives and `checksums.txt`, the
  container image, the GitHub Action in `action/` (how it downloads and runs
  the binary), the CI workflows (for example, pull request workflows that can
  be abused), and the knowledge-base refresh pipeline (`tools/gen-kb`,
  `tools/eol-sync`, `kb-refresh.yml`).

Out of scope:

- **Running `serve` without `--read-token`.** The flag help and the chart's
  `values.yaml` both document that this leaves the read API open. A missing
  token is a configuration choice, not a vulnerability.
- Findings that are wrong (false positives or false negatives) but have no
  security impact. Open a regular bug report for those.
- Vulnerabilities in dependencies that upgradescope does not reach. Do report
  ones it does reach.
- Denial of service that needs a valid ingest token or cluster-admin access.
