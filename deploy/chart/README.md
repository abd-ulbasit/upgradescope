# upgradescope Helm chart

Installs the upgradescope agent (continuous upgrade-readiness scanning,
results in a `ClusterReadiness` custom resource) and, optionally, the
upgradescope server (snapshot history, what-if API, Slack notifications).

## Install

Agent only (CRD-only mode — no server, results via `kubectl`):

    helm install upgradescope deploy/chart -n upgradescope --create-namespace
    kubectl get clusterreadiness cluster

Agent + server in one cluster:

    helm install upgradescope deploy/chart -n upgradescope --create-namespace \
      --set server.enabled=true

With no `server.ingestToken`, the chart generates a random token into the
`<fullname>-server-tokens` Secret, keeps it on `helm upgrade` (it looks the
live Secret up), and points the in-chart agent at it. The install notes
print the command that reads it back for agents in other clusters. Tools
that render without cluster access (`helm template`, Argo CD) cannot look
it up and would generate a new token on every render, so there set
`server.ingestToken` or `server.existingSecret`.

Reach the server, which has a ClusterIP Service only:

    kubectl -n upgradescope port-forward svc/upgradescope-server 8080:8080
    SERVER=http://127.0.0.1:8080
    curl $SERVER/api/v1/clusters

Agent pushing to an existing server elsewhere:

    helm install upgradescope deploy/chart -n upgradescope --create-namespace \
      --set agent.serverUrl=https://uscope.example.com \
      --set agent.existingSecret=my-push-token   # key: serverToken

## RBAC: what the agent can do, and why

The agent ClusterRole (`templates/rbac.yaml`) lists every resource it
covers. It has no wildcard groups, resources or verbs, so it never covers
subresources such as `nodes/proxy` (kubelet exec), `pods/log` or
`pods/exec`. Reads are `get`/`list` only; the agent polls and never
watches.

| Rule | Why |
|---|---|
| `get`/`list` namespaces, nodes, pods | Cluster ID (kube-system UID), team labels, kubelet versions, control-plane pods, add-on images |
| `get` `/version`, `/metrics` | Server version; `apiserver_requested_deprecated_apis` (who still calls deprecated APIs) |
| `get`/`list` on each group/resource the KB flags as deprecated or removed | Counting objects still stored at deprecated APIs. Generated into `files/kb-rbac-rules.yaml`; `rbac_test.go` fails when it drifts from the embedded KB |
| `get`/`list` Secrets (only with `rbac.helmSecrets=true`, the default) | Helm release detection reads Secrets of type `helm.sh/release.v1`. RBAC cannot filter by type, so **this lets the agent read every Secret in the cluster** |
| `get`/`update`/`patch` on the CRD `clusterreadinesses.upgradescope.dev` only (only with `agent.manageCRD=true`, the default) | Keeping the CRD schema in step with the agent binary by server-side apply |
| `get`/`list`/`create` clusterreadinesses; `update`/`patch` and status `get`/`update`/`patch` on the one named `agent.crName` | The agent's own results object |

`rbac.helmSecrets=false` removes the Secret rule. The Helm capability is
then not assessed, with the forbidden Secret list as the reason, and the
report has no Helm chart findings; everything else works. `rbac.create=false` lets you bind a
role of your own; collectors without access degrade the same way.

The agent cannot create CRDs: `crds/` installs the `ClusterReadiness` CRD.
With `agent.manageCRD=true` (the default), a missing CRD makes the agent
exit at startup with an error that says so. With `agent.manageCRD=false`
the agent never reads or writes the CRD itself, so a missing CRD shows up
as a "CRD not installed" error on every tick instead.

`agent.manageCRD=false` drops the CRD write permissions (`get`/`update`/
`patch` on `clusterreadinesses.upgradescope.dev`), e.g. when GitOps manages
the CRD. It does not remove all CRD access: the KB-derived read rule still
grants `get`/`list` on every CRD, because the KB flags
`apiextensions.k8s.io/v1beta1` and the api-usage collector counts CRDs.
With `agent.manageCRD=true`, the agent applies only the fields it owns, so
labels and annotations that Argo CD, Flux or `kubectl apply` set on the
CRD survive restarts.

`deploy/chart/rbac_test.go` renders this role and checks it with the
upstream RBAC rule matcher: every call the collectors make is allowed, and
`nodes/proxy`, `pods/log`, `pods/exec`, other CRDs, other ClusterReadiness
objects, `watch`, and Secrets with the toggle off are denied.

## Target versions

The chart never renders the `ClusterReadiness` object; the agent creates
it. `agent.targets` is passed to the agent as `--targets`, and while it is
non-empty the agent resets `spec.targets` to it every tick, so the Helm
value wins over `kubectl edit`. Values are `MAJOR.MINOR` (`1.37`), the
format the CRD accepts; the schema rejects `v1.37` and `1.37.2`. Changing
it with `helm upgrade` touches only the agent Deployment. With
`agent.targets` empty, set targets on the object directly:

    kubectl patch ucr cluster --type merge -p '{"spec":{"targets":["1.37"]}}'

Emptying `agent.targets` after it was set leaves `spec.targets` at the last
list (an empty flag means "leave the object alone"). To return to the
default next minor, clear it with the same command and `"targets":[]`.

Upgrading from a chart version that rendered the object (agent.targets
was set): Helm deletes it as no longer part of the release, and the agent
recreates it with the same targets on a later tick. That can take up to
`agent.interval` (10m by default) when the new agent pod ticks before Helm
deletes the old object.

## Secrets

Tokens and webhook URLs reach the containers only as environment variables
from `secretKeyRef`, never as arguments, and never appear in a Deployment.

| Env var | Secret key | Source |
|---|---|---|
| `UPGRADESCOPE_INGEST_TOKEN` (server) | `ingestToken` | `server.ingestToken`, generated, or `server.existingSecret` |
| `UPGRADESCOPE_READ_TOKEN` (server) | `readToken` | `server.readToken`, or `server.existingSecret` with `server.readTokenFromSecret=true` |
| `UPGRADESCOPE_SLACK_WEBHOOK` (server) | `slackWebhook` | `server.slackWebhook`, or optional key of `server.existingSecret` |
| `UPGRADESCOPE_WEBHOOK_URL` (server) | `webhook` | `server.webhook`, or optional key of `server.existingSecret` |
| `UPGRADESCOPE_SERVER_TOKEN` (agent) | `serverToken`, or the server's `ingestToken` | `agent.existingSecret`, `agent.serverToken`, else the in-chart server's Secret |

`server.existingSecret` alone is enough for a combined install: the server
and the in-chart agent both use its `ingestToken` key.

## Read API exposure

With `server.enabled=true` and no read token, the read API is
**unauthenticated** behind the ClusterIP Service. Set `server.readToken`
(or `readTokenFromSecret`) before exposing it via Ingress/LoadBalancer.
`networkPolicy.enabled=true` admits traffic to the server only from this
release's agent and the peers in `networkPolicy.serverIngressFrom`.

The server also serves the web dashboard at `/` (images built from the
repo Dockerfile embed it). Static assets are unauthenticated; every API
call the dashboard makes carries the read token you set in its header
settings, so the read token still protects all data.

## Production settings

- Private registry: `imagePullSecrets: [{name: regcred}]`.
- Scheduling, per component (`agent.*`, `server.*`): `nodeSelector`,
  `tolerations`, `affinity`, `priorityClassName`, `podAnnotations`,
  `podLabels`, `resources`.
- Extra flags, env and mounts: `extraArgs`, `extraEnv`, `extraVolumes`,
  `extraVolumeMounts`.
- Server behind a private CA: mount the CA bundle into the agent and set
  `SSL_CERT_DIR` to its directory; Go adds those certificates to the
  image's system roots (example in `values.yaml`).
- OpenShift `restricted-v2`: unset the fixed IDs so the SCC can assign
  them, e.g. `agent.podSecurityContext: {runAsUser: null, runAsGroup: null}`
  and `server.podSecurityContext: {runAsUser: null, runAsGroup: null, fsGroup: null}`.
  `runAsNonRoot` and the `RuntimeDefault` seccomp profile stay.
- The server runs under its own ServiceAccount and mounts no API token.
- `values.schema.json` rejects unknown keys, intervals under one minute
  (`agent.interval` takes any Go duration of at least `1m`: `10m`, `90s`,
  `1.5h`) and malformed targets (`agent.targets` must be `MAJOR.MINOR`) at
  render time.

## Uninstall

    helm -n upgradescope uninstall upgradescope

Helm leaves CRDs in place by design (`crds/` semantics), and the
`ClusterReadiness` object the agent created stays too. Full removal:

    kubectl delete crd clusterreadinesses.upgradescope.dev

That deletes the CRD and any `ClusterReadiness` objects. Everything else
(Deployments, RBAC, Secrets, Service, PVC) is removed by `helm uninstall`;
the server PVC is deleted with the release because it is chart-managed.

## Values

See the commented `values.yaml` — every knob is documented there.
