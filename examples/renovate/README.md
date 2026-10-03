# Renovate preset for upgradescope's add-ons

`upgradescope.json` is a Renovate preset that groups and prioritizes Helm
chart bumps for the add-ons upgradescope's registry tracks.

**It is static.** The chart names and the end-of-life list were written by
hand from `registry/data`; the preset does not read a scan, your cluster, or
the registry at run time, so it knows nothing about what *your* scan flags.
(Generating it from `upgradescope scan --format json` is possible and is not
done here.) `go test ./examples` fails when the preset's chart list stops
matching the registry's, so a new add-on or a changed support status cannot
drift in unnoticed, but a copy of the file in your repository will not
follow.

## What it does

For packages from the `helm` datasource named like a registry chart:

- **minor and patch** bumps go into one PR, `upgradescope add-ons`, labelled
  `upgradescope`;
- **major** bumps stay out of the group, are never automerged, wait for
  approval on the Dependency Dashboard, and are labelled `upgradescope` and
  `major`;
- **end-of-life add-ons** (today `ingress-nginx`, archived 2026-03-24) are
  first in the queue (`prPriority: 10`), never automerged, labelled
  `upgradescope` and `eol`, and the PR says that the bump does not make a
  retired project supported.

The rules match on the `helm` datasource and the chart name, not on a
manager, so they apply wherever a manager that reads charts (`helmv3`, `flux`,
`argocd`, and others) finds one. A chart Renovate names differently, for
example one pulled from an OCI registry by path, may not match. The preset has
not been run against a real repository.

## Use

Copy the file into your repository, or extend it from your Renovate config:

```json
{
  "extends": ["github>abd-ulbasit/upgradescope//examples/renovate/upgradescope"]
}
```

Extending follows the default branch of this repository (the `//` path form
is Renovate's, and was not tried here); copying pins you to a version of the
file that you control and review.

## Checks

`make examples-test` runs `renovate-config-validator --strict --no-global` on
the file, with the Renovate release pinned in `hack/renovate`. The
`--ignore-scripts` install leaves Renovate's RE2 binding unbuilt, so the
validator warns that regex validation may be inaccurate; the preset contains
no regular expressions.
