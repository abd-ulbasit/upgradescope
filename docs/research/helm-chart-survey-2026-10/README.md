# Helm chart survey, October 2026

**The 50 most-starred Helm charts on Artifact Hub, rendered at default values
and judged against Kubernetes 1.37.**

This is the run of 2026-10-12 (UTC), made with the published upgradescope
v0.3.0 release: `upgradescope_darwin_arm64.tar.gz` from the
[v0.3.0 release](https://github.com/abd-ulbasit/upgradescope/releases/tag/v0.3.0),
sha256 `91d3b5c1a98cc9b021cc770d8c1f3ca44d11237d7e3bc2b7bbb124bad3495b7f`,
commit `d946ecb06b698cb1e9371582be5338ca60847a24`, knowledge base
`k8s.io/api v0.37.1; lifecycle 91e89ab0; registry 80526ac2`. It replaces the
preliminary run of 2026-10-11; [what changed](#what-changed-since-the-preliminary-run)
says how and why.

The question: if you install today's most-starred Helm charts at their
default values, what stands between them and Kubernetes 1.37? That means
removed or deprecated APIs, end-of-life or incompatible components, custom
resources at versions that are no longer served, and charts whose own
`kubeVersion` excludes 1.37.

Artifact Hub publishes no install counts. The sample is the 50 most-starred
Helm charts, which is not the same as the 50 most installed or most used.

## What we found

Each number below names its source. **upgradescope** means the JSON report
of `upgradescope scan --files`. **helm** means `helm pull` or
`helm template`.

- **None of the 42 scanned charts uses a removed or deprecated Kubernetes
  API at default values (upgradescope).** 42 of the 50 charts rendered at
  `--kube-version 1.37.0` and were scanned. None had a `removed-api` finding
  or a `deprecated-api` finding. At default values, API removals are not
  what blocks these charts from 1.37. Custom resource definitions are
  another matter: kyverno 3.9.1 (17) and metallb 0.16.1 (1) have
  `crd-version` info findings for CRD versions their own CRDs mark
  deprecated. Those are the projects' APIs, not Kubernetes', and are not
  blockers.
- **3 of the 42 are blocked at 1.37 (upgradescope).** Every one of these
  blockers comes from add-on lifecycle data, and none comes from an API:
    - cert-manager 1.21.2 supports Kubernetes up to 1.36;
    - Istio 1.30.5 (chart `istiod`) supports up to 1.36;
    - Ingress NGINX (chart `ingress-nginx` 4.15.1, controller 1.15.1) has been
      end-of-life since 2026-03-24.
- **1 chart refuses 1.37 by its own `kubeVersion` (helm).** Rancher 2.15.2
  declares `kubeVersion: < 1.37.0-0`. `helm template --kube-version 1.37.0`
  refuses it, and the same command accepts `--kube-version 1.36.0`. Its 1.36
  render, scanned against 1.37, is `ready` (upgradescope): apart from its
  own version cap, the scan found nothing. The registry has no Rancher entry
  (`docker.io/rancher/rancher` and `docker.io/rancher/shell` are in
  `unrecognizedImages`), so `ready` means nothing known was found, not that
  Rancher itself was assessed. The cap, which upgradescope's files mode
  cannot see, is Rancher's own statement that 2.15 does not support 1.37.
- **5 charts do not render at default values (helm).** All 5 stop on a
  value they require: loki, gitlab, external-dns (kubernetes-sigs),
  aws-load-balancer-controller and sonarqube.
- **2 charts could not be downloaded (helm).** Artifact Hub lists
  kubernetes-dashboard 7.14.0 at `https://kubernetes.github.io/dashboard` and
  sealed-secrets 2.18.6 at `https://bitnami-labs.github.io/sealed-secrets/`.
  Both repositories answered `index.yaml` with 404 Not Found on the survey
  day, as they had the day before.
- **At 1.36 only ingress-nginx is blocked (upgradescope).** 43 charts
  rendered at `--kube-version 1.36.0`, Rancher among them. Scanned at
  `--target 1.36`, 1 was blocked (ingress-nginx, end of life) and 42 were
  ready. cert-manager 1.21 and Istio 1.30 support 1.36, so they become
  blockers only at the move to 1.37.
- **One add-on is approaching end of life (upgradescope, warning).** Istio
  1.30 reaches end of life on 2026-12-31.

Some things were not what we expected:

- **Version caps sit in the add-ons, not in the charts.** 17 of the 48
  charts that were downloaded declare a `kubeVersion`. Only Rancher sets an
  upper bound. The other 16 set a minimum only, and a minimum of
  `>= 1.22.0-0` lets helm install cert-manager 1.21.2 on Kubernetes 1.37,
  even though upstream supports that release only up to 1.36.
- **5 of the 12 Bitnami charts render `:latest` at default values (from the
  rendered `image:` lines).** They are redis 28.3.1, postgresql 18.12.4,
  mongodb 20.0.0, nginx 25.2.1 and wordpress 34.1.3 (with its mariadb), each
  rendering `registry-1.docker.io/bitnami/<app>:latest`. The other 7 Bitnami
  charts in the sample (keycloak, rabbitmq, kafka, external-dns, mysql, minio
  and thanos) render dated tags, for example keycloak 25.2.0 with
  `docker.io/bitnami/keycloak:26.3.3-debian-12-r0`. Two charts from other
  publishers pull Bitnami images from `docker.io/bitnamilegacy/` through
  their dependencies: airflow 1.22.0 (`postgresql:16.1.0-debian-11-r15`) and
  gitea 12.7.0 (pgpool, postgresql-repmgr, valkey-cluster). This is not a
  Kubernetes upgrade question, and the survey did not check that any image
  can be pulled.
- **The registry knows the add-on but not the release (upgradescope,
  info).** It has no release-line (end-of-life) data for these releases:
  kube-state-metrics 2.20.0 (in kube-prometheus-stack and prometheus),
  Prometheus Operator 0.94.1, Metrics Server 0.9.0, ExternalDNS 0.18.0 (the
  Bitnami chart) and Velero 1.18.2. Their end of life was not assessed.
- **37 of the 42 scanned charts render at least one image that no add-on
  matcher recognises.** That is 69 distinct image repositories in all
  (upgradescope `unrecognizedImages`). Most are applications such as
  databases, which the add-on registry does not set out to cover. The table
  below lists the most common.

## What changed since the preliminary run

The preliminary run (2026-10-11, upgradescope built from `2e524c93`,
registry `fe1fdf07`, 27 registry entries) and this run (2026-10-12,
v0.3.0, registry `80526ac2`, 34 entries) selected the same 50 charts, in
the same order, at the same versions: Artifact Hub's latest stable version
did not move for any of them. helm and jq were the same versions. Compared
record by record, every chart has the same appVersion, `kubeVersion`, fetch
and render outcome (with the same helm error where there was one) and the
same rendered image list in both runs. (The preliminary run's directory was
not kept, so the renders themselves could not be compared file by file.) So
every difference below comes from the registry, except the star counts.

| What changed | Preliminary | This run | Why |
|---|---|---|---|
| kyverno 3.9.1 (Kyverno 1.19.1) at 1.37 | blocked: "Kyverno 1.19.1 supports Kubernetes up to 1.35" | ready (score 75 to 100); the 17 `crd-version` infos are unchanged | v0.3.0's registry no longer encodes a Kubernetes range for Kyverno. Kyverno's [releases page](https://kyverno.io/docs/installation/releases/) gives "Kubernetes Versions Supported: v1.33 - v1.35" for 1.19 but says "Other Kubernetes versions may work, but are not tested and therefore no guarantees are made": a tested-with range, not a limit (`registry/data/kyverno.yaml`, `registry/CONTRIBUTING.md`, "Maximum, minimum or tested with"). The preliminary blocker was a false one. Kyverno has still not tested 1.19 on 1.37. |
| kyverno at 1.36 | blocked | ready | The same change: 1.36 is outside 1.33 to 1.35 too |
| Charts blocked at 1.37 | 4 | 3 | kyverno |
| Charts blocked at 1.36 | 2 | 1 | kyverno |
| Incompatible add-on class | 3 | 2 | kyverno |
| "None of the above" class | 38 | 39 | kyverno |
| Distinct unrecognised images | 71 | 69 | v0.3.0 adds a `longhorn` entry whose matchers claim `longhornio/longhorn-manager` and `longhornio/longhorn-ui`. Longhorn 1.13.0 is matched now and has no finding: its compat row needs Kubernetes 1.34 or later (no maximum) and the 1.13 line's end of life is 2027-11-29. The number of charts with an unrecognised image stays 37, since longhorn still renders `docker.io/longhornio/longhorn-share-manager`, which no matcher claims |
| Stars | kube-prometheus-stack 1235, velero 191 | 1236, 192 | Artifact Hub's counts moved overnight. The order did not change |
| "Most common findings" table | ends with metallb's BGPPeer | also lists `policies.kyverno.io/v1alpha1 DeletingPolicy` | Not a new finding: the table keeps the first 20 rows, and the Kyverno blocker row that left made room for a Kyverno `crd-version` info that was already in the preliminary data |

Three other new registry entries name charts in the sample and changed
nothing, for reasons the data shows:

- **cluster-autoscaler 9.59.0** renders no Deployment and no `image:` line
  at default values: its `templates/deployment.yaml` renders only when
  `autoDiscovery.clusterName`, `autoDiscovery.namespace`,
  `autoDiscovery.labels` or `autoscalingGroups` is set. There is nothing for
  the new `cluster-autoscaler` entry to match.
- **aws-load-balancer-controller 3.6.0** still fails to render at default
  values (a required value), so it was never scanned.
- **Istio** gained compat rows from istio.io beside the endoflife.date
  cycles. Both give 1.32 to 1.36 for 1.30, so the finding is the same.

The registry also re-read argo-cd, traefik, kube-state-metrics, ingress-nginx
and fluent-bit and changed only their comments and citations; their results
are the same.

## Method

The harness is
[`hack/chart-survey.sh`](https://github.com/abd-ulbasit/upgradescope/blob/main/hack/chart-survey.sh),
and
[`hack/chart-survey_test.sh`](https://github.com/abd-ulbasit/upgradescope/blob/main/hack/chart-survey_test.sh)
tests its selection, error classification and aggregation offline. The
survey day's run used:

| Tool | Version |
|---|---|
| upgradescope | 0.3.0, the published release archive `upgradescope_darwin_arm64.tar.gz` (sha256 `91d3b5c1a98cc9b021cc770d8c1f3ca44d11237d7e3bc2b7bbb124bad3495b7f`, matching the release's `checksums.txt`; `gh attestation verify` passed for source ref `refs/tags/v0.3.0`, commit `d946ecb06b698cb1e9371582be5338ca60847a24`), knowledge base `k8s.io/api v0.37.1; lifecycle 91e89ab0; registry 80526ac2` |
| helm | v4.1.4 (`GitCommit 05fa37973dc9e42b76e1d2883494c87174b6074f`, `KubeClientVersion v1.35`) |
| jq | 1.8.1 |

The run started at 2026-10-12T05:24:48Z and finished at
2026-10-12T05:31:23Z, on one machine. `results.json` → `meta` holds the same
facts, the complete `upgradescope version` output among them.

1. **Select.** Read
   `https://artifacthub.io/api/v1/packages/search?kind=0&sort=stars&limit=60&offset=0`
   (Helm charts, by stars, 60 to a page; one page was enough), then
   `https://artifacthub.io/api/v1/packages/helm/<repository>/<name>` for each
   result. Walking down the stars order:
    - drop deprecated packages;
    - drop `type: library` charts;
    - drop exact duplicates, meaning the same chart name, version and
      appVersion from another repository (the more-starred copy is kept);
    - keep the first 50.

   On the survey day, none of the top 50 was dropped (`dropped` is empty):
   the 50 charts are the first 50 search results, from 1236 stars down to
   102. Each chart is surveyed at its latest stable version.
   When Artifact Hub's latest version is a prerelease, the newest stable
   version is used instead. A version counts as a prerelease when it is
   flagged as one or has a semver suffix. Two charts were affected: cilium
   (latest `1.21.0-pre.3`, surveyed `1.20.2`) and istiod (latest
   `1.31.0-rc.0`, surveyed `1.30.5`). Both versions are in the record
   (`latestVersion`, `version`).
2. **Fetch.** For an HTTP repository, `helm pull --repo <url> <name> --version <version>`.
   For an OCI repository, `helm pull <oci-url> --version <version>`. All 50
   were HTTP. Then `helm show chart` reads `kubeVersion`, `type`,
   `appVersion` and the dependencies. Helm's cache, config and data
   directories were fresh ones under the output directory.
3. **Render.** At default values:
   `helm template survey <tgz> --namespace survey --include-crds --kube-version 1.37.0 --output-dir rendered-1.37`,
   and the same with `1.36.0`. A failure is classified from helm's error as
   one of three classes:
    - `kubeVersion`: the chart's constraint refuses the version;
    - `required-value`: a `required` or `fail` call asking for a value, or a
      values-schema error for a missing value or a null where a value is
      wanted;
    - `other`.

   Values were never edited, and no retry was made with a values file.
4. **Scan.**
    - `upgradescope scan --files rendered-1.37 --target 1.37 --output json --fail-on never`;
    - `upgradescope scan --files rendered-1.36 --target 1.36 --output json --fail-on never`;
    - for a chart whose `kubeVersion` refuses 1.37, its 1.36 render scanned
      with `--target 1.37` as well.
5. **Aggregate.** `hack/chart-survey.sh aggregate` counts the charts in each
   class. Within the blocker classes, a chart counts once per class it has a
   blocker in. It also tallies the findings and the add-ons they name, and
   writes `results.json`, `results.csv` and the tables below.

A chart that cannot be fetched, rendered or scanned is a data point. The
harness fails only when the harness itself fails. No cluster was contacted
(`KUBECONFIG=/dev/null`). The only network traffic was read-only downloads
from Artifact Hub's API and the chart repositories.

## Results

The tables below are `tables.md` as `hack/chart-survey.sh aggregate` wrote
it from the run, unedited.

### Charts by class (50 charts, target 1.37)

A chart can be in more than one blocker class.

| Class | Charts |
|---|---|
| Removed API (blocker) | 0 |
| Incompatible add-on (blocker) | 2 |
| End-of-life add-on (blocker) | 1 |
| Custom resources at an unserved version (blocker) | 0 |
| Other blocker | 0 |
| Any blocker | 3 |
| Deprecated API only (no blocker) | 0 |
| Add-on approaching end of life (warning) | 1 |
| `kubeVersion` excludes 1.37 | 1 |
| ... of which accept 1.36 | 1 |
| Render failure at default values | 5 |
| Fetch failure | 2 |
| Scan failure | 0 |
| None of the above | 39 |

Render failures by class: required-value 5.

### Verdicts

| Verdict | At 1.37 (rendered at 1.37.0) | At 1.36 (rendered at 1.36.0) |
|---|---|---|
| blocked | 3 | 1 |
| ready | 39 | 42 |

### Charts with a blocker at 1.37

| Chart | Version | Category | Finding | Rendered file |
|---|---|---|---|---|
| cert-manager | 1.21.2 | chart-incompat | cert-manager 1.21.2 supports Kubernetes up to 1.36 (target 1.37) | - |
| ingress-nginx | 4.15.1 | eol-addon | Ingress NGINX Controller is end-of-life since 2026-03-24 | - |
| istiod | 1.30.5 | chart-incompat | Istio 1.30.5 supports Kubernetes up to 1.36 (target 1.37) | - |

### Charts whose `kubeVersion` excludes 1.37

| Chart | Version | `kubeVersion` | Accepts 1.36 | 1.36 render scanned at 1.37 |
|---|---|---|---|---|
| rancher | 2.15.2 | `< 1.37.0-0` | yes | ready |

### Most common findings

| Finding (first title seen) | Category | Severity | Charts |
|---|---|---|---|
| no lifecycle data for kube-state-metrics 2.20.0 | addon-no-data | info | 2 |
| no lifecycle data for ExternalDNS 0.18.0 | addon-no-data | info | 1 |
| no lifecycle data for Kubernetes Metrics Server 0.9.0 | addon-no-data | info | 1 |
| no lifecycle data for Prometheus Operator 0.94.1 | addon-no-data | info | 1 |
| no lifecycle data for Velero 1.18.2 | addon-no-data | info | 1 |
| cert-manager 1.21.2 supports Kubernetes up to 1.36 (target 1.37) | chart-incompat | blocker | 1 |
| Istio 1.30.5 supports Kubernetes up to 1.36 (target 1.37) | chart-incompat | blocker | 1 |
| kyverno.io/v1 ClusterPolicy is a deprecated CRD version | crd-version | info | 1 |
| kyverno.io/v1 Policy is a deprecated CRD version | crd-version | info | 1 |
| kyverno.io/v2 CleanupPolicy is a deprecated CRD version | crd-version | info | 1 |
| kyverno.io/v2 ClusterCleanupPolicy is a deprecated CRD version | crd-version | info | 1 |
| kyverno.io/v2 PolicyException is a deprecated CRD version | crd-version | info | 1 |
| kyverno.io/v2alpha1 GlobalContextEntry is a deprecated CRD version | crd-version | info | 1 |
| kyverno.io/v2beta1 CleanupPolicy is a deprecated CRD version | crd-version | info | 1 |
| kyverno.io/v2beta1 ClusterCleanupPolicy is a deprecated CRD version | crd-version | info | 1 |
| kyverno.io/v2beta1 ClusterPolicy is a deprecated CRD version | crd-version | info | 1 |
| kyverno.io/v2beta1 Policy is a deprecated CRD version | crd-version | info | 1 |
| kyverno.io/v2beta1 PolicyException is a deprecated CRD version | crd-version | info | 1 |
| metallb.io/v1beta1 BGPPeer is a deprecated CRD version | crd-version | info | 1 |
| policies.kyverno.io/v1alpha1 DeletingPolicy is a deprecated CRD version | crd-version | info | 1 |

### Add-ons matched to the registry (from findings)

| Add-on | Finding category | Charts |
|---|---|---|
| kube-state-metrics | addon-no-data | 2 |
| cert-manager | chart-incompat | 1 |
| external-dns | addon-no-data | 1 |
| ingress-nginx | eol-addon | 1 |
| istio | chart-incompat | 1 |
| istio | eol-approaching | 1 |
| metrics-server | addon-no-data | 1 |
| prometheus-operator | addon-no-data | 1 |
| velero | addon-no-data | 1 |

### Charts that render an image by a `latest` tag or by no tag

From the `image:` lines of the render at 1.37.0 (a grep, see `images` in the record); digest-pinned references are left out.

| Chart | Version | Images |
|---|---|---|
| redis | 28.3.1 | `registry-1.docker.io/bitnami/redis:latest` |
| postgresql | 18.12.4 | `registry-1.docker.io/bitnami/postgresql:latest` |
| mongodb | 20.0.0 | `registry-1.docker.io/bitnami/mongodb:latest` |
| kyverno | 3.9.1 | `ghcr.io/kyverno/readiness-checker:latest` |
| gitea | 12.7.0 | `busybox:latest` |
| fluent-bit | 0.58.3 | `busybox:latest` |
| nginx | 25.2.1 | `registry-1.docker.io/bitnami/nginx:latest` |
| wordpress | 34.1.3 | `registry-1.docker.io/bitnami/mariadb:latest`, `registry-1.docker.io/bitnami/wordpress:latest` |

### Images no add-on matcher recognised

69 distinct image repositories in 37 charts. The most common:

| Image repository | Charts |
|---|---|
| docker.io/library/busybox | 3 |
| docker.io/bats/bats | 2 |
| docker.io/bitnami/postgresql | 2 |
| quay.io/prometheus/node-exporter | 2 |
| docker.elastic.co/elasticsearch/elasticsearch | 1 |
| docker.gitea.com/gitea | 1 |
| docker.io/apache/airflow | 1 |
| docker.io/bitnami/kafka | 1 |
| docker.io/bitnami/keycloak | 1 |
| docker.io/bitnami/mariadb | 1 |
| docker.io/bitnami/minio | 1 |
| docker.io/bitnami/minio-object-browser | 1 |
| docker.io/bitnami/mongodb | 1 |
| docker.io/bitnami/mysql | 1 |
| docker.io/bitnami/nginx | 1 |
| docker.io/bitnami/rabbitmq | 1 |
| docker.io/bitnami/redis | 1 |
| docker.io/bitnami/thanos | 1 |
| docker.io/bitnami/wordpress | 1 |
| docker.io/bitnamilegacy/pgpool | 1 |
| docker.io/bitnamilegacy/postgresql | 1 |
| docker.io/bitnamilegacy/postgresql-repmgr | 1 |
| docker.io/bitnamilegacy/valkey-cluster | 1 |
| docker.io/dpage/pgadmin4 | 1 |
| docker.io/goharbor/harbor-core | 1 |

### Every chart

| # | Chart | Repository | Version | Stars | Render at 1.37.0 | Verdict at 1.37 | Blockers | Warnings | Verdict at 1.36 |
|---|---|---|---|---|---|---|---|---|---|
| 1 | kube-prometheus-stack | prometheus-community | 92.3.0 | 1236 | ok | ready | 0 | 0 | ready |
| 2 | cert-manager | cert-manager | 1.21.2 | 997 | ok | blocked | 1 | 0 | ready |
| 3 | argo-cd | argo | 10.10.2 | 846 | ok | ready | 0 | 0 | ready |
| 4 | ingress-nginx | ingress-nginx | 4.15.1 | 822 | ok | blocked | 1 | 0 | blocked |
| 5 | prometheus | prometheus-community | 29.36.1 | 555 | ok | ready | 0 | 0 | ready |
| 6 | redis | bitnami | 28.3.1 | 507 | ok | ready | 0 | 0 | ready |
| 7 | traefik | traefik | 41.7.1 | 431 | ok | ready | 0 | 0 | ready |
| 8 | postgresql | bitnami | 18.12.4 | 430 | ok | ready | 0 | 0 | ready |
| 9 | kubernetes-dashboard | k8s-dashboard | 7.14.0 | 365 | fetch failed | - | 0 | 0 | - |
| 10 | loki | grafana | 7.3.0 | 333 | failed (required-value) | - | 0 | 0 | - |
| 11 | metrics-server | metrics-server | 3.14.0 | 320 | ok | ready | 0 | 0 | ready |
| 12 | vault | hashicorp | 0.34.1 | 298 | ok | ready | 0 | 0 | ready |
| 13 | gitlab | gitlab | 10.4.1 | 282 | failed (required-value) | - | 0 | 0 | - |
| 14 | harbor | harbor | 1.19.2 | 275 | ok | ready | 0 | 0 | ready |
| 15 | keycloak | bitnami | 25.2.0 | 275 | ok | ready | 0 | 0 | ready |
| 16 | jenkins | jenkinsci | 5.9.67 | 255 | ok | ready | 0 | 0 | ready |
| 17 | external-secrets | external-secrets-operator | 2.12.0 | 249 | ok | ready | 0 | 0 | ready |
| 18 | external-dns | external-dns | 1.23.0 | 240 | failed (required-value) | - | 0 | 0 | - |
| 19 | longhorn | longhorn | 1.13.0 | 225 | ok | ready | 0 | 0 | ready |
| 20 | rabbitmq | bitnami | 16.0.14 | 215 | ok | ready | 0 | 0 | ready |
| 21 | gitlab-runner | gitlab | 0.93.0 | 213 | ok | ready | 0 | 0 | ready |
| 22 | aws-load-balancer-controller | aws | 3.6.0 | 210 | failed (required-value) | - | 0 | 0 | - |
| 23 | kafka | bitnami | 32.4.3 | 204 | ok | ready | 0 | 0 | ready |
| 24 | cilium | cilium | 1.20.2 | 201 | ok | ready | 0 | 0 | ready |
| 25 | external-dns | bitnami | 9.0.3 | 200 | ok | ready | 0 | 0 | ready |
| 26 | airflow | apache-airflow | 1.22.0 | 198 | ok | ready | 0 | 0 | ready |
| 27 | mysql | bitnami | 14.0.3 | 197 | ok | ready | 0 | 0 | ready |
| 28 | elasticsearch | elastic | 8.5.1 | 193 | ok | ready | 0 | 0 | ready |
| 29 | mongodb | bitnami | 20.0.0 | 193 | ok | ready | 0 | 0 | ready |
| 30 | velero | vmware-tanzu | 12.2.1 | 192 | ok | ready | 0 | 0 | ready |
| 31 | nextcloud | nextcloud | 9.4.0 | 190 | ok | ready | 0 | 0 | ready |
| 32 | minio | bitnami | 17.0.21 | 169 | ok | ready | 0 | 0 | ready |
| 33 | cluster-autoscaler | cluster-autoscaler | 9.59.0 | 168 | ok | ready | 0 | 0 | ready |
| 34 | metallb | metallb | 0.16.1 | 154 | ok | ready | 0 | 0 | ready |
| 35 | istiod | istio-official | 1.30.5 | 143 | ok | blocked | 1 | 1 | ready |
| 36 | rancher | rancher-stable | 2.15.2 | 141 | failed (kubeVersion) | - | 0 | 0 | ready |
| 37 | kyverno | kyverno | 3.9.1 | 140 | ok | ready | 0 | 0 | ready |
| 38 | cloudnative-pg | cloudnative-pg | 0.29.1 | 140 | ok | ready | 0 | 0 | ready |
| 39 | argo-workflows | argo | 2.0.12 | 139 | ok | ready | 0 | 0 | ready |
| 40 | gitea | gitea | 12.7.0 | 135 | ok | ready | 0 | 0 | ready |
| 41 | sonarqube | sonarqube | 2026.5.1002 | 135 | failed (required-value) | - | 0 | 0 | - |
| 42 | fluent-bit | fluent | 0.58.3 | 128 | ok | ready | 0 | 0 | ready |
| 43 | datadog | datadog | 3.253.2 | 117 | ok | ready | 0 | 0 | ready |
| 44 | oauth2-proxy | oauth2-proxy | 10.7.1 | 112 | ok | ready | 0 | 0 | ready |
| 45 | pgadmin4 | runix | 1.70.4 | 112 | ok | ready | 0 | 0 | ready |
| 46 | nginx | bitnami | 25.2.1 | 107 | ok | ready | 0 | 0 | ready |
| 47 | thanos | bitnami | 17.3.1 | 107 | ok | ready | 0 | 0 | ready |
| 48 | wordpress | bitnami | 34.1.3 | 107 | ok | ready | 0 | 0 | ready |
| 49 | authentik | goauthentik | 2026.8.3 | 105 | ok | ready | 0 | 0 | ready |
| 50 | sealed-secrets | bitnami-labs | 2.18.6 | 102 | fetch failed | - | 0 | 0 | - |

## Checked by hand

After the run, we checked 12 charts by hand: 7 with findings and 5 without.
For each, we rendered the downloaded archive again with
`helm template survey <tgz> --namespace survey --include-crds --kube-version 1.37.0`
(fresh helm directories, `KUBECONFIG=/dev/null`) and scanned that render with
the v0.3.0 binary (`scan --files --target 1.37 --output json --fail-on never`).
For all 12, the finding keys matched the record exactly. The new renders
matched the run's file for file, except in kube-prometheus-stack and redis,
where only generated passwords (and kube-prometheus-stack's `checksum/secret`
annotation over them) differ. We then read each finding against the
rendered YAML line and the registry entry in `registry/data/` at v0.3.0.
All 12 agreed, and the harness needed no change.

| Chart | Finding | Rendered evidence | Registry entry |
|---|---|---|---|
| cert-manager 1.21.2 | chart-incompat, up to 1.36 | `cert-manager/templates/deployment.yaml:46` `image: "quay.io/jetstack/cert-manager-controller:v1.21.2"` | `cert-manager.yaml:47`: `range: ">=1.21.0 <1.22.0", k8s_min: "1.33", k8s_max: "1.36"` |
| ingress-nginx 4.15.1 | eol-addon | `ingress-nginx/templates/controller-deployment.yaml:39` `image: registry.k8s.io/ingress-nginx/controller:v1.15.1@sha256:594c...` | `ingress-nginx.yaml:42,45`: `status: eol`, `eol_date: "2026-03-24"` |
| istiod 1.30.5 | chart-incompat, up to 1.36; eol-approaching 2026-12-31 | `istiod/templates/deployment.yaml:57` `image: "registry.istio.io/release/pilot:1.30.5"`, labels `app: istiod`, `istio.io/rev: "default"` (lines 10-11; the report says it was detected via labels) | `istio.yaml:35`: `cycle: "1.30", eol: "2026-12-31", k8s_min: "1.32", k8s_max: "1.36"`, and the compat row at line 73 agrees (1.32 to 1.36) |
| kube-prometheus-stack 92.3.0 | addon-no-data, Prometheus Operator 0.94.1 and kube-state-metrics 2.20.0 | `templates/prometheus-operator/deployment.yaml:44` `quay.io/prometheus-operator/prometheus-operator:v0.94.1`; `charts/kube-state-metrics/templates/deployment.yaml:54` `registry.k8s.io/kube-state-metrics/kube-state-metrics:v2.20.0` | Both entries say `status: supported` and have no release-line cycles, so no row gives an end of life for either version, which is what the finding says. `kube-state-metrics.yaml` has no compatibility rows on purpose: upstream publishes a client-go build version, not a supported range |
| velero 12.2.1 | addon-no-data, Velero 1.18.2 | `velero/templates/deployment.yaml:44` `image: "docker.io/velero/velero:v1.18.2"` | `velero.yaml:25`: compat row `>=1.14.0 <1.19.0`, `k8s_min: "1.18"` (open-ended, so 1.37 is fine) and no cycles, so no end of life to assess |
| kyverno 3.9.1 | 17 crd-version infos, no blocker | `kyverno/templates/admission-controller/deployment.yaml:107` `image: "reg.kyverno.io/kyverno/kyverno:v1.19.1"`; `charts/crds/templates/kyverno.io/kyverno.io_clusterpolicies.yaml:65-69` `deprecated: true` on `name: v1` | `kyverno.yaml:46`: `cycle: "1.19", eol: false` with no Kubernetes range (the `eol-sync: ignore-k8s-range` directive, line 6). No blocker is correct for v0.3.0's data; the preliminary blocker came from the range that was removed |
| metallb 0.16.1 | crd-version info, metallb.io/v1beta1 BGPPeer | `charts/crds/templates/crds.yaml:428-430` `deprecated: true` on `name: v1beta1` of `bgppeers.metallb.io` | not a registry finding: the CRD itself marks the version deprecated |
| longhorn 1.13.0 | none (newly matched) | `longhorn/templates/daemonset-sa.yaml:53` `image: docker.io/longhornio/longhorn-manager:v1.13.0`, `deployment-ui.yaml:68` `longhornio/longhorn-ui:v1.13.0`; neither is in `unrecognizedImages` now | `longhorn.yaml:60`: `range: ">=1.13.0", k8s_min: "1.34"`; line 64: `cycle: "1.13", eol: "2027-11-29"`. Nothing to report is correct |
| argo-cd 10.10.2 | none | 8 × `quay.io/argoproj/argocd:v3.5.4`, which is not in `unrecognizedImages`, so a matcher claimed it | `argo-cd.yaml:30`: `cycle: "3.5", eol: false`, and no Kubernetes range recorded. Nothing to report is correct |
| redis 28.3.1 | none | `redis/templates/master/application.yaml:67` `registry-1.docker.io/bitnami/redis:latest`, in `unrecognizedImages` | no redis entry. Nothing to report is correct |
| traefik 41.7.1 | none | `traefik/templates/deployment.yaml:68` `image: docker.io/traefik:v3.7.14` | `traefik.yaml:29`: `cycle: "3.7", eol: false`, and no Kubernetes range recorded. Nothing to report is correct |
| cluster-autoscaler 9.59.0 | none | no `image:` line at all: the render holds only RBAC, a PodDisruptionBudget, a Service and a ServiceAccount, because the Deployment needs `autoDiscovery.clusterName` or `autoscalingGroups` | `cluster-autoscaler.yaml` (new in v0.3.0) has nothing to match. Nothing to report is correct, and it says nothing about Cluster Autoscaler itself |

We also checked Rancher by hand. Its `Chart.yaml` reads
`kubeVersion: < 1.37.0-0`; `helm template --kube-version 1.37.0` exits 1
with `chart requires kubeVersion: < 1.37.0-0 which is incompatible with Kubernetes v1.37.0`,
and `--kube-version 1.36.0` exits 0.

## Limitations

- **Stars, not installs.** Artifact Hub has no install counts. Stars measure
  attention, and the sample leans toward charts that people browse.
- **Default values only.** Real installs set values. A value can switch on
  templates that use other APIs, images or versions. 5 charts did not render
  at all without values, and their results are missing here. Others render
  less than a real install: cluster-autoscaler renders no Deployment.
- **Helm's built-in capabilities.** Without a cluster, `helm template` fills
  `.Capabilities.APIVersions` with its own built-in list. Here that was helm
  4.1.4, client v1.35. We set `.Capabilities.KubeVersion` with
  `--kube-version` but passed no `--api-versions`. A chart that picks an API
  version from `.Capabilities.APIVersions.Has` may therefore have rendered a
  different version from the one it would install on a real 1.37 cluster.
  That can cause a false blocker or hide a real one (see
  [Scan rendered manifests](../../getting-started/cli.md#scan-rendered-manifests)).
- **What files mode can see.** A `--files` scan assesses API usage, add-ons
  (from images, labels and IngressClass controllers) and CRD versions. It
  does not assess version skew, Helm releases or deprecated API callers.
  It cannot read a chart's `kubeVersion` either, which is why Rancher's cap
  comes from helm.
- **Add-ons with no finding are not listed.** The JSON report names an
  add-on only when it produces a finding, and an add-on finding names no
  object or file. The add-on table therefore counts findings, not matches.
  An add-on that was matched and found fine, such as argo-cd, traefik,
  cilium 1.20.2, fluent-bit, kyverno or longhorn, does not appear in it.
- **Registry coverage.** The registry used here had 34 add-on entries (the
  `.yaml` files in `registry/data/` at v0.3.0, commit `d946ecb0`). An
  add-on outside it, or a release it has no row for, is not judged. The
  `addon-no-data` rows and the unrecognised images show where it ran out.
- **Ready is not tested.** The registry encodes only stated minimums and
  maximums, never a tested-with range. A `ready` verdict means no stated
  limit is crossed, not that upstream has tested the release on 1.37:
  Kyverno 1.19, for example, is tested on 1.33 to 1.35.
- **Image tags come from a grep.** The `images` lists and the `:latest`
  table come from the `image:` lines of the rendered YAML, skipping values
  that are still template text. They are not from the scan. A ConfigMap that
  holds YAML text would count too. No image was pulled or checked.
- **One day, one machine.** Stars, latest versions and repositories change.
  Two repositories were gone on the survey day. A re-run on another day is a
  new survey, not a reproduction.

## Reproduce

```sh
export KUBECONFIG=/dev/null          # no cluster is needed; make sure none is contacted
mkdir -p /tmp/survey/bin && cd /tmp/survey
base=https://github.com/abd-ulbasit/upgradescope/releases/download/v0.3.0
curl -fsSLO "$base/upgradescope_darwin_arm64.tar.gz"    # or the archive for your platform
curl -fsSLO "$base/checksums.txt"
shasum -a 256 -c --ignore-missing checksums.txt
gh attestation verify upgradescope_darwin_arm64.tar.gz -R abd-ulbasit/upgradescope \
  --source-ref refs/tags/v0.3.0
tar -xzf upgradescope_darwin_arm64.tar.gz -C bin
bin/upgradescope version                # 0.3.0, commit d946ecb0...
# from a checkout of this repository:
hack/chart-survey.sh run /tmp/survey/run 50 1.37 /tmp/survey/bin/upgradescope
```

The run keeps every download, render, scan and helm error under the output
directory: `charts/<rank>-<repository>-<name>/`. That is the material for
checking a row by hand. Artifact Hub's order and the charts' latest versions
move, so a later run selects and renders what is current then.

Every count and table in "What we found" and "Results" can be recomputed
from the committed `results.json` with jq. The other numbers have other
sources: the tool versions and the start and end times are in
`results.json` → `meta`, the archive's sha256 is in the release's
`checksums.txt`, the registry entry counts are in `registry/data/` at
`2e524c93` (27) and v0.3.0 (34), the preliminary numbers are in this file's
history (`git log -p -- docs/research/helm-chart-survey-2026-10/results.json`),
and the line numbers under "Checked by hand" are in the run directory's
renders, which are not committed. For example:

```sh
jq '.summary.classes' results.json                      # the class counts
jq -r '.summary.blockerList[] | "\(.chart) \(.version): \(.title)"' results.json
jq -r '.charts[] | select(.render.ok == false) | "\(.name): \(.render.error)"' results.json
jq '[.charts[] | select((.chart.kubeVersion // "") | test("<")) | .name]' results.json
jq '[.charts[] | select(.repository == "bitnami") | any(.images[]?; endswith(":latest"))] | group_by(.) | map({latest: .[0], charts: length})' results.json
```

## Files

- [`results.json`](results.json) has these keys:
    - `meta`: the date, the start time, the tool versions and the
      commands, and the end time (`finishedAt`);
    - `dropped`: the selection's drop log;
    - `summary`: every count and table above;
    - `charts`: one record per chart. A record holds its Artifact Hub
      metadata (stars, repository URL and kind, official and verified
      publisher flags, version and latest version), its `Chart.yaml` facts,
      the fetch and render results with helm's error and class, the scan at
      1.37 (verdict, score, and each finding's category, severity, key,
      title, detail and first objects, plus what was not assessed and the
      unrecognised images), the scan at 1.36, and the render's image
      references.
- [`results.csv`](results.csv) has one row per chart, with the columns
  `rank`, `repository`, `name`, `version`, `appVersion`, `stars`,
  `repoKind`, `official`, `verifiedPublisher`, `chartType`, `kubeVersion`,
  `fetchOk`, `renderOk`, `renderClass`, `acceptsPrev`, `verdict`, `score`,
  `blockers`, `warnings`, `infos`, `blockerCategories`, `findingKeys`,
  `addons`, `prevVerdict` and `prevBlockers`.
