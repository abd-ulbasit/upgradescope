# Uninstall

## The chart

```sh
helm -n upgradescope uninstall upgradescope
```

This removes the Deployments, ServiceAccounts, ClusterRole and binding,
Secrets, Services, the server's PVC (chart-managed, so its SQLite history
goes with it) and any ServiceMonitor, PrometheusRule or dashboard ConfigMap
the chart rendered. The kind end-to-end test checks that nothing else is
left behind (`e2e:uninstall_leaves_nothing` in the
[claims ledger](../claims.md)).

To keep the server's SQLite history across an uninstall (to reinstall
later, or to copy it off first), choose before you uninstall:

- `server.persistence.retain=true` annotates the chart's PVC
  `helm.sh/resource-policy: keep`, so `helm uninstall` leaves it. Set it with
  a `helm upgrade` first: the annotation must be on the PVC when you
  uninstall. A reinstall under the same release name and namespace mounts
  it again; delete it with `kubectl -n upgradescope delete pvc
  upgradescope-server-data` once it is no longer wanted.
- `server.persistence.existingClaim=<pvc>` mounts a PVC you created, and the
  chart renders none, so the claim is never the chart's to delete.

A Postgres database (`server.database.existingSecret`) is never the chart's
either ([A fleet server](#a-fleet-server)).

Two things stay, by design:

- the `ClusterReadiness` CRD, because Helm never deletes what it installed
  from `crds/`;
- the `ClusterReadiness` object the agent created.

To remove both:

```sh
kubectl delete crd clusterreadinesses.upgradescope.basit.engineer   # deletes every ClusterReadiness too
```

A cluster that ran v0.1.x or a v0.2.0 release candidate may also still
have the CRD of the old API group
([The API group moved](upgrade.md#the-api-group-moved)). Nothing uses
it; delete it too:

```sh
kubectl delete crd clusterreadinesses.upgradescope.dev --ignore-not-found
```

Nothing else in the cluster changed because of upgradescope: the agent
writes only its own object and that CRD, adds no webhooks, finalizers or
owner references, and annotates nothing.

## A fleet server

Uninstalling a hub's chart removes the server; a Postgres database it used
(`server.database.existingSecret`) is yours and stays. To decommission one
cluster while keeping the server, stop its agent first, then delete its
record, history and ingest tokens:

```sh
UPGRADESCOPE_ADMIN_TOKEN=... upgradescope clusters delete prod-eu-1 \
  --server https://upgradescope.example.com
```

An agent that keeps pushing under the name registers it again.

## The CLI

Remove the binary the way you installed it (`brew uninstall upgradescope`,
your package manager, or delete the file). It keeps no state: `scan` writes
only the reports you ask for.
