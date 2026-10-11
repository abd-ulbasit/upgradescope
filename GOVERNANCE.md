# Governance

How upgradescope is run today, how decisions are made, and how someone
becomes a reviewer or a maintainer. It adapts the maintainer-based model of
the [CNCF project template](https://github.com/cncf/project-template) to a
project that has one maintainer now and means to have several.

## Today: one maintainer

upgradescope has a single maintainer, listed in [MAINTAINERS.md](MAINTAINERS.md),
who decides. That is a fact about the project's size, not a design: the
rest of this document is how the project works as it grows, and the
sections below say where the one-maintainer case differs.

The maintainer commits to:

- **Security response** within the targets (not an SLA) in
  [SECURITY.md](SECURITY.md): acknowledgement within 3 business days, an
  initial assessment within 10, and a fix or mitigation for a confirmed high
  or critical issue within 30 days of acknowledgement, shipped as a patch
  release.
- **Releases** cut from a tag by the release workflow, signed, with every
  breaking change under Changed in [CHANGELOG.md](CHANGELOG.md), as the
  [compatibility policy](docs/compatibility-policy.md) requires.
- **Review** of every pull request and issue, including the maintainer's
  own work: changes reach `main` through a pull request whose required
  checks (`ci-ok` and `breaking-change`) pass. The `main` ruleset lets a
  repository admin bypass it; every first-parent commit on `main` after
  `4bca610a` (27 July 2026) up to `2edd2862` (12 October 2026), 135 of them, is
  a merged pull request. Much of the code is written by coding agents (the
  `Co-Authored-By` trailers say which); it goes through the same pull
  requests and checks, and the maintainer who merges it is accountable for
  it.
- **Honest claims**: every public claim is in the
  [claims ledger](docs/claims.md) with the test that proves it, or the
  issue that tracks its gap.

## How decisions are made

- **Issues and pull requests are where decisions happen.** Anything larger
  than a bug fix or a registry entry starts as an issue, as
  [CONTRIBUTING.md](CONTRIBUTING.md#pull-requests) asks, so the design is
  agreed before the code is written. The [roadmap](ROADMAP.md) and the
  [milestones](https://github.com/abd-ulbasit/upgradescope/milestones)
  are maintained the same way.
- **`needs-decision`** marks an issue that is waiting on a maintainer's
  decision rather than on work. The decision, and the reason for it, is
  written in the issue before the label comes off.
- **With one maintainer**, the maintainer decides, in the open, on the
  issue or pull request.
- **With more than one**, decisions are made by lazy consensus: a proposal
  stands when no maintainer objects within 5 business days of it being
  marked `needs-decision`. An objection is discussed on the issue; if that
  does not settle it, the maintainers vote, and a simple majority of them
  decides. A change to this document, adding or removing a maintainer, or a
  change to the project's license needs two thirds of the maintainers.

## The contributor ladder

Each step adds to the one before. Nobody is required to climb it.

### Contributor

Anyone who opens an issue, reports a wrong finding, adds or corrects a
registry entry, reviews a pull request, improves the docs or sends code.
A contributor follows the [Code of Conduct](CODE_OF_CONDUCT.md) and
[CONTRIBUTING.md](CONTRIBUTING.md).

### Reviewer

A reviewer's approval counts toward merging in the areas they review, and
they are expected to review pull requests there within about a week.

To become one, a contributor has:

- had at least 5 substantial pull requests merged (code, docs or registry
  entries) over at least 3 months;
- reviewed at least 5 pull requests with feedback that the author acted on;
- shown they know the design rules in CONTRIBUTING.md (a pure engine,
  "not assessed" instead of a crash, a read-only agent, cited registry data,
  clean-room sources).

A contributor may nominate themselves, or a maintainer may nominate them,
in a public issue; the maintainers decide as above. A reviewer is named in
`.github/CODEOWNERS` for their areas.

### Maintainer

Maintainers own the project: they merge, cut releases, answer security
reports, set the roadmap and keep this document.

To become one, a reviewer has:

- been a reviewer for at least 6 months and reviewed at least 20 pull
  requests;
- led at least one change across components (a feature, a release, or a
  migration) from issue to release;
- shown judgment on the project's commitments: the compatibility policy,
  the security model, citations and the clean-room rule.

A maintainer nominates them in a public issue, and two thirds of the
maintainers approve. The new maintainer is added to
[MAINTAINERS.md](MAINTAINERS.md) and gets the repository's maintain role.

## Removing a maintainer or reviewer

- **Stepping down**: anyone may step down at any time by saying so; a
  maintainer becomes Emeritus in MAINTAINERS.md.
- **Inactivity**: a reviewer or maintainer with no review, merge or
  release for 12 months is asked whether they want to stay; with no answer
  within a month, they move to Emeritus.
- **Removal**: for a breach of the Code of Conduct or of these rules, two
  thirds of the other maintainers may remove a reviewer or maintainer.

## Code of Conduct

The project follows the [Code of Conduct](CODE_OF_CONDUCT.md), which says
how to report a breach privately. The maintainers handle reports, and a
maintainer who is the subject of a report takes no part in handling it.
While there is one maintainer, there is no one else in the project to hand
such a report to; GitHub's own abuse reporting is the route outside it.

## Toward vendor neutrality

The intent is a multi-maintainer, vendor-neutral project: maintainers from
more than one organisation, and no single organisation holding a majority
of maintainers once there are three or more. upgradescope is Apache-2.0
and owned by no company; the steps toward that model (a second maintainer,
contributor sign-off, an application to the CNCF Sandbox) are tracked on
the [roadmap](ROADMAP.md).
