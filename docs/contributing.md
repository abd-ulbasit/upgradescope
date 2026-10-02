# How to contribute

- **Code**: [CONTRIBUTING.md](https://github.com/abd-ulbasit/upgradescope/blob/main/CONTRIBUTING.md)
  covers the development setup, the tests each change needs, commit and
  pull request conventions, and the design boundaries a change must
  respect. Read the [architecture guide](architecture.md) first.
- **Add-on data**: add or fix an entry in the add-on registry following
  [registry/CONTRIBUTING.md](https://github.com/abd-ulbasit/upgradescope/blob/main/registry/CONTRIBUTING.md).
  Every claim needs a public citation. The
  [add-on request template](https://github.com/abd-ulbasit/upgradescope/issues/new/choose)
  is the place to ask for one without writing it.
- **A wrong finding** (a false blocker, a missed removal): open a bug with
  the `upgradescope version` output and the finding's `key`.
- **Security issues**: privately, as described in
  [SECURITY.md](https://github.com/abd-ulbasit/upgradescope/blob/main/SECURITY.md).

## The docs

This site is built from `docs/` with MkDocs Material (`mkdocs.yml`). The
pinned, hash-locked toolchain is in `hack/docs/requirements.txt`:

```sh
make docs         # build into bin/site with --strict, as CI does
make docs-serve   # live preview on http://127.0.0.1:8000
```

Both create a virtualenv under `bin/` on first use (Python 3.10 or newer).
`--strict` fails on a broken link or anchor, and on a page missing from the
nav.

Some pages are generated; edit their source, then regenerate:

| Page | Source | Command |
|---|---|---|
| `reference/cli/*` | the cobra commands in `internal/cli` | `make docs-gen` |
| `reference/crd.md` | `internal/crd/manifest.yaml` | `make docs-gen` |
| `reference/api.md` | `api/openapi.yaml` | `make docs-gen` |
| `reference/helm-values.md`, `deploy/chart/README.md` | the `# --` comments in `deploy/chart/values.yaml`, `hack/docs/*.gotmpl` | `make helm-docs` |

`make docs-check` fails when any of them is stale; CI's docs workflow runs
it on every pull request, and `go test ./...` covers the Go-generated ones.
Other tests keep the hand-written pages honest: the OpenAPI document against
the server's routes and responses, the metrics reference against the
registered metrics, the webhook and JSON report schemas against real output,
the configuration reference against the loader, and the README's numbers
against the knowledge base. Every public claim is in the
[claims ledger](claims.md), with its proof.
