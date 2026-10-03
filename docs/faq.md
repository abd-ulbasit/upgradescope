# FAQ

**Is it an operator?**
No. The agent is a timer loop: it collects, evaluates and writes its
`ClusterReadiness` status every interval. The verdict depends on the
knowledge base and the calendar (end-of-life dates), neither of which
produces a Kubernetes event, so a watch-driven reconciler would requeue on
unrelated churn and still miss the day an EOL date passes. There is no
controller-runtime, informer cache, leader election or webhook. The
argument in full is in the [architecture guide](architecture.md#design-boundaries).

**Does it change anything in my cluster?**
No. The agent writes only its own `ClusterReadiness` object and (by
default) that object's CRD. It never upgrades, patches or annotates
anything. [Security model and RBAC](operations/security-model-and-rbac.md).

**Why does it need to read every Secret?**
It does not read them all, but RBAC cannot grant less: listing Helm's
release Secrets (`owner=helm`) needs `list` on all Secrets.
`rbac.helmSecrets=false` removes the grant, at the cost of Helm chart
findings. [The trade-off](operations/security-model-and-rbac.md#the-rbachelmsecrets-trade-off).

**Can it tell me which client still calls a deprecated API?**
Not by name. It reads the apiserver's `apiserver_requested_deprecated_apis`
metric, which says that some client requested an API, not which one, and
only for the apiserver replica that answered. The apiserver's audit log
records the caller (the `k8s.io/deprecated` annotation); upgradescope does
not read audit logs. [Deprecated-API detection](concepts/api-usage-detection.md#requests-the-apiservers-own-record).

**Why is my cluster `unknown` and not `ready`?**
A required check could not run, so a blocker may have been missed: most
often a target past the knowledge base's horizon.
[Troubleshooting](troubleshooting.md#the-verdict-is-unknown-and-the-gate-fails).

**How fresh is the knowledge base, and does it phone home?**
It is compiled into the binary and nothing is fetched at runtime; it is as
fresh as the release you run. [Knowledge base](concepts/knowledge-base.md).

**Is the score comparable between clusters and over time?**
Yes, for the same target and knowledge base: it is a fixed formula of the
blocker and warning counts. Gate on the verdict, though; the score is for
trends. [Verdict and score](concepts/verdict-and-score.md).

**Will the same scan give the same answer tomorrow?**
For the same inventory, knowledge base and target, yes, except where a date
passes: end-of-life is calendar-based. Pin the binary in CI for a gate that
changes only when you change it.

**Does it work offline or air-gapped?**
`scan --files` needs nothing but the manifests. The agent talks only to its
own apiserver and, optionally, your server. Mirror the image and the chart
into your registry; `image.repository` and `imagePullSecrets` point the
chart at it.

**Can teams see only their own clusters on the server?**
Yes. A read token minted with
`upgradescope tokens create --read --teams payments` reads only the clusters payments owns a namespace in, and only
payments' findings and team scores; a finding that also covers other
teams' namespaces is cut to payments' namespaces and objects, and any other
cluster answers 404. An
authenticating proxy's group header can set the scope instead. Tenants
that must not share a database still need one server each.
[Read access](operations/auth.md).

**Does it support kubectl client skew, CRD versions, feature gates?**
CRD versions yes: a scan reports deprecated, unserved and
stored-but-unserved versions of the CRDs it finds, live and in `--files`
([#157](https://github.com/abd-ulbasit/upgradescope/pull/157)). Not today:
client skew needs audit logs, and feature gates are not in the knowledge
base.

**Who wrote it?**
The design, the decisions in the [architecture guide](architecture.md)
and the review are the maintainer's; most of the code was written by coding
agents from specs and plans he wrote and reviewed. The author interned at
chkk.io, which sells in this category. upgradescope is clean-room: no
proprietary code, data, schemas or documents were used, every knowledge-base
entry is generated from upstream source or carries a public citation, and
CI checks both. See [Comparison](comparison.md#chkk) and the
[background research](research.md).
