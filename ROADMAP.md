# Roadmap

Where upgradescope is going from October 2026 into 2027, as three
milestones. Dates are intentions, not promises; the issues in each
[milestone](https://github.com/abd-ulbasit/upgradescope/milestones) are the
current plan, and a change to the plan is made there, in the open, as
[GOVERNANCE.md](GOVERNANCE.md#how-decisions-are-made) describes.

## Where upgradescope stands

As of 2026-10-12: v0.2.2 is published (11 October 2026). It is distributed
through the Homebrew tap, the signed image and OCI Helm chart (Artifact Hub,
as a Verified Publisher), the GitHub Action on GitHub Marketplace, and the
release archives and packages; the krew plugin is in review in krew-index.
There are no external users yet: [ADOPTERS.md](ADOPTERS.md) is empty, and no
issue has come from outside the project.

The direction follows from an assessment of the field in October 2026.
Removed-API detection is a commodity: pluto, kubent and kubepug do it, the
managed providers report live callers for free, and since Kubernetes 1.32
upstream has removed only alpha and beta APIs. What is harder, and what
users ask, is about add-ons: is this add-on end of life, will its version
run on my next Kubernetes, and are its CRD versions still served? And the
cheapest way to try upgradescope is the Action, which needs no install.
So the next releases put add-on depth first, then go looking for users;
the fleet server, dashboard, team rollups, auditor exports, MCP and the
score stay, but are not where new effort goes until users ask for them.

## 1. v0.3.0: add-on depth

Where it stands: 34 add-ons in the registry, each claim cited; 15 synced
from endoflife.date, 15 with Kubernetes ranges, and 4 with no lifecycle
data.
Target: end of November 2026.

- **More add-ons, and more of each**: the add-ons most clusters run, with
  their supported Kubernetes ranges per release line, not only an
  end-of-life date.
- **CRD versions per add-on**: which versions of an add-on's CRDs a
  release serves, so an upgrade that drops one is caught before it lands.
- **Next steps, not only verdicts**: for an end-of-life add-on, the
  supported line or the replacement to move to, with its citation.
- **Keeping the data true**: the weekly knowledge-base refresh and the
  citation checks cover every new entry.

## 2. Launch: first users

Where it stands: published and installable, with no users yet. Target:
December 2026 to February 2027.

- **A first article from real data**: upgradescope run over the most
  popular public Helm charts, with what it found and how it was measured.
- **The Action as the front door**: a demo, a copy-paste workflow and a
  short path from a red pull request to the fix.
- **Places people can talk to the project**: Discussions, an adopters
  thread, and fast answers to the first issues from outside.
- **The krew plugin** merged in krew-index.
- **Listening**: what the first users ask for decides what comes after
  this roadmap.

## 3. CNCF readiness

Where it stands: Apache-2.0, an OpenSSF Scorecard, signed releases, a
security policy, a code of conduct, and, with this roadmap, governance,
maintainer and adopter files. One maintainer, independent. Target: through
2027.

- **Contributor sign-off** (DCO) on every commit.
- **A second maintainer**, ideally from another organisation, through the
  [contributor ladder](GOVERNANCE.md#the-contributor-ladder).
- **The OpenSSF Best Practices badge** and a security self-assessment.
- **Adopters** willing to be named in [ADOPTERS.md](ADOPTERS.md).
- **An application to the CNCF Sandbox**, once the points above hold.

## Issues by milestone

<!-- roadmap-issues: filled in after the issues are created -->

All open milestones and their issues:
https://github.com/abd-ulbasit/upgradescope/milestones
