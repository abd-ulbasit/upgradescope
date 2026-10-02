# Troubleshooting

## The verdict is `unknown` and the gate fails

`unknown` means no blocker was found, but a required check could not run,
so one may have been missed. The report's `NOT ASSESSED` section (JSON
`notAssessed`, the CR's `status.notAssessed`) names the gap and why:

| Gap | Usual cause | What to do |
|---|---|---|
| `kb-coverage (required)` | The target is newer than the knowledge base's horizon (`upgradescope version` prints it). | Target the horizon or lower, upgrade to a release with a newer knowledge base, or accept the gap with `--allow-incomplete`. |
| `api-usage (required)` | The scan could not list resources (RBAC, an unavailable API group), or a partial list skipped an API removed by the target. | Fix the access the reason names. |
| `versions (required)` | `/version` or nodes could not be read, or the server version did not parse. | Fix the access; a version string that does not parse is a bug, please report it. |
| `addons (required)` | Pods could not be listed. | Grant `list` on pods. |

`--allow-incomplete` (CLI) gates on findings alone. The GitHub Action has no
such input yet; see [CI gate](getting-started/ci-gate.md).

## `deprecated-calls: apiserver /metrics forbidden`

Managed control planes (EKS, GKE, AKS) often deny `/metrics` regardless of
RBAC. The check is optional: the verdict does not depend on it, and stored
objects are still checked. Nothing to fix;
see [Managed clusters](guides/managed-clusters.md).

The same holds when `/metrics` does not answer: the request is given up
after `--request-timeout` (default 30s), the reason
(`get /metrics: … context deadline exceeded`) says so, and the verdict can
still be `ready`, judged on stored objects without the request signal.

## A check not assessed with a timeout or a step deadline

A live scan has 5 minutes. Each API request is given up after
`--request-timeout` (default 30s; `0` turns the per-request limit off), and
each of the five collector steps runs under its own share of the time left,
so a slow or stalled API server degrades only the step it stalled: that
capability is not assessed, and the other steps still run. Its reason says
`context deadline exceeded`, with `Client.Timeout exceeded` when a single
request timed out, or `(step deadline: gave up after …)` when the step's
share of the time ran out. When the step is a required one
(`versions`, `addons`, `api-usage`) the verdict is `unknown`. Check the API
server's health and the network path to it, or raise `--request-timeout`
when single requests are slow but complete (a very large list page, a big
`/metrics`).

## `helm` not assessed: Secrets forbidden

The agent was installed with `rbac.helmSecrets=false`, or your own identity
cannot list Secrets. Helm chart findings are missing from the report;
add-ons are still found from images. The trade-off is in
[Security model and RBAC](operations/security-model-and-rbac.md#the-rbachelmsecrets-trade-off).

## Exit code 1

An operational error, never a verdict: the cluster could not be read at all
(check `--context` and `--kubeconfig`), `--files` found no Kubernetes
objects (`no Kubernetes manifests found under ...`), or a flag, config file
or baseline is invalid. The message says which.

## A removed-API finding I did not expect

Live findings name the field manager that wrote the object through the
deprecated version (`Written by: ...`). Re-apply the object through the
current version with that tool and the finding clears on the next scan. If
the manager has moved to the new version but an old entry remains (a Helm
upgrade that changed nothing but the apiVersion), see the known limits in
[Deprecated-API detection](concepts/api-usage-detection.md#known-limits).
To accept a finding for now, with a reason and an expiry, use an
[ignore rule](guides/suppressions-and-baselines.md).

## The agent

- **`ClusterReadiness CRD is not installed`.** The chart installs the CRD
  from `crds/`; it is missing when the chart was installed with
  `--skip-crds`, or the CRD was deleted. Apply `deploy/chart/crds/`.
  With `agent.manageCRD=true` the agent stops at startup with this error;
  with `false`, every tick fails with it.
- **The pod never becomes Ready.** Readiness waits for a successful tick.
  `kubectl logs` shows one line per tick, with `tick failed` and the error.
  A tick fails when no target can be evaluated (no `spec.targets` and an
  unreadable server version), or the status cannot be written.
- **`health listener: address already in use`** when running the agent
  outside a pod: something holds port 8081. Pass `--health-addr
  127.0.0.1:<port>`, or `--health-addr ""` to turn it off.
- **`LASTEVALUATED` is old** (more than about two intervals): the agent is
  not ticking. Check the pod; the `UpgradescopeAgentNotTicking` alert fires
  on this ([Prometheus and Grafana](guides/prometheus-grafana.md)).

## The server

- **`refusing to serve the read API and /api/v1/gate without a token`.**
  `serve` listens on a non-loopback address with no `--read-token`. Set
  one, listen on loopback, or pass `--allow-anonymous-read` when something
  in front of the server authenticates ([Tenancy](operations/tenancy.md)).
- **A push gets 409: `cluster name ... is registered to clusterId ...`.**
  Another cluster already uses this `--cluster-name`, or the cluster was
  rebuilt. Give the agent a distinct name, or delete the old record with
  `upgradescope clusters delete <name> --server ...` (admin token); the
  message spells out both.
- **A push gets 401 or 403.** 401: the token is missing or unknown. 403: a
  per-cluster token pushing as another cluster name.
- **A cluster shows as stale.** Its agent has not pushed within
  `serve --stale-after` (2h). Check the agent's logs for `push=failed`.
- **The dashboard says `dashboard not built`.** The binary was built with
  `-tags nodashboard`, or from a tree without the committed dashboard. The
  API still works. Release binaries, images and `go install` builds include
  the dashboard.
- **Ingest token changes on every sync under Argo CD.** Renderers without
  cluster access cannot look up the chart's generated token. Set
  `server.ingestToken` or `server.existingSecret`
  ([GitOps](guides/gitops-argo-flux.md)).

## Still stuck

Open an issue with `upgradescope version`, the command you ran, and the
`NOT ASSESSED` section of the report:
[new issue](https://github.com/abd-ulbasit/upgradescope/issues/new/choose).
