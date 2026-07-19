# 0003 — Use BurntSushi/toml for config parsing

- **Status:** proposed
- **Date:** 2026-07-19
- **Confidence:** high

## Context

DESIGN.md fixes the config format: a single hand-edited TOML file at
`~/.config/bsearch/config.toml`. Go's standard library has no TOML support,
so parsing it means taking on the project's first non-stdlib dependency —
exactly the class of choice the ADR filter catches. Requirements: mature and
maintained, decodes into structs, supports `encoding.TextUnmarshaler` (the
`Interval` type parses `"5m"` or the literal `"defer"`), and can detect
unknown keys so a typo'd key fails loudly instead of silently falling back
to defaults (decided with Ben during issue #1 planning).

## Decision

We will use `github.com/BurntSushi/toml` for all config parsing.

It is the oldest and most widely used Go TOML library — the "boring, mature
dependency" the project conventions call for. `toml.DecodeFile` returns
`MetaData`, whose `Undecoded()` lists every key the struct didn't consume,
giving strict unknown-key rejection without a separate strict mode. It
honors `encoding.TextUnmarshaler`, covering the duration-or-`"defer"`
`Interval` type. Pure Go, no transitive dependencies.

## Alternatives considered

- **pelletier/go-toml/v2** — actively developed, faster, strict decode with
  line-numbered errors. A fine library; rejected only because BurntSushi is
  the more boring choice and config files are tiny, so parse speed and
  benchmark deltas are irrelevant here.
- **Stdlib-only format (JSON via `encoding/json`)** — zero dependencies, but
  contradicts DESIGN.md (Config row: TOML, chosen for human-edited config
  with comments). JSON has no comments; not re-litigated.
- **Hand-rolled TOML subset** — no dependency, but a parser is exactly the
  kind of clever code the maintainability goal forbids.

## Consequences

- First third-party dependency in `go.mod`; version pinned like everything
  else, Dependabot watches it.
- Unknown-key strictness is implemented in our loader via
  `MetaData.Undecoded()` — a convention the config package must keep as
  fields are added, enforced by a test.
- BurntSushi/toml's error messages carry less position detail than
  pelletier v2's; accepted, config files are ~20 lines.
- Swap cost is low: parsing is confined to `internal/config`, and the
  schema is plain structs + `TextUnmarshaler`, which both libraries
  support.
