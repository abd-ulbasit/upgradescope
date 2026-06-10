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
      --set server.enabled=true \
      --set server.ingestToken=$(openssl rand -hex 16)

Agent pushing to an existing server elsewhere:

    helm install upgradescope deploy/chart -n upgradescope --create-namespace \
      --set agent.serverUrl=https://uscope.example.com \
      --set agent.existingSecret=my-push-token   # key: serverToken

## RBAC: what the agent can read, and why

The chart grants the agent **get/list/watch on all resources in all API
groups** — cluster-wide, strictly read-only. This is deliberate and worth
your scrutiny:

| Need | Why narrower RBAC doesn't work |
|---|---|
| Helm release detection (Secrets of type `helm.sh/release.v1`) | RBAC cannot filter Secrets by type, and release secret names are dynamic. **This means the agent can read every Secret in the cluster.** |
| Deprecated-API usage (metadata-only lists at deprecated group/versions) | The set of flagged group/versions comes from the knowledge base and changes with every KB update; an enumerated RBAC list would silently rot. |

If cluster-wide secret read is unacceptable, set `rbac.create=false` and
bind your own narrower ClusterRole. Affected collectors degrade to
"not assessed (reason)" in reports — nothing crashes (spec §9).

Write access is exactly one resource: the `ClusterReadiness` CR, its
status, and the CRD itself (`apiextensions` create/get/update/patch so the
agent can self-heal the CRD). The agent mutates nothing else, ever.

It also reads two non-resource URLs: `/metrics` (the
`apiserver_requested_deprecated_apis` metric — active deprecated-API
callers) and `/version`.

## Read API exposure

With `server.enabled=true` and `server.readToken` unset, the read API is
**unauthenticated** behind the ClusterIP Service. Set `server.readToken`
before exposing it via Ingress/LoadBalancer.

The server also serves the web dashboard at `/` (images built from the
repo Dockerfile embed it). Static assets are unauthenticated; every API
call the dashboard makes carries the read token you set in its header
settings, so `server.readToken` still protects all data.

## Uninstall

    helm -n upgradescope uninstall upgradescope

Helm leaves CRDs in place by design (`crds/` semantics). Full removal:

    kubectl delete crd clusterreadinesses.upgradescope.dev

That deletes the CRD and any `ClusterReadiness` objects. Everything else
(Deployments, RBAC, Secrets, Service, PVC) is removed by `helm uninstall`;
the server PVC is deleted with the release because it is chart-managed.

## Values

See the commented `values.yaml` — every knob is documented there.
