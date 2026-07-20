# 0006 — CLI subcommands via stdlib flag

- **Status:** proposed
- **Date:** 2026-07-20
- **Confidence:** high

## Context

Issue #6 lands the first real subcommand (`bsearch index`), forcing the CLI
plumbing choice every later subcommand will follow — DESIGN.md's surface is
six flat subcommands (`serve`, `search`, `list`, `get`, `status`, `reindex`)
plus `index` and `version`. A cross-cutting convention like this is exactly
the ADR filter's territory: switching frameworks later means touching every
command. Project conventions demand boring, mature dependencies and stdlib
where reasonable.

## Decision

We will dispatch subcommands with a plain `switch` in `run()`
(`cmd/bsearch/main.go`) and give each subcommand its own stdlib
`flag.FlagSet` (`flag.ContinueOnError`, output to the injected writer, a
custom `Usage`). No CLI framework.

The surface is small and flat: no nested commands, no plugin discovery, a
handful of flags each. `flag.FlagSet` covers that completely with zero
dependencies, and the `run(args, out)` seam keeps every command testable
in-process.

## Alternatives considered

- **spf13/cobra** — the de-facto Go CLI framework; buys shell completion,
  auto-help trees, and nested commands. Rejected: none of that is needed for
  a flat eight-command surface, and it drags in a dependency tree (cobra +
  pflag) for what a `switch` does. Revisit only if the CLI grows nesting or
  completion becomes a real want.
- **urfave/cli** — same trade as cobra with a different API; same rejection.
- **ffcli / ff** — lighter, flag-first; closer in spirit, but still a
  dependency that adds nothing over `flag.FlagSet` at this scale.

## Consequences

- Zero new dependencies; `go.mod` stays inference-and-storage only.
- Every subcommand follows the same pattern: `runX(args []string, out
  io.Writer) error` + its own `FlagSet` — uniform and test-friendly.
- We own help text formatting; no auto-generated completion or man pages.
  Accepted: personal tool, `bsearch <cmd> -h` per command suffices.
- If the surface ever grows nesting (unlikely per DESIGN.md), a framework
  migration touches every command's flag wiring — the known cost of the
  boring choice.
