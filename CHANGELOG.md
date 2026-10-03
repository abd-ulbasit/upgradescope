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

- Each team in a report and on the dashboard has a `verdict` (`blocked`,
  `unknown` or `ready`). A team is `unknown` when a required check it
  depends on could not run, and `blocked` by a cluster-wide blocker it
  cannot be cleared of. `ready` is true only when the verdict is `ready`,
  and the CLI's team table prints the verdict (#196).
- Inventories carry `collectorSchema`, the collector's version of the
  inventory contract. The server tells a v0.1.x agent by its absence,
  together with an `agentVersion` below 0.2.0, instead of by the
  free-text version alone (#194).
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

- Notifications: a blocker or EOL warning that came from a capability the
  current pass did not assess (unavailable, or partial over it; required or
  optional) is carried forward in the baseline. It is not *resolved* in
  that pass and not *new* when the capability returns, and `became-ready`
  waits while one is carried. A capability that a finding was already seen
  without cannot hide it later, so a steady gap such as
  `rbac.helmSecrets=false` still resolves and re-alerts. An apiserver
  restart empties `apiserver_requested_deprecated_apis`, so a deprecated
  caller that has not called since is resolved, and is announced again
  when it calls (#189, #204).
- A pushed cluster inventory that does not report a required capability is
  `unknown`, not judged on what it holds: an empty `capabilities` map no
  longer reads `ready`. Ingest refuses an inventory whose `source` is not
  a cluster and a `collectorSchema` this server does not know (422). In a
  v0.1.x inventory, a usage count that names no object is a required
  api-usage gap, not a removed-API blocker (#194).
- `spec.targets` of a `ClusterReadiness` takes at most 8 entries (schema
  `maxItems`). An agent reading an older object with more evaluates the
  first 8 and lists the rest in `status.notAssessed`. Finding titles in the
  status are clipped to 512 bytes; the version-skew upgrade-path title
  names its first and last hops past a few. The status stays under about
  273 KiB (#191).
- The agent refuses `--interval 0` (minimum 1m, as for any other value
  below it) and a `--cr-name` that is not an RFC 1123 subdomain, at
  startup, instead of failing every tick (#192). The chart's values schema
  refuses the same names, and more than 8 `agent.targets`, at install.
- After at least 3 failed ticks in a row, and once the last success is
  older than twice the interval plus 12 minutes, the agent stops exporting
  its verdict, score, finding and capability gauges, so
  `UpgradescopeUpgradeBlocked` and `UpgradescopeVerdictUnknown` resolve
  while `UpgradescopeAgentNotTicking` fires: alert on that one too (#199).
- The Action, with `version` unset, runs its own ref's release: the tag at
  a release tag ref (`@vX.Y.Z`, `@vX.Y.Z-rc.N`), or at a full commit SHA
  the release tag that points at that commit (`git ls-remote --tags`).
  Otherwise, or when the lookup fails, it runs the latest release and
  warns. Only a ref of this repository counts: inside a wrapping composite
  action the default stays latest. It used to run GitHub's latest release,
  so `@v0.2.0-rc.2` ran v0.1.1 (#197).
- `--db`: a path containing a `%XX` escape opens the file of that literal
  name; it used to open the percent-decoded name. Rename a database created
  under the decoded name before upgrading (#195).
- An object of a flagged kind with nothing `managedFields` or the
  last-applied annotation can attribute is an info finding, `authorship
  unknown`, which never changes the verdict or score; it used to be
  missed (#199).
- One-segment add-on image matchers are exact; an entry opts a distinctive
  name into matching under any registry prefix with `"*/name"` (etcd
  only), so etcd behind a mirror is still recognized.
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
- `go.mod` requires Go 1.26.8.
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

- The agent no longer follows a redirect when it pushes: a 3xx answer is a
  permanent failure that names the status and `Location` and says to use
  the final URL in `--server-url`. A 301, 302 or 303 used to turn the push
  into a body-less GET that could read as delivered. `Retry-After` on 429
  and 503 is honoured, up to 1 minute (#190).
- When the cluster-wide pod list fails, Helm-installed add-ons are still
  matched from their releases and IngressClass controllers, and the
  `addons` capability is partial with the reason (#199).
- The rpm is byte-reproducible: its Build Host header is pinned, and the
  reproducibility check varies the build path. Pre-release deb and rpm
  file names match their `checksums.txt` entries, and the release notes of
  a pre-release no longer offer the Homebrew tap (#198).
- Switching `server.sharedIngestToken` off removes the `ingestToken` key
  from an existing chart Secret (#200).
- The Helm manifest heap bound is measured as live heap, so its test no
  longer fails on GC timing (#182).
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

- The Action prefixes every line it relays from the scanner (stderr, the
  Markdown summary, file names) with `| `, and escapes `%`, CR and LF in
  every input it echoes, so a pull request's file names or manifests
  cannot start a workflow command. The runner strips every Unicode
  whitespace character before it parses one, so the earlier check for a
  leading `::` could be passed with a form feed, a vertical tab or a
  no-break space. The release-tag lookup runs from an empty directory of
  its own, never the workspace, with repository discovery stopped there
  and a stall limit, so files a fork places in the workspace cannot
  redirect it or run a credential helper (#197).
- The SQLite database, and its `-wal` and `-shm` files, are created 0600
  under exactly the name given: a `%3F`, `%23` or `%00` in `--db` could
  bypass the `?`/`#` guard and leave a 0644 file under another name next
  to an empty 0600 decoy (#195).
- Release tags must point at a commit on `main` (release.yml preflight),
  and a pull request whose title or commits carry a `BREAKING CHANGE:`
  footer without `!` fails `pr-lint` (#198).
- Built with Go 1.26.8 and current dependencies. govulncheck finds 22
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
- One Helm release object can no longer exhaust the agent's memory. A
  951 KB release Secret whose gzip held 700 MiB took a scan to 1.93 GB and
  OOM-killed the agent at its 256Mi limit; anyone who can create a Secret
  or ConfigMap in one namespace could plant one, and a valid release built
  to amplify parsing (a 127 KiB Secret of tiny ConfigMaps took 564 MiB)
  could too. Each release payload is now decoded within 4 MiB stored and
  16 MiB decompressed (real releases decode to under 7 MiB), and its
  manifest is parsed in runs of at most 1 MiB and 64Ki YAML nodes; a
  single document over 2 MiB or 64Ki nodes is not parsed. The worst
  releases now peak at up to 47 MiB of live heap. A release over a bound is
  skipped (`release payload too large`) or recorded without that document,
  named on a partial `helm` capability, and the rest are still read
  (#168).
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
