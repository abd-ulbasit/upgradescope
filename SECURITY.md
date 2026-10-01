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

- **Agent RBAC and in-cluster footprint.** The chart grants the agent
  cluster-wide `get`/`list`/`watch` on all resources, including Secrets,
  because Helm release Secrets cannot be filtered by type in RBAC. It also
  grants `get` on `/metrics` and `/version`, write access to the
  `ClusterReadiness` CRD, and write access to `clusterreadinesses`. Report
  anything that lets the agent, or someone abusing its ServiceAccount, write
  outside that surface, or that leaks Secret contents beyond the Helm release
  metadata the agent needs.
- **Server authentication and authorization.** This covers the ingest bearer
  tokens (the shared `--ingest-token` and per-cluster tokens from
  `upgradescope tokens`), the read token, the cluster binding of per-cluster
  tokens, and input handling on `POST /api/v1/snapshots` and
  `POST /api/v1/gate` (size limits, gzip handling, malformed input). It also
  covers the store layer (SQL injection) and the exported HTML reports
  (injection or XSS).
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
