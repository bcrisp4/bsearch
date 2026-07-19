# bsearch

Local-first semantic search for macOS. Indexes local files (markdown, PDFs,
office docs; later images and Apple Mail) and serves semantic + keyword search
to the CLI, a unix-socket HTTP API, and AI agents via MCP.

**Read `DESIGN.md` before making architectural decisions — it records the
decisions, their rationale, and the rejected alternatives (Closed issues).
Don't re-litigate them; if a change contradicts the design doc, update the doc
in the same PR.**

## Architecture decisions (ADRs)

`DESIGN.md` holds the pre-build decisions; ADRs in `docs/adr/` capture
decisions made *during* development, one immutable record each. Consult
existing ADRs before contradicting one; supersede with a new ADR rather than
editing an accepted one.

Write an ADR when the penalty for being wrong is high and the choice is
expensive to reverse — schema changes, API/socket contracts, auth, new
external dependencies, swapping a core library, or cross-cutting conventions.
Cheap-to-swap choices don't need one.

Invoke the `adr` skill to draft one (`Skill` tool, name `adr`). It writes
`docs/adr/NNNN-*.md` as **Proposed**; only Ben flips a record to Accepted.
If `docs/adr/` doesn't exist yet, the skill bootstraps it on first use.

## Quick facts

- **Language:** Go. Single binary: daemon (`bsearch serve`, launchd) + CLI
  subcommands as clients.
- **Architecture:** Hexagonal (ports & adapters). Ports are Go interfaces
  (`ConverterPort`, `EmbedderPort`, `SummarizerPort`, `WatcherPort`, storage
  ports). Domain logic never imports adapters.
- **Storage:** one SQLite database — catalog + queue + FTS5 + sqlite-vec
  (statically compiled via cgo). Production pragmas (WAL, busy_timeout,
  foreign_keys ON, IMMEDIATE writers). The index is derived data: worst-case
  migration is drop-and-reindex.
- **Search:** hybrid — brute-force KNN (sqlite-vec) + BM25 (FTS5) fused with
  RRF. No ANN; quantize + rescore is the scaling lever.
- **Doc conversion:** bscribe HTTP service (localhost:18000) for binary
  formats; text/markdown parsed in-process. Handle bscribe-down gracefully
  (queue + retry, never fail search).
- **Inference:** BYO OpenAI-compatible server (LM Studio locally). No models
  in-process, ever.
- **Privacy:** everything local; never log query text or document content at
  default log level; no telemetry.

## Constraints to respect in code

- Runs on a battery-powered laptop: background work is batched and
  power-aware; near-zero CPU when idle.
- Search latency SLO: p95 < 500 ms warm. Indexing is allowed to be slow;
  queries are not.
- Single user, single machine. No auth on the unix socket in v1; any TCP
  listener requires an auth story first (see DESIGN.md Security).

## Conventions

- Prefer boring, mature dependencies; stdlib where reasonable.
- Keep write transactions short and batched (busy-timeout discipline).
- Version pipeline stages (chunker, models) in catalog metadata so partial
  rebuilds work.
