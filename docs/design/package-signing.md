# Design note: signing the deb, rpm and apk packages

Status: decided (October 2026): keep the packages unsigned, and tell users
to verify before installing. Revisit if users ask for a package repository.

## Problem

Each release attaches `.deb`, `.rpm` and `.apk` packages built by
GoReleaser's nfpm. They are covered by `checksums.txt`, whose keyless cosign
signature ties it to the release workflow at the release's tag
([Install](../operations/install.md#verify-a-download)), but the packages
carry no signature of their own. `apk add` refuses an unsigned package
unless given `--allow-untrusted`, and the release notes used to say only
that Alpine "needs" that flag. A user who installs a package without first
verifying `checksums.txt` trusts whatever the release page served, and is
taught to switch off the package manager's check.

## Options

1. **Unsigned packages, verification first** (the decision). The release
   notes and the packaging page say that `--allow-untrusted` skips apk's
   check and that the Verify commands (cosign `verify-blob` of
   `checksums.txt` against the exact tag, then `sha256sum -c`) come first.
   No key exists anywhere, which is the rest of the release pipeline's
   stance.
2. **Sign each package with nfpm** (GPG for rpm and deb, an RSA key for
   apk). Brings a long-lived private key into the release: a secret to
   store (as a `release` environment secret, used only by the sign job, not
   by the job that builds), rotate and publish the public half of. It buys
   less than it seems: apt does not check a signature on a `.deb` file
   (dpkg-sig and debsigs signatures are ignored unless debsig-verify is
   configured), and users still have to import our key by hand, which is
   the same out-of-band trust step as running cosign.
3. **A signed package repository** (apt, yum/dnf and apk metadata signed by
   a repository key, hosted on GitHub Pages or a package host). `apt
   install` and `apk add` then verify signatures and get updates the
   normal way. It needs the key of option 2, plus repository metadata
   generation in the release, plus hosting, plus a rollback story (the
   repository metadata, like Latest and the tap, must never move back by
   accident). Real operational work for a single-maintainer project in
   maintenance mode.
4. **Keyless package signatures.** No mainstream package manager verifies
   Sigstore signatures natively today, so this reduces to option 1.
5. **Stop publishing packages.** The archives, Homebrew, the image and
   `go install` cover the same users; fewer channels to sign and yank. It
   removes a convenience people may already depend on.

## Decision

Option 1. The packages stay unsigned, and every place that mentions
`--allow-untrusted` says it skips apk's own check and that the Verify
commands come first. Option 3, with the key held as a `release` environment
secret that only the sign job reads, is the path if users ask for `apt
install` or `apk add` from a repository; option 2 alone is not worth its
key.

## Migration (for option 3, if it is ever taken)

- Generate the repository key offline; store the private half only as a
  `release` environment secret; publish the public half in the
  repository and on the Install page, with its fingerprint.
- Build the repository metadata in the sign job from the packages the
  goreleaser job built (checked against `checksums.txt`, as the job does
  for the image), and publish it only after the release's other checks.
- Document key rotation and a revocation path, and add the repository to
  the yank runbook (a yanked version must be removed from the metadata,
  not only from the release page).
