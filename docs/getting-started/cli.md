# CLI in two minutes

## Install

Any of these gives you the same binary; [Install](../operations/install.md)
covers verification and the other channels.

```sh
go install github.com/abd-ulbasit/upgradescope/cmd/upgradescope@latest
upgradescope version
```

`upgradescope version` prints the build and the knowledge base it carries,
including its **horizon**: the newest Kubernetes minor the knowledge base
describes.

## Scan rendered manifests

No cluster needed. Render what you deploy, then scan it:

```sh
helm template my-release ./chart --output-dir rendered   # or kustomize build > rendered/all.yaml
upgradescope scan --files rendered --target 1.37
```

`--files` takes a directory (every `*.yaml`, `*.yml` and `*.json` under it)
or one file, and decodes each document the way `kubectl apply -f` does.
In files mode API usage and add-ons are assessed. Add-ons are found in the
container and init-container images and the labels of Pod, Deployment,
DaemonSet, StatefulSet, ReplicaSet, Job and CronJob pod templates (in
`kind: List` items too), and in an Ingress NGINX `IngressClass`, matched as
a live scan matches them. Images injected at admission time, such as a mesh
sidecar, are not in the manifests, so they are not seen. Version skew,
deprecated-API requests and Helm releases need a cluster: they are listed
under `NOT ASSESSED` with the reason `files mode`, and do not count against
the verdict.

What you scan is what the renderer produced. Without a cluster,
`helm template` fills `.Capabilities` with Helm's built-in defaults (its
own Kubernetes version and API list), not your cluster's or the target's.
A chart that picks an API version from `.Capabilities.KubeVersion` or
`.Capabilities.APIVersions.Has` can then render a different version from
the one deployed: a false blocker, or a missed one. Pin them to the cluster
you are judging with `--kube-version` (for example `1.36.0`) and one
`--api-versions` per API the chart tests (for example
`policy/v1beta1/PodDisruptionBudget`). For what a release has actually
installed, scan `helm get manifest <release>` output, or scan the cluster.

## Scan a live cluster

```sh
upgradescope scan --target 1.37                    # current kubeconfig context
upgradescope scan --context prod --target 1.37      # another context
```

A live scan only reads: discovery, `/version`, nodes, namespaces, pods, the
apiserver's `/metrics`, Helm release Secrets and ConfigMaps (`owner=helm`),
and metadata-only lists of the resources the knowledge base flags. Each part
that your credentials cannot read is reported as not assessed with the
reason, and the rest of the scan still runs. A scan that could read nothing
at all (an unreachable cluster) is an error, exit 1.

A slow or stalled API server cannot hang the scan: each request is given
up after `--request-timeout` (default 30s), and each collector step has its
own share of the scan's 5 minutes, so a stalled step leaves only its own
check not assessed. The report's header names the kube context and the API
server it read (`Context:  prod (API server https://10.0.0.1:6443)`), so a
saved report says which cluster it is about.

## Read the result

```text
SCORE  50/100
READY  no
```

- **`READY`** is the verdict: `yes` (ready), `no` (blocked: at least one
  blocker) or `unknown` (no blocker found, but a required check could not
  run, so one may have been missed). [Verdict and score](../concepts/verdict-and-score.md)
  has the rules.
- **Findings** come in three severities. Blockers break the upgrade,
  warnings break the one after it or need attention soon, info findings are
  listed but never scored. Each one has a `fix:` and a `see:` citation.
- **`NOT ASSESSED`** lists what the scan could not see, and why.

## Pick an output

```sh
upgradescope scan --target 1.37 --output json      # the machine-readable report
upgradescope scan --target 1.37 --output sarif     # SARIF 2.1.0 for code scanning
upgradescope scan --target 1.37 --output markdown  # a summary for a pull-request comment
```

The JSON report is versioned (`schemaVersion`) and described in the
[JSON report reference](../reference/json-report.md).

## Gate on it

| Exit code | Meaning |
|---|---|
| 0 | The gate passed. |
| 1 | An operational error: an unreachable cluster, an invalid flag, config file or baseline. |
| 2 | The gate failed: a finding at or above `--fail-on` (default `blocker`), or a verdict of `unknown`. |

`--fail-on warning` is stricter; `--fail-on never` always exits 0.
A verdict of `unknown` fails the gate on purpose, since a blocker may be
behind the gap. The common case is a target newer than the knowledge base's
horizon:

```console
$ upgradescope scan --files rendered --target 1.38
...
READY  unknown (required checks were not assessed)
  kb-coverage (required): knowledge base covers Kubernetes up to 1.37; target 1.38 cannot be assessed
...
$ echo $?
2
$ upgradescope scan --files rendered --target 1.38 --allow-incomplete; echo $?
0
```

`--allow-incomplete` gates on the findings alone. Use it knowingly: the
report still says what was not assessed.

## Next

- Gate pull requests: [CI gate](ci-gate.md).
- Accept known findings, or fail only on new ones: [Suppressions and baselines](../guides/suppressions-and-baselines.md).
- Keep the answer current in the cluster: [In-cluster agent](in-cluster.md).
- Every flag: [`upgradescope scan`](../reference/cli/upgradescope_scan.md).
