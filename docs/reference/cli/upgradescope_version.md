## upgradescope version

Print the version, build and knowledge base details

### Synopsis

Print the upgradescope version, the commit and date it was built from, the Go
toolchain, and the embedded knowledge base: the k8s.io/api release its API
lifecycle data comes from, the newest Kubernetes minor it covers (the
horizon: --target beyond it reports a kb-stale warning), and the date the
add-on registry was last updated. --version prints the same text.

```
upgradescope version [flags]
```

### Examples

```
  upgradescope version
  upgradescope version --output json | jq -r .kbHorizon
```

### Options

```
  -h, --help            help for version
      --output string   output format: text|json (default "text")
```

### SEE ALSO

* [upgradescope](upgradescope.md)	 - Continuous Kubernetes upgrade-readiness scanner

