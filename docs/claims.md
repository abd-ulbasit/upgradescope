# Claims ledger

Every public claim upgradescope makes (README, chart README, Action,
CLI help, docs) is listed here, with the automated tests that keep it true
or the issue that tracks why there are none. A claim that stops being true
should turn CI red, not wait for the next audit.

The ledger comes out of the October 2026 claims verification (#99): red-team
missions attacked each claim on real clusters and integrations (results in
#131 to #137). Each claim is in one of three places:

- **the tables by area**: claims that held when audited, or the part of a
  claim that held, with what proves it. A few are still checked by hand
  only; those say "not automated" and name #99;
- **[Under repair](#under-repair)**: claims that did not hold, with the issue
  that fixes them; they move up when the fix lands with a test;
- **[Not yet audited](#not-yet-audited)**: claims the missions of round 2
  (#99) have not reached, with the tests that cover them so far.

IDs are the audit's, so a row can be traced back to its mission.

**How to read "Proven by".** Each reference is checked by
`hack/claims-check.sh` (`make claims-check`, run by CI's test job), so a
renamed or deleted test fails CI:

- `TestName`: a Go test (unit, golden, or an env-gated integration test);
- `e2e:fn`: a gate of the kind end-to-end run, `hack/e2e.sh`, on Kubernetes
  1.31 and 1.37 for every PR and 1.29 to 1.37 weekly (CI's kube job);
- `ci:job`: a CI job of `.github/workflows/ci.yml`;
- `make target`, or a path such as a `hack/` test script.

A row with no reference says "not automated" (or, in the last table, "not
audited") and names the issue that tracks it.

## API usage: removed and deprecated APIs

| ID | Claim | Proven by |
|---|---|---|
| API-01 | Removed and deprecated APIs are found by who writes them, from managedFields and the last-applied annotation, not by whether the API is still served. An object written through a deprecated version is reported with its field manager; once re-applied through the GA version it is not, and apiserver-maintained objects never are. | `TestCollectAPIUsageFlagsOnlyObjectsAuthoredViaDeprecatedVersion` `TestCollectAPIUsageObjectWrittenViaGAVersionIsNotAFinding` `TestCollectAPIUsageClearsAfterManagerMigrates` `TestCollectAPIUsageAPFBootstrapObjectsAreNotBlockers` `TestAuthoringManagerIgnoresInternalManagersAndStatusEntries` `e2e:deprecated_object_reported` |
| API-01b | A freshly created cluster, scanned at its next minor, has no removed-API blocker (#3). | `e2e:no_removed_api_blockers` |
| API-01c | When the kind itself goes away (no surviving version), every stored object counts. | `TestCollectAPIUsageTypeRemovedKindCountsEveryObject` `TestCollectAPIUsageRealKBPodSecurityPolicyCountsEveryObject` |
| API-02 | `removed-api` is a blocker when the API is removed at or before the target and a warning when it is removed in the next minor. | `TestEvalAPIUsageRemovedAtTarget` `TestEvalAPIUsageRemovedAtTargetPlusOne` `TestEvaluateGolden` |
| API-04 | The scanner lists every resource at a version that is not deprecated whenever the cluster serves one. On the e2e kind cluster, which serves one deprecated group/version on purpose, neither `scan` nor the agent calls a deprecated API (the e2e's own request through that version shows the audit log records them), apart from the self-LISTs in `hack/e2e/deprecated-request-allowlist.txt` (#123 removes them); a cluster that serves a kind only at deprecated versions still gets a LIST there (API-04c). | `TestCollectAPIUsageNeverListsDeprecatedVersionWhenAnotherIsServed` `TestCollectAPIUsageListsAtReplacementGroupWhenOwnGroupIsAllDeprecated` `e2e:audit_no_deprecated_requests` |
| API-05 | A caller with no stored objects is a standalone `deprecated-api-in-use` finding; caller evidence otherwise merges onto the object finding. | `TestEvaluateCallerWithoutObjectsStaysStandalone` `TestEvaluateMergesCallersIntoAPIUsageFinding` `TestEvaluateMergesSubresourceCallers` `TestEvaluateMoreSevereCallerIsNotFolded` `TestEvaluateGolden` |
| PF-02 | Every cluster-wide list is paged (500 objects a page); the api-usage and Helm lists are metadata-only, and object references are capped. | `TestCollectAPIUsageFollowsListPagination` `TestCollectAddOnsFollowsListPagination` `TestCollectVersionsFollowsListPagination` `TestCollectHelmFollowsListPagination` `TestCollectHelmFetchesOnlyTheChosenRevision` `TestCollectAPIUsageCapsObjectRefs` |

## Deprecated-API callers

| ID | Claim | Proven by |
|---|---|---|
| DC-01 | Clients still calling deprecated APIs are found from the apiserver's `apiserver_requested_deprecated_apis` metric (which, until #123, also counts the scanner's own LISTs: API-04b). | `TestCollectDeprecatedCalls` `TestEvalDeprecatedCallsSubresource` |
| DC-02 | When `/metrics` is forbidden (401 or 403) or lacks the metric, the capability is reported as unavailable with the reason; the rest of the scan still runs. | `TestCollectDeprecatedCallsForbidden` `TestCollectDeprecatedCallsFamilyAbsent` `TestCollectDeprecatedCallsOtherErrorNotRewritten` |
| DC-03 | `deprecated-api-in-use` severity follows the removal window, and is info when the removal release is unknown. | `TestEvalDeprecatedCallsSeverityVsTarget` `TestEvalDeprecatedCallsUnparseableReleaseIsInfo` `TestEvalDeprecatedCallsUnparseableIsInfo` |

## Add-ons past end of life

| ID | Claim | Proven by |
|---|---|---|
| AO-01 | Add-ons past end of life are detected by container image and by Helm chart. | `TestCollectAddOnsUsesPodImagesAndHelmReleases` `TestMatchAddOns` `TestMatchAddOnsRealWorldImages` `TestEvalAddOnsHelmEvidence` |
| AO-02 | Ingress NGINX, past EOL since March 2026, is a blocker: installed from the upstream chart on a real cluster, `scan` reports it and exits 2. | `TestIngressNginxRetirement` `TestImageOnlyIngressNginxVerdicts` `TestScanIntegration_KindEOLIngressNginx` `e2e:eol_ingress_nginx_blocks` `e2e:cr_has_verdict` |
| AO-03 | Matching is by image repository prefix or exact chart name; the image tag, `v` stripped, is the detected version. | `TestMatchAddOns` `TestParseImage` `TestVersionFromTag` `TestHelmInstallsJudgedByAppVersion` |
| AO-04 | Unmatched images go to `unrecognizedImages` (capped) and never become findings. | `TestMatchAddOnsUnrecognizedCap` |
| AO-05 | `eol` status is a blocker, an EOL date within 90 days a warning, and a target above a compat row's `k8s_max` a compat finding. | `TestEvalAddOnsEOLBlocker` `TestEvalAddOnsApproachingEOLWarning` `TestEvalAddOnsChartIncompatBlocker` `TestEvalAddOnsCycleLifecycle` `TestEvalAddOnsJudgesTheInstalledCycle` |
| AO-07 | Node container runtimes past EOL are flagged per release line, naming the nodes. | `TestEvalAddOnsNodeRuntimes` `TestEvalAddOnsNodeRuntimeDetail` `TestEvaluateNodeRuntimeVerdict` |
| AO-08 | An add-on the registry has no lifecycle data for says so instead of passing silently. | `TestEvalAddOnsNoDataDetail` `TestEvalAddOnsNodeRuntimeNoDataDetail` |

## Helm releases

| ID | Claim | Proven by |
|---|---|---|
| HE-01 | Helm releases whose chart `kubeVersion` excludes the target, or whose stored manifest uses a removed API, are findings. | `TestEvalHelmChartKubeVersion` `TestEvalHelmManifestRemovedAPIBlocks` `TestEvalHelmManifestDeprecatedAPIWarns` `TestHelmManifestWithRemovedAPIBlocksEndToEnd` `TestCollectHelmChartKubeVersionAndManifestAPIs` |
| HE-02 | Every version range that passes registry validation is matchable. | `TestMatchesRange` |
| HE-03 | Only the installed revision is read: a failed upgrade is judged by what still runs, and a release uninstalled with `--keep-history` yields nothing. | `TestCollectHelmInstalledRevision` `TestCollectHelmFetchesOnlyTheChosenRevision` `TestUninstalledHelmReleaseRaisesNoEOLBlocker` `TestEvalHelmSkipsUninstalledReleases` `e2e:keep_history_not_reported` |
| HE-04 | One corrupt release never fails the capability, and memory stays bounded by one release however long the history; the secrets and configmaps drivers degrade independently. | `TestCollectHelmSkipsCorruptSecretKeepsValid` `TestCollectHelmPeakHeapIsBoundedByOneRelease` `TestCollectHelmReadsConfigMapDriver` `TestCollectHelmDegradesPerDriver` `TestCollectHelmPrefersSecretsDriver` |

## Version skew

| ID | Claim | Proven by |
|---|---|---|
| SK-01 | Kubelets that would fall outside the skew policy after the upgrade are blockers, ones already outside it warnings, and kubelets newer than the apiserver violations. | `TestEvalSkewPostUpgradeOnlyBlocker` `TestEvalSkewCurrentViolationWarnsAndBlocksPostUpgrade` `TestEvalSkewKubeletNewerThanAPIServer` `TestEvalSkewKubeletBehindNewestHAApiserver` `TestEvalSkewWithinPolicyNoFindings` |
| SK-02 | Control-plane skew covers HA apiserver spread, controller-manager, scheduler and kube-proxy; a managed control plane that hides its pods yields no false finding. | `TestEvalControlPlaneSkewHASpread` `TestEvalControlPlaneSkewCtrlMgrNewerIsBlocker` `TestEvalControlPlaneSkewSchedulerBehind` `TestEvalControlPlaneSkewKubeProxy` `TestEvalControlPlaneSkewEmptyControlPlaneNoFindings` |
| SK-04 | Managed-cluster version strings (EKS, GKE, k3s, RKE2, OpenShift suffixes) parse. | `TestParseVersion` `TestParseVersionObservedGitVersions` `TestCollectVersionsManagedClusterEmptyControlPlane` |
| SK-05 | The policy follows upstream: n-3 kubelets from 1.28, n-2 before. | `TestDefaultSkewPolicy` `TestSkewLegacyComponentsAllowTwoMinors` |

## Verdict, score and exit codes

| ID | Claim | Proven by |
|---|---|---|
| VS-01 | `engine.Evaluate` is pure: the same inventory, knowledge base, target and time give the same bytes. | `TestEvaluateGolden` `TestScanBaselineGolden` |
| VS-02 | `score = max(0, 100 - min(75, 25 x blockers) - min(20, 5 x warnings))`; info findings are never scored. | `TestScore` `TestRescore` `TestEvaluateGolden` |
| VS-05 | Exit codes: 0 below the `--fail-on` threshold, 1 on a scan error (an unreachable or unreadable cluster included), 2 when the gate fails. | `TestScanFailOnExitCodeMapping` `TestScanPipelineErrorIsExitOne` `TestScanUnreadableClusterIsError` `e2e:unreachable_scan_exits_1` `e2e:eol_ingress_nginx_blocks` |
| VS-06 | `--fail-on blocker`, `warning` or `never`; an unknown verdict fails the gate unless `--allow-incomplete`; an invalid value is an error. | `TestScanGateOnUnknownVerdict` `TestScanRejectsBadFailOn` `TestScanBaselineStillGatesIncomplete` |
| VS-09 | A live scan in which every capability failed is an error, not "no findings"; `--context` is honoured. | `TestScanUnreadableClusterIsError` `e2e:unreachable_scan_exits_1` `TestScanIntegration_KindEOLIngressNginx` |
| VS-10 | `--target` takes a minor such as 1.36; `2.0` and other typos are rejected. | `TestParseTarget` `TestScanRejectsBadTarget` `TestScanRejectsNonMajorOneTarget` `TestScanRequiresTarget` |
| VS-11 | A target newer than the knowledge base is allowed and reported with a `kb-stale` warning (the verdict is then unknown). | `TestEvalKBStale` `TestEvaluateUnknownKeepsScoreAndKBStaleWarning` |
| VS-12 | Findings are sorted by severity, then category, then title. | `TestSortFindings` `TestEvaluateGolden` |

## Offline scans, SARIF, ignore rules and baselines

| ID | Claim | Proven by |
|---|---|---|
| FS-01 | `scan --files` reads rendered manifests offline; only API usage is assessed there, and every other capability is reported as not assessed ("files mode"). | `TestCollectFiles` `TestManifestInventoriesAreFilesSource` `TestWriteTableFilesModeGolden` `TestScanFilesNoManifestsIsExitOne` |
| FS-02 | `--files` reads `*.yaml`, `*.yml` and `*.json`, expands `kind: List`, and turns documents that do not parse into warnings, not errors. | `TestCollectFilesExpandsLists` `TestCollectFilesListVariants` `TestCollectFilesUnrenderedTemplateDoc` `TestScanFilesWarnsOnInvalidFiles` `TestCollectFilesSkipsVCSAndDependencyDirs` |
| FS-03 | `--output sarif` places each result on its file and line, and GitHub code scanning accepts it. | `TestWrite` `TestWriteGitHubAcceptable` `TestScanFilesSARIFLocations` `ci:action` |
| FS-06 | A known finding can be accepted with an expiring, reasoned ignore rule or an object annotation, and `--baseline` gates only on new findings. | `TestApplyCategoryRule` `TestApplyAnnotations` `TestApplyExpiredRuleWarnsAndDoesNotSuppress` `TestBaselineMark` `TestScanWriteBaselineRoundTrip` `TestScanFilesSARIFSuppressedAndBaseline` `ci:action` |

## GitHub Action

| ID | Claim | Proven by |
|---|---|---|
| AC-02 | Exit code 2 (findings at or above `fail-on`) fails the step. | `ci:action` `hack/action_test.sh` |
| AC-03 | `version` installs a release archive verified against its `checksums.txt`, or `preinstalled` uses the binary on PATH. | `hack/action_test.sh` `ci:action` |
| AC-05 | Inputs never reach a `run:` script unquoted, and are validated before use. | `hack/action_test.sh` |
| AC-07 | Run as a consumer runs it, on Linux and macOS, the Action installs the latest release and fails on a fixture holding a removed API. | `ci:action` |
| AC-08 | The Action sets `score`, `verdict`, `blockers` and the other outputs, annotations, and a job summary listing suppressed findings and the baseline diff. | `hack/action_test.sh` `ci:action` |

## Agent and ClusterReadiness CRD

| ID | Claim | Proven by |
|---|---|---|
| AG-02 | One tick at start, then every `--interval` (minimum 1m) with jitter. | `TestRunFirstTickThenGracefulStop` `TestConfigIntervalMinimum` `TestJitterBounds` |
| AG-03 | A tick has a deadline, and a stop mid-tick is not a failure. | `TestRunTickHasDeadline` `TestTickTimeout` `TestRunStopMidTickIsNotAFailure` |
| AG-04 | With no server, the agent still writes the CRD status every tick. | `TestTickCRDOnlyModeNoPusher` `TestAgentIntegration_CRDStatusOnKind` |
| AG-05 | Pushes happen only when the canonical inventory hash changes, retry transient failures with capped backoff, keep only the latest payload, and drop on a permanent 4xx. | `TestTickDedupsUnchangedInventory` `TestSnapshotHashIgnoresCollectedAt` `TestFlushRetriesTransientWithBackoff` `TestBackoffCapped` `TestFlushLatestOnlyBuffer` `TestFlushOther4xxPermanentDropsPayload` `TestFlush401DropsPayloadNoRetry` |
| AG-06 | The status write retries on conflict. | `TestWriteStatusRetriesOnConflict` |
| AG-07 | With `--manage-crd` the agent converges the CRD to its embedded manifest and leaves foreign metadata alone; without it, it never touches the CRD. | `TestEnsureCRDUpdatesExisting` `TestEnsureCRDNoWriteWhenInSync` `TestEnsureCRDPreservesForeignMetadata` `TestRunSkipsCRDWhenNotManaged` |
| AG-08 | The ClusterReadiness is created if absent and never overwritten; user-set `spec.targets` survive. | `TestEnsureObjectCreatesAndIsIdempotent` `TestTickHonorsSpecTargets` `TestTickNoSpecWriteWhenFlagTargetsMatch` |
| AG-09 | Empty `spec.targets` means the next minor above the server; invalid entries are skipped and reported. | `TestResolveTargetsDefaultNextMinor` `TestResolveTargetsSkipsInvalidWithNote` `TestResolveTargetsFromSpec` `TestTickWritesStatusWithDefaultTarget` |
| AG-10 | Status holds, per target, the score, verdict, counts and at most the top 20 findings. | `TestTargetStatusFromReportTop20Cap` `TestTargetStatusFromReportCounts` `TestStatusFromReports` |
| AG-11 | `ClusterReadiness` is cluster-scoped, `upgradescope.dev/v1alpha1`, short name `ucr`. | `TestManifestShape` `TestConstantsMatchManifest` |
| AG-12 | Installed from the chart on a real cluster, the agent keeps a ClusterReadiness with a score and verdict, and `agent.targets` reaches `spec.targets`. | `e2e:cr_has_verdict` `e2e:upgrade_with_targets` `TestAgentIntegration_CRDStatusOnKind` |
| AG-15 | `--cr-name` (chart `agent.crName`) names the object, and the write rule follows it. | `TestAgentCmdFlagDefaults` `TestRenderedRBACCustomCRName` |
| AG-13 | A passing EOL date reaches the ClusterReadiness within one interval, with no Kubernetes event. | not automated: #99 (the clock-shifted run did not finish) |

## Chart, RBAC and what the agent touches

| ID | Claim | Proven by |
|---|---|---|
| RB-02 | The agent writes only its own ClusterReadiness, that object's status, and (with `manageCRD`) the ClusterReadiness CRD; nothing else, ever. Measured from the API server's audit log of a real install. | `e2e:audit_agent_writes_only_its_cr` `TestRenderedRBACDefault` |
| RB-03 | Write access is limited to the `agent.crName` object and the one CRD, with no delete. | `TestRenderedRBACDefault` `TestRenderedRBACCustomCRName` `TestRenderedRBACManageCRDOff` `hack/test-chart.sh` |
| RB-04 | No webhooks, no finalizers, nothing in the cluster changes because of a finding: with an EOL blocker installed, the chart install adds no admission webhook, the ClusterReadiness carries no finalizer or owner reference, and the agent writes nothing but that object and its CRD. | `e2e:no_webhooks_or_finalizers` `e2e:audit_agent_writes_only_its_cr` `TestRenderedRBACDefault` |
| RB-05 | Helm detection reads Secrets only as `owner=helm` lists and GETs of Helm release Secrets; `rbac.helmSecrets=false` removes the grant. | `e2e:audit_secrets_helm_only` `TestCollectHelmFetchesOnlyTheChosenRevision` `TestRenderedRBACHelmSecretsOff` |
| RB-06 | The chart grants no non-resource URL but `/version` and `/metrics` (discovery, `/api` and `/apis`, comes from Kubernetes' default `system:discovery` role). | `TestRenderedRBACNonResourceURLs` `TestRenderedRBACDefault` |
| RB-09 | Token wiring: `existingSecret`, the in-chart ingest token, `readToken`, and a token required with `serverUrl`. | `hack/test-chart.sh` |
| RB-10 | `agent.interval` is at least 1m and targets are MAJOR.MINOR strings, enforced by the values schema. | `hack/test-chart.sh` `TestConfigIntervalMinimum` |
| RB-11 | Pods run as non-root 65532 with a read-only root, no privilege escalation, RuntimeDefault seccomp and all capabilities dropped. | `hack/test-chart.sh` |
| RB-12 | The chart lints strictly, every documented values combination renders and validates at the oldest and newest tested minor, and `crds/` matches the embedded CRD. | `ci:helm` `hack/helm-test.sh` `hack/test-chart.sh` `TestKBRBACRulesInSync` |
| RB-13 | Every chart knob is documented in the commented `values.yaml`. | not automated: #99 (verified by hand in #137: every value path the templates read is in values.yaml, and every leaf is read) |
| RB-14 | `helm uninstall` removes everything but the CRD (and the CR the agent made). | `e2e:uninstall_leaves_nothing` |
| RB-15 | `server.persistence.enabled=false` falls back to emptyDir; the server Deployment uses Recreate (one SQLite writer). | `hack/test-chart.sh` |

## Server, fleet and notifications

| ID | Claim | Proven by |
|---|---|---|
| SV-01 | SQLite (WAL, no cgo) or Postgres, and both pass one conformance suite; Postgres 17 on every PR, 14 to 18 weekly. | `TestSQLiteConformance` `TestPostgresConformance` `TestOpenSetsPragmas` `ci:pg-conformance` |
| SV-02 | Duplicate pushes are detected by the canonical inventory hash; key order and whitespace never change it. | `TestIngestDuplicateCanonicalHash` `TestInsertSnapshotDedup` `TestInsertSnapshotConcurrentIngest` |
| SV-03 | Fleet, reports and exports serve stored evaluations, re-judged when the knowledge base or the team map changes, and after a date change on the next push or background re-evaluation (SV-03b). | `TestReadPathsUseLatestSnapshotOnly` `TestDuplicatePushReevaluatesAcrossEOLDate` `TestRestartWithNewKBReevaluatesOnDuplicatePush` `TestTeamMapChangeReevaluates` `TestStartRunsBackgroundPassAndDelivery` |
| SV-07 | The fleet matrix and per-team rollups (worst score, total blockers, affected clusters). | `TestFleetMatrixExplicitTargets` `TestFleetMatrixDefaultTargets` `TestFleetTeams` `TestTeamsEndpoint` `TestTeamMapApply` |
| SV-09 | Auditor exports: one self-contained HTML report and a CSV per cluster and target. | `TestExportHTMLGolden` `TestExportCSVGolden` |
| SV-11 | Read and write timeouts carry the default 20 MiB snapshot over a link of about 350 KiB/s or faster (slower: SV-11b), and a stalled body cannot hold a connection. | `TestLargeSnapshotWithinDefaultTimeouts` `TestStalledBodyDisconnectedByReadTimeout` `TestShutdownWithStalledClient` |
| SV-13 | Wrong methods get 405 with `Allow`; unknown paths under `/api/`, `/metrics/` and the probes get a JSON 404, not the dashboard (bare `/api` still gets it: SV-13b). | `TestMethodNotAllowed` `TestReservedPathsNeverServeDashboard` `TestStaticDoesNotShadowAPI` |
| SV-15 | Stored times are fixed-width UTC, so they sort in instant order. | `TestTimeFormatFixedWidthUTC` `TestTimesStoredUTCFixedWidth` `TestParseStoredTimeRoundTrip` |
| SV-16 | `--db` creates its parent directory and is mutually exclusive with `--db-url`. | `TestRunServeCreatesDBParentDir` `TestServeDBAndDBURLMutuallyExclusive` |
| NT-01 | For one target, notifications fire on changes only: a new blocker, a warning entering its EOL window, a cluster turning ready (with several targets they repeat: NT-01b). | `TestComputeDelta` `TestIngestEmitsDeltaNotifications` |
| NT-02 | Notifications are best-effort: a hung sink never blocks ingest, and a message is delivered once after retries. | `TestIngestDoesNotWaitForNotifiers` `TestOutboxRetriesThenDeliversOnce` `TestOutboxGivesUpAfterMaxAttempts` `TestSlackDefaultTimeoutIsTwoSeconds` |
| SV-05 | What-if and the gate store nothing. | not automated: #99 (verified by hand in #132) |

## Security

| ID | Claim | Proven by |
|---|---|---|
| SE-01 | Ingest and reads take bearer tokens; with no read token, a non-loopback `--listen` address is refused unless `--allow-anonymous-read` (a host name is not resolved: SE-01b). | `TestIngestAuth` `TestReadAuth` `TestServeAnonymousReadGuard` `TestFleetReadAuth` |
| SE-02 | A per-cluster token writes only its own cluster (403 before anything is written) and can be revoked. | `TestIngestPerClusterTokens` `TestIngestMismatchedTokenWritesNothing` `TestTokensRevoke` `TestTokensRevokeByID` |
| SE-04 | Tokens and the database URL can come from the environment or a file instead of argv. | `TestSecretFlagPrecedence` `TestServeSecretsFromEnvAndFiles` `TestAgentServerTokenSources` |
| SE-05 | Snapshot bodies are capped at 20 MiB on the wire and after gunzip, and nothing is read before auth. | `TestIngestBodyLimits` `TestSnapshotBodyCapIsConfigurable` `TestIngestEncodingErrors` |
| SE-06 | Security headers on the dashboard, its assets, the API and exports. | `TestSecurityHeadersOnDashboard` `TestSecurityHeadersOnAPIAndExport` |
| SE-08 | `--tls-cert-file` and `--tls-key-file` serve HTTPS (TLS 1.2 minimum), the pair validated at startup. | `TestServeTLS` `TestNewTLSValidation` `TestServeTLSFlags` |
| SE-10 | The collectors are read-only: `scan` makes no write request, measured from the API server's audit log. | `e2e:audit_scan_writes_nothing` |
| SE-12 | Reachable vulnerabilities in the shipped binary fail CI, weekly too, with an expiring per-advisory allowlist. | `ci:vuln` `hack/vulncheck_test.sh` |
| SE-14 | Integration tests refuse any context that is not a kind cluster on loopback, unless `UPGRADESCOPE_IT_CONTEXT` names one, and the e2e fails when an integration test skips. | `TestITKubeContext` `e2e:integration_tests` `hack/e2e_test.sh` |

## Dashboard

| ID | Claim | Proven by |
|---|---|---|
| DB-01 | `serve` embeds the dashboard at `/`: the built binary serves `index.html` and every asset it references, with the right content types. | `make dashboard-smoke` `hack/dashboard-smoke_test.sh` `TestSPAHandlerServesIndexAndAssets` `ci:build` |
| DB-02 | The committed bundle is the byte-for-byte Vite build of `web/`; CI fails on a stale one. | `ci:web` `hack/web-test.sh` |
| DB-04 | The read token is kept in localStorage and sent as a bearer header on every API call. | `web/src/api.test.ts` |
| DB-03 | No runtime JavaScript dependency beyond react and react-dom; the charts are hand-rolled SVG. | not automated: #99 (verified by hand in #133: npm ls lists react, react-dom and scheduler; every request is same-origin) |
| DB-05 | Without a built bundle (or with `-tags nodashboard`) the API still serves; client-side routes fall back to `index.html`. | `TestSPAHandlerWithoutBuiltDashboard` `TestSPAHandlerFallbackForClientRoutes` |
| DB-06 | The dashboard dev loop: `cd web && npm run dev` proxies `/api` to `:8080`. | not automated: #99 (verified by hand in #133, macOS included) |
| DB-07 | Before a tag, the release binary is proven to serve the dashboard and every asset it references. | `ci:release-check` `hack/dashboard-smoke_test.sh` |

## Knowledge base and add-on registry

| ID | Claim | Proven by |
|---|---|---|
| KB-01 | The API-lifecycle dataset is generated from `k8s.io/api`, and CI fails when the committed copy is stale. | `ci:kb-freshness` `TestDatasetSanity` `TestDeprecationGuideCoverage` |
| KB-02 | Generated entries win over the hand-written supplement; every supplement entry is cited. | `TestMergeEntries` `TestSupplement` |
| KB-05 | Every add-on EOL claim carries an upstream citation, enforced by `registry.Validate`. | `TestValidate` `TestEmbeddedEntriesProperties` |
| KB-06 | `tools/eol-sync` reconciles registry entries with endoflife.date, and a PR touching `registry/` fails when they drift. | `TestRun` `TestComputeCycles` `ci:registry` |
| KB-09 | The knowledge base and the add-on registry are compiled into the binary; nothing is fetched at runtime, so a KB update reaches users only through a release. | `TestLoad` `TestLoadFS`; no outbound connection: not automated, #99 (strace in #133 saw none for scan --files and serve) |
| KB-10 | The knowledge-base version names the `k8s.io/api` release and digests of both datasets. | `TestDatasetVersion` |
| KB-13 | `registry` is importable on its own. | `TestLoadFS` |
| RM-01 | A remediation never points at an API that is itself removed. | `TestRemediationNeverPointsAtRemovedAPI` `TestResolveReplacement` |

## Build, CI and release

| ID | Claim | Proven by |
|---|---|---|
| IR-03 | Unit tests, golden tests, lint and the build need only Go. | `ci:test` `ci:lint` `ci:build` |
| IR-04 | Integration tests are env-gated; the kind e2e covers scan, agent, chart, CRD, server and the audit log, on 1.31 and 1.37 per PR and 1.29 to 1.37 weekly. | `ci:kube` `hack/e2e_test.sh` `hack/kind-images_test.sh` (the weekly matrix has not run yet: #128) |
| IR-05 | A new push supersedes a PR's run; main, schedule, dispatch and tag runs never cancel each other. | `hack/ci-concurrency_test.sh` |
| IR-10 | Every package compiles for linux, darwin and windows on every PR, so a platform-only break fails its PR, not a tag. | `make cross-build` `hack/cross-build_test.sh` `ci:build` |
| IR-16 | One required check, `ci-ok`, fails if any job failed or was cancelled. | `ci:ci-ok` `hack/ci-ok_test.sh` |
| DO-03 | One binary runs `scan`, `agent` and `serve`. | `TestRootHasAgentSubcommand` `TestRootRegistersServe` `e2e:eol_ingress_nginx_blocks` |
| DO-04 | `engine`, `inventory`, `kb` and `registry` have no client-go dependency; the engine is a pure function with no Kubernetes or network dependency. | `TestEvaluateGolden`; the dependency set: not automated, #99 (verified by hand in #131: go list -deps and a wasm build) |
| DO-07 | Golden files cover every finding category and the score formula. | `TestEvaluateGolden` |
| DO-08 | `make lint` is CI's lint (pinned staticcheck), and gofmt covers every module. | `ci:lint` `hack/test.sh` |
| CL-01 | Every test this ledger names exists. | `make claims-check` `hack/claims-check_test.sh` |

## Under repair

These claims did not hold, wholly or in part, when audited. Each issue
carries the reproduction; the claim moves into a table above when its fix
lands with a test.

| ID | What did not hold | Proven by |
|---|---|---|
| API-01d | An object created with no fields records no managedFields and goes undetected. | not true yet: #122 |
| API-02b | `deprecated-api` info also fires for deprecations after the target ("deprecated since 1.40" at 1.37). | not true yet: #124 |
| API-03 | Deprecated CRD versions and stale `status.storedVersions` are not reported. | not true yet: #48 |
| API-04b | Repeat scans count the scanner's own deprecated-endpoint LISTs as callers. | not true yet: #123 |
| API-04c | A cluster that serves `coordination.k8s.io/v1beta1` gets a LIST of `leasecandidates` there on every scan, which the engine then scores as a caller. | not true yet: #123 |
| DC-02b | A `/metrics` that hangs spends the whole scan budget and starves the other collectors. | not true yet: #48 |
| KB-01b | Alpha kinds deleted from `k8s.io/api` (DRA v1alpha1 to v1alpha3, ClusterCIDR, ServiceCIDR and IPAddress v1alpha1, LeaseCandidate v1alpha1) are missing from the knowledge base, so `--files` passes them. | not true yet: #124 |
| PF-02b | Add-on and version collection reads whole Pod, Node and Namespace objects, not metadata only. | not true yet: #121 |
| VS-03 | README's `ready = (blockers == 0)`: ready now means verdict ready. | not true yet: #130 |
| VS-04 | Severity tiers by target do not cover EOL add-ons, runtimes, kb-stale or current skew. | not true yet: #130 |
| VS-05b | `--output table` to a full disk exits 0. | not true yet: #124 |
| VS-06b, VS-07, VS-08 | A 403 on one resource or on pods/secrets hides blockers and is not surfaced as not assessed. | not true yet: #122 |
| VS-10b | A target at or below the current version is accepted silently. | not true yet: #124 |
| VS-13 | Two control-plane skew findings can share a key. | not true yet: #124 |
| VS-14 | Team scores can read ready while the cluster verdict is unknown. | not true yet: #122 |
| VS-15 | JSON contract changes since v0.1.1 are not in the changelog. | not true yet: #127 |
| FS-02b, FS-05 | Concatenated JSON, duplicate keys and untyped Lists in `--files`; the `helm template` recipe. | not true yet: #119 |
| FS-04, SV-06 | The `/gate` README example and `cluster=` gate pass a PR that adds a removed API. | not true yet: #120 |
| SV-03b | After an EOL date passes, reads serve the previous verdict until the next push or background pass, up to an hour. | not true yet: #125 |
| SV-13b | Bare `/api` serves the dashboard (200 `text/html`) instead of a JSON 404. | not true yet: #125 |
| SV-16b | A `--db` path containing `?` silently opens another file and drops the SQLite write-lock mode, so concurrent writes get `SQLITE_BUSY`. | not true yet: #125 |
| NT-01b | With several targets a change notifies once per target, and a backwards clock step replays events. | not true yet: #125 |
| SV-04, SV-08, SV-10, SV-12, SV-14, NT-03 | Ingest validation, empty cluster IDs, `--targets` parsing, schemaVersion, fleet memory, webhook redirects. | not true yet: #125 |
| PF-06, SE-05b, SV-11b | `/gate` stays under 512Mi with concurrent large documents; ingest has no shared memory budget for concurrent 20 MiB pushes; a slow push gets 422, not a retryable 408. | not true yet: #121 |
| SE-01b | `--listen localhost:…` counts as loopback without resolving the name. | not true yet: #126 |
| SE-03, SE-07b, SE-09 | Token prefix stored; CSV formula guard checks one byte; push is per-cluster and encrypted. | not true yet: #126 |
| RB-01 | README and SECURITY.md describe the pre-#16 RBAC grant. | not true yet: #130 |
| RB-07 | Losing status write access leaves the CR reading ready. | not true yet: #122 |
| RB-08, IR-01, IR-02, IR-06 to IR-09, IR-11 to IR-14, DB-08, SE-15 | The published release, image and chart predate the fixes; signing and publishing have never run end to end. | not true yet: #127 |
| KB-14 | A stored snapshot re-judged by a newer knowledge base drops the fields it does not know, and v0.1.1 snapshots' residency rows become false APF blockers (verdicts otherwise re-judge correctly). | not true yet: #125 |
| DB-09 | The README quickstart (`serve --ingest-token $TOKEN`) works on loopback, but creates the SQLite database and its `-wal` and `-shm` files 0644 under umask 022. | not true yet: #126 |
| AO-01b | Several installs of one add-on merge into the oldest. | not true yet: #129 |
| DO-03b | README's "three subcommands": the binary has five, `tokens` and `clusters` too. | not true yet: #130 |
| KB-04, PF-01, PF-03, PF-04 | Stale numbers: KB horizon, scan time, binary and image sizes. | not true yet: #130 |

## Not yet audited

Round 2 of the audit (#99: the missions on the Action, the agent's
footprint, the knowledge-base pipeline, performance and the docs) has not
run yet, so these claims are neither confirmed nor refuted. Each lists the
tests that already cover part of it; none is fully pinned. A claim moves
into a table above, or under repair, when its mission reports.

| ID | Claim | Tests so far |
|---|---|---|
| AC-01 | The Action (composite, in `action/`) scans a directory of rendered manifests for removed and deprecated APIs and emits SARIF. | `ci:action` `hack/action_test.sh` `TestWriteGitHubAcceptable`; not audited: #99 |
| AC-04 | The README's workflow (`security-events: write`, `abd-ulbasit/upgradescope/action@main`, `upload-sarif` with `if: always()`) works as written, from a fork PR too. | not audited: #99 |
| AC-06 | The binary the Action downloads and runs can be trusted. | `hack/action_test.sh` (the archive is checked against the release's checksums.txt, AC-03); not audited: #99 |
| AG-01 | The agent uses no controller-runtime, informer cache, leader election, webhooks, finalizers or owner references. | `e2e:no_webhooks_or_finalizers` `e2e:audit_agent_writes_only_its_cr`; the dependency set and watch/lease traffic: not audited, #99 |
| AG-14 | The knowledge base moves only with the binary and the target only with a new upstream minor, with no Kubernetes event. | `TestResolveTargetsDefaultNextMinor` `TestDatasetVersion`; not audited: #99 |
| AO-06 | Everything but control-plane skew works identically on managed and self-managed clusters (managed add-on images included). | `TestMatchAddOnsRealWorldImages` `TestCollectVersionsManagedClusterEmptyControlPlane`; not audited: #99 |
| DO-01 | Every number in the README is dated and says how it was measured. | not audited: #99 (KB-04 and PF-01 to PF-04, under repair, are stale numbers) |
| DO-02 | The comparison with pluto and kubent, and the evidence in `docs/research.md`. | not audited: #99 |
| DO-05 | The README's note on `Co-authored-by: Claude` trailers. | not audited: #99 |
| DO-06 | Apache-2.0; third-party licenses in `NOTICE`; endoflife.date data under MIT (`registry/DATA-LICENSE.md`). | not audited: #99 |
| IR-15 | Every behaviour change is listed in the changelog, and 0.1.1 changed only packaging. | not audited: #99 (VS-15, under repair, is one such gap) |
| KB-03 | CI reruns the API-lifecycle generator on every push and fails when the committed copy drifts; upstream drift is a warning. | `ci:kb-freshness` `TestDatasetSanity`; not audited: #99 |
| KB-07 | CI validates the registry and runs `eol-sync -check` on pull requests that touch `registry/`. | `ci:registry` `TestValidate`; not audited: #99 (a pre-audit read of PR #82 found both steps skipped: "no merge base") |
| KB-08 | The weekly `kb-refresh` opens a reviewable PR, CI re-validates it, and a broken refresh is never silent. | not audited: #99 (a pre-audit read found five failed Mondays and no issue) |
| KB-11 | `kb.Load` fails loudly on an empty or corrupt dataset, never yielding a silently empty knowledge base. | `TestParseLifecycleFile` `TestLoadFS` `TestLoad`; an empty registry: not audited, #99 |
| KB-12 | Clean-room: no proprietary code, data, schemas or documents, and no other scanner's dataset copied. | not audited: #99 (KB-05's citations are the automated part) |
| PF-05 | The agent's default requests and limits (50m/64Mi, 200m/256Mi) are enough. | not audited: #99 |
| PF-07 | Each tick issues bounded, paged lists, so the agent's cost is predictable, unlike an informer cache. | `TestCollectAPIUsageFollowsListPagination` `TestCollectHelmPeakHeapIsBoundedByOneRelease`; not audited: #99 |
| SE-11 | CI jobs run with least privilege, actions are pinned by full commit SHA, and the release job uses no caches. | not audited: #99 |
| SE-13 | Vulnerabilities can be reported privately through GitHub's private vulnerability reporting. | not audited: #99 |
| SK-03 | kubectl version skew: the README both lists client skew among the questions answered and calls it out of scope. | not audited: #99 (the two statements contradict each other) |
