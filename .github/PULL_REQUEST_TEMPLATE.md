<!-- Thanks for contributing! See CONTRIBUTING.md for the dev loop and conventions. -->

## What and why

<!-- What this changes, and why. Lead with the user-visible effect. -->

Fixes #<!-- issue number, or "Refs #N" if this only partly addresses it -->

## How it was tested

<!-- The commands you ran and what they showed. For detection or scoring changes,
     explain every golden-file diff (internal/engine/testdata). -->

## Checklist

- [ ] `make test` and `make lint` pass, and `gofmt -l cmd internal registry tools` prints nothing
- [ ] `go test -race` passes for the packages I touched
- [ ] New behaviour has tests, and any golden diff is intentional and explained above
- [ ] User-facing changes are documented (README, `deploy/chart/README.md`, flag help)
- [ ] Registry changes follow `registry/CONTRIBUTING.md`, and `make eol-check` passes
- [ ] Commit subjects use conventional prefixes (`feat:`, `fix:`, `docs:`, …), and a breaking change has `!` in the subject (`feat!:`)
