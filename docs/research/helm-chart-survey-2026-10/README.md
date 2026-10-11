# Helm chart survey, October 2026

**The 50 most-starred Helm charts on Artifact Hub, rendered at default values
and judged against Kubernetes 1.37.**

!!! note "Preliminary dataset"
    This is the run of 2026-10-11 (UTC), made with upgradescope built from
    commit `2e524c9365b280802f6055276ff1662a771fda21` (knowledge base
    `k8s.io/api v0.37.1; lifecycle 91e89ab0; registry fe1fdf07`). The survey
    will be run again with the v0.3.0 registry data, and that run will replace
    these files. Numbers that come from the registry (the add-on findings) may
    change then. Numbers that come from helm (render failures and
    `kubeVersion` refusals) change only if the charts change.

The question: if you install today's most popular Helm charts at their
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

- **No chart uses a removed or deprecated Kubernetes API (upgradescope).**
  42 of the 50 charts rendered at `--kube-version 1.37.0` and were scanned.
  None had a `removed-api` finding or a `deprecated-api` finding. At default
  values, API removals are not what blocks these charts from 1.37.
- **4 of the 42 are blocked at 1.37 (upgradescope).** Every one of these
  blockers comes from add-on lifecycle data, and none comes from an API:
    - cert-manager 1.21.2 supports Kubernetes up to 1.36;
    - Istio 1.30.5 (chart `istiod`) supports up to 1.36;
    - Kyverno 1.19.1 (chart `kyverno` 3.9.1) supports up to 1.35;
    - Ingress NGINX (chart `ingress-nginx` 4.15.1, controller 1.15.1) has been
      end-of-life since 2026-03-24.
- **1 chart refuses 1.37 by its own `kubeVersion` (helm).** Rancher 2.15.2
  declares `kubeVersion: < 1.37.0-0`. `helm template --kube-version 1.37.0`
  refuses it, and the same command accepts `--kube-version 1.36.0`. Its 1.36
  render, scanned against 1.37, is `ready` (upgradescope). So the only thing
  holding Rancher back from 1.37 is its own version cap, which upgradescope's
  files mode cannot see.
- **5 charts do not render at default values (helm).** All 5 stop on a
  value they require: loki, gitlab, external-dns (kubernetes-sigs),
  aws-load-balancer-controller and sonarqube.
- **2 charts could not be downloaded (helm).** Artifact Hub lists
  kubernetes-dashboard 7.14.0 at `https://kubernetes.github.io/dashboard` and
  sealed-secrets 2.18.6 at `https://bitnami-labs.github.io/sealed-secrets/`.
  Both repositories answered `index.yaml` with 404 Not Found on the survey
  day.
- **At 1.36 the picture is a little better (upgradescope).** 43 charts
  rendered at `--kube-version 1.36.0`, Rancher among them. Scanned at
  `--target 1.36`, 2 were blocked (ingress-nginx and kyverno) and 41 were
  ready. cert-manager 1.21 and Istio 1.30 support 1.36, so they become
  blockers only at the move to 1.37.

Some things were not what we expected:

- **Version caps sit in the add-ons, not in the charts.** 17 of the 48
  charts that were downloaded declare a `kubeVersion`. Only Rancher sets an
  upper bound. The other 16 set a minimum only, and a minimum of
  `>= 1.22.0-0` lets helm install cert-manager 1.21.2 on Kubernetes 1.37,
  even though upstream supports that release only up to 1.36.
- **Bitnami charts default to `:latest` (from the rendered `image:` lines).**
  At default values, redis 28.3.1, postgresql 18.12.4, mongodb 20.0.0, nginx
  25.2.1 and wordpress 34.1.3 (with its mariadb) render
  `registry-1.docker.io/bitnami/<app>:latest`. Other Bitnami charts in the
  sample render dated tags, for example keycloak 25.2.0 with
  `docker.io/bitnami/keycloak:26.3.3-debian-12-r0`. Two charts from other
  publishers pull Bitnami images from `docker.io/bitnamilegacy/` through
  their dependencies: airflow 1.22.0 (`postgresql:16.1.0-debian-11-r15`) and
  gitea 12.7.0 (pgpool, postgresql-repmgr, valkey-cluster). This is not a
  Kubernetes upgrade question, and the survey did not check that any image
  can be pulled.
- **The registry knows the add-on but not the release (upgradescope,
  info).** It has no lifecycle data for these releases: kube-state-metrics
  2.20.0 (in kube-prometheus-stack and prometheus), Prometheus Operator
  0.94.1, Metrics Server 0.9.0, ExternalDNS 0.18.0 (the Bitnami chart) and
  Velero 1.18.2. Their end of life and compatibility were not assessed.
- **37 of the 42 scanned charts render at least one image that no add-on
  matcher recognises.** That is 71 distinct image repositories in all
  (upgradescope `unrecognizedImages`). Most are applications such as
  databases, which the add-on registry does not set out to cover. The table
  below lists the most common.

## Method

The harness is
[`hack/chart-survey.sh`](https://github.com/abd-ulbasit/upgradescope/blob/main/hack/chart-survey.sh),
and
[`hack/chart-survey_test.sh`](https://github.com/abd-ulbasit/upgradescope/blob/main/hack/chart-survey_test.sh)
tests its selection, error classification and aggregation offline. The
survey day's run used:

| Tool | Version |
|---|---|
| upgradescope | built from `2e524c9365b280802f6055276ff1662a771fda21` (`go build -ldflags "-X github.com/abd-ulbasit/upgradescope/internal/cli.version=dev -X github.com/abd-ulbasit/upgradescope/internal/cli.commit=<commit>" ./cmd/upgradescope`), knowledge base `k8s.io/api v0.37.1; lifecycle 91e89ab0; registry fe1fdf07` |
| helm | v4.1.4 (`GitCommit 05fa37973dc9e42b76e1d2883494c87174b6074f`, `KubeClientVersion v1.35`) |
| jq | 1.8.1 |

The run started at 2026-10-11T23:09:24Z and took 6 min 42 s on one machine.
`results.json` → `meta` holds the same facts, the complete `upgradescope
version` output among them.

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
   the 50 charts are the first 50 search results, from 1235 stars down to
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
| Incompatible add-on (blocker) | 3 |
| End-of-life add-on (blocker) | 1 |
| Custom resources at an unserved version (blocker) | 0 |
| Other blocker | 0 |
| Any blocker | 4 |
| Deprecated API only (no blocker) | 0 |
| Add-on approaching end of life (warning) | 1 |
| `kubeVersion` excludes 1.37 | 1 |
| ... of which accept 1.36 | 1 |
| Render failure at default values | 5 |
| Fetch failure | 2 |
| Scan failure | 0 |
| None of the above | 38 |

Render failures by class: required-value 5.

### Verdicts

| Verdict | At 1.37 (rendered at 1.37.0) | At 1.36 (rendered at 1.36.0) |
|---|---|---|
| blocked | 4 | 2 |
| ready | 38 | 41 |

### Charts with a blocker at 1.37

| Chart | Version | Category | Finding | Rendered file |
|---|---|---|---|---|
| cert-manager | 1.21.2 | chart-incompat | cert-manager 1.21.2 supports Kubernetes up to 1.36 (target 1.37) | - |
| ingress-nginx | 4.15.1 | eol-addon | Ingress NGINX Controller is end-of-life since 2026-03-24 | - |
| istiod | 1.30.5 | chart-incompat | Istio 1.30.5 supports Kubernetes up to 1.36 (target 1.37) | - |
| kyverno | 3.9.1 | chart-incompat | Kyverno 1.19.1 supports Kubernetes up to 1.35 (target 1.37) | - |

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
| Kyverno 1.19.1 supports Kubernetes up to 1.35 (target 1.37) | chart-incompat | blocker | 1 |
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

### Add-ons matched to the registry (from findings)

| Add-on | Finding category | Charts |
|---|---|---|
| kube-state-metrics | addon-no-data | 2 |
| cert-manager | chart-incompat | 1 |
| external-dns | addon-no-data | 1 |
| ingress-nginx | eol-addon | 1 |
| istio | chart-incompat | 1 |
| istio | eol-approaching | 1 |
| kyverno | chart-incompat | 1 |
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

71 distinct image repositories in 37 charts. The most common:

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
| 1 | kube-prometheus-stack | prometheus-community | 92.3.0 | 1235 | ok | ready | 0 | 0 | ready |
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
| 30 | velero | vmware-tanzu | 12.2.1 | 191 | ok | ready | 0 | 0 | ready |
| 31 | nextcloud | nextcloud | 9.4.0 | 190 | ok | ready | 0 | 0 | ready |
| 32 | minio | bitnami | 17.0.21 | 169 | ok | ready | 0 | 0 | ready |
| 33 | cluster-autoscaler | cluster-autoscaler | 9.59.0 | 168 | ok | ready | 0 | 0 | ready |
| 34 | metallb | metallb | 0.16.1 | 154 | ok | ready | 0 | 0 | ready |
| 35 | istiod | istio-official | 1.30.5 | 143 | ok | blocked | 1 | 1 | ready |
| 36 | rancher | rancher-stable | 2.15.2 | 141 | failed (kubeVersion) | - | 0 | 0 | ready |
| 37 | kyverno | kyverno | 3.9.1 | 140 | ok | blocked | 1 | 0 | blocked |
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

Before writing this up, we checked 8 charts from the final run against the
rendered YAML and the registry entry in `registry/data/`: 5 with findings and
3 without. All 8 agreed, and the harness needed no change.

| Chart | Finding | Rendered evidence | Registry entry |
|---|---|---|---|
| cert-manager 1.21.2 | chart-incompat, up to 1.36 | `cert-manager/templates/deployment.yaml:46` `image: "quay.io/jetstack/cert-manager-controller:v1.21.2"` | `cert-manager.yaml`: `range: ">=1.21.0 <1.22.0", k8s_min: "1.33", k8s_max: "1.36"` |
| ingress-nginx 4.15.1 | eol-addon | `ingress-nginx/templates/controller-deployment.yaml:39` `image: registry.k8s.io/ingress-nginx/controller:v1.15.1@sha256:594c...` | `ingress-nginx.yaml`: `status: eol`, `eol_date: "2026-03-24"` |
| istiod 1.30.5 | chart-incompat, up to 1.36; eol-approaching 2026-12-31 | `istiod/templates/deployment.yaml:57` `image: "registry.istio.io/release/pilot:1.30.5"`, labels `app: istiod`, `istio.io/rev: "default"` (the report says it was detected via labels) | `istio.yaml`: `cycle: "1.30", eol: "2026-12-31", k8s_min: "1.32", k8s_max: "1.36"` |
| kyverno 3.9.1 | chart-incompat, up to 1.35 | `kyverno/templates/admission-controller/deployment.yaml:59` `image: "reg.kyverno.io/kyverno/kyvernopre:v1.19.1"` | `kyverno.yaml`: `cycle: "1.19", eol: false, k8s_min: "1.33", k8s_max: "1.35"` |
| kube-prometheus-stack 92.3.0 | addon-no-data, Prometheus Operator 0.94.1 and kube-state-metrics 2.20.0 | the operator and kube-state-metrics Deployments | Both entries say `status: supported` and have no release-line cycles, so no row covers either version. `prometheus-operator.yaml` has only minimum compatibility rows (`>=0.84.0`, `k8s_min: "1.25"`). `kube-state-metrics.yaml` has no compatibility rows on purpose: upstream publishes a client-go build version, not a supported range |
| argo-cd 10.10.2 | none | 8 × `quay.io/argoproj/argocd:v3.5.4`, which is not in `unrecognizedImages`, so a matcher claimed it | `argo-cd.yaml`: `cycle: "3.5", eol: false`, and no Kubernetes range recorded. Nothing to report is correct |
| redis 28.3.1 | none | 2 × `registry-1.docker.io/bitnami/redis:latest`, in `unrecognizedImages`. The APIs are `apps/v1`, `policy/v1`, `networking.k8s.io/v1` and `v1` | no redis entry. Nothing to report is correct |
| traefik 41.7.1 | none | `docker.io/traefik:v3.7.14`, 25 `apiextensions.k8s.io/v1` CRDs, one `networking.k8s.io/v1` IngressClass | `traefik.yaml`: `cycle: "3.7", eol: false`, and no Kubernetes range recorded. Nothing to report is correct |

We also checked Rancher by hand. Its `Chart.yaml` reads
`kubeVersion: < 1.37.0-0`, and helm's error at 1.37.0 is
`chart requires kubeVersion: < 1.37.0-0 which is incompatible with Kubernetes v1.37.0`.

## Limitations

- **Stars, not installs.** Artifact Hub has no install counts. Stars measure
  attention, and the sample leans toward charts that people browse.
- **Default values only.** Real installs set values. A value can switch on
  templates that use other APIs, images or versions. 5 charts did not render
  at all without values, and their results are missing here.
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
  cilium 1.20.2 or fluent-bit, does not appear in it.
- **Registry coverage.** The registry used here had 27 add-on entries. An
  add-on outside it, or a release it has no row for, is not judged. The
  `addon-no-data` rows and the unrecognised images show where it ran out.
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
git checkout 2e524c9365b280802f6055276ff1662a771fda21
go build -ldflags "-X github.com/abd-ulbasit/upgradescope/internal/cli.version=dev \
  -X github.com/abd-ulbasit/upgradescope/internal/cli.commit=$(git rev-parse HEAD)" \
  -o /tmp/survey/upgradescope ./cmd/upgradescope
hack/chart-survey.sh run /tmp/survey 50 1.37 /tmp/survey/upgradescope
```

The run keeps every download, render, scan and helm error under the output
directory: `charts/<rank>-<repository>-<name>/`. That is the material for
checking a row by hand. Artifact Hub's order and the charts' latest versions
move, so a later run selects and renders what is current then.

Every number on this page can be recomputed from the committed
`results.json` with jq. For example:

```sh
jq '.summary.classes' results.json                      # the class counts
jq -r '.summary.blockerList[] | "\(.chart) \(.version): \(.title)"' results.json
jq -r '.charts[] | select(.render.ok == false) | "\(.name): \(.render.error)"' results.json
jq '[.charts[] | select((.chart.kubeVersion // "") | test("<")) | .name]' results.json
```

## Files

- [`results.json`](results.json) has these keys:
    - `meta`: the date, the tool versions and the commands;
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
