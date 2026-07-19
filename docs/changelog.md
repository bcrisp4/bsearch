# Changelog policy

bsearch keeps a human-curated changelog at [`CHANGELOG.md`](../CHANGELOG.md) in
the [Keep a Changelog](https://keepachangelog.com/en/1.1.0/) format. It is the
single source of truth for release notes — at release time the `[Unreleased]`
section becomes the GitHub Release body (no commit-message changelog is
generated).

This document is the requirement for **every contributor, human or AI agent**.

## The rule

**Every pull request that changes behaviour adds an entry under `[Unreleased]`
in `CHANGELOG.md`.** Write the entry from the *user's* point of view — what
changed for someone running bsearch — not a restatement of the commit. One net
entry per change beats one per commit.

Use the Keep a Changelog categories, in this order:

| Category     | Use for                        |
|--------------|--------------------------------|
| `Added`      | new features                   |
| `Changed`    | changes to existing behaviour  |
| `Deprecated` | soon-to-be-removed features    |
| `Removed`    | removed features               |
| `Fixed`      | bug fixes                      |
| `Security`   | vulnerabilities fixed          |

Only include the categories you actually need. Keep entries terse and in the
present tense.

## When you may skip an entry

Some PRs genuinely have no user-facing change: CI/tooling tweaks, internal
refactors with no behaviour change, test-only changes, dependency bumps,
documentation. For those, apply the **`skip-changelog`** label to the PR. Prefer
adding an entry when in doubt — the label is a deliberate "this changes nothing
a user would notice".

Design and ADR changes are documentation: `skip-changelog`. The changelog
records what the software does, not what was decided about it.

## Enforcement

Not yet automated. The enforcing `changelog` CI job, the Dependabot changelog
helper, and the release wiring are tracked in the CI follow-up issue (see
[`ci.md`](ci.md)). Until then the rule is honour-system, applied during review.

Once automated, the job will fail a PR unless `CHANGELOG.md` appears in its diff
against the base branch, and will be skipped by the `skip-changelog` label. That
requires two one-time repo settings outside the codebase: a `skip-changelog`
label, and the `changelog` check marked **required** on protected `main`.

## How it flows into a release

Before tagging, rename `[Unreleased]` to the new version and open a fresh empty
one:

```markdown
## [Unreleased]

## [0.1.0] - 2026-08-01

### Added
- ...
```

The release workflow (once it exists) extracts that section's body and passes it
to GoReleaser as the release notes. Also update the compare/release links at the
bottom of `CHANGELOG.md` when you cut a version.
