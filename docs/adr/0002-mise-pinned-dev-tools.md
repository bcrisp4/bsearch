# 0002 — Pin dev tools per project with mise

- **Status:** accepted
- **Date:** 2026-07-19
- **Confidence:** medium

## Context

CI and local development must run the *same* linter: a golangci-lint version
difference between them turns a green local run into a red CI run (or worse,
the reverse) for reasons unrelated to the code.

The pattern carried over from Ben's other Go repos installs tools with
`go install …@version` into `GOPATH/bin` and calls them from a Makefile. That
directory is **shared by every Go repo on the machine**, so the last
`go install` wins: bfeed pins golangci-lint v2.12.2, pi5_exporter pins v2.12,
and whichever repo ran `make tools` most recently decides what all of them
actually lint with. They agree today by coincidence. Separately, the CI
workflow pinned the version a *second* time (in `golangci-lint-action`'s
`version:` input), giving two places to drift.

mise is already installed on the machine but was not being used for any of
this.

## Decision

We will pin dev tools per project in `mise.toml` and invoke them through
`mise exec --` from the Makefile. CI installs from that same file via
`jdx/mise-action` and then runs the **Makefile targets** (`make lint`,
`make vulncheck`), so the Makefile is the single definition of what those
checks mean and `mise.toml` the single definition of which versions run them.

Currently pinned: `golangci-lint` and `govulncheck` (the latter through mise's
`go:` backend, as it has no registry entry).

Go itself is deliberately **not** pinned in `mise.toml`: the `toolchain`
directive in `go.mod` is the anchor, honoured by both the local `go` command
and `actions/setup-go`. Pinning it in two places would recreate the drift this
decision exists to remove.

## Alternatives considered

- **`go install …@version` into GOPATH/bin** (the other repos' pattern) — one
  global binary shared across every Go project; last install wins. This is the
  specific problem being fixed.
- **`go run …@version` in the Makefile** — genuinely per-project and needs no
  new tooling, which fits the "be boring" rule. Rejected narrowly: it rebuilds
  each tool from source on every version change, offers nothing for non-Go
  tools (syft, goreleaser) that the release work will want, and CI would still
  need its own install path.
- **Go 1.24+ `tool` directives in go.mod** — the canonical modern Go answer and
  Dependabot-updatable. Rejected: it drags golangci-lint's large dependency
  graph into `go.mod`/`go.sum`, adding noise to the tidy-drift check and to
  vulnerability scanning of a project whose own dependency list should stay
  short and auditable.
- **Homebrew / whatever is on PATH** — no per-project pinning at all; the
  status quo that motivated this.

## Consequences

- Local and CI run byte-identical tool versions, pinned in one file.
- mise becomes a **prerequisite for developing bsearch** — `make lint` fails
  without it. Documented in README and `docs/ci.md`. This is the main cost:
  a contributor with a working Go toolchain still cannot lint.
- Dependabot has no mise ecosystem, so tool bumps are manual (`mise outdated`).
  Previously the golangci-lint action version was Dependabot-visible; that
  automation is lost. Go module dependencies and GitHub Actions are unaffected.
- Dropping `golangci-lint-action` also dropped its inline PR annotations —
  golangci-lint v2 removed the `github-actions` output format and does not
  auto-annotate (verified against v2.12.2). Recovered with a GitHub problem
  matcher committed at `.github/golangci-lint-matcher.json`.
- Adding future tools (syft and goreleaser at release time) is now a one-line
  change in one file.
- The editor is covered by the same pin: `golangci-lint-langserver` is pinned
  too, because the Zed extension passes the project shell environment only to a
  langserver it finds on `PATH`. Without that, in-editor diagnostics would come
  from whatever `golangci-lint` a bare `PATH` resolved — the shared-`GOPATH/bin`
  problem re-entering through the editor. See `docs/ci.md`.

Confidence is medium, not high: the mechanism is sound, but most of it is
untested by use. What would raise it:

- [x] **The editor resolves to the pinned binary.** Verified 2026-07-19: a
      `misspell` diagnostic in Zed, served by the mise-installed langserver.
- [ ] **Green CI runs, including a cold cache.** `jdx/mise-action` installing
      the `go:` backend on a macOS runner is unproven, as is what it costs.
- [ ] **One bump cycle survived** — `mise outdated` → bump → green. The lost
      Dependabot automation is this decision's biggest unmitigated regression;
      the open question is whether manual bumps happen or quietly rot.
- [ ] **Release tools added via mise** (syft, goreleaser at #25). "One line to
      add another tool" is a main reason this beat `go run …@version`.
- [ ] **A second repo adopts it.** Until bfeed and pi5_exporter stop sharing
      `GOPATH/bin`, this is one repo opting out of a machine-wide problem, not
      a fix for it.

If the `mise exec` prefix on every tool target grates in practice, the
`go run …@version` alternative above remains a small, local change.
