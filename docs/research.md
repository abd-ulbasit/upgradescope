# Where the gap is — Kubernetes upgrade readiness (June 2026)

Compiled 2026-06-10 from public sources. This is the evidence base for the
problem upgradescope targets, and the record of which adjacent tools were
checked first so as not to rebuild them. Every claim below is linked; nothing
here is inferred from private or internal information.

## The gap in one paragraph

Open-source tooling for upgrade safety is **point-in-time and manifest-shaped**: pluto (Fairwinds) statically scans YAML/Helm in repos; kubent inspects a live cluster once and exits. Both only catch deprecated/removed APIs — nothing about add-on EOL, chart compatibility, or version skew — and both must be hand-wired into CI and re-run around upgrade windows. The *continuous* version — always-on scanning of what actually runs, fleet-wide, with curated risk knowledge ("this controller version is EOL", "this chart breaks on 1.34") — exists only as commercial products (chkk.io "Operational Safety", and partially Plural, Fairwinds Insights). Quote from the tooling guides: tools like Pluto and kubent "require manual setup and maintenance — look for tools that can continuously scan your entire Kubernetes ecosystem, both IaC repositories and live configurations."

## Evidence

### 1. The Ingress NGINX retirement proved "EOL add-on detection" is a real, urgent category
- kubernetes/ingress-nginx — the most-deployed ingress controller — reached EOL **March 24, 2026**: no releases, no bugfixes, **no security fixes**. ([kubernetes.io blog, Nov 2025](https://kubernetes.io/blog/2025/11/11/ingress-nginx-retirement/))
- Compliance impact: "EOL software in the L7 data path" triggers automatic findings in SOC 2, PCI-DSS, ISO 27001, HIPAA; compliance teams are blocking production promotions over it ([chkk.io blog on the deprecation](https://www.chkk.io/blog/ingress-nginx-deprecation)).
- Migration tooling exists (ingress2gateway 1.0, March 2026) but *detection* — "you are running EOL software, here is the blast radius" — is exactly what no OSS tool does continuously.

### 2. OSS deprecation scanners are explicitly point-in-time
- pluto: static analysis of manifests/Helm in repos; blind to anything deployed outside the scanned repo. ([Fairwinds pluto](https://github.com/FairwindsOps/pluto))
- kubent (kube-no-trouble): one-shot live-cluster audit. ([doitintl/kube-no-trouble](https://github.com/doitintl/kube-no-trouble))
- Standard practice per 2026 guides: "never open an upgrade change request without a passing pluto detect-files, pluto detect-helm, and kubent run" — i.e. humans gluing CLIs around change windows. ([Plural's tool guide](https://www.plural.sh/blog/kubernetes-api-deprecation-tool-guide/), [oneuptime guide](https://oneuptime.com/blog/post/2026-02-09-identify-deprecated-apis-upgrades/view))
- Detection accuracy caveat that a live watcher solves: deprecated-API usage is best detected from **apiserver audit/applied state**, since `kubectl get` returns objects converted to the newest version — a known pluto/kubent blind spot when manifests aren't available.

### 3. Fleet reality makes point-in-time scanning untenable
- 2026 platform-engineering surveys: orgs run dozens of clusters with no complete inventory; "patching windows, certificate rotations, and incident response" dominate operational cost; >60% of Kubernetes incidents trace to misconfiguration. (platformengineering.org 2026 tooling report; Medium/F8010 "Don't waste 2026 on the wrong Kubernetes practices")
- Multi-cluster config-drift tools (Argo/Flux/KubeFleet) reconcile *desired vs live* — none evaluate *live vs version-lifecycle knowledge*.

### 4. The commercial benchmark, and the disclosure that goes with it
- chkk.io sells exactly this: Kubernetes "operational safety" — upgrade readiness, add-on EOL tracking ("Keep your Ingress NGINX safe" campaigns), curated risk signatures, preverified upgrade plans.
- **Disclosure:** I interned at chkk.io. upgradescope is clean-room — no proprietary code, data, schemas, or internal documents were used or consulted. Every entry in the knowledge base is derived from upstream `k8s.io/api` source or carries a public citation URL, and both are machine-checked in CI.
- What makes the commercial product commercial-grade is the **curated knowledge base** (risk signatures per add-on version) and the **collectors** (safe, read-only, fleet-scale). The open-source opening is the 80% case: API lifecycle data is machine-derivable from upstream Kubernetes, and add-on EOL data for the top ~30 add-ons is a maintainable curated registry.

## Adjacent tools (do not rebuild these)
- **ingress2gateway** — migration executor; we detect and recommend, optionally link to it.
- **Goldilocks/KRR** — rightsizing, different category.
- **Trivy/kube-bench** — CVE/CIS scanning; same *shape* (scan + findings) but different knowledge domain; good architectural reference for an OSS scanner that won.
- **Pluto/kubent** — subsume their checks as one detector among several; both are good references for the API-lifecycle dataset format.

## Positioning one-liner

"pluto + kubent + an EOL registry, running continuously in your cluster, with a readiness score you can show your auditor — self-hosted and free."
