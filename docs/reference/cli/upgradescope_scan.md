## upgradescope scan

Scan a cluster (or rendered manifests) for upgrade readiness

### Synopsis

Scan a cluster (or rendered manifests) for upgrade readiness.

Exit codes: 0 when the gate passes; 1 on an operational error, including an
invalid config file or baseline and a report that could not be written; 2
when the gate fails, which includes an unknown verdict.

The gate (--fail-on) fails when a finding at or above the threshold remains,
or (unless --allow-incomplete) when a required check was not assessed, so a
blocker may have been missed. A --target that is not an upgrade of the
cluster (at or below the minor its kube-apiserver runs) always fails it.

Files mode (--files): every *.yaml, *.yml and *.json file under the directory,
or the one file named, is decoded as kubectl apply -f decodes it: each
document of a YAML stream and each object of a JSON stream (NDJSON,
pretty-printed or adjacent), List items expanded, a duplicate key taking its
last value. Every document is also decoded by kubectl's own decoder: where it
finds an object the scan's line-tracking YAML reading did not, kubectl's
objects are counted (located at the document's first line), with a warning.
Documents that are not Kubernetes objects are skipped. A document
that cannot be decoded is skipped with a warning; when its text names an API
the knowledge base lists as removed, api-usage is not assessed, so the verdict
is at least unknown and the gate fails unless --allow-incomplete. VCS metadata,
node_modules and Go vendor/ directories (with modules.txt) are not walked, nor
are symlinked directories (kubectl apply -R does not follow them either); each
of these but VCS metadata is a warning.

Suppression: ignore rules in .upgradescope.yaml (found in the scan root,
i.e. the --files directory or else the working directory, then at the git
repository root; or named with --config) accept findings by key or category,
optionally only for objects matching namespace/name/file globs, with a
required reason and an optional expires date. Objects can opt out with the
upgradescope.dev/ignore and upgradescope.dev/ignore-reason annotations.
Suppressed findings do not count toward score, verdict or gate, and every
output lists them with their reason. An expired rule stops applying and
prints a warning.

Baseline: --baseline takes the JSON report of an earlier scan (--output json,
or --write-baseline). Findings whose key and listed objects it already had
are marked unchanged and do not fail the gate; anything else is new. Score
and verdict still count unchanged findings, and an unassessed required check
still fails the gate.

```
upgradescope scan [flags]
```

### Examples

```
  # The current kubeconfig context's cluster against Kubernetes 1.37
  upgradescope scan --target 1.37

  # Another context, as JSON
  upgradescope scan --context prod --target 1.37 --output json

  # Rendered manifests in CI: SARIF for code scanning, exit 2 on a blocker
  upgradescope scan --files rendered/ --target 1.37 --output sarif > upgradescope.sarif

  # Fail only on findings that are new since an accepted scan
  upgradescope scan --files rendered/ --target 1.37 --write-baseline baseline.json
  upgradescope scan --files rendered/ --target 1.37 --baseline baseline.json
```

### Options

```
      --allow-incomplete        with --fail-on blocker|warning, do not fail when the verdict is unknown (required checks not assessed); a --target that is not an upgrade still fails
      --baseline string         JSON report of an earlier scan (--output json or --write-baseline): the gate fails only on findings that are new since
      --config string           config file with ignore rules (default: .upgradescope.yaml in the scan root, else at the git repository root)
      --context string          kubeconfig context to use
      --fail-on string          exit 2 if findings at/above this severity, or the verdict is unknown: blocker|warning|never (default "blocker")
      --files string            scan rendered manifests in this file or directory (*.yaml, *.yml, *.json) instead of a live cluster
  -h, --help                    help for scan
      --kubeconfig string       path to kubeconfig (default: standard loading rules)
      --output string           output format: table|json|sarif|markdown (default "table")
      --target string           target Kubernetes minor version, e.g. 1.36 (required)
      --team-label string       namespace label used for team attribution (default "team")
      --write-baseline string   also write this scan's JSON report to this path, for a later --baseline
```

### SEE ALSO

* [upgradescope](upgradescope.md)	 - Continuous Kubernetes upgrade-readiness scanner

