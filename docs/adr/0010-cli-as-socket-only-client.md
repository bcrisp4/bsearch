# 0010 — `bsearch search` is a socket-only client

- **Status:** accepted
- **Date:** 2026-07-24
- **Confidence:** medium

## Context

DESIGN.md's architecture has the CLI talking to the daemon — "one `bsearch`
binary: daemon (`bsearch serve`) + CLI subcommands as clients", with the
diagram drawing the CLI's only edge into the API, never into the database.
The first milestone shipped before the daemon existed, so `bsearch search`
opened the index directly and embedded the query in-process.

With the daemon landing (issue
[#11](https://github.com/bcrisp4/bsearch/issues/11)) both paths can exist,
and neither the design doc nor the earlier ADRs say what happens when the
daemon isn't running. Leaving it unstated means the answer gets decided
accidentally, by whichever path someone edits next.

## Decision

We will make `bsearch search` a client of the daemon's unix socket, with **no
direct-database fallback**. Its flags become `--socket`, `--limit`, `--json`;
`--config` and `--db` move to `bsearch serve`, which is now the only process
that reads inference configuration or opens the index for querying.

The client is deliberately thin. The daemon validates the request, applies the
prefix templates, checks that the index's embedding identity still matches the
configured one, and writes every error message; the CLI renders what it is
told. `--json` copies the daemon's response body through verbatim rather than
re-encoding it, so fields a newer daemon adds (M4's `score` and `summary`)
reach consumers that understand them even when the local binary does not.

`bsearch index` and `bsearch eval` keep direct database access. Indexing is a
writer and the daemon has no scheduler yet (#12, #13); eval runs against its
own scratch index and must stay reproducible in one process. Both are
transitional: DESIGN.md's long-term CLI has no `index` at all, and its
successor `bsearch reindex` is a call to the daemon (#24).

Rationale: one query path means one place where a query is validated, embedded
and matched against the index's identity. Two paths would be two — and they
would drift silently, because the failure mode is a query landing in a
different vector space and returning plausible nonsense, not an error anyone
would notice.

## Alternatives considered

- **Socket-first with direct-DB fallback.** The friendliest behaviour, and the
  reason to reject it: a fallback that silently answers from a different code
  path is the worst version of a bug report — "search gives different results
  sometimes". It also doubles the surface that has to agree about prefixes and
  compatibility, and would keep inference configuration loaded in a process
  that otherwise needs none.
- **Keep the CLI direct-DB, daemon additive.** Smallest change, contradicts
  DESIGN.md's architecture, and leaves the daemon's API untested by daily use —
  the socket would only be exercised by tests and, later, the MCP shim.
- **A `--local` flag to force the old path.** An escape hatch nobody would
  remove, keeping both paths alive under a nicer name.

## Consequences

- Searching requires a running daemon. Until launchd packaging lands (#15)
  that means starting `bsearch serve` by hand, which is a real regression in
  convenience for the M1 workflow, accepted because the window is one
  milestone. The client says so precisely: "the bsearch daemon is not running
  (no socket at …) — start it with 'bsearch serve'", and distinguishes that
  from a stale socket left by a daemon that died.
- The daemon reads its configuration once, at startup. Changing
  `config.toml` needs a restart, and error messages that mention re-indexing
  now also mention restarting the daemon, because either can be the stale
  half. A SIGHUP reload is a follow-up, not part of this.
- The socket contract is exercised by ordinary use, so a break in it shows up
  immediately rather than when the MCP shim is written.
- `--json` becomes forward-compatible by construction, and the CLI no longer
  needs to know the response shape except to render it.
- Errors reach the user as prose on stderr with exit status 1 — the envelope
  is a wire format and never printed. Machine consumers get the exit status;
  those wanting structured errors should speak to the socket directly, which
  is what the MCP shim will do.
