# 0001 — Run CI natively on macOS runners

- **Status:** proposed
- **Date:** 2026-07-19
- **Confidence:** high

## Context

bsearch links SQLite and sqlite-vec statically through cgo (DESIGN.md, "SQLite
driver"): pure-Go SQLite drivers cannot load C extensions, and static linking is
what keeps distribution to a single self-contained binary. Two properties follow
and shape every CI decision:

- **Nothing cross-compiles.** Cross-compiling cgo is painful enough that
  DESIGN.md already records a future Linux port as "builds on Linux CI".
- **A growing share of the code will be `//go:build darwin`** — the FSEvents
  watcher, launchd integration, and power-state detection are all macOS APIs
  behind ports.

Ben's other Go repos (bfeed, pi5_exporter) run CI on `ubuntu-latest` and prove
their deployment target with a `CGO_ENABLED=0 GOOS=… go build` step. That
pattern cannot work here, so the question had to be answered from scratch before
any code landed.

## Decision

We will run **both CI jobs (`test` and `lint`) on `macos-latest`**, with
`CGO_ENABLED=1` set at workflow level.

Linting runs there too, not just testing. On a Linux runner every
`//go:build darwin` file is excluded before the linter parses it, so the
platform-specific code — exactly where the macOS-specific risk lives — would be
silently unlinted while the job reported green.

`CGO_ENABLED=1` is explicit rather than relying on the default, so a stray
environment setting cannot produce a pure-Go build that doesn't reflect what
ships.

## Alternatives considered

- **Ubuntu runners only** — cheaper and faster to queue, but cannot link the
  cgo build at all, and silently skips darwin-tagged files during lint. The
  green check would be misleading in precisely the risky area.
- **Matrix: macOS + Ubuntu** — would back DESIGN.md's "nothing should
  gratuitously block Linux later". Rejected for now: the Linux half breaks the
  moment the FSEvents/launchd adapters land, unless build-tag discipline is
  already in place and Linux adapters exist. Worth revisiting at the point
  someone actually wants a Linux port.
- **Self-hosted macOS runner** — no queue wait, but an always-on machine to
  maintain for a hobby project. Rejected; GitHub's macOS runners are free for
  public repositories.

## Consequences

- CI proves the thing that actually ships: an arm64 macOS cgo build.
- macOS runners queue more slowly than Linux ones and are metered at a higher
  multiplier — free here only because the repository is public. If bsearch ever
  goes private, CI cost becomes a real consideration.
- Every job pays a macOS setup cost, so job splitting is not free; see the note
  in `docs/ci.md`.
- A future Linux port needs a second runner and build-tag discipline before the
  matrix can be re-introduced. That is the condition that would revisit this.
