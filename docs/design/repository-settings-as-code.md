# Design note: repository settings as code

Status: proposed (October 2026). Today the rulesets are JSON files applied
by hand; everything else below is set in the GitHub UI.

## Problem

The security of a release rests on settings outside the code: the
rulesets on `main` and on release tags, the `release` environment and its
reviewer, GHCR package visibility, GitHub's immutable releases setting,
Pages, private vulnerability reporting, push protection, Actions settings
and the `KREW_PLUGIN_IN_INDEX` variable. Only the rulesets are written down
(`.github/rulesets/*.json`), and even they have no plan or apply step: a
change is a hand-run `gh api -X PUT`, `hack/rulesets-live.sh` notices drift
afterwards, and the CI token cannot see `bypass_actors`, so a bypass added
in the UI goes unseen until the owner runs the check by hand (last on
2026-10-11). Everything else can change with no record at all.

## Options

1. **Keep the JSON files and the drift check** (today), and add the other
   settings to that check one by one, read-only (environments, immutable
   releases, Actions permissions). Cheap; still no review of a change before
   it is applied, and the owner's token is still needed to see bypasses.
2. **Terraform with the `integrations/github` provider**: rulesets
   (`github_repository_ruleset`), the environment and its reviewers and
   tag policy (`github_repository_environment`,
   `github_repository_environment_deployment_policy`), Actions variables,
   repository security settings (`github_repository` with
   `security_and_analysis`). A pull request shows `terraform plan`; apply
   runs from a workflow with a token that can administer the repository,
   or by hand. State needs a backend with locking (an S3 bucket with
   `use_lockfile`, or HCP Terraform), and the admin token becomes the most
   sensitive secret in the project, so apply should run in its own
   environment with a required reviewer, never on pull requests.
3. **A GitHub App or Probot-style settings sync** (`.github/settings.yml`).
   Less to run than Terraform, but another third-party app with admin
   rights, and no plan step.

## Recommendation

Option 2, in a separate `infra/github/` root, introduced in two steps:

1. Import the existing rulesets and the `release` environment
   (`terraform import`), so the first plan is empty; pin the provider and
   commit `.terraform.lock.hcl`; run `terraform fmt -check`, `validate` and
   `plan` (read-only token) on pull requests that touch `infra/`.
2. Retire `hack/rulesets-live.sh` and the JSON files once a scheduled
   `terraform plan -detailed-exitcode` (with a token that can see bypass
   actors) replaces the drift check, and apply from a `workflow_dispatch`
   job gated by an environment.

Until then, keep the JSON files authoritative and note every UI change in
`.github/rulesets/README.md` in the same pull request.

## Migration

- No user-facing change. The claims that rest on live settings (IR-06,
  IR-11) move their "Proven by" from `hack/rulesets-live.sh` to the
  scheduled plan.
- The owner creates the admin token (fine-grained, this repository only,
  Administration read and write) and stores it as a secret of the apply
  environment only.
