# Changelog

All notable changes to upgradescope are documented here.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and the project follows [Semantic Versioning](https://semver.org/spec/v2.0.0.html).
Before 1.0, a minor release may change flags, API responses or the CRD schema.
Any such change is listed under **Changed**.

The embedded knowledge base (API lifecycle data and the add-on registry) ships
inside the binary and the image. Knowledge-base updates therefore reach you
only through a new release.

## [Unreleased]

The first release built, signed and published by CI. It changes a lot
of scanning behaviour and gating, so read **Changed** before you upgrade
a CI gate.

### Added

- `GET /api/v1/fleet/teams` carries each team's `verdict`: the worst of its
  verdicts in the clusters evaluated, `blocked` over `unknown` over `ready`.
  A team with a clean score can still be `blocked` by a blocker no team
  owns, or `unknown` because a required check did not run, so read the
  verdict, not the score. The response also has a required `excluded` array,
  one `{name, clusterId, reason}` for each cluster left out of the rollup,
  with `reason` `no-snapshot`, `too-large` or `unreadable` (`missing` still
  lists the same names). Score history points (`ScorePoint`, `GET
  /api/v1/clusters/{id}/history`) gain an optional `verdict`, absent from
  servers that predate it. The Teams page and the score sparkline colour by
  verdict, so a blocked or unknown evaluation is never drawn green, and the
  score badge is red for any blocked verdict (#243).
- A finding of an API usage check carries `callers` (optional, in
  `api/report.schema.json` and the server's responses): the
  `apiserver_requested_deprecated_apis` rows folded into it, in row order,
  each with `group`, `version`, `resource`, `subresource`, `key`,
  `severity`, `title` and `detail` of the `deprecated-api-in-use` finding it
  is on its own (#236).
- Chart: with `server.replicas` above 1 the chart renders a
  PodDisruptionBudget for the server (`server.podDisruptionBudget`,
  `enabled: true`; `minAvailable: 1`, or `maxUnavailable`, which replaces
  it; the render fails when both are empty) and spreads the server pods
  across nodes with a soft `kubernetes.io/hostname` topology spread
  (`server.defaultTopologySpread: true`; your own
  `server.topologySpreadConstraints` replace it). With one replica no budget
  is rendered, since `minAvailable: 1` would block every node drain (#242).
- `serve --allowed-host <name>` (repeatable or comma separated;
  `$UPGRADESCOPE_ALLOWED_HOSTS`): a host name or IP that requests may name
  in their Host header, at any port. A port, scheme or wildcard is refused
  at startup. Chart: `server.allowedHosts` (default `[]`) adds names to the
  ones the chart always passes (the server Service's DNS names and
  `server.ingress.host`); a port or scheme fails the render. Chart:
  `clusterDomain` (default `cluster.local`; a DNS name, a trailing dot is
  dropped) is the domain of the server Service's fully qualified name, which
  the chart passes to `--allowed-host` and names in the cert-manager
  Certificate; the Certificate always named `.svc.cluster.local`, so set it
  when your cluster uses another domain, or a client using the full name
  gets `421` and a certificate without that name. What the option is for is
  under **Changed** (#240).
- Team-scoped read tokens: `upgradescope tokens create --read --teams
  <team>` (repeatable, a team name taken as written; `--teams '*'` for the
  whole fleet), `tokens list --read` and `tokens revoke --read --id <n>`. A
  read token is printed once; the database keeps its sha256 hash and first 8
  characters, and the server looks it up on every request, so one minted or
  revoked takes effect without a restart. A team-scoped token reads only the
  clusters one of its teams owns a namespace in (as the cluster's current
  evaluations attribute namespaces, after `--team-map`), and of those only
  its teams' findings and suppressed findings (one spanning teams cut to
  theirs) and team scores; the cluster's score, verdict and counts stay the
  whole cluster's. Any other cluster answers `404`, as an unknown one does,
  and is left out of `/clusters`, `/fleet` and `/fleet/teams`. `/metrics`
  answers `403` to a scoped credential. A scoped `POST
  /api/v1/gate?cluster=` judges the pull request on the scope's share of the
  cluster. The dashboard says which teams it shows. A scoped answer names
  its teams in `X-Upgradescope-Teams` and is sent `Cache-Control: private,
  no-store`. See `docs/operations/auth.md` (#72).
- `serve --trust-team-header <header>`, which needs `--trusted-proxy-cidr
  <cidr>`, scopes a read from a TCP peer in those ranges (never
  `X-Forwarded-For`) to the comma-separated, percent-encoded teams the
  header lists; `*` there is never the whole fleet. It is safe only when
  nothing but the proxy can reach the server from those addresses (on
  loopback, mind `kubectl port-forward` and mesh sidecars) and the proxy
  strips client-supplied copies of the header:
  `deploy/examples/oauth2-proxy/` runs oauth2-proxy v7.15.5 as such a proxy,
  in a sidecar. The chart has no values for this mode yet (#72).
- `upgradescope mcp`, a read-only Model Context Protocol server for AI
  assistants. It speaks MCP on stdio; `--http ADDR` serves streamable HTTP
  at `http://ADDR/mcp` instead (a bare port binds 127.0.0.1, an address that
  is not loopback needs `--allow-remote`, and a request from another origin
  in a browser is refused). Without `--http-token`
  (`$UPGRADESCOPE_MCP_HTTP_TOKEN`, `--http-token-file`) the HTTP endpoint
  has no authentication; with it, a request without the token gets 401.
  Tools: `scan` reads the cluster `--kubeconfig` and `--context` name (the
  current context is fixed when the server starts), and its only input is 1
  to 4 `targets`; `list_findings` and `get_report` read the latest scan, a
  `report_file` (at most 8 MiB), an `inventory_file` (at most 20 MiB,
  refused where ingest would refuse it) or, with `--server-url`, a fleet
  `cluster`; `registry_lookup` searches the embedded registry; with
  `--server-url`, `fleet_summary` returns the fleet matrix, using the read
  token from `--read-token-file`, `$UPGRADESCOPE_READ_TOKEN` or
  `--read-token`. Results follow `api/report.schema.json`. No
  `.upgradescope.yaml` is read; `upgradescope.dev/ignore` annotations still
  apply. A refused `report_file` or `inventory_file` is answered with the
  field and the rule broken, quoting nothing of the file except an unknown
  `collectorSchema` number. The binary grows by about 2 MiB (2.00 MiB on
  linux/amd64, stripped, measured at `67f30be`) (#76).
- A `support-lifecycle` finding for clusters on EKS, GKE and AKS, from
  provider calendars in `registry/data/providers/` (EKS and AKS synced from
  endoflife.date, GKE curated from Google's release schedule): a warning
  when standard support for the cluster's minor ends within 90 days, and a
  blocker from the day it ends, through extended support and after, keyed
  `support-lifecycle/<provider>/<minor>` and judged whatever the target.
  Where a list price is cited (EKS, and GKE's Extended channel) it states
  what extended support adds per cluster per year, `(extended − standard) ×
  8760` cluster-hours ($4,380 at $0.60 against $0.10), with the price's
  as-of date; AKS gets dates only. On GKE and AKS, where extended support is
  opt-in, the wording is conditional. The inventory gains `provider` (`eks`,
  `gke`, `aks` or `other`), set from server-version suffixes and managed
  node-pool labels; a `providerID` scheme alone never claims a provider.
  Ingest accepts a `provider` this build does not know, but refuses (422)
  one that is not a label value: longer than 63 bytes, with characters
  outside letters, digits, `-`, `_` and `.`, or not starting and ending with
  a letter or digit. `scan` prints a `Support:` line, the JSON report and
  the server's report carry `support`, and the `ClusterReadiness` status
  gains `supportPhase`, `extendedSupportFrom`, `extendedSupportEnds`,
  `annualCostDelta`, `currency`, `priceAsOf`, `annualCostNote` and
  `extendedSupportCondition` (#77).
- Add-ons that Argo CD and Flux deploy are found from the charts those tools
  declare: each Argo CD Application source that sets `chart`, for
  Applications whose destination is this cluster, and each Flux
  HelmRelease's `spec.chart.spec` or `chartRef` to an OCIRepository (the
  newest of `v2`, `v2beta2` and `v2beta1` served). A chart the registry
  knows is an install in the namespace it deploys into (add-on `source:
  gitops` when nothing stronger found it), so an Ingress NGINX that Argo CD
  renders with `helm template` is an `eol-addon` blocker without a Helm
  release. The inventory lists the references as `gitopsCharts`. Reading
  them needs the new chart values `rbac.gitops.argocd` (get, list on
  Applications) and `rbac.gitops.flux` (get, list on HelmReleases, get on
  OCIRepositories), both `false` by default. Ingest refuses (422) a
  `gitopsCharts` entry whose `name` is not an RFC 1123 subdomain, or whose
  `namespace` or `target` is not a namespace name. See the GitOps guide for
  what is and is not read (#70).
- `agent --server-ca-file`: a PEM bundle of CA certificates the agent trusts
  for pushes, on top of the system roots, for a server behind a private CA;
  read at startup. It needs an `https` `--server-url`, and a missing file, a
  file with no certificate, or a certificate that does not parse stops the
  agent at startup. Chart: `agent.serverCA.configMap` or
  `agent.serverCA.secret`, with `agent.serverCA.key` (default `ca.crt`),
  mounts the bundle read-only and passes it as `--server-ca-file`; set, it
  replaces `server.tls.caKey` for the in-chart server. The render fails when
  both sources are set, the key is empty, nothing is pushed, or the push URL
  is not `https` (#65).
- `serve --tls-cert-file` re-reads the certificate and key when either file
  changes, checked at most once a second on a new handshake, so a renewed
  certificate (cert-manager rewriting a mounted Secret) needs no restart. A
  pair that does not load is logged and the previous certificate stays in
  service. TLS 1.2 is the minimum. The README's quickstart (the Fleet
  paragraph) and *Exposing the server to remote agents* in the fleet guide
  say how to serve over TLS, and that plain `http` sends the ingest token
  and the inventories in the clear (#65).
- `POST /api/v1/gate?allow-incomplete=true`, as `scan --allow-incomplete`:
  an `unknown` verdict with no finding at the `fail-on` threshold answers
  200. A blocker, a warning under `fail-on=warning`, and a target that is
  not an upgrade of the cluster still answer 422, and the verdict header and
  body still say `unknown`. Only `true` or `false`, given once, is accepted;
  any other value is a 422. Without the parameter nothing changes (#177).
- Registry entries for Kubernetes Dashboard (archived 2026-01-21), Promtail
  (end of life 2026-03-02), Grafana Agent (2025-11-01) and Weave Net
  (archived 2024-06-20), each retired as a whole and so an `eol-addon`
  blocker at any version, and for Karpenter, Gatekeeper and Fluent Bit,
  synced from endoflife.date: 27 add-ons in all. Bitnami's
  `bitnamilegacy/nginx-ingress-controller` is matched as Ingress NGINX
  (#49).
- `--registry-dir <file-or-directory>` on `scan`, `agent` and `serve` loads
  more registry entries (`<id>.yaml`, in the schema of
  `registry/CONTRIBUTING.md`), validated like the embedded ones, citations
  included. An invalid file, a path with no `*.yaml` entry, or an entry that
  claims an image or chart another entry claims stops the command at start,
  naming the file or both entries. An entry with an embedded `id` replaces
  it. The entries are part of the knowledge base version label. `serve`
  judges stored inventories and `/gate` manifests against its own registry,
  so give it the agents' entries; `GET /api/v1/registry` lists the embedded
  registry only. An image matcher of two or more segments matches as a path
  suffix on whole segments; a one-segment matcher is that repository
  exactly, and `"*/name"` matches the name under any registry prefix (only
  for distinctive names; only etcd uses it, so etcd is still found behind a
  mirror). Chart: `agent.extraRegistry` maps `<id>.yaml` file names to
  entries, rendered into a ConfigMap mounted as the volume `extra-registry`
  (a name to avoid in `agent.extraVolumes`); the server has no such value,
  so use `server.extraArgs` and `server.extraVolumes` (#49).
- Example policies over `ClusterReadiness` in `examples/`: a Kyverno
  ClusterPolicy and a Gatekeeper template and constraint that report a
  workload annotated `upgradescope.dev/upgrade-target: "<minor>"` when the
  object (`cluster` by default) has `ready: false` for that minor. They only
  audit or warn by default, and allow the request when `lastEvaluated` is
  more than an hour old or missing, or when there is no object or no entry
  for the minor; each ships a read-only reader ClusterRole. A static
  Renovate preset (`examples/renovate/upgradescope.json`) groups minor and
  patch bumps of the registry's charts, holds major bumps for approval and
  prioritizes the end-of-life ones. CI checks them without a cluster, with
  pinned, checksum-verified Kyverno CLI and gator and Renovate's validator;
  live admission is not tested. The *Acting on readiness* guide describes
  them (#79).
- The inventory lists kube-proxy once per node it runs on, with the node
  (`controlPlane[].node`, from `spec.nodeName`), and a
  `version-skew/kube-proxy-kubelet/<node>` warning names a node whose
  kube-proxy is more than 3 minors (2 for a kube-proxy older than 1.25)
  older or newer than its kubelet, citing the version skew policy. It does
  not depend on the target. A kube-proxy with no node, or on a node that is
  not listed, is not paired. Ingest refuses (422) a `controlPlane[].node`
  that is not an RFC 1123 subdomain (#148).
- `make bench-agent KUBECONFIG=<lab kubeconfig>` measures the agent's tick
  on a disposable lab cluster it fills with 2,000 KWOK nodes (2,001 with the
  control plane), about 14,000 pods and 1,000 Helm release Secrets (requests
  by verb and resource, bytes, wall time, CPU, peak heap and RSS); it
  refuses a kubeconfig taken only from the environment and a cluster with
  more than 3 real nodes. `make bench-ingest` measures `serve` ingesting 200
  clusters with three targets each, on SQLite and on a throwaway Postgres.
  The results, hardware and what is simulated are in
  `docs/operations/scale.md`: there, all 200 pushing at once, one `serve`
  took 56 new snapshots a second on SQLite and 40 on Postgres 17, on a
  dual-core i3-7100U that also ran the database (#71).
- Each team in a report and on the dashboard has a `verdict` (`blocked`,
  `unknown` or `ready`). A team is `blocked` by a blocker of its own or by
  one no team is attributed, which cannot be ruled out as its own;
  otherwise `unknown` when the report has any required gap, which may hide
  a blocker of any team; otherwise `ready`. Another team's blocker leaves
  it `ready` (#196).
- Inventories carry `collectorSchema`, the collector's version of the
  inventory contract. An inventory that carries it is judged as current;
  one without it is judged as a v0.1.x inventory unless its `agentVersion`
  is a semantic version at or after 0.2.0-0, so `dev`, `unknown` and an
  empty version count as v0.1.x (#194).
- The agent marks its `ClusterReadiness` with the
  `upgradescope.dev/status-error` annotation while the status cannot be
  written, and clears it on the next successful write (#199).
- Documentation site (MkDocs Material, published to GitHub Pages) with
  getting-started guides, concepts, operations pages, and CLI, Helm values,
  CRD, REST API, metrics and configuration references generated from the
  code and checked for freshness in CI.
- Published contracts: `api/openapi.yaml` (OpenAPI 3.1 for every server
  route, tested against the real handlers), `api/report.schema.json` (the
  JSON report) and `api/webhook.schema.json` (webhook payloads), plus a
  compatibility policy (`docs/compatibility-policy.md`) that says which
  changes a version may make. Each release attaches the three as assets
  (`report.schema.json`, `webhook.schema.json`, `openapi.yaml`), listed in
  its signed `checksums.txt` (#60).
- Deleted built-in APIs are in the knowledge base. `tools/gen-kb` reads
  every `k8s.io/api` release since v0.17 and records each type a later
  release dropped, removed at the release that stopped serving it. These
  include the DRA v1alpha1 to v1alpha3 kinds, ClusterCIDR, ServiceCIDR and
  IPAddress v1alpha1, LeaseCandidate v1alpha1, and older deletions such as
  `batch/v2alpha1` CronJob and `settings` PodPreset. Manifests and callers
  that use them now block instead of passing silently (#124).
- An `unknown-api` info finding: an object or API call at a version of a
  built-in group that the knowledge base does not know. CRD groups are
  not affected.
- A readiness verdict: `ready`, `blocked` or `unknown`. The verdict is
  shown in every output format, in the `ClusterReadiness` status and as
  its `READY` column. `unknown` means that no blocker was found but a
  required check could not run, so a blocker may have been missed.
- Suppression with an auditable reason. Ignore rules in
  `.upgradescope.yaml` (`scan --config`), the `upgradescope.dev/ignore`
  and `upgradescope.dev/ignore-reason` object annotations, and
  `ClusterReadiness` `spec.ignore` accept findings. Rules can match by key
  or category and by namespace, name or file globs, and can expire.
- Baselines: `scan --baseline` fails the gate only on findings that are new
  since an earlier JSON report, and `--write-baseline` writes one.
- `scan --output markdown`, for CI step summaries.
- The table output lists the affected objects, the fix and its citations.
  The JSON report carries `schemaVersion`, `toolVersion` and, for
  `--files` scans, `filesBase`.
- Helm charts are judged by their `kubeVersion` constraint and by the APIs
  in the stored release manifests. Releases stored by Helm's configmaps
  driver are read too.
- Node container runtimes (containerd) are judged against the registry.
  A runtime the registry does not cover (cri-o, docker) is an
  `addon-no-data/<runtime>` info finding naming its nodes, instead of
  nothing (#169).
- Add-on end of life is judged per release line (Istio 1.24, not "Istio"),
  keyed on the installed app version. Registry schema v2 adds
  `tools/eol-sync`-generated cycles, path-suffix image matchers and Helm
  `appVersion` matching.
- Add-ons are also detected from `helm.sh/chart` and `app.kubernetes.io/*`
  pod labels (the version from `app.kubernetes.io/version`) and from an
  `IngressClass` whose controller is `k8s.io/ingress-nginx`, so Ingress
  NGINX installs that run mirrored or rebuilt images are found (#18).
- `scan --files`, and `/gate` without `?cluster=`, detect add-ons from the
  container and init-container images and labels of workload pod templates
  (Pod, Deployment, DaemonSet, StatefulSet, ReplicaSet, Job, CronJob), so a
  CI gate catches an end-of-life add-on in a pull request (#47). Images
  injected at admission time are not in the manifests.
- `/api/v1/gate?cluster=` merges the add-ons and CRDs in the posted
  manifests into the cluster's stored inventory, as it merged their API
  usage. An add-on install replaces the cluster's install in its
  namespace; a posted CRD replaces the cluster's (keeping its
  `status.storedVersions`), and posted custom resources are judged against
  the merged CRDs. An end-of-life add-on the manifests deploy, and a custom
  resource they write at a version the CRDs do not serve, are the
  manifests' findings (`source: manifest`), also when the cluster already
  has the same (#150).
- `/api/v1/gate` applies suppression as `scan` does, with the same code:
  the `upgradescope.dev/ignore` annotations of the posted (and, with
  `?cluster=`, the stored) objects, and the ignore rules of a
  `.upgradescope.yaml` sent URL-encoded in the new `config` query
  parameter (at most 32 KiB; an invalid config is a 422). Suppressed
  findings count toward neither the verdict nor `fail-on`; the JSON
  answer lists them in `suppressed` with `suppressedCount` and names
  expired rules and annotations without a reason in `warnings`; SARIF marks them suppressed, JUnit skips
  them and Code Quality leaves them out. The gate has no baseline input:
  with `?cluster=` the cluster is its baseline (#44).
- The server's report-shaped responses, a cluster's report and the gate's
  JSON answer, lead with `schemaVersion` and `toolVersion` like
  `scan --output json` (#60).
- The report lists the image repositories no add-on matcher recognised
  (`unrecognizedImages`, at most 200, and `unrecognizedImagesOmitted`) in
  JSON, the table, Markdown and the dashboard's cluster view (not in
  SARIF). They are a gap in add-on detection, not findings, and change
  neither score nor verdict.
- The knowledge base covers Kubernetes 1.37 (`k8s.io/api` v0.37.1), and the
  weekly refresh regenerates it without hand edits.
- Agent: `--targets` reconciles `spec.targets`; `--manage-crd` controls
  whether the agent touches the CRD; `/healthz`, `/readyz` and Prometheus
  `/metrics`; a `Ready` condition, `observedGeneration` and a
  `LastEvaluated` column on `ClusterReadiness`.
- Server: Prometheus `/metrics` and `/readyz`; HTTPS with `--tls-cert-file`
  and `--tls-key-file`; `/api/v1/gate` takes `fail-on`, returns a verdict
  header and accepts a cluster baseline; every secret flag has an
  environment-variable and a `-file` form; `tokens list`,
  `tokens revoke --id`, and `clusters delete`.
- Chart: a values schema, production settings (resources, probes, security
  contexts that work on OpenShift, pull secrets, scheduling), a
  ServiceMonitor, a PrometheusRule and a Grafana dashboard. The ingest
  token is generated, and every secret reaches the pods through
  `secretKeyRef`.
- GitHub Action: a root `action.yml` for the Marketplace. Inputs are passed
  through the environment, the install is checksum-verified, and the
  Action writes outputs (verdict, score, counts, report paths) and a step
  summary. It takes `config`, `baseline` and `write-baseline` inputs, and
  `allow-incomplete` (`scan --allow-incomplete`), so a gate whose target
  is past the knowledge base's horizon can fail on findings alone instead
  of on the `unknown` verdict (#130).
- `upgradescope version` (and `--version`) prints the commit, the build
  date, the Go version, the knowledge base version and horizon, and the
  registry date. `--output json` prints the same as JSON.
- Every command's `--help` has examples. A mistyped flag points to
  `--help`.
- Packaging: deb, rpm and apk packages. Bash, zsh, fish and PowerShell
  completions and man pages in every archive and package. A Homebrew tap
  (`brew install abd-ulbasit/tap/upgradescope`), a krew plugin manifest,
  and Artifact Hub metadata for the chart.
- `LICENSE`, `NOTICE` and a generated `THIRD_PARTY_NOTICES` file (Go
  modules and the dashboard's npm packages) ship in every archive and
  package, and in the image under `/licenses/`. A CI license check fails
  on a dependency with an unknown or disallowed license.
- `NOTICE` and `registry/DATA-LICENSE.md` give endoflife.date and the
  Kubernetes sources their attribution. The repository adds CONTRIBUTING,
  SECURITY, an architecture guide and an observability guide.
- `--request-timeout` for `scan` and `agent` (default 30s; 0 = no
  per-request limit): client-go gives up on a single API request after it.
- A live scan's report names the kube context and the API server it read:
  JSON `kubeContext` and `apiServer` (scheme, host and port only, never
  credentials), a `Context:` line in the table header, and `Context ... ·
  API server ...` in the markdown summary.
- CRD versions are judged (#48). A new `crds` capability (the inventory's
  `crds` field) records each CustomResourceDefinition's versions (served,
  storage, deprecated and the deprecation warning) and its
  `status.storedVersions`. A new `crd-version` finding category has three
  keys:
  - `unserved`: a blocker for custom resources at a version the CRD does
    not serve (or, in files mode, does not list). It can make the verdict
    `blocked`.
  - `deprecated`: a warning, which quotes the CRD's `deprecationWarning`,
    when custom resources use a version the CRD deprecates. It is info when
    nothing uses that version.
  - `stored-unserved`: a warning when `status.storedVersions` lists a
    version the CRD no longer serves. Its remediation describes the storage
    version migration.

  Live scans read CRDs through the apiextensions client. They list custom
  resources metadata-only, at a served version that is not deprecated, and
  judge them by managedFields authorship. `--files` scans take the served
  versions from the CRD manifests in the files. A custom resource whose CRD
  is not in the files makes `crds` partial. The agent's chart role can read
  CRDs but no custom resources, so the agent reports `crds` as partial for a
  CRD with a deprecated or unserved version. These findings concern the
  add-on's CRDs, whatever the Kubernetes target.
- `scan --output junit`: one test suite per finding category and one test
  case per finding. The outcomes follow the gate (`--fail-on`,
  `--allow-incomplete`, `--baseline`): a finding that fails the gate is a
  failure, one below the threshold passes, a suppressed or
  baseline-unchanged one is skipped with its reason, and a required check
  that could not run is an error, so an `unknown` verdict fails the test
  report too (#73).
- `scan --output gitlab-codequality`: a GitLab Code Quality report for the
  merge request widget. Severity maps blocker to critical, warning to
  minor and info to info; each entry is anchored to the object's file and
  line, and fingerprints hash the finding key and object identity (not
  the line), so they are stable across runs. Findings without a file are
  anchored to the virtual path `upgradescope/<key>`; suppressed findings
  are left out (#73).
- `/api/v1/gate?format=junit|gitlab-codequality`, with the same status
  code and verdict header as `json` and `sarif` (#73).
- CI templates for GitLab CI (`ci/gitlab/upgradescope.gitlab-ci.yml`, Code
  Quality and JUnit reports), Jenkins (`ci/jenkins/Jenkinsfile`, the
  `junit` step) and Azure Pipelines (`ci/azure/azure-pipelines.yml`,
  `PublishTestResults@2`). Each installs a pinned, checksum-verified
  release and keeps the report when the gate fails (#73).
- `scan --plan`: an upgrade plan for a multi-minor upgrade. The control
  plane is upgraded one minor at a time, so `--plan` judges the cluster at
  every minor from the one it runs (the oldest kube-apiserver) to
  `--target`, and lists each finding in full at the first upgrade it
  affects, with the upgrade where its severity changes (a warning at 1.33
  that blocks at 1.34 shows both). The table and markdown outputs show a
  per-upgrade section before the findings; the JSON report gains `hops[]`,
  an addition within `schemaVersion` 1 that the server does not serve.
  With `--files`, `--from` gives the minor the cluster runs now. Ignore
  rules and annotations apply to every upgrade. The verdict, score and
  `--fail-on` gate stay those of `--target`. SARIF, JUnit and Code Quality
  have no place for upgrade steps, so `--plan` with those outputs is an
  error. Hops come
  from the knowledge base's upgrade steps, one minor each by default; a
  step that skips minors needs a citation, and none ship (#78).
- Kubernetes 1.24 to 1.28, which kind has no node images for, are tested
  against a real kube-apiserver and etcd (envtest) in CI: GA-only objects
  give no removed-API finding or blocker, a second scan of an unchanged
  cluster is identical, and an object written through a beta API the
  minor still serves blocks at its removal minor. Only the live collector
  and engine are tested there (no nodes, agent or chart); the tested range
  is on the compatibility page. `make envtest` runs it locally (#135, #69).
- Chart: HTTPS on the server's own port (`server.tls`), from an existing
  certificate Secret or a cert-manager `Certificate` issued into
  `<fullname>-server-https`. The in-chart agent then pushes over HTTPS and
  trusts the Secret's CA (`server.tls.caKey`, empty for a publicly trusted
  certificate); probes and the ServiceMonitor use HTTPS, and the chart's
  Ingress is told the backend speaks HTTPS.
- `GOMEMLIMIT` is set when unset: the chart sets 90% of each container's
  memory limit, and `serve` and `agent` take 90% of the cgroup's memory
  limit (v2 or v1) outside the chart. An explicit value wins.
- Chart: `server.sharedIngestToken=false` drops the shared, any-cluster
  ingest token, so only per-cluster tokens can push.

### Changed

- Chart: a release that already runs `server.replicas` above 1 (which needs
  Postgres) gets the PodDisruptionBudget and the soft spread of the server
  pods across nodes on `helm upgrade`, as both are on by default, so a node
  drain no longer takes the last server pod. To keep the release as it was,
  set `server.podDisruptionBudget.enabled=false` and
  `server.defaultTopologySpread=false` (#242).
- `serve` answers `421` to a request whose Host it does not answer for,
  before any route, credential, team header or scope is looked at, when it
  listens on a loopback address or `--trust-team-header` is set (a routable
  listener without a trusted header answers any Host, as before). It answers
  for `localhost`, a loopback address, the address the request arrived on
  (probes and scrapes of the pod IP), the `--listen` host (not `0.0.0.0` or
  `::`) and every `--allowed-host`; the port is never compared. This closes
  DNS rebinding: a page on a name an attacker controls, resolved to the
  loopback address, could read an open loopback read API, or a server in
  header mode through `kubectl port-forward`. If clients reach such a server
  under another name (an `/etc/hosts` alias, a LoadBalancer IP, a Gateway
  host), add it with `--allowed-host` or `server.allowedHosts`. In header
  mode, a proxy that forwards the client's Host (oauth2-proxy's default, and
  the earlier example manifest's) now gets `421` for every browser request:
  set `--pass-host-header=false`, as the example now does, or add that host
  with `--allowed-host`. The chart always passes `--allowed-host` (the
  Service's DNS names and `server.ingress.host`), so an older server image
  with this chart stops on an unknown flag: upgrade the image with the
  chart. Refusals are counted in
  `upgradescope_http_requests_total{route="host-refused",code="421"}` and
  logged at most once a minute (#240).
- `serve` refuses to start unless `--slack-webhook` and `--webhook` are
  absolute `http(s)` URLs with a host, naming the flag and never the value;
  before, a bad URL started and then dropped every notification. `clusters
  list`, `clusters delete` and `clusters rename` with `--server` no longer
  follow a redirect: any 3xx is an error naming its status and `Location`,
  so an http to https `301` or `302` cannot turn a delete or rename into a
  GET reported as done, and `clusters list` behind such a redirect, which
  used to work, now fails. Before you upgrade, fix or remove a webhook URL
  that is not an absolute `http(s)` URL with a host, and pass the URL the
  redirect names (for example `https://`) as `--server` (#240).
- `serve` refuses to start when `--read-token` equals `--ingest-token`
  (every agent's push token would read the whole fleet). Every command's
  secret read from its `$UPGRADESCOPE_*` variable (`serve`'s tokens, webhook
  URLs, webhook secret and `--db-url`, the agent's server token, `clusters`,
  `mcp`, `tokens --db-url`) is trimmed of surrounding whitespace, as one
  read from `--<name>-file` already was, and a variable that holds only
  whitespace is now an error. The trusted team header trims only space and
  tab from each team name it lists, no other whitespace (#250).
- `unattributed`, the key of the findings no team owns, is now
  `(unattributed)` in the `teams` of `scan -o json`, the report, `GET
  /api/v1/clusters/{id}/teams`, the gate response, `GET /api/v1/fleet/teams`
  and the HTML export (`schemaVersion` stays 1), and in the `scan` table and
  the dashboard's team filter (`team=(unattributed)` in the hash), because a
  team called `unattributed` shared its row. That team is now its own row,
  and `serve --team-map` refuses a team named `(unattributed)`. Update
  anything that reads the old key (#243).
- The dashboard's extensionless paths of more than one segment, or ending in
  `/` (`/cluster/3`, `/teams/`, `/index.html/`), answer `302` to the root
  with the route in the hash, as a relative `Location` that keeps a proxy's
  path prefix, where they used to serve a page whose assets did not load; a
  single segment such as `/teams` still serves the dashboard, as before. A
  path with a file extension that is not a file stays a JSON `404`;
  `/assets/` (which listed the asset files) and any other path under it that
  is not a file are now a JSON `404` too (#243).
- Chart: the agent's CPU limit is now `1` CPU
  (`agent.resources.limits.cpu`), not `200m`; the request stays `50m`, so
  scheduling is unchanged. A ResourceQuota on `limits.cpu` without room for
  800m more per agent pod, or a LimitRange whose CPU maximum is below 1,
  will refuse the pod: give the quota the room or set
  `agent.resources.limits.cpu` to what fits. Measured as a pod against 2,001
  KWOK nodes and 1,001 Helm releases (`docs/operations/scale.md`): at 200m
  the first tick took 73.4 s and its Helm step gave up at its deadline with
  132 releases unread, and a steady tick took 21 s, throttled in 58 to 80%
  of its periods; at 500m the first tick took 47.9 s and at 1 CPU 35.2 s,
  both complete, and a steady tick 6.9 s at 1 CPU. Peak RSS was 66 to 71 MiB
  at every limit. The new *Sizing the agent* section of the install guide
  has the rest (#229).
- The agent keeps a reserve of each tick for what follows collection: the
  ClusterReadiness status write, the `upgradescope.dev/status-error` marker
  and the push, each on its own slice of it, so a slow collector step no
  longer leaves the object stale and unmarked. The reserve is 30 s, or half
  the tick deadline (`--interval` / 2, at most 5 minutes) when that is under
  a minute; collection gets the rest. At the default `--interval 10m`
  collection gets 4 min 30 s (was 5 min); at the 1m minimum it gets 15 s
  (was 30 s). The computed ceiling of the Helm step at 1 CPU and the default
  interval is now about 1,900 releases, about 1,600 with `rbac.gitops.*`
  (arithmetic, not measured): give a larger cluster more CPU or a longer
  `agent.interval` (#238).
- The agent refuses at start what could never work, so it fails fast instead
  of failing every tick: `--cluster-name ""` given explicitly (leave the
  flag out to name the cluster by its UID); `--force-sync-every` of 0 or
  below; a `--server-url` that is not an `http` or `https` URL with a host;
  and, with `--server-url`, a push token with whitespace or a control
  character inside it (surrounding whitespace is trimmed).
  `--force-sync-every` at or below `--interval` force-syncs every tick. An
  agent given one of these crash-loops with `invalid --<flag>` in its log:
  check `agent.extraArgs` and the token Secret before you upgrade. The chart
  schema now rejects an `agent.serverUrl` without a host, and is stricter
  than the agent about non-ASCII host names and whitespace in the path, so a
  `helm upgrade` with an unusual value can fail the schema (#238).
- A deprecated-API caller is graded by the knowledge base's removal release
  when the knowledge base has one for that group, version and resource, else
  by the metric's `removed_release` label. A caller the label alone would
  call a warning is now a blocker when the knowledge base has the API
  removed at the target (and the reverse, when the knowledge base removes it
  later than the label says), so a gate can turn red or green on the same
  cluster. When they differ, the finding names the knowledge base as the
  source. A remediation names only a replacement the target serves;
  otherwise it says "no replacement Kubernetes X serves is known", naming
  the release a later replacement is served from, for live and Helm manifest
  findings alike (#236, #237).
- Folding caller rows into findings is indexed: an evaluation of 16,000
  usage entries and 16,000 caller rows takes about 30 ms (27.8 to 31.7 ms
  measured), where it took 12.9 to 18.4 s (#236).
- Server database: migration 0009 (SQLite and Postgres) adds indexes for
  "newest evaluation by id", a `carries_hold` column, and backfills
  `server_version` from stored inventories, on the first start. Retention
  now also keeps each cluster's newest decided evaluation for every target
  the server evaluates for it (and the three minors below its default
  target), and its snapshot, however old, so a cluster can keep one older
  snapshot and evaluation per target in use. With a notification sink
  configured, a push whose notification baseline another writer replaced on
  each of its three attempts answers `503` with `Retry-After: 10` and stores
  nothing; the agent retries. A `GET /api/v1/fleet/teams?target=` rollup
  still computing after 20 s answers `503` with `Retry-After`, keeping what
  it computed for the retry. A what-if is kept per cluster snapshot
  (or stored evaluation), target, knowledge base, team map and UTC day, so a
  later request computes only what changed and a what-if's `evaluatedAt` is
  when it was computed, not the request time (#241).
- Chart: upgrade with `helm upgrade --reset-then-reuse-values` (Helm 3.14 or
  later), as the docs now say everywhere, not `--reuse-values`, which pins
  the new chart to the old release's image digest and can fail the render
  when the chart adds a value. The chart never restarted pods when a secret
  changed, and still does not, because no pod annotation carries a function
  of a secret value (readable by everyone who can get Deployments): after
  you change a token, a webhook URL or the contents of a Secret you named,
  run `kubectl rollout restart` on the server and agent Deployments (the
  install notes now print the command). Every key of the chart's Secrets is
  now written under `data`, so from this version on, removing a value
  removes its key; the one exception is a value you remove in the same
  upgrade from an earlier chart, which keeps an inert key in the Secret
  (`docs/operations/upgrade.md` has the one-time `kubectl patch`) (#242).
- Chart: `server.staleAfter` defaults to empty, which follows
  `agent.interval`: the larger of 2h and three intervals (2h for any
  interval up to 40m); the release candidates defaulted to `2h`. A value you
  set at or below `agent.interval` fails the render, and the install notes
  warn when one would flap.
  `metrics.prometheusRule.clusterStaleAfterSeconds` defaults to `0` (follow
  the server's threshold; the release candidates defaulted to `7200`); one
  at or below the interval fails the render (#242).
- Chart: the server's container port is its own value,
  `server.containerPort` (default `8080`), so `server.service.port` may be
  `80` or `443`. Before, the pod listened on `server.service.port`: if you
  set that to something other than 8080, the pod now listens on 8080 (the
  Service still answers on your port), so a NetworkPolicy, sidecar or pod
  port-forward of your own that names the old port must follow it, or set
  `server.containerPort` to the old value (1024 or above). Service names are
  cut to 63 characters for a long release name, and everything that names a
  Service follows the cut (other resource names are unchanged).
  `networkPolicy.enabled` with `server.ingress.enabled` or
  `metrics.serviceMonitor.enabled` fails the render while
  `networkPolicy.serverIngressFrom` is empty: list the ingress controller's
  or Prometheus's pods there, which the policy would otherwise cut off.
  `server.tmp.sizeLimit` bounds the new SQLite temp volume (#242).
- A steady agent tick on a cluster of 2,001 nodes, about 14,000 pods and
  1,000 Helm releases makes 31 API requests, down from 53, reading the same
  50 MiB with 3.0 CPU-seconds (`docs/operations/scale.md`). The pod and node
  lists size each page after the first by the largest object of the one
  before, as many as fit 8 MiB encoded, at least 500 and at most 1,000. API
  discovery is kept between ticks (asked again when the server version or
  the CRDs change, or after an hour, which costs 4 more requests), and a
  tick reads its ClusterReadiness once and writes the status over what it
  read. The larger pages raised the benchmark's peak heap from 29.3 to 39.2
  MiB and its peak RSS from 57.1 to 65.0 MiB, and a page of 1,000 pods of
  about 70 KiB each, after small ones, would take the agent past its
  `GOMEMLIMIT` at the chart's 256Mi limit (computed): raise
  `agent.resources.limits.memory` on a cluster with pods that large. The
  target of under 25 requests is not met (#228).
- The first tick, and every one-shot `scan`, fetches Helm releases on 8
  workers and still decodes one at a time, in order, so results and the
  release cache are unchanged; memory stays bounded at one release's decode
  plus up to 7 fetched payloads (about 21 MiB more, computed). At 60 ms of
  added round-trip time the whole first tick took 30.4 s and read all 1,000
  releases, where the code before it stopped at the Helm step's deadline
  after 67.6 s with 231 unread. Under a CPU quota, where decoding is the
  bound, it is not expected to help (#226).
- Minting the first read token closes an open read API: a server run without
  `--read-token` (on loopback, or with `--allow-anonymous-read`) answers
  `401` from then on to a request without a valid credential, as it does
  once `--trust-team-header` is set. Revoking the last read token does not
  open it again. `--read-token` still reads the whole fleet and answers
  exactly as before. A `--team-map` team named `*` is read as the team
  `(*)`, with a warning at startup, since `*` is the whole-fleet scope
  (#72).
- Server database: migration 0008 (SQLite and Postgres) adds the
  `read_tokens` table and `evaluations.teams`. The first start re-evaluates
  every stored evaluation once; until its pass reaches a cluster, that
  cluster's evaluations read `outdated: true` and no team-scoped read sees
  it. `serve` now opens, and so migrates, the database before it refuses an
  open read API on an address that is not loopback. A server rolled back
  past it ignores read tokens, so a read API closed only by minted tokens is
  open again (#72).
- A managed cluster (EKS, GKE, AKS) whose minor is past the provider's
  standard support now has a `support-lifecycle` blocker, so its verdict is
  `blocked` and a `--fail-on blocker` gate fails; within 90 days of the end
  it has a warning, which lowers the score and fails `--fail-on warning`.
  Upgrade, or accept it with an ignore rule (`key:
  support-lifecycle/eks/1.34`, or `category: support-lifecycle`). The server
  reports support only for agents that send `provider`: v0.1.x agents and
  v0.2.0's release candidates send none. With `agent.manageCRD=false`, apply
  the new `deploy/chart/crds/` before the agent starts, or the new status
  fields are pruned. The knowledge base version label changes (#77).
- A cluster where a node's kube-proxy is more than 3 minors (2 for a
  kube-proxy older than 1.25) older or newer than that node's kubelet now
  has a `version-skew/kube-proxy-kubelet/<node>` warning, which lowers the
  score and fails `--fail-on warning`. It appears only for inventories that
  record kube-proxy's node, from agents and CLIs of this release. Bring the
  kube-proxy DaemonSet's version within the policy, or accept it with an
  ignore rule (`key: version-skew/kube-proxy-kubelet/<node>`) (#148).
- The `helm` capability is partial, naming `argocd` or `flux` in `skipped`,
  when a GitOps tool deploys charts it cannot read releases for: on a
  cluster with no Helm release that serves the tool's CRD or has workloads
  with its tracking metadata; for every chart read from an Argo CD
  Application, whether or not the cluster has Helm releases (`helm template`
  leaves none, so `kubeVersion` and stored-manifest checks did not run);
  when the tool's list is forbidden; when a HelmRelease `chartRef` does not
  resolve to a chart; and, for both tools, when API discovery fails. A Flux
  whose HelmRelease list is served and empty adds no gap. `helm` is
  optional, so this alone never makes the verdict `unknown`, but a partial
  `helm` holds add-on and Helm-release findings in the notification baseline
  instead of resolving them. Without the grant for the tool it runs
  (`rbac.gitops.argocd` or `rbac.gitops.flux`), a cluster that serves either
  CRD reports `helm` partial, since the list is forbidden; this includes
  upgrading the chart on a Flux cluster whose releases were read in full
  before. With `rbac.gitops.flux`, the Flux gap clears once its Helm
  releases are read and its chartRefs resolve. On Argo CD, `helm` stays
  partial, grant or not, whenever an Application deploys a chart or the
  cluster has no Helm release (#70).
- Clusters that run Kubernetes Dashboard, Promtail, Grafana Agent or Weave
  Net, or the `bitnamilegacy` Ingress NGINX image, now have an `eol-addon`
  blocker, so their verdict is `blocked` and `--fail-on blocker` gates fail.
  Karpenter, Gatekeeper and Fluent Bit installs, whose images used to be
  listed in `unrecognizedImages`, are judged by release line and can raise
  `eol-addon` or `eol-approaching` findings. Replace the add-on, or accept
  the finding with an ignore rule (`key: eol-addon/promtail`). The knowledge
  base version label changes (#49).
- An empty Node list is no longer read as clean: the `versions` capability
  is partial, with the reason `no nodes listed: kubelet skew and node
  runtimes not assessed` and `nodes` in `skipped`. On a live cluster that is
  a required gap, so a cluster with no listed node (a control-plane-only
  test cluster, say) reads `unknown` instead of `ready`, as it does when the
  Node list is forbidden; gate it with `--allow-incomplete` if that is
  expected. The server does not flag an empty Node list itself, so clusters
  whose agents predate this read as before until the agent is upgraded
  (#174).
- Notifications: a deprecated-call blocker missing from a scrape of an
  apiserver that has been up for less than 24 hours is held in the baseline
  instead of resolved, so an apiserver restart, which empties
  `apiserver_requested_deprecated_apis`, no longer sends `became-ready` or a
  later `new-blocker` for a caller that has not called again yet. The agent
  reports the scraped apiserver's start time as `apiServerStartTime`
  (`process_start_time_seconds`), which is not part of the snapshot's
  identity, so moves between HA apiservers store no new snapshot. The hold
  ends on the first scrape at least 24 hours after that apiserver started,
  measured on the agent's `collectedAt`: within 24 hours plus
  `--force-sync-every` (default 1h) and one `--interval` of the restart. A
  caller of an API the scraped server version no longer serves is resolved
  at once. A start time before 2014 or more than 10 minutes after
  `collectedAt` is ignored. Agents without the field behave as described in
  the #189 entry below. A fix is held too, and a client that calls less
  often than daily is resolved when the hold ends (#204).
- `unknown-api` covers every API group the knowledge base generator's scheme
  registers (`k8s.io/api` plus the apiextensions and apiregistration
  schemes), not only groups with lifecycle entries: `--files` scans and
  `/gate` manifests of `internal.apiserver.k8s.io` StorageVersion and
  `imagepolicy.k8s.io` ImageReview objects now give an `unknown-api` info
  finding, which changes neither score nor verdict. CRD and aggregated API
  groups stay silent. Live scans read only the APIs the knowledge base
  flags, so they are unaffected. The knowledge base version label changes
  (#172).
- The agent keeps what it decoded from each Helm release, keyed by the
  storage object's namespace, name, UID and resourceVersion, so a tick
  fetches only releases that are new or changed; a one-shot `scan` keeps
  nothing. Measured in `docs/operations/scale.md` on a kind lab with 2,000
  KWOK nodes (2,001 with the control plane), about 14,000 pods and 1,000
  Helm releases, before the `kube-system` change below: a steady tick went
  from 1,061 to 61 API requests, 90 to 68 MiB read (4.3 MiB on the wire), 33
  to 5.6 s and 23.5 to 3.6 CPU-seconds. The first tick after a start still
  reads every release (1,061 requests, 36 s there). At 200m, the chart's
  default until the CPU limit moved to 1 CPU (see above), such a first tick
  reached the Helm step's deadline and left 132 of 1,001 releases for the
  next tick (#71, #229).
- Each `kube-system` pod is read once per tick: the add-ons take the
  `kube-system` pods' images and labels from the control-plane version
  check's read and list the other namespaces with the field selector
  `metadata.namespace!=kube-system`; about 4,000 pods at 2,000 nodes are no
  longer listed twice. When the version check fails before it has read them,
  the add-ons list every pod as before, and a server or proxy that rejects
  the selector with 400 is asked again without it. The RBAC is unchanged
  (#227).
- The Helm manifest memory test runs only in `make test-heap` and CI's
  test-heap job (`UPGRADESCOPE_HEAP=1`), no longer in a plain `go test
  ./...`, where a busy machine inflated its reading. The quoted memory
  figures are re-measured on a GitHub-hosted `ubuntu-latest` runner: the
  worst Helm releases peak at 32 to 46 MiB of live heap against the 64 MiB
  bound (CI run 37160469086); the `/fleet` of the widest gaps grows the heap
  11.4 MiB against a 20 MiB bound (CI run 37152753746); and for 2000
  clusters with 200-byte names, 100 `/metrics` clients that never read peak
  the heap at 125 MiB above idle, both fleet slots and the held responses
  included (CI run 37160469086). No bound changed (#212, #213).
- Notifications: a blocker or EOL warning that came from a capability the
  current pass did not assess (unavailable, or partial over it; required or
  optional) is carried forward in the baseline. It is not *resolved* in
  that pass and not *new* when the capability returns, and `became-ready`
  waits while one is carried. A capability that a finding was already seen
  without cannot hide it later, so a steady gap such as
  `rbac.helmSecrets=false` still resolves and re-alerts. For an agent that
  does not report `apiServerStartTime` (v0.1.x, v0.2.0's release
  candidates), an apiserver restart empties
  `apiserver_requested_deprecated_apis`, so a deprecated caller that has not
  called since is resolved, which can send `became-ready`, and is announced
  again when it calls; newer agents get the hold described above (#189,
  #204).
- A cluster inventory that does not report a required capability
  (`api-usage`, `versions`, and `addons` when the knowledge base lists
  add-ons) has a required gap, so it reads `unknown` unless it holds a
  blocker: an empty `capabilities` map no longer reads `ready`. Ingest
  refuses (422) an inventory whose `source` is set to anything but
  `cluster`, or whose `collectorSchema` this server does not know. In an
  inventory without `collectorSchema` from an agent reporting 0.2.0-0 or
  later (v0.2.0-rc.1 and v0.2.0-rc.2), a usage count that names no object
  is not judged: api-usage is partial over that API, a required gap when
  the target removes it, so the verdict is `unknown`, not `blocked` (#194).
- A team's `ready` is true only when its verdict is `ready`: a required gap
  in the report, or a blocker no team is attributed, now makes it false,
  where it used to be true whenever the team had no blocker of its own. The
  CLI's team table prints the verdict instead of `ready yes/no` (#196).
- `spec.targets` of a `ClusterReadiness` takes at most 8 entries (schema
  `maxItems`). An agent reading an older object with more evaluates the
  first 8 distinct valid targets, in spec order, and counts the rest in one
  `status.notAssessed` note that names the first left out. Given more than
  8 distinct minors in `--targets`, the agent refuses to start. Finding
  titles in the status are clipped to 512 bytes and remediation to 1024,
  as stored (JSON escapes counted). Past 3 hops, the version-skew
  upgrade-path title, in every output, shows only the start, the first
  step and the end of the path (`1.29 → 1.30 → … → 1.36`). The worst-case
  status is about 273 KiB (#191).
- The agent refuses at startup `--interval 0`, which used to mean the 10m
  default (minimum 1m, as for any other value below it), and a `--cr-name`
  that is not an RFC 1123 subdomain, which used to fail every tick (#192).
  The chart's values schema refuses the same names, and more than 8
  `agent.targets`, at install.
- After at least 3 failed ticks in a row, and once the last success is
  older than twice the interval plus 12 minutes, the agent stops exporting
  its verdict, score, finding and capability gauges, so
  `UpgradescopeUpgradeBlocked` and `UpgradescopeVerdictUnknown` resolve
  while `UpgradescopeAgentNotTicking` fires: alert on that one too (#199).
- The Action, with `version` unset, runs its own ref's release: the tag at
  a release tag ref (`@vX.Y.Z`, `@vX.Y.Z-rc.N`), or at a full commit SHA
  the release tag that points at that commit (`git ls-remote --tags`). At
  a commit SHA no release tag points at, or when the lookup fails, it runs
  the latest release and warns; at any other ref (a branch, `v0`) it runs
  the latest release. With `version: latest` at a release tag ref, it warns
  when latest is older than that ref. Only a ref of this repository
  counts: inside a wrapping composite action the default stays latest. It
  used to run GitHub's latest release, so `@v0.2.0-rc.2` ran v0.1.1 (#197).
- `--db`: a path containing a `%XX` escape opens the file of that literal
  name; it used to open the percent-decoded name. Rename a database created
  under the decoded name before upgrading. A path containing `?` or `#`,
  which rc.2 refused, now opens that exact file, and a NUL byte is refused
  (#195).
- An object of a flagged kind that neither `managedFields` nor the
  last-applied annotation attributes to a writer is an info finding,
  `authorship unknown`, which never changes the verdict or score; it used
  to be missed. Inventories record such objects in `apiAuthorshipUnknown`
  (#199).
- The hand-written `supplement.json` is gone: the four entries it held
  (autoscaling HPA v2beta1 and v2beta2, both PodSecurityPolicy versions)
  were already in the generated dataset, which won on overlap. The
  lifecycle dataset is now the generated file alone, and the facts the
  generator adds by hand carry citations (#166).
- **Breaking for webhook receivers:** the generic webhook now sends one
  JSON body per cluster and evaluation pass (`schemaVersion` 1, lowercase
  keys: `deliveryId`, `type`, `timestamp`, `cluster`, `targets`, `changes`,
  `omitted`) instead of one PascalCase event (`Cluster`, `Target`, `Kind`,
  `Title`, `Detail`) per change and target. With `--webhook-secret` the body
  is signed (`X-Upgradescope-Signature`, HMAC-SHA256). Delivery is at least
  once: deduplicate on `deliveryId`. Update receivers; see
  `docs/reference/webhook.md` and `api/webhook.schema.json`.
- **Breaking for `serve` outside the chart:** `serve --listen` defaults to
  `127.0.0.1:8080` (v0.1.1: `:8080`). A binary or `docker run … serve`
  that other hosts, a Service or a port-forward must reach now binds
  loopback and is unreachable: pass `--listen :8080`. Open read access off
  loopback is refused, so also pass `--read-token` (or
  `--read-token-file`), or `--allow-anonymous-read` to accept open reads.
  The Helm chart already passes `--listen`, so chart users are unaffected.
- **Breaking:** `tokens revoke <cluster>` no longer revokes every active
  token of the cluster. It exits 1 unless you pass `--id <id>` (one token,
  from `tokens list` or `tokens create`) or `--all`. Scripts that revoked
  a cluster's tokens by name alone must add `--all`; rotation can now use
  `tokens create <cluster>` then `tokens revoke <cluster> --id <old id>`.
- **Breaking for SARIF consumers:** each result's `ruleId`, and each rule in
  `tool.driver.rules`, is the finding key (for example
  `removed-api/extensions/v1beta1/Ingress`) instead of the category
  (`removed-api`), and there is one rule per key. GitHub code scanning
  accepts the new output. Update any other SARIF filter,
  dashboard or suppression list that matches a category `ruleId`, such as
  `removed-api` or `eol-addon`.
- The JSON report (`scan --output json`, `--write-baseline`) is a versioned
  contract: `schemaVersion` 1 starts after v0.1.1, with `toolVersion`
  and `verdict` (`ready`, `blocked`, `unknown`). `ready` stays and is now
  true only for the verdict `ready`, where v0.1.1 meant "no blocker" (see
  the fail-closed gate below). `kbVersion` has a new format. Readers should
  ignore unknown fields; see `api/report.schema.json`.
- `serve --retention` defaults to `90d`: the server now prunes evaluations
  and snapshots older than that at startup and then daily, keeping each
  cluster's latest snapshot and its evaluations. v0.1.1 kept everything.
  Pass `--retention 0` to keep all history.
- `serve --ingest-token` is optional (v0.1.1 required it). Without it the
  server accepts pushes only with per-cluster tokens from
  `tokens create`. A shared token may push as any cluster, so prefer
  per-cluster tokens.
- A Slack or webhook sink that answers 429 or 503 with `Retry-After` is left
  alone for that delay (capped at an hour): the sink is not called for that
  message or any other queued for it, and the held messages keep their
  attempts. A queued notification is given up once it has been queued for
  8 hours, so a receiver limited for good cannot hold a growing backlog.
  The hold is kept in memory, so a restart forgets it, and messages still
  queued after a server outage longer than 8 hours are dropped unsent.
- Inventories from collectors that predate this release report `crds` as
  not assessed. This includes the reports the server re-evaluates for
  existing v0.1 agents, and saved `--files` inventories. The gap is not
  required, so the verdict and the score are unchanged.

- Chart: the agent can read the resources of the newly known deleted APIs
  (for example `auditsinks`, `clustercidrs`, `podpresets` and the DRA
  alpha kinds), so a cluster that still serves them is checked.
- Chart: the agent's ClusterRole can list `ingressclasses`
  (`networking.k8s.io`) for add-on detection; a test pins the grant.
- `--files` reports no longer list `addons` under `notAssessed`: add-ons
  are assessed from the manifests.
- **The gate fails closed.** Under `--fail-on blocker|warning` (the
  default), a scan with verdict `unknown` exits 2. A required check that
  could not run gives `unknown`: for example, RBAC denied, the cluster was
  unreadable, or the target is beyond the knowledge base. v0.1.1 reported
  such scans as `ready: true` and exited 0. Pass `--allow-incomplete` to
  gate on findings alone.
- **The server gate fails closed too.** `POST /api/v1/gate` defaults to
  `fail-on=blocker`: a blocker or an `unknown` verdict answers 422 with
  the full report body, and `fail-on=never` keeps 200. v0.1.1 always
  answered 200. Use `curl --fail-with-body` to fail the CI step and keep
  the SARIF; `?path=` gives SARIF results file locations, and with
  `?cluster=` only what the manifests introduce counts (#120).
- A live scan whose control-plane skew could not be judged where upstream
  would have told the version now gives `unknown` and exits 2 (it gave
  `ready` and exited 0); the agent's `ClusterReadiness` status and the
  server's reports read `unknown` too. That is a component pod in
  `kube-system` (labelled `component=` or `k8s-app=`, or named after the
  component) on an upstream-named image under a digest or a tag that is
  not a version (`kube-proxy@sha256:…`, `kube-scheduler:latest`), or a
  kube-apiserver, kube-controller-manager or kube-scheduler pod whose
  version is not read. Pin the image to a version tag, or pass
  `--allow-incomplete`. A kube-proxy pod on a vendor image of another
  name (Oracle OKE's `oke-public-kube-proxy`) does not change the
  verdict. A component image tagged 0.x is never read as Kubernetes 0.x:
  a kube-scheduler is read as a scheduler-plugins build, any other is
  unreadable (#169).
- A scan of a cluster that could not be read at all exits 1, and so does a
  `--files` scan that found no Kubernetes objects. Neither reports
  100/100 any more.
- Deprecated-API detection asks who writes the deprecated API
  (`managedFields`), not whether the apiserver still serves it. This
  removes the false removed-API blockers on clean clusters (#3). One
  removed API is scored once, with its caller evidence attached.
- SARIF results carry file and line locations, so GitHub code scanning
  accepts them. Findings without a file are reported as notifications.
- Kubernetes versions with a vendor suffix (EKS, GKE, k3s, RKE2,
  OpenShift) are parsed. The skew policy now enforces the kube-proxy and
  the kubelet upper bound after the upgrade.
- The knowledge base version label is derived from the embedded data:
  `k8s.io/api vX; lifecycle <digest>; registry <digest>`.
- Server: open read access is refused unless the address `serve` actually
  binds is loopback or you pass `--allow-anonymous-read`. Connections have read, write
  and idle timeouts. Snapshot and gate bodies are capped
  (`--max-snapshot-bytes`, `--max-gate-bytes`). Responses carry CSP and
  other security headers. A push whose cluster ID differs from the ID
  that first registered the cluster name is refused with 409. Stored
  verdicts are re-evaluated, and reads come from the latest snapshot.
  Ingest and notifications are written atomically through an outbox.
- Chart: the agent's ClusterRole names every rule; it has no wildcards.
  The chart no longer renders the `ClusterReadiness` object, because the
  agent owns it. The image defaults to the chart's `appVersion`, and the
  published chart pins it by digest (`image.digest`).
- `go install …@vX.Y.Z` binaries report their real version and serve the
  dashboard, because the built dashboard is committed.
- `go.mod` requires Go 1.26.9.
- Release notes keep breaking changes in housekeeping commits
  (`chore!:`, `docs!:`).
- A `--target` that is not an upgrade of the cluster (a downgrade, the
  same minor, or a typo) gives verdict `unknown` with a required `target`
  gap. The gate exits 2 even with `--allow-incomplete`. The JSON report
  has a new `serverVersion` field, and the table header shows it.
- Add-on findings describe each install. Every namespace that runs an
  add-on is its own instance. Findings are grouped by release line, name
  only the affected namespaces and teams, and list at most 10 installs
  ("and N more"). A newer Helm release in one namespace no longer hides an
  older end-of-life install in another.
- A finding lists at most 100 affected namespaces, in `namespaces` and
  in its evidence sentence, sorted, and counts the rest in the new
  `namespacesOmitted`, as `objects` and `objectsOmitted` do; `teams`
  still names the teams of every affected namespace. A namespace-scoped
  ignore rule suppresses such a finding only when it lists every
  namespace (#121).
- Version-skew finding keys: `version-skew/<component>` is split into
  `version-skew/<component>-newer` and `version-skew/<component>-behind`,
  and the upgrade-path finding is `version-skew/upgrade-path`. Baselines
  and notifications see these findings as new once.
- A baseline marks a finding `unchanged` only if its severity has not
  risen since the baseline.
- Deprecations that take effect after the target are titled as future
  deprecations, with "(projected)" beyond the knowledge base's horizon.
- Server: `GET /api/v1/clusters/{id}` and its `report`, `findings`,
  `teams`, `history` and `export`, and `GET /api/v1/fleet/teams`, run one
  at a time and can answer `503` with `Retry-After` after waiting 30s for
  their turn; retry them as the agent retries pushes. `/api/v1/clusters`,
  `/api/v1/fleet` and `/metrics` read no snapshot inventory and no stored
  report; a `/metrics` scrape waits at most 5s for its turn. When the
  responses those reads hold for slow clients fill their budget, a
  response is sent while the read holds its turn, and a client that has
  not taken it within 20s is disconnected (#121).
- Server: `/api/v1/fleet?targets=` takes at most 16 distinct minors, and
  `/api/v1/clusters/{id}/history?limit=` at most 1000; more is `422`. A
  snapshot push that is not valid UTF-8 is `422` (#121).
- Server: JSON responses and stored reports write `<`, `>`, `&`, U+2028
  and U+2029 as themselves, not as `\u` escapes (#121).
- Server: `POST /api/v1/snapshots` answers `422`, naming the field and
  the rule, for an inventory no collector writes, before anything is
  stored: a `clusterName` that is not an RFC 1123 subdomain of at most
  253 bytes (#37); a namespace that is not an RFC 1123 label, an object
  name over 253 bytes or with `/` or `%`, a node or Helm release name
  that is not an RFC 1123 subdomain, a team label that is not a label
  value; a string over 16 KiB (a capability reason over 64 KiB, an
  object's field manager over the apiserver's 128 bytes or not
  printable), more than 100 objects in an API usage entry, a
  group/version/kind listed twice in one list, more than 200
  unrecognized images or more than 32 capabilities. The free text a
  collector copies whole from the cluster is cut to those limits, by the
  agent and at ingest, not refused: a capability's reason (one failure
  per resource the agent could not read) and skipped entries, and the
  `upgradescope.dev/ignore` and `ignore-reason` annotations. v0.1 agents'
  inventories are within the rest, but not necessarily their cluster
  names: `tokens create`, cluster rename (the CLI's too) and the agent's
  `--cluster-name` (checked at startup, with `--team-label`) take only
  RFC 1123 subdomains now, and v0.1 took any name (#121, #37).
- **Upgrade note:** a cluster a v0.1 server registered under a name that
  is not an RFC 1123 subdomain (uppercase, `_` or spaces: `Prod-EU`,
  `prod_eu`) gets `422` on every push once the server is upgraded, and
  its per-cluster tokens are bound to that name. For each such cluster
  (`upgradescope clusters list`), run `upgradescope clusters rename
  <old> <new>`, which keeps its history and re-binds its tokens, then set
  the agent's `--cluster-name` (chart `agent.clusterName`) to the new
  name. See the operations guide's cluster lifecycle (#37).
- Server: `/api/v1/fleet` without `?targets=` opens at most 16 columns,
  as `?targets=` takes at most 16: the minors with the most clusters to
  fill them, the older on a tie. The rest are counted in a new
  `targetsOmitted`, which the dashboard shows; ask for them with
  `?targets=` (#121).
- Server: a report is at most `--max-snapshot-bytes`. A push whose report
  for a target would be larger is `413` and stores nothing; a what-if
  report is `413` (the fleet teams rollup lists such a cluster as
  `missing`); `/gate?cluster=` is `413`; the re-evaluation pass keeps
  what is stored. An export larger than the limit is `413`, saying to
  read the JSON report (#121).
- Server: the evaluation summaries that `/clusters`, `/fleet` and a
  cluster's detail carry list each gap's capability cut to 64 bytes, its
  reason to 256 and at most 3 skipped entries of 128 bytes, with a new
  `skippedOmitted` count; `/clusters` and `/fleet` list at most 1 KiB of
  gaps per evaluation, the required ones first, and count the rest in a
  new `notAssessedOmitted`. The report keeps every gap whole (#121).
- The kubelet skew findings name at most 100 nodes, and count the rest
  ("and N more"); their titles count every node.
- Server: `POST /api/v1/gate` answers `413` for a stream over its node
  budget, which a realistic kubectl YAML stream reaches at about 4.4 MiB
  (v0.1 decoded streams up to 20 MiB), or a document whose YAML
  aliases expand past 4 MiB, or a stream they expand past
  `--max-gate-bytes`; split such streams. It answers `413` too when its
  answer could be larger than `--max-gate-bytes` (it lists every object
  the findings name, with `?path=`; with `?cluster=`, the cluster's
  findings count too), and `422` for a `?path=` over 512 bytes or not
  UTF-8. A UTF-16 stream gets `422`. A `/gate` or snapshot body that does not arrive within the 60s
  read timeout gets `408`, which the agent retries (#121, #100).
- Server database: migration 0007 (SQLite and Postgres) adds
  `evaluations.not_assessed` and fills it from every stored report on the
  first start, so that start takes longer on a large database. A server
  rolled back after it still runs, but summaries do not follow what it
  writes: an evaluation it creates shows no `notAssessed`, and one it
  refreshes keeps the `notAssessed` it had, until a newer server writes
  that evaluation again.
- Chart: the server's memory limit is 1Gi (was 512Mi), and its
  `GOMEMLIMIT` ~921MiB: the worst case of one `/gate` request, one push,
  one read, two reads of a 500-cluster fleet, the responses held for
  their clients and the re-evaluation pass, measured on SQLite with four
  `server.targets` and notifications configured, is ~865 MiB (a limit
  below about 962Mi does not fit it). Each extra target adds about one
  report of up to `--max-snapshot-bytes` to every push and one to the
  re-evaluation pass (18-33 MiB each at the default 20 MiB): ~54 MiB of
  that sum per extra target, so without `server.targets` it is about
  650 MiB, which 768Mi holds.
- `serve --targets` and the chart's `server.targets` take at most 4
  distinct minors; more is refused at startup (and by the chart's schema)
  with a message saying why. The server's memory bounds are measured at
  that many (#121).

### Fixed

- Suppressing every listed object of a removed-API finding (an
  `upgradescope.dev/ignore` annotation, or a rule with `namespace`, `name`
  or `file`) no longer hides the live callers folded into it, which could
  show a cluster with a blocker caller as ready. Each folded caller now
  stands as its own `deprecated-api-in-use` finding at its own severity, in
  severity order; only a rule with no object selectors, taking the object
  finding or the caller's own key or category, suppresses it. This holds for
  `scan`, `POST /api/v1/gate` and `spec.ignore`: at the gate a re-emitted
  caller is `source: cluster`, and a blocker one keeps `clusterVerdict`
  blocked, not the gate's own `verdict`. A team-scoped read drops the
  cluster-wide callers from a finding it cuts to its teams. A Helm
  manifest's warning is no longer dropped when the live scan flags the same
  object as info, so the score and `--fail-on warning` are not lowered by
  more evidence (#236).
- The in-chart server on SQLite lost a retention prune or a `clusters
  delete` of more than a few tens of MB to `disk I/O error (6410)`: the root
  filesystem is read-only and SQLite had no temp directory. The chart now
  mounts an emptyDir `sqlite-tmp` at `/tmp` (`server.tmp.sizeLimit`, default
  `1Gi`) and sets `SQLITE_TMPDIR`; measured on a 60 MB backlog, a delete
  failed without it and worked with it. The volume is node ephemeral
  storage, and one that outgrows its limit evicts the pod. A volume of your
  own mounted at `/tmp` (`server.extraVolumeMounts`) is used instead;
  without one, a `server.extraVolumes` entry named `sqlite-tmp` fails the
  render. If prunes had been failing, the first one after the upgrade
  deletes the whole backlog at once: raise `server.tmp.sizeLimit` to the
  size of the database before you upgrade (#242).
- A push's notifications are diffed against the baseline the push committed
  against, so a push racing another replica's push or the background pass no
  longer loses or repeats a `became-ready` or `new-blocker` notification
  (the push is evaluated up to three times in all: see **Changed**). Each
  notification sink is delivered in queue order with its own timeout, up to
  four sinks at once, so one hung sink no longer delays the others. The
  background pass and the reads that need a snapshot's server version,
  `/history` among them, no longer decode a whole inventory to learn it. On
  SQLite, behind 4,000 history rows with 30 KB reports, the notification
  baseline is read in under 2 ms and 100 points of history in under 5 ms;
  the notification delta of 20,000 new callers against 20,000 carried
  findings takes 38 to 54 ms, where it took 46 s to about 1 min 32 s (#241).
- The agent's tick no longer leaves the ClusterReadiness stale and unmarked
  after a slow collector step (see **Changed**). A failed spec read, or a
  failed `spec.targets` patch, writes no status for targets it guessed and
  stamps no `observedGeneration`: it marks the object
  `upgradescope.dev/status-error`, fails the tick, and the next tick reads
  the spec again; the patch path no longer drops earlier errors of the tick.
  A CRD check that failed or ran out its 30 s at start is retried on every
  later tick until it succeeds; meanwhile each tick line carries `crdError`
  at WARN and `status.notAssessed` leads with a note, without failing the
  tick. Push retries wait a random time from 0 to the backoff step (a
  `Retry-After` is the least wait plus up to a quarter more), so agents
  answered with the same `503` do not retry together (#238).
- `POST /api/v1/gate` made room for the pull request's objects under the
  100-object listing cap before it evaluated, so a cluster object pushed out
  of the listing could not be accepted by its annotation or a `?config=`
  rule selecting it, and a pull request that posted many objects could turn
  the gate red, or lower the score, for findings it did not cause. The gate
  now evaluates and suppresses with every object, then cuts each listing to
  100, counting the rest in `objectsOmitted` (#72).
- The agent no longer follows a redirect when it pushes: a 3xx answer is a
  permanent failure that names the status and `Location` and says to use
  the final URL in `--server-url`. A 301, 302 or 303 used to turn the push
  into a body-less GET that could read as delivered. `Retry-After` on 429
  and 503 is honoured, up to 1 minute (#190).
- When the cluster-wide pod list fails, add-ons are still matched from Helm
  releases and IngressClass controllers. The `addons` capability is then
  partial with the reason, and a required gap, so the verdict stays
  `unknown` unless a blocker is found; when no Helm release was read and
  the IngressClass list could not be read either, `addons` is not
  assessed, as before (#199). Upgrade the server before the agent of a
  cluster whose agent role cannot list pods: an older server takes that
  partial `addons` for an optional gap, so a cluster it read as `unknown`
  can read `ready`.
- The Action sets `sarif-file` only after a scan that completed (exit 0 or
  2). After a scan error it used to point at an empty or partial file, so
  the documented upload step guarded on `sarif-file != ''` failed with
  Invalid SARIF (#197).
- A what-if evaluation (a target with no stored evaluation, in the cluster
  report, findings and teams endpoints and the `/api/v1/fleet/teams`
  rollup) is timestamped in UTC, as a stored one is; it used to carry the
  server's local offset (#196).
- The rpm is byte-reproducible: its Build Host header is pinned to
  `upgradescope`, and the reproducibility check scans the packages for the
  build machine's host name and build paths. Pre-release deb and rpm file
  names use `.` where nfpm puts `~`, so they match their `checksums.txt`
  entries as downloaded, and the release notes of a pre-release no longer
  offer the Homebrew tap (#198).
- Switching `server.sharedIngestToken` off removes the `ingestToken` key
  from a chart Secret written by this version or later, which writes it
  under `data`. A Secret written by v0.2.0-rc.2 or earlier keeps the key:
  remove it once with the `kubectl patch` that the
  `server.sharedIngestToken` comment gives. Checked by rendering; not yet
  on a live API server (#200).
- A control-plane or kube-proxy pod whose version upstream would have
  told but cannot be read makes the verdict `unknown`, not `ready`: an
  upstream-named component image under a digest or a tag that is not a
  version (`latest`), or an unread kube-apiserver, kube-controller-manager
  or kube-scheduler pod. Its skew was not evaluated, and it may be the
  component past the policy. A kube-proxy pod on a vendor image of
  another name (Oracle OKE's `oke-public-kube-proxy`) is an optional,
  disclosed `versions` gap, and the verdict is unaffected. The gate
  change this brings is under **Changed** (#169).
- Notifications after a cluster upgrade: a blocker that the cluster's new
  default target adds (for example `networking.k8s.io/v1beta1` ServiceCIDR,
  removed in 1.37, once the cluster runs 1.36) is notified instead of being
  taken as a silent baseline. Blockers it already had at its previous
  default target are not announced again (#34).
- `--output table` and `--output markdown` exit 1 when the report cannot
  be written, like JSON and SARIF.
- Pre-GA kinds written only by control-plane components (for example
  `LeaseCandidate`) are no longer counted as removed-API blockers (#108).
- Istio's end-of-life date. Pre-1.0 ingress-nginx images, and the
  ingress-nginx builds of AKS and RKE2, are now detected. GKE and AKS
  builds of an add-on are no longer judged by upstream's lifecycle.
- Remediation follows the replacement chain to an API that is still
  served.
- `--files` skips non-manifest YAML and unrendered chart templates,
  expands `List` objects, and records object locations.
- The agent evaluates a repeated target once, and a stop in the middle of
  a tick is no longer counted as a failure.
- A stalled or unresponsive API server no longer hangs `scan` (one ran for
  more than 7 minutes). Each request is bounded by `--request-timeout`,
  `/version` and discovery honour the scan's deadline, and each collector
  step has its own share of the 5-minute budget, so one stalled step
  degrades only its own capability (#94).
- An add-on older than the oldest release line the registry tracks is end
  of life when that line has ended: cert-manager 1.5, Istio 1.5, Calico
  3.24, Cilium 1.12, Kyverno 1.7, Argo CD 0.12 and Flux 1.24 were only an
  `addon-no-data` info and read READY 100/100. They are now an `eol-addon`
  blocker keyed `eol-addon/<id>/below-<oldest line>` (a warning for a node
  runtime), citing that line and the product's lifecycle pages. Versions
  between tracked lines or newer than the newest, and products without
  release lines, stay `addon-no-data` (#165).
- A Helm release no longer hides an older install of the same add-on in
  its namespace. Its `appVersion` now stands only for image tags and
  labels on its own release line; an image or label version on another
  line is a second install there, judged at its own version. An istioctl
  canary running `istio/pilot:1.28.10` beside an `istiod` 1.31.1 release
  in `istio-system` read clean; it now gets the ended 1.28 line's
  blocker. The manifest gate replaces every install of an add-on in a
  namespace its manifests deploy it to (#165).
- The same inventory gives the same report whatever the order of its rows:
  deprecated-API caller rows are sorted before they are judged and folded
  into usage findings, whose evidence sentence listed subresources in the
  order the rows arrived (#165).
- Knowledge base: `rbac.authorization.k8s.io/v1alpha1` (ClusterRole,
  ClusterRoleBinding, Role, RoleBinding; removed in 1.23) and
  `node.k8s.io/v1alpha1` RuntimeClass (removed in 1.24) now block at the
  release that stopped serving them; they scanned as ready with only an
  `unknown-api` info. Kinds kube-apiserver never stored as resources, and no manifest can
  create (`PodStatusResult`, `EphemeralContainers`,
  `ReplicationControllerDummy`, `JobTemplate`, the removed `Scale` and
  `DeploymentRollback` bodies, the v1beta1 `AdmissionReview`,
  `ConversionReview` and `policy` `Eviction`, and the `apidiscovery.k8s.io`
  v2beta1 `APIGroupDiscovery`) are no longer removed-API blockers.
  `scheduling.k8s.io/v1alpha3` types stay `unknown-api` infos (#166).
- `kb.Load` refuses a lifecycle dataset with fewer than 150 entries or 100
  removals, with an error that says the embedded data is corrupt, instead of
  judging every API as served (#166).
- Registry validation rejects a citation on a placeholder or local host
  (`example.com`, `localhost`, an IP address, or a private name such as
  `.local`, `.lan` or `.internal`, which an intranet page cited for a private
  add-on now trips) and `status: unknown` with an
  `eol_date`, which printed an uncited end-of-life blocker; a test fails on
  any file in `registry/data` that is not `*.yaml`. The registry notes
  that endoflife.date puts Istio 1.29's end of life at 31 October 2026
  where istio.io says 12 October (#166).
- Control-plane and kube-proxy versions are read from per-architecture
  images (`gke.gcr.io/kube-proxy-amd64`, `kube-scheduler-amd64`), so GKE's
  kube-proxy is skew-checked, and from VMware TKG's tags, whose build
  suffix follows an underscore (`v1.28.7_vmware.1`). A component pod whose version cannot be read
  (a digest-only image, a tag such as `latest`, or a labelled pod running
  an image of another name) makes the `versions` capability partial,
  naming the components and the first pod, instead of being dropped
  silently. scheduler-plugins' `kube-scheduler` is read by the Kubernetes
  minor its tag is built on (`v0.31.8` as `v1.31.8`), and kOps' and
  Talos' `k8s-app=<component>` labels mark component pods (#169).
- The containerd compat blocker (`chart-incompat/containerd/<line>`) names
  the nodes that cannot run the target even when they all run one version,
  so blocker-only outputs (gate, JUnit, code quality) say where (#169).
- `serve` stopped during startup (a signal while it opens the store) no
  longer leaves its notification and re-evaluation workers running on a
  closed store: a shutdown that came before or during the server's start
  did not always stop them.

### Security

- Built with Go 1.26.9 and `golang.org/x/net` v0.60.0 (`go.mod` requires Go
  1.26.9, and the Dockerfile pins the `golang:1.26.9` image by digest). This
  fixes the `net/http` and HTTP/2, `crypto/tls`, `html/template`,
  `net/textproto` and `mime/multipart` advisories `govulncheck` reported
  against Go 1.26.8 and `golang.org/x/net` v0.59.0; `make vuln` passes with
  an empty allowlist (#259).
- Slack and generic webhook URLs, which are secrets, never reach the
  server's log or the outbox's `last_error` past `scheme://host/…`, and
  the host is withheld too when the URL holds an `@` (#240).
- A snapshot push authenticated before its token was revoked, or before its
  cluster was deleted or renamed, no longer commits: both stores re-check
  the token and the cluster inside the commit transaction. A revoked token
  answers `401`, a rename or delete `409`, with nothing stored and no
  duplicate cluster created (#240).
- The oauth2-proxy example sets `--pass-host-header=false`, so the upstream
  sees the Host the server's new check accepts. The
  `docs/operations/auth.md` danger box now states that group naming in the
  identity provider is part of the trust boundary: a group name holding a
  comma or a `%XX` escape, or with a space or tab around it, reads as other
  teams (#240, #250).
- Argo CD `repoURL`s and Flux OCIRepository URLs are recorded in the
  inventory without userinfo, query string or fragment: everything between
  the scheme (or the start, where there is none) and the last `@` (before
  any trailing OCI digest) is dropped, whether or not the value parses as a
  URL (an OCI reference pinned by digest, such as
  `ghcr.io/acme/chart@sha256:…`, keeps its digest), and a URL with a `?` or
  `#` before that `@`, or with a scheme and no host, is recorded empty. A
  token embedded in the URL path (a Cloudsmith entitlement URL) cannot be
  told from a path and is kept. The agent does not read repository
  credential Secrets. GitOps chart references are written by whoever can
  create an Application or HelmRelease, who can therefore raise a false
  `eol-addon` finding; the GitOps guide says so (#70).
- The Action prefixes every line of the gate's stderr and of the Markdown
  summary it echoes to the log (both carry the scanned tree's file names)
  with `| `, and writes a CR in them as `%0D` and a `##[` as `# #[`; the
  JSON and Markdown passes' errors reach the log only escaped inside a
  `::warning`. It escapes `%`, CR and LF in every input it echoes. So a
  pull request's file names or manifests, or an input, cannot start a
  workflow command; v0.2.0-rc.2 relayed them as written. A check for a
  leading `::` would not be enough: the runner strips all leading Unicode
  whitespace (a form feed, a vertical tab, a no-break space) before it
  parses a line. The release-tag lookup runs from an empty directory of
  its own, never the workspace, with repository discovery stopped there
  and a stall limit, so files a fork places in the workspace cannot
  redirect it or run a credential helper (#197).
- The SQLite database, and its `-wal` and `-shm` files, are created 0600
  under exactly the name given: a `%3F`, `%23` or `%00` in `--db` could
  bypass the `?`/`#` guard and leave a 0644 file under another name next
  to an empty 0600 decoy (#195).
- Release tags must point at a commit on `main` (release.yml preflight),
  and a pull request whose description, or one of whose commits, has a
  `BREAKING CHANGE:` (or `BREAKING-CHANGE:`) footer without a lowercase
  `type!:` subject (the PR title, for the description) fails the
  `pr-lint / breaking-change` check, which is not yet a required check on
  `main` (#198).
- Built with current dependencies. govulncheck finds 22
  reachable vulnerabilities in the v0.1.1 binary and none in this build.
  A daily workflow now scans the latest published release, not only
  `main`.
- Releases come from CI only. Archives, packages and images are
  reproducible from the tag. `checksums.txt` and every image (the index
  and each platform) carry keyless cosign signatures, and there are SBOMs,
  SLSA provenance and build attestations. A ruleset makes release tags
  immutable.
- An OpenSSF Scorecard workflow publishes its results.
- `/api/v1/gate` and snapshot ingest bound their memory by the input's
  YAML or JSON structure and by concurrency budgets, not only by bytes.
  Nodes are counted before anything is decoded. YAML aliases are charged
  at their expanded size, and that is measured inside the single
  evaluation slot. UTF-16 streams are refused. A body over a budget gets
  413, and one too slow for the read timeout gets 408, before any 503
  (#121, #100).
- Reads of stored data are bounded too. Reads that load a cluster's
  snapshot run in a read slot of their own, and only a what-if decodes
  the whole inventory: one 370 KB push, from any ingest token, had made
  10 concurrent reads of it grow the heap ~400 MiB. The cluster list, the
  fleet matrix and `/metrics` read evaluation summaries instead of whole
  reports (after one 17 MB push, 30 concurrent requests to them had
  grown the heap by up to 584 MiB), and run two at a time in fleet slots
  of their own. Every read's response and every `/gate` answer is built
  in its slot and waits for its client in one budget of twice
  `--max-snapshot-bytes`; one that does not fit what is left gets 503
  with `Retry-After`, and one larger than the whole budget is sent in its
  slot under a 20s write deadline. Unbounded, 20 clients that asked for a
  17.5 MB report and never read it had held 366 MiB, and 100 that asked
  for a 2000-cluster fleet's `/fleet` 251 MiB (#121).
- A `/gate` answer is bounded before it is encoded: a 60 KB `?path=` had
  made a 1.2 MB stream answer 817 MB, YAML aliases across 136 documents
  532 MB, and names of `<` 72 MB of SARIF. `/fleet?targets=` with 8,718
  minors had held a fleet slot for over two minutes. Object names of `<`
  had made a push store reports six times its size (a 323 MiB ingest);
  stored reports and responses no longer escape them (#121).
- With notifications configured (`--slack-webhook`, `--webhook`), each
  evaluation of a target decided before decoded that earlier report whole
  as the baseline of what changed, and a cluster's later push loaded its
  previous inventory to compare hashes. At the report limit with four
  `--targets`, a later push grew the heap up to ~257 MiB and a
  re-evaluation that changed every finding up to ~265 MiB; the memory
  bounds had been measured on first pushes without notifications only.
  A push now reads the previous snapshot's head alone, a baseline only
  its findings' keys and severities, and one pass's changes are merged
  target by target, keeping a digest of those past the notification's
  cap: ~216 and ~198 MiB, and the bounds are measured that way (#121).
- The CSV export guards every cell against formula injection, including
  after leading white space. Anonymous read access is decided on the
  resolved bind address. The `Bearer` scheme is case-insensitive. The
  SQLite database and its WAL and SHM files are created 0600. The agent
  warns when it would push its token over plain HTTP to a host that is
  not loopback (#126).
- One Helm release object can no longer exhaust the agent's memory. A 951 KB
  release Secret whose gzip held 700 MiB took a scan to 1.93 GB and
  OOM-killed the agent at its 256Mi limit; anyone who can create a Secret or
  ConfigMap in one namespace could plant one, and a valid release built to
  amplify parsing (a 127 KiB Secret of tiny ConfigMaps took 564 MiB) could
  too. Each release payload is now decoded within 4 MiB stored and 16 MiB
  decompressed (real releases decode to under 7 MiB), and its manifest is
  parsed in runs of at most 1 MiB and 64Ki YAML nodes; a single document
  over 2 MiB or 64Ki nodes is not parsed. The worst releases that fit the
  bounds peak at 32 to 46 MiB of live heap against a 64 MiB bound, on a
  GitHub-hosted runner (CI run 37160469086). A release over a bound is
  skipped (`release payload too large`) or recorded without that document,
  named on a partial `helm` capability, and the rest are still read (#168).
- The security model documents that Helm release Secrets, pod images and
  labels are tenant-controlled evidence: findings are only as trustworthy
  as namespace write access. A forged release can raise a finding in its
  namespace; its `appVersion` stands only for pods on its own release
  line, so it cannot hide an older image on another line there, only a
  patch-level difference on the same line, an untagged image, or an
  image no matcher recognizes (#165).

## [0.1.1] - 2026-07-27

A packaging release. Scanning, scoring, the agent, the server and the
knowledge base are unchanged since 0.1.0.

### Fixed

- The GitHub Action can install from a release. 0.1.0 published archives as
  `upgradescope-v0.1.0-<os>-<arch>.tar.gz`, but the Action requested
  `upgradescope_<os>_<arch>.tar.gz`. Every run fell back to `go install` and
  built from source. Archive names no longer contain the version, so
  `/releases/latest/download/<asset>` resolves to a stable filename. The
  Action also now extracts the binary from the tarball.
- The README examples target the knowledge-base horizon (Kubernetes 1.36,
  `maxKnownK8s` from `k8s.io/api` v0.36.1) instead of 1.38. With 1.38, the
  headline command's only finding was a `kb-stale` warning about the tool
  itself.

### Changed

- CI and `make lint` run `go vet` plus staticcheck pinned to v0.7.0, instead
  of golangci-lint or an unpinned `staticcheck@latest`, so an upstream
  linter release cannot fail an unrelated pull request. `make lint` runs the
  pinned version through `go run` and ignores any `staticcheck` on `PATH`.
- The release archives are `upgradescope_{linux,darwin}_{amd64,arm64}.tar.gz`,
  published with `checksums.txt`.

### Removed

- The Go Report Card badge. goreportcard.com retired the service.
- The unused `.golangci.yml`.

## [0.1.0] - 2026-06-11

First release.

### Added

- `upgradescope scan`: a point-in-time readiness scan of a live cluster
  (kubeconfig) or of rendered manifests (`--files`). It prints a table, JSON
  or SARIF 2.1.0. The CI exit codes are: 2 when findings reach `--fail-on`,
  1 on an operational error.
- Detection of:
  - deprecated and removed API objects, probed at the deprecated
    group/version endpoints with paged, metadata-only lists;
  - clients still calling deprecated APIs, read from the apiserver
    `apiserver_requested_deprecated_apis` metric;
  - end-of-life and soon-to-be-EOL add-ons, matched by container image and
    Helm chart;
  - add-on and Helm chart compatibility with the target Kubernetes version;
  - version skew between kubelets, HA apiservers, controller-manager,
    scheduler and kube-proxy;
  - a knowledge base too old for the cluster or the target (`kb-stale`).
- A deterministic readiness score,
  `max(0, 100 − min(75, 25×blockers) − min(20, 5×warnings))`, with
  `ready = no blockers`, and per-team scores from a namespace label.
- Collectors that degrade independently. A collector that is denied or
  broken is reported as "not assessed (reason)" instead of failing the scan.
- `upgradescope agent`: an in-cluster loop that maintains a cluster-scoped
  `ClusterReadiness` CRD (`upgradescope.dev/v1alpha1`, short name `ucr`) and
  optionally pushes content-deduplicated inventory snapshots to a server.
- `upgradescope serve`: snapshot ingest, SQLite or Postgres storage, score
  history, what-if re-evaluation (`?target=`), fleet rollups, the
  `POST /api/v1/gate` CI gate, auditor CSV and HTML exports, Slack and
  generic-webhook notifications on finding deltas, and an embedded React
  dashboard (fleet matrix, cluster drill-down, registry browser).
- `upgradescope tokens create|revoke`: per-cluster ingest tokens, stored
  hashed.
- A Helm chart (`deploy/chart`) for the agent and an optional server.
- A composite GitHub Action (`action/`) that runs
  `upgradescope scan --files` over a directory of rendered manifests and
  emits SARIF for code-scanning annotations.
- A knowledge base with an API-lifecycle dataset generated from upstream
  `k8s.io/api` source (`tools/gen-kb`) and an 18-entry add-on registry. Every
  claim in the registry requires an upstream citation, and 10 entries are
  synced with endoflife.date (`tools/eol-sync`). A weekly `kb-refresh`
  workflow opens a pull request when either dataset changes.

### Known limitations

- kubectl client skew is not evaluated, because client versions are visible
  only in apiserver audit logs.
- Managed control planes often block apiserver `/metrics`. The
  deprecated-callers signal is then reported as not assessed.
- `kind: List` items are not expanded in `--files` mode.

[Unreleased]: https://github.com/abd-ulbasit/upgradescope/compare/v0.1.1...HEAD
[0.1.1]: https://github.com/abd-ulbasit/upgradescope/compare/v0.1.0...v0.1.1
[0.1.0]: https://github.com/abd-ulbasit/upgradescope/releases/tag/v0.1.0
