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
| `versions (partial, required)` | A component pod in `kube-system` whose version upstream would have told was not read: the reason names the first pod and its image, an upstream-named image under a digest or a tag that is not a version (`kube-proxy@sha256:…`, `kube-scheduler:latest`), or a kube-apiserver, kube-controller-manager or kube-scheduler pod on a vendor image. | Pin the image to a version tag (`kube-proxy:v1.33.4`, `…:v1.33.4@sha256:…` reads too) so its skew is judged, or accept the gap with `--allow-incomplete`. A `versions (partial)` gap without `required` (a vendor kube-proxy image such as Oracle OKE's) leaves the verdict alone. |
| `addons (required)` | Pods could not be listed, and there was nothing else to match add-ons from: no Helm release was found and IngressClasses could not be read. | Grant `list` on pods. |
| `addons (partial, required)` | Pods could not be listed, but a Helm release or the IngressClasses could be read: add-ons were matched from what was read (the reason says which), so an EOL chart-installed add-on still blocks while one installed any other way is not seen. | Grant `list` on pods, or accept the gap with `--allow-incomplete`. |

`--allow-incomplete` (CLI), or `allow-incomplete: true` in the GitHub
Action, gates on findings alone; see [CI gate](getting-started/ci-gate.md).

## `deprecated-calls: apiserver /metrics forbidden`

Managed control planes (EKS, GKE, AKS) often deny `/metrics` regardless of
RBAC. The check is optional: the verdict does not depend on it, and stored
objects are still checked. Nothing to fix;
see [Managed clusters](guides/managed-clusters.md).

The same holds when `/metrics` does not answer: the request is given up
after `--request-timeout` (default 30s), the reason (`get /metrics: …`
naming a timeout, in one of the forms below) says so, and the verdict can
still be `ready`, judged on stored objects without the request signal.

## A check not assessed with a timeout or a step deadline

A live scan has 5 minutes. Each API request is given up after
`--request-timeout` (default 30s; `0` turns the per-request limit off), and
each of the five collector steps runs under its own share of the time left,
so a slow or stalled API server degrades only the step it stalled: that
capability is not assessed, and the other steps still run.

When a single request timed out, the reason names a timeout:
`Client.Timeout exceeded`, `request canceled` or
`context deadline exceeded`, alone or together. Which form appears varies
from run to run, because client-go's per-request context and the HTTP
client's own timeout race each other. When the step's share of the time ran
out, the reason ends with
`(step deadline: gave up after …, this step's share of the scan's time)`,
typically after `context deadline exceeded`. When the step is a required one
(`versions`, `addons`, `api-usage`) the verdict is `unknown`. Check the API
server's health and the network path to it, or raise `--request-timeout`
when single requests are slow but complete (a very large list page, a big
`/metrics`).

## `helm` not assessed: Secrets forbidden

The agent was installed with `rbac.helmSecrets=false`, or your own identity
cannot list Secrets. Helm chart findings are missing from the report;
add-ons are still found from images. The trade-off is in
[Security model and RBAC](operations/security-model-and-rbac.md#the-rbachelmsecrets-trade-off).

## `helm` partial: Argo CD or Flux present, no Helm releases

The reason reads "no Helm releases read, but Argo CD (or Flux) is present".
The cluster has no Helm release Secrets or ConfigMaps but shows the tool, so
chart `kubeVersion` and stored-manifest checks were not assessed for the
charts it deploys; they are not clean. Argo CD's `helm template` leaves no
release to read, so with `rbac.gitops.argocd` set every chart read from an
Application raises the same gap ("N Argo CD chart(s) read from Applications
leave no Helm release"), even on a cluster that has other Helm releases.
Add-ons are still found from images, and, with
`rbac.gitops.argocd` or `rbac.gitops.flux` set, from the charts the tool's
resources name. A reason that says "chart sources not read" with a
forbidden error means the tool is installed and the agent's role lacks that
grant. See
[GitOps with Argo CD and Flux](guides/gitops-argo-flux.md#charts-your-gitops-tool-deploys).

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
  from `crds/` on first install only; it is missing when the chart was
  installed with `--skip-crds`, the CRD was deleted, or the chart was
  upgraded from v0.1.x or a release candidate across the
  [API group move](operations/upgrade.md#the-api-group-moved). The error
  carries the fix: `kubectl apply -f` of the CRD manifest at the agent's
  release (`https://raw.githubusercontent.com/abd-ulbasit/upgradescope/v<version>/deploy/chart/crds/clusterreadinesses.upgradescope.basit.engineer.yaml`,
  or `crds/` from `helm pull --untar`). With `agent.manageCRD=true` the
  agent stops at startup with this error; with `false`, every tick fails
  with it.
- **`could not bring the ClusterReadiness CRD up to date`** (startup, WARN)
  **and `crdError` on tick lines.** The CRD exists but the agent could not
  check or upgrade its schema: a transient apiserver fault, an apiserver
  that did not answer within the startup check's 30 seconds, or a role
  without `patch` on the CRD. Every tick tries again until it succeeds;
  meanwhile `status.notAssessed` leads with a note, because an older schema
  makes the apiserver drop the status fields it lacks (conditions, for
  Argo CD and `kubectl wait`). A role that may not patch the CRD should run
  with `agent.manageCRD=false` and the CRD applied by hand.
- **The agent exits at once with `invalid --server-url`, `invalid
  --server-token`, `invalid --cluster-name` or `invalid
  --force-sync-every`.** These settings could never work, so they are
  refused before the agent touches the cluster: the server URL must be an
  `http://` or `https://` URL with a host (`fleet.example.com` alone is
  not); the token must have no whitespace or control character inside it
  (surrounding whitespace, such as the newline of a Secret made with
  `--from-file`, is trimmed from every source); `--cluster-name` may not
  be set to `""` (leave it out to use the cluster UID); and
  `--force-sync-every` must be positive (0 no longer means the 1h
  default). A `--force-sync-every` at or below `--interval` is not refused:
  it means an unchanged inventory is pushed on every tick, and the
  agent logs a warning with the period in effect, nine tenths of the
  interval, the shortest gap the tick jitter leaves between two ticks.
- **The agent exits at once with `invalid --pod-pass-every` or `invalid
  --pod-pass-max-age`.** `--pod-pass-every` is a number of ticks, at least
  1 (1 reads the pods outside `kube-system` on every tick, as before
  #228), and `--pod-pass-max-age` a positive duration; 0 does not mean the
  default (3 and 1h). The chart's `agent.podPassEvery` and
  `agent.podPassMaxAge` are refused at `helm install` for the same values.
- **The agent logs `pod-pass-max-age is too short for pod-pass-every`, and
  some or all ticks list every pod.** A `--pod-pass-max-age` that is
  not above 1.1 times `--interval` times (`--pod-pass-every` minus one),
  the `mustExceed` field of the warning, is not refused, but it cannot work
  as set. Ticks are spaced by the interval with a jitter of 10% either way
  (0.9 to 1.1 times the interval), and the sleep starts after the tick
  ends, so the spacing also holds the tick's run time. A pass is reused
  only while it is younger than the maximum age, and the last reuse after a
  pass (the one `--pod-pass-every` minus one ticks later) is about that
  many spacings old, up to 1.1 times the interval plus the run time each.
  At or below the interval the pass is too old by the next tick, so only a
  tick that the jitter brings early reuses it; between the interval and the
  floor the first reuses work and the last ones depend on the draw. The
  request savings of [the pod pass
  setting](operations/scale.md#the-pod-pass-every-nth-tick-228) are then
  partly or wholly not there. Raise `--pod-pass-max-age` above the
  `mustExceed` value, and leave room on top of it for the ticks' run
  time, which the floor cannot know and which is a few seconds each at
  2,001 nodes. Exceeding the floor is needed and, because of that run
  time, not quite enough: at `--interval 10m --pod-pass-every 3` the floor
  is 22 minutes, and 21 minutes fails on the second reuse whenever the
  two spacings and their run time come to 21 minutes or more; 25 minutes
  works unless the ticks take more than a minute and a half each (two
  spacings of up to 11 minutes and two run times must stay under 25). At
  a 30-minute interval the default hour is under the floor of 66 minutes,
  so the last of the two reuses is lost on the ticks the jitter spaces
  widely. Instead of raising the maximum age, lower `--pod-pass-every`
  (`agent.podPassEvery`) until its floor, `--interval` times 1.1 times
  (`--pod-pass-every` minus one), fits under the maximum age: the warning's
  `podPassEveryThatFits` field is the largest value whose floor alone is
  under it. That field leaves out the ticks' run time, which comes on top,
  so on slow ticks you may need one less (the warning text says so too).
  At the 30-minute interval and the default hour that is 2 (floor 33
  minutes). The price is a full pass every other tick instead of every
  third (a cycle of 2 averages 25.5 requests at the 2,001-node fill,
  computed, against 23.7 for 3), and an add-on is then behind for at most
  one tick (up to about 33 minutes) instead of two. Or set `--pod-pass-every=1`
  (`agent.podPassEvery=1`) if reading every pod on every tick is what you
  want, which also ends the warning.
- **An add-on I just installed or upgraded is missing, or still shows its
  old version.** The agent lists the pods outside `kube-system` only every
  `--pod-pass-every` ticks (3 by default, so about every 30 minutes at the
  default interval), or sooner when the last full pass is
  `--pod-pass-max-age` old (1h), and detects add-ons from that pass's
  images and labels in between. An add-on changed right after a pass is
  reported as it was for at most `--pod-pass-every` minus one ticks, and a
  pass that is `--pod-pass-max-age` old or more is not reused (the age is
  measured at the tick that reuses it, and that tick's report stays up until
  the next one). `status.addOnEvidenceAgeSeconds`
  (also `addOnEvidenceAgeSeconds` in the report and the
  `upgradescope_addon_evidence_age_seconds` gauge) says how old the evidence
  of the last tick was: absent or 0 means every pod was read. Wait for the
  next full pass, or set `agent.podPassEvery=1` to read the pods on every
  tick, at the request count [Scale and cost](operations/scale.md) gives
  for it. The `kube-system` pods, Helm releases, Argo CD and Flux charts
  and IngressClasses are read on every tick, and a change in the Helm
  releases or chart references that name an add-on (a new release or
  revision, a new chart reference) makes the next tick list every pod. So
  an add-on in `kube-system`, or installed or upgraded through Helm or a
  GitOps chart reference, is not behind; one installed another way, or whose
  image was changed in place (`kubectl set image`), is, for the ticks above.
  `scan` reads every pod.
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
- **The ClusterReadiness has an `upgradescope.basit.engineer/status-error`
  annotation.** The agent could not write the status, at the time and for the
  reason the annotation gives, so the verdict shown is the last one it could
  write: stale. The usual cause is a role without `update` on
  `clusterreadinesses/status` (a hand-written role, `rbac.create=false`);
  it is also set when the agent could not read the object's spec or set
  its `spec.targets`, since a status for targets it did not read is not
  written. The next successful write removes the annotation. If the role also lacks
  `patch` on the object itself, there is no annotation, and the signals are
  `/readyz`, the agent's logs (`tick failed`) and the alert above.

## The server

- **`refusing to serve the read API and /api/v1/gate without a read token`.**
  `serve` bound a non-loopback address with no read credential. Set
  `--read-token`, mint a read token
  (`upgradescope tokens create --read --teams '*'`), trust an
  authenticating proxy's team header, listen on loopback, or pass
  `--allow-anonymous-read` when something in front of the server
  authenticates ([Read access](operations/auth.md)). To keep the API closed
  even after the database is lost, `--require-read-credential` starts
  without a credential and answers `401` until one is minted.
- **`401` from an open server, for a token that "worked before".** An open
  read API now refuses a bearer it does not know instead of reading the
  whole fleet for it. Send no `Authorization` header, or a token the server
  holds (`tokens list --read`). **`421` from `serve` for a name that worked.**
  An open read API answers an anonymous request only for `localhost`, a
  loopback address, the address it arrived on and the `--allowed-host`
  names ([the Host check](operations/auth.md#the-host-check-dns-rebinding)).
- **A scoped read token gets 404 for a cluster that exists, or 403 on
  `/metrics`.** The cluster has no namespace of the token's teams in its
  latest evaluated snapshot, and `/metrics` takes a fleet-wide credential
  ([what a team-scoped read sees](operations/auth.md#what-a-team-scoped-read-sees)).
- **A push gets 409: `cluster name ... is registered to clusterId ...`.**
  Another cluster already uses this `--cluster-name`, or the cluster was
  rebuilt. Give the agent a distinct name, or delete the old record with
  `upgradescope clusters delete <name> --server ...` (admin token); the
  message spells out both.
- **A push gets 401 or 403.** 401: the token is missing or unknown. 403: a
  per-cluster token pushing as another cluster name.
- **A push or `/gate` gets 413 `... is too large to evaluate ...`.** The
  body is within its byte cap but holds more JSON values or YAML nodes
  than one request may decode (YAML aliases count at what they expand
  to). Split the manifest stream, or a large List, into several requests;
  for a push, check what the agent collects
  ([Memory and request limits](operations.md#memory-and-request-limits)).
- **A push or `/gate` gets 408 or 503.** 408: the body did not arrive
  within the server's read timeout (60s); 503 with `Retry-After`: other
  requests hold the shared body budget or the decode slot, or, for
  `/gate`, responses still waiting for their clients leave no room for
  this answer. The agent retries both; a CI caller should retry too.
- **A cluster's page, report, findings, teams, history or export, or the
  fleet teams rollup, gets 503.** Reads that load a stored snapshot run
  one at a time, and this one waited more than 30s for its turn (other
  reads, usually what-if reports of a large cluster, held it), or the
  responses still waiting for slow clients leave no room for this one.
  Retry after `Retry-After` seconds
  ([Memory and request limits](operations.md#memory-and-request-limits)).
- **The cluster list, the fleet matrix or a `/metrics` scrape gets 503.**
  Reads of the whole fleet run two at a time, and this one waited more
  than 30s for its turn (5s for a scrape), or the responses still waiting
  for slow clients leave no room for this one. The dashboard and Prometheus try again at
  their next poll or scrape; if it persists, look for clients that ask
  for large reports or fleet reads and do not read them (the server's
  `upgradescope_http_requests_total` by route and code shows the 503s).
- **`/gate` gets 422 `a UTF-16 byte order mark`.** Re-encode the manifests
  as UTF-8 (`iconv -f UTF-16 -t UTF-8`).
- **`/gate` gets 413 `the answer to this stream could take up to`.** The
  answer would list more objects, with longer names, namespaces and
  `?path=`, than `--max-gate-bytes` allows (with `?cluster=`, the
  cluster's own findings count too). Split the stream into several
  requests, or raise `--max-gate-bytes`, which raises the body cap too.
- **`/fleet` gets 422 `targets lists more than 16 distinct minors`.** Ask
  for at most 16 targets at a time.
- **The agent warns `pushing snapshots to ... over plain http`.** Its
  `--server-url` is `http://` to a host that is not loopback. Serve HTTPS
  (`--tls-cert-file`, the chart's `server.tls`, or an Ingress).
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
