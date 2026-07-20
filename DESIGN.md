# bsearch — Design

| | |
|---|---|
| Author | Ben Crisp (ben@thecrisp.io) |
| Status | Draft |
| Created | 2026-07-19 |
| Updated | 2026-07-19 (adversarial review folded — see Closed issues; "crawler" renamed to "discovery") |

## Objective

bsearch indexes the files on your Mac — documents, PDFs, emails, images — and
lets you and your AI tools search them semantically, entirely locally.

## Background

Two motivations, honestly ranked: this is a fun project first, and a practical
one second.

The practical need: AI agents (Claude Code and similar) work dramatically
better with relevant context, but that context is scattered across the
filesystem in different formats — markdown notes, PDFs, office documents,
emails, images. There is no good local way to say "find me the documents
relevant to X" and hand results to an agent. Spotlight is keyword/metadata
search only — no semantic retrieval.

Prior art, own: `lore`, a semantic search engine for my Obsidian wiki,
vibe-coded quickly and now hard to maintain — I've lost the mental model of how
it fits together. It also has a design flaw worth fixing: it indexes
new/modified files at query time, adding user-visible lag, with no background
indexing. bsearch starts fresh rather than extending lore, with lessons carried
over: index ahead of time in the background; keep the design documented (this
doc) so the mental model survives.

## Goals

- **Give AI agents cheap access to local context.** An agent should be able to
  find the documents relevant to a task and pull them into context with minimal
  context-window spend — summaries first, full content on demand.
- **Find anything by meaning.** Any supported document on the machine is
  findable by describing what it's about, not by remembering filenames or
  keywords. Search must feel snappy — indexing happens in the background and is
  allowed to be slow; query speed is what the user experiences. Retrieval
  quality beats pure-vector: semantic and keyword signals combine (hybrid
  search).
- **Nothing leaves machines I control.** Content, embeddings, queries, and
  metadata stay on-device by default; at most they reach inference endpoints I
  choose (a private-tailnet inference box is legitimate — see Security §3).
  Privacy and data sovereignty are goals in their own right, not side effects
  of the architecture.
- **Stay understandable for years.** The design must survive long gaps between
  hacking sessions. Boring choices, documented decisions, clear boundaries —
  explicitly the anti-`lore` goal.

## Non-goals

- **No multi-user support.** Single user, single machine. There is no concept
  of accounts, tenants, or sharing.
- **No bundled inference.** bsearch never runs models itself — you bring an
  inference server (Ollama, LM Studio, vLLM, …) speaking a standard protocol.
  Keeps bsearch small and lets the model stack evolve independently.
- **No remote access by default.** The API listens locally only. Exposing it
  (e.g. over Tailscale) is a deliberate user action, not a supported default.
- **No commercial ambitions.** Personal tool, open source. No hosted version,
  no telemetry, no growth features.
- **Not cross-platform in v1.** Designed for Apple Silicon macOS. Nothing
  should gratuitously block Linux later, but no effort is spent on it.
- **No cloud sync or backup of the index.** The index is derived data; it can
  always be rebuilt from local files (with one caveat: doc_id continuity —
  see Data retention).
- **No cloud sources.** Gmail, Drive, Notion, web pages — out of scope.
  bsearch indexes the local filesystem only (Apple Mail counts: its store is
  local files).

## Missing features (deferred, wanted eventually)

- **Native macOS frontend.** A Swift menu-bar/settings app for monitoring
  indexing progress and editing config. Consequence: v1 is configured by file
  and observed via CLI (`bsearch status`).
- **Image indexing.** Text search over images via a vision model (captioning +
  embedding). Consequence: v1 indexes text-bearing documents only; the
  ingestion pipeline must still be designed so a new media type slots in as
  another converter, not a rework.
- **Email (Apple Mail).** Parsing the local Mail store. Consequence: v1 leaves
  a major personal corpus unsearchable; the discovery design must not assume
  "documents are files a user saved" (mail messages are many small files in a
  library directory — and one that requires Full Disk Access, see the TCC
  constraint).
- **Third-party integrations** (Alfred, Raycast, etc.). Consequence: none for
  v1 — the local API is the integration surface; anyone can build these later.

## Scenarios

### 1. Agent pulls context (primary)

Ben asks Claude Code to review his mortgage options. The agent calls bsearch
over MCP: `search("mortgage renewal terms and rates")`. bsearch returns ten
ranked hits, each with file path, score, and a short summary. The agent scans
the summaries — costing a few hundred tokens, not tens of thousands — decides
two documents matter, and fetches their full markdown with `get`. Total: two
round trips, context window spent almost entirely on the two documents that
matter.

### 2. Interactive search from the terminal

Ben half-remembers a PDF about heat-pump installation quotes from last year.
`bsearch search "heat pump quote"` returns ranked hits with paths and summaries
in well under a second. He opens the right file directly. No remembering
filenames, no Spotlight keyword roulette.

### 3. Background ingest

Ben exports a 40-page PDF report into `~/Documents`. He does nothing else.
Within a few minutes the bsearch daemon notices the new file, converts it to
markdown, chunks it, summarises it, embeds it, and stores the result. The first
search that mentions the report's topic finds it. Ben never ran an "index"
command and never felt the indexing cost. (Prerequisite: the daemon has been
granted Full Disk Access — see the TCC constraint below.)

## Constraints

- **Hardware:** MacBook Pro, Apple M5 Max, 128 GB unified memory. Ample for
  local models, but bsearch shares it with everything else — the index and
  daemon must be lightweight when idle.
- **Portable, often on battery.** Indexing is background work on a laptop, not
  a server job. Continuous CPU/GPU churn is unacceptable on battery. Design
  consequences: batched indexing intervals rather than constant activity,
  modest model sizes, and configurable power-aware behaviour (e.g. defer heavy
  indexing until on AC power, or throttle batch sizes on battery).
- **macOS TCC gates most of the corpus.** On macOS 10.15+, `~/Documents`,
  `~/Desktop`, `~/Downloads`, iCloud Drive, removable/network volumes, and
  most of `~/Library` (including `~/Library/Mail`) are consent-gated. A
  background launchd daemon gets **no consent dialog — just silent EPERM**;
  `~/Library` paths never prompt at all and need a manually-granted,
  non-scriptable **Full Disk Access** entry in System Settings. Design
  consequences: the daemon requires an FDA grant, documented in onboarding;
  discovery treats permission errors as first-class state, surfaced in
  `bsearch status` ("no access to ~/Documents — grant Full Disk Access"),
  never a silent skip. One-shot CLI use (M1) inherits the terminal's grants
  and is unaffected.
- **BYO inference server.** LM Studio today; must not be a hard dependency.
  bsearch speaks a standard protocol (OpenAI-compatible API) so any server
  works.
- **Summary model loaded only when needed.** The summarizer should not sit in
  memory between indexing runs — prefer servers that JIT-load and auto-unload
  after idle (LM Studio TTL; Ollama `keep_alive`); bsearch tolerates
  cold-start latency on the first request of a batch. The **embedding model is
  the deliberate exception**: it is small (hundreds of MB) and must serve
  interactive queries, so it stays resident — that residency is what makes the
  warm-search SLO honest.
- **macOS-first.** Filesystem watching, power detection, and launchd
  integration may use macOS-specific APIs behind ports; nothing should
  gratuitously block a Linux port later.

## SLOs

| Metric | Target | Consequence |
|---|---|---|
| Search latency (warm) | p95 < 500 ms | vector index in-process/local; embedding model resident (see Constraints) |
| Search latency (cold daemon) | < 3 s | acceptable to lazily open index |
| Index freshness (on AC) | new/changed file searchable ≤ 5 min | polling/FSEvents batch interval, not per-write |
| Index freshness (battery) | ≤ 60 min or deferred, configurable | battery constraint |
| Corpus scale | ~100k docs, ~1M chunks | single-node embedded storage sufficient; no server DB. A PDF/email-heavy corpus may exceed 1M chunks — quantization is the planned configuration at that scale (see Vector search) |
| Availability | daemon auto-restarts (launchd); no nines target | it's a laptop |

These numbers are deliberately small and are the justification for the boring
architecture below. If they ever grow by an order of magnitude, revisit the
storage and vector-search rows first.

## Architecture & technology choices

| Concern | Choice | Why | Swap cost |
|---|---|---|---|
| Language | Go | Self-contained single-file binary (cgo links only system libraries — no third-party dylibs; fully static isn't possible on macOS); strong daemon/concurrency story; near-zero background CPU when idle and small RSS (avoids memory-pressure churn); a language I know | Rewrite — mitigated by hexagonal boundaries |
| Structure | Hexagonal (ports & adapters), ports as Go interfaces | Maintainability goal; makes the swap costs in this table real | n/a — this IS the swap mechanism |
| Process model | One `bsearch` binary: daemon (`bsearch serve`, run as launchd LaunchAgent) + CLI subcommands as clients | launchd gives native supervision, start-at-login, restart | Low |
| Storage | SQLite, one database file: catalog + queue + summaries in plain tables, FTS5 for keyword, sqlite-vec for vectors. Production pragmas from day one (WAL, `synchronous=NORMAL`, `busy_timeout=5000`, `foreign_keys=ON`, `temp_store=MEMORY`, tuned `mmap_size`/`cache_size`); writers use `BEGIN IMMEDIATE`; indexing writes in small batches so no write transaction outlives the busy timeout. Schema carries a version; migrations preferred, drop-and-reindex is the fallback of last resort (its true cost is battery-gated local inference over the whole corpus — potentially days) | One file, one engine, transactional consistency across catalog/queue/vectors/FTS; single-writer model fits (indexer writes, queries read; WAL keeps readers unblocked) | Storage behind ports; index is derived data (except doc_id continuity — see Data retention) |
| SQLite driver | cgo-based driver (mattn/go-sqlite3-class) with sqlite-vec statically compiled in via its Go bindings | Pure-Go drivers can't load C extensions; static linking keeps self-contained distribution, no runtime extension loading | Locked to native builds (cross-compiling cgo is painful — a future Linux port builds on Linux CI). Escape hatch: ncruces/go-sqlite3 (wasm) — sqlite-vec ships officially documented bindings for it (`asg017/sqlite-vec-go-bindings/ncruces`); that pure-Go route would also ease cross-compilation |
| Vector search | sqlite-vec `vec0`. Float32 brute-force KNN up to a few hundred thousand chunks; **binary quantization + rescore is the planned configuration at full corpus scale (~1M chunks)**, not an emergency lever. No ANN | Exact (or near-exact with rescore), zero index maintenance, delete-friendly. Scan cost is ~linear in vectors × dims: published 100k×768 runs well under 100 ms warm, but extrapolating the same numbers puts 1M×768 ≈ ~700 ms — over the SLO; the author's own ceiling for float vectors is "the hundreds of thousands". Quantized scan (32× smaller, XOR+popcount distance) + full-precision rescore of top-k×8 retains ~95% recall at ~1.03× total storage. ANN (DiskANN/IVF) immature in sqlite-vec and unneeded at this scale | Further levers: raise mmap/cache (make scan RAM-bound), partition keys. **Acknowledged bet:** sqlite-vec is pre-1.0 (no stable on-disk format guarantee) — version pinned; a format break is covered by drop-and-reindex |
| Keyword search | FTS5 + BM25 over chunks, fused with chunk-level KNN via RRF (k=60 default; semantic/keyword weights configurable). Results collapse to best-chunk-per-document | Hybrid beats pure vector on exact terms (invoice numbers, names); same engine, same database file, one consistency/backup story. Pattern implemented in lore (chunk breadcrumbs + RRF) though never measured there — M2 measures it here | Same DB, additive |
| Doc conversion | bscribe over HTTP behind `ConverterPort`; plain text/markdown handled in-process. Adapter sends bscribe's required bearer token (from config); v1 uses the sync `POST /v1/convert` endpoint and reads the JSON envelope's `content` field. bscribe's native port is 8000 — `localhost:18000` is this machine's host mapping | No parser deps in the binary; LibreOffice/OCR churn isolated in a hardened container (non-root baked into the image; read-only rootfs, capability drop, and memory cap are operator run-flags — required flags recorded in deployment docs); bscribe already runs here and anticipated bsearch as a consumer | Adapter swap (lit CLI subprocess, docling) without touching domain; async job API available if large-doc sync timeouts warrant it |
| Change detection | FSEvents watch (macOS API behind `WatcherPort`) for near-real-time creates, edits, **and deletes**, plus periodic full scan for missed events; change = cheap size/mtime check, content hash to confirm | Freshness SLO without constant scanning; battery-friendly | Linux port = new watcher adapter (inotify) |
| Chunking | Markdown-aware, hand-rolled in Go (see below) | Everything is markdown post-conversion; tractable, dependency-free algorithm | Isolated pure function, versioned |
| Summaries | Pyramid summaries per document: 4 / 16 / 64 words + full text, generated at index time by the summary LLM. Word counts are targets, not contracts (validated and trimmed post-hoc). Documents exceeding the summarizer's context are handled map-reduce style: section/chunk summaries reduced to document summaries; the summarizer's minimum context requirement is part of the versioned stage | Agent context economy — survey cheap, zoom on demand (StrongDM pyramid-summaries technique, which pairs the ladder with MapReduce for over-context inputs) | Additive; regenerable without touching vectors |
| Embeddings / LLM | OpenAI-compatible HTTP (`EmbedderPort`, `SummarizerPort`); model names + endpoints in config. **Per-model query/passage prefix templates** (E5 `query:`/`passage:`, Nomic `search_query:`/`search_document:`, BGE/Qwen3 query instructions) stored in versioned pipeline metadata and applied identically at index and query time — asymmetric embedders lose substantial recall without matched prefixes (lore solves this per model family; lesson carried alongside the breadcrumb one) | BYO inference; LM Studio today | Config change; embedding model swap requires full re-embed (see pipeline metadata for the migration path) |
| API | HTTP+JSON over unix domain socket at `~/Library/Application Support/bsearch/bsearch.sock`, mode 0600 (comfortably under the ~104-char `sun_path` limit); listener abstraction so a TCP listener (with auth) can be added later | OS-enforced same-user access, no open ports, zero auth machinery in v1 | TCP = new listener + auth story; designed-for, not bolted-on |
| MCP | MCP server as a thin stdio shim over the same domain services | First-class agent access — the primary scenario | Thin layer over the API |
| Config | Single TOML file, `~/.config/bsearch/config.toml` (sample in Interfaces). Hand-edited config lives in `~/.config` (CLI convention); machine data lives in `~/Library/Application Support` (macOS convention) — deliberate split | Human-edited, no UI in v1; boring | Trivial |

### Indexing pipeline and queue

Pipeline per document: **discover → convert → chunk → (embed ∥ summarize) →
store.** Embedding and summarization are parallel branches after chunking: a
document becomes searchable as soon as it is embedded; summaries are
fill-later fields that enrich results when ready. A summarizer outage degrades
summary richness, never searchability.

The queue is a SQLite-backed state machine — no external queue infrastructure.

- **Catalog row per file:** `path, content_hash, size, mtime, state,
  stage_versions, attempts, next_retry_at, last_error`. States: `discovered →
  converted → chunked → embedded → indexed`, plus `failed` (permanent) and
  `deleted`. Summarization is tracked as a separate per-document field, not a
  pipeline gate.
- **Enqueue:** FSEvents callbacks and the periodic scan both upsert
  "needs work" rows — idempotent, so rapid saves coalesce naturally. A
  debounce window (~10 s) avoids grabbing files mid-write. Permission errors
  (TCC) are recorded per path and surfaced in `status`, never silently
  skipped. Dataless iCloud files (Optimize Storage placeholders) are skipped,
  not materialized — indexing must never trigger cloud downloads.
- **Dispatch:** a scheduler loop wakes on timer/notify and claims a batch in
  one short `IMMEDIATE` transaction (`SELECT … WHERE state NOT IN ('indexed',
  'failed', 'deleted') AND next_retry_at <= now LIMIT n`). Terminal states
  never re-enter dispatch; purging `deleted` rows is a separate path. Claims
  are tracked in memory — single daemon process, so no claimed-state
  machinery; a crash mid-batch redoes in-flight items on restart, which is
  safe because every stage is an idempotent upsert.
- **Transactions never wrap network calls.** Convert/embed/summarize happen
  first; then a short batched write. An open write transaction must never wait
  on bscribe or an inference server (busy-timeout discipline).
- **Batching where it pays:** embedding calls batch many chunks per HTTP
  request; DB writes batch per transaction and stay short.
- **Health gates for every external service.** Before draining a batch that
  needs an external service, the scheduler probes it (bscribe `/healthz`;
  inference endpoints likewise). Down → skip that batch, log once, retry next
  cycle. Outage time burns no per-file attempts — this applies equally to
  converter, embedder, and summarizer, so a transient outage can never mark
  healthy documents `failed`.
- **Retry:** transient failures with the service healthy → exponential backoff
  via `next_retry_at`; attempts capped, then `failed` with reason. Permanent
  failures (unparseable document) → `failed` immediately. A file change resets
  `failed`.
- **Power-aware gate:** the scheduler consults power state before dispatching
  heavy stages (convert/summarize/embed); on battery it lengthens intervals,
  shrinks batches, or defers entirely, per config. Cheap stages (catalog scan)
  always run.
- **Crash-safe:** all durable state is in SQLite; a daemon restart resumes
  where it left off.
- **Priority:** newly-changed files index before backlog (recency ordering,
  with aging so the initial bulk backlog and due retries can't be starved
  indefinitely).
- **Observable:** `bsearch status` shows per-state counts, failure reasons,
  last-scan and last-progress timestamps, and the current gate reason
  ("deferred: on battery", "embedder unreachable", "no access to ~/Documents")
  — a stalled queue is always distinguishable from a deferred one.

### Converter degradation (bscribe down)

- Conversion is one pipeline stage; bscribe unreachable → binary-format items
  stay pending and retry with backoff (attempts not burned — health gate
  above). Nothing is lost — the queue is durable.
- Partial degradation, not outage: text/markdown items skip conversion and
  keep flowing. Search never touches bscribe.
- Response classification: connection refused / 5xx / timeout → transient,
  retryable. **422** (supported format, unparseable content) → permanent
  `failed`. **415** (unsupported format) and **413** (too large) → permanent
  `failed` with distinct reasons. Never retried until the file changes.
  Prevents poison-file retry loops.
- Visible in `bsearch status`: "1,204 indexed · 37 pending (converter
  unreachable) · 2 failed".

### Chunking

Post-conversion everything is markdown, so chunking is markdown-structural:

- Parse to a heading tree (H1–H6); the base unit is section content under a
  heading.
- Target ~256–512 tokens per chunk; merge tiny neighbours (min ~64); split
  long sections at paragraph boundaries with ~10–15% overlap (max ~1024).
  Token counts are heuristic (≈ chars/4) — there is no tokenizer in-process
  ("no models in-process, ever"), so budgets are approximate and
  model-relative; the min/max bounds carry the slack.
- **Hard ceiling: the embedding model's input limit** (recorded in pipeline
  metadata — distinct from the *dimension* cap; both happen to be measured in
  units of ~1024 but are different limits). An atomic chunk that exceeds it is
  split as a fallback and flagged in `status` — never silently truncated.
- **Breadcrumb prefix:** each chunk is embedded with its heading path
  prepended ("Mortgage Renewal 2026 > Offers > Broker A") — contextualizes the
  chunk for the embedding model. Cheap, implemented in lore (unmeasured there;
  M2's harness can A/B it here).
- Tables and code blocks are atomic — never split mid-table (subject to the
  hard ceiling above).
- **Encoding:** in-process text ingestion detects BOM/UTF-16/UTF-8 and
  transcodes; undecodable files are marked `failed` with reason, never
  ingested garbled.
- Stored per chunk: heading path, byte offsets into the source markdown,
  position ordinal. Offsets let `get` return chunk-in-context.

### Pipeline metadata and model migration

Recorded per document/chunk: content hash, converter version (bscribe),
chunker version, embedding model + dimensions + **prefix templates** + input
ceiling, summarizer model + context requirement. The database schema itself is
versioned.

A search can only use one embedding model — a query embedded with model A is
meaningless against model B's vectors — so swapping embedding models always
means re-embedding everything. The metadata buys:

- **Staged migration:** the old vector table keeps serving while a new-model
  table fills in the background; atomic cutover when complete. No search
  downtime, no big-bang rebuild. (Different dimensions force a separate `vec0`
  table anyway — blue/green falls out naturally.) Note: migration transiently
  doubles vector storage. *Implementation status: M1 cuts over immediately on
  model change (search serves the new, initially empty generation until
  re-embedding fills it — acceptable while reindex is a manual one-shot);
  the staged fill + deferred cutover lands with `bsearch reindex` (issue
  #24).*
- **Partial rebuilds:** chunker change → re-chunk + re-embed only affected
  docs; summarizer change → regenerate summaries only, vectors untouched.
- **Auditability:** `bsearch status` reports exactly what's stale against
  current config.

**Disk budget:** vectors are the dominant term — ~4 GB float32 at the 1M ×
1024-dim cap (quantized index adds ~3%; FTS + stored markdown on top).
Footprint is reported in `bsearch status`.

## System diagram

```mermaid
flowchart LR
    subgraph clients["Clients"]
        CLI["bsearch CLI"]
        MCP["MCP shim (stdio)"]
        AGT["AI agents"]
        ALF["Future: Alfred, GUI, …"]
    end

    subgraph daemon["bsearch daemon (Go, launchd)"]
        API["HTTP API (unix socket, 0600)"]
        QRY["Query service<br/>(hybrid: KNN + BM25 + RRF)"]
        SCHED["Indexing scheduler<br/>(queue, backoff, health gates,<br/>power-aware)"]
        DISC["Discovery<br/>(FSEvents watcher + periodic scan)"]
        PIPE["Pipeline workers<br/>convert → chunk → embed ∥ summarize"]
    end

    subgraph external["Local services"]
        BSC["bscribe container<br/>localhost:18000 → 8000<br/>(binary docs → markdown; bearer auth)"]
        INF["Inference server (LM Studio)<br/>OpenAI-compatible: embeddings + summaries"]
    end

    DB[("SQLite<br/>catalog + queue + FTS5 + sqlite-vec")]
    FS[("Filesystem<br/>~/ configured paths (TCC-gated)")]

    AGT --> MCP
    CLI & MCP --> API
    API --> QRY
    QRY --> DB
    QRY -->|"embed query"| INF
    DISC --> FS
    DISC --> SCHED
    SCHED --> DB
    SCHED --> PIPE
    PIPE -->|"PDF/office"| BSC
    PIPE -->|"embed + summarize"| INF
    PIPE --> DB
```

## Interfaces

### CLI

```
bsearch serve                     # run daemon (launchd invokes this)
bsearch search "heat pump quote" [--limit 10] [--level 4|16|64] [--mode hybrid|semantic|keyword] [--json]
bsearch list [path-prefix] [--sort modified|path] [--level 4|16|64] [--limit 100]
bsearch get <doc-id> [--level 4|16|64|full]
bsearch status                    # index counts, queue depth, gate reasons, permission failures, disk footprint
bsearch reindex [path]            # force re-index of path or everything
```

### HTTP API (unix socket, JSON)

Socket: `~/Library/Application Support/bsearch/bsearch.sock` (0600).

**`POST /v1/search`**

```json
{"query": "heat pump installation quote", "limit": 10, "mode": "hybrid", "summary_level": 16}
```

`mode`: `hybrid` (default) | `semantic` | `keyword`. `summary_level`:
`4 | 16 | 64`, default `16` — drop to 4 for wide surveys, raise to 64 for
fewer, richer hits. Optional `min_score`: no default floor — distance scores
are model-dependent and uncalibrated, so callers (especially agents) should
judge relevance from summaries, not scores.

Retrieval granularity: KNN and FTS both run at **chunk level**; RRF fuses the
chunk rankings; results collapse to the best chunk per document. The response
is per-document; `chunk_preview` and `heading_path` come from the winning
chunk.

Response:

```json
{
  "hits": [
    {
      "doc_id": "d_8f3a91",
      "path": "~/Documents/quotes/heatpump-vaillant-2025.pdf",
      "score": 0.83,
      "summary": "Vaillant aroTHERM quote from March 2025: supply and install, 7kW, £11,400 including cylinder.",
      "chunk_preview": "…total supply and installation cost of £11,400 inc. VAT…",
      "heading_path": "Quote > Cost breakdown",
      "modified": "2025-03-14T10:22:00Z"
    }
  ],
  "took_ms": 87
}
```

- `summary` — whole-document pyramid summary at the requested level (may be
  absent if not yet generated — summaries are fill-later, never a gate).
- `chunk_preview` — ~150-char excerpt of the best-matching chunk: why this hit
  matched (match evidence), complementing the summary (what the doc is about).
- `doc_id` — opaque surrogate ID minted at first discovery. Stable across
  content edits (path unchanged) and across renames/moves: a rename is
  detected when a file's content hash matches an existing catalog row **whose
  path no longer exists on disk**. A hash match whose old path still exists is
  a copy, not a rename → new id (no false merge of duplicate content — empty
  files, boilerplate, `cp`). Multiple candidate rows disqualify rename
  detection → new id. Known limitation: rename + edit within one scan window
  looks like delete + create and mints a new id. A full drop-and-reindex
  re-mints all ids (see Data retention).

**`GET /v1/docs`** — enumeration (the pyramid "survey the terrain" interface).

```
GET /v1/docs?prefix=~/Documents/tax&sort=modified&limit=100&summary_level=4
```

```json
{
  "docs": [
    {"doc_id": "d_8f3a91", "path": "~/Documents/tax/heatpump-vaillant-2025.pdf", "summary": "Heat pump installation quote", "modified": "2025-03-14T10:22:00Z"}
  ],
  "total": 342
}
```

`summary_level`: `4 | 16 | 64`, default `4` — enumeration is where the 4-word
level earns its place: results aren't query-ranked, so summaries carry all the
signal, and lists are long.

**`GET /v1/docs/{doc_id}?level=full|64|16|4`** — single document: full
markdown or pyramid level.

**`GET /v1/status`** — same payload as `bsearch status --json`.

**Summary ladder: 4 / 16 / 64 words / full text.** Generated at index time;
stored, not computed per query.

### Sample config

```toml
# ~/.config/bsearch/config.toml

[paths]
include = ["~"]
# extends the built-in deny-list (secrets, ~/Library, caches, VCS/deps dirs)
exclude = ["~/Archive/old-junk"]

[inference]
endpoint        = "http://localhost:1234/v1"   # OpenAI-compatible (LM Studio)
embedding_model = ""                           # decided by M2 bake-off
summary_model   = ""                           # decided by M2 bake-off
# Optional per-field overrides of the built-in per-model prefix registry —
# for embedding models the registry doesn't know. {q}=query, {d}=passage,
# {t}=heading-path breadcrumb (title slot).
#query_template       = "query: {q}"
#passage_template     = "title: {t} | text: {d}"
#input_ceiling_tokens = 2048

[converter]
endpoint   = "http://localhost:18000"          # bscribe (host mapping of native 8000)
token_file = "~/.config/bsearch/bscribe-token"

[power]
ac.index_interval      = "5m"
battery.index_interval = "60m"                 # or "defer"
```

### MCP (stdio shim over unix socket)

Three tools mirroring the API:

- `search(query, limit?, mode?, summary_level?)`
- `list_documents(prefix?, sort?, limit?, summary_level?)`
- `get_document(doc_id, level?)`

Tool descriptions encode the intended drill-down: survey with
`list_documents`/`search` at coarse levels first; `get_document` full text
only for chosen hits — and note that scores are uncalibrated (judge relevance
from summaries).

## Security

Threat model: single-user machine, no network exposure by default. (macOS TCC
is treated as a constraint, not a threat — see Constraints; the security
property it provides is that *other* sandboxed apps can't read bsearch's data
dir, and the cost is that bsearch itself needs Full Disk Access.) Threats
considered:

**1. The index is a honeypot.** The SQLite database concentrates full text,
summaries, and embeddings of everything indexed — a stolen laptop or leaked
backup exposes it all in one file.

- Database lives under `~/Library/Application Support/bsearch/`, mode 0600.
- At-rest protection is FileVault full-disk encryption (assumed on). No
  application-level encryption: it would add key-management complexity without
  protecting against the realistic threat (same-user malware reads the source
  files anyway).
- The backup half of the threat is closed mechanically: the daemon marks its
  data dir excluded from Time Machine at startup (see Data retention).

**2. Same-user processes can reach the API.** The unix socket is 0600, so the
OS blocks other users — but any process running as Ben can query it. Accepted:
such a process can already read the source documents directly; bsearch adds
convenience, not new access. No auth on the socket in v1.

**3. Inference endpoint determines where content flows.** Every chunk and
summary passes through the configured embedding/summary endpoints. bsearch
does not police this — endpoint choice is the user's (a remote inference box
on a private tailnet is a legitimate setup). The privacy guarantee is
therefore conditional: content stays as local as the inference endpoints you
configure. Documented prominently in the config reference. (The bscribe
endpoint is the same class of sink; both are user-controlled local/tailnet
services.)

**4. Malicious documents.** Untrusted files (downloaded PDFs) hit parsers with
long CVE histories.

- Binary formats are parsed inside the bscribe container, isolated from the
  daemon. Non-root is baked into bscribe's image; read-only rootfs, capability
  drop, and the memory cap are **run-flags** — bsearch's deployment docs
  record the required flags, since the isolation claim depends on them. A
  parser exploit lands in a disposable container, not in the process holding
  the index.
- In-process parsing is markdown/plain text only, in memory-safe Go.

**5. Prompt injection via indexed content.** A malicious document can contain
text crafted to manipulate the LLM that summarizes it, or the agent that later
reads search results. Summaries are generated with a fixed instruction
template and treated strictly as data; but no mitigation fully prevents an
agent from reading attacker-authored text in results. Residual risk,
documented for consumers: treat bsearch results as untrusted content.

**6. Future TCP exposure.** Out of scope for v1, but the listener abstraction
requires an auth story (bearer tokens, as bscribe does) before any TCP
listener ships. Recorded so it isn't bolted on casually.

## Privacy

- **Sensitive data held:** converted document text, chunk embeddings
  (partially invertible — treat as content-equivalent), summaries, file paths,
  and index metadata. All local, all in the one database.
- **Excluded by default — secrets:** `~/.ssh`, `~/.gnupg`, keychains, `.env`,
  private key patterns, browser profiles.
- **Excluded by default — noise and volume:** `~/Library` (until Mail support
  deliberately carves in), caches, `node_modules`, `.git` and other VCS
  internals, package/bundle internals, and bsearch's own data directory. This
  is a battery/idle-CPU measure as much as a privacy one — a `$HOME` scan
  without it stat-churns millions of junk files every scan cycle.
- Config extends the deny-list; exclusions win over includes. Indexing scope
  is opt-in-by-path (`$HOME` default with the excludes above).
- **Queries are sensitive too.** Query text is never written to logs at
  default level (queries reveal what you're thinking about). Debug logging
  that includes queries/content is explicit opt-in and flagged as such in
  config.
- **Logs generally:** operational events only (files indexed, failures,
  timings). Never document content, never summaries, never query text at
  default level.
- **No telemetry of any kind.**

## Data retention

- **The index is derived data — with one caveat.** Source files are never
  modified or moved; everything in the database is regenerable from them
  except **doc_id continuity**: ids are opaque surrogates, so a full
  drop-and-reindex re-mints them and breaks references held by agents or
  integrations. Rare and accepted — schema migrations are preferred precisely
  so rebuilds stay exceptional.
- **Deletion follows source, near-real-time.** Deletes arrive via FSEvents
  like creates and edits (the periodic scan is the backstop for missed
  events) → catalog row, chunks, vectors, summaries, FTS entries purged. At
  the index level the content is gone — no longer searchable or retrievable.
  Storage-layer honesty: SQLite leaves deleted bytes in freelist/WAL pages
  until checkpoint/vacuum; accepted residue, covered by FileVault, not worth
  `secure_delete` write-amplification.
- **No history.** Only the current version of a file is indexed; edits replace
  prior chunks/embeddings.
- **Backups:** the daemon sets a Time Machine exclusion on its data directory
  at startup (`tmutil`/`CSBackupd` API) — a mechanism, not a recommendation.
  The index is derived (minus id continuity, accepted); excluding it keeps a
  content-concentrating file out of backups (Security threat 1).

## Licensing

MIT. No commercial ambitions to protect, maximum simplicity, zero friction for
anyone who finds it useful. (Alternative considered: PolyForm Noncommercial —
rejected; no interest in policing use.)

## Milestones

Ordering philosophy: user-visible value first; scaffolding only when forced.
M1 replaces lore's core function — already useful on day one.

**M1 — Search my markdown.** One-shot `bsearch index` + `bsearch search` (no
daemon). Scans configured paths, text/markdown only; chunks, embeds via LM
Studio, stores in SQLite + sqlite-vec; semantic CLI search. Demo: semantic
search over the Obsidian vault from the terminal. (No TCC issues: one-shot CLI
inherits the terminal's grants.)

**M2 — Model bake-off.** Small eval harness over a personal golden set
(~30–50 real queries with known-correct documents, drawn from own corpus).
The harness is **prefix-aware** — per-model query/passage templates applied
correctly — or the bake-off measures the wrong thing for asymmetric models.
Compare candidate embedding models on retrieval quality (recall@10, MRR),
index cost (time, battery), and query latency; compare summarizer candidates
on summary quality at each pyramid level (spot-check) and tokens/sec. Output:
default embedding + summary models recorded in this doc (Closed issues), with
eval scripts kept in-repo for re-runs when new models appear. Demo: a table
justifying the defaults.

**M3 — Always fresh.** Daemon (`serve`), FSEvents + periodic scan, durable
queue with retry/backoff and health gates, unix-socket API, `status`. launchd
agent. **TCC onboarding:** daemon detects permission failures and surfaces
them in `status`; docs cover granting Full Disk Access. Demo: save a note,
search finds it a minute later, no manual indexing.

**M4 — Hybrid + pyramid.** FTS5 + RRF fusion; pyramid summaries (4/16/64)
generated at index time; `list`, `get`, level params. Demo: exact-term queries
work (invoice numbers, names); survey/drill-down flow via CLI.

**M5 — Agents.** MCP server (`search` / `list_documents` / `get_document`).
Demo: Claude Code answers a question from local documents it found itself —
the primary scenario, end to end.

**M6 — Beyond markdown.** bscribe integration: PDFs + office docs flow through
the pipeline; converter health in `status`; degradation handling. Demo: search
finds content inside a PDF quote (in a TCC-granted directory).

**M7 — Live like a good laptop citizen.** Power-aware scheduling (AC/battery
policies), tuned batch intervals, `reindex`, operational polish.

Deferred beyond v1: image indexing, Apple Mail, native macOS frontend (see
Missing features).

## Open issues

- **Default models.** Unresolved by design — the M2 bake-off decides.
  Constraints recorded: OpenAI-compatible endpoints; embedding dimensions
  ≤ ~1024 (scan latency, storage) and input ceiling recorded per model;
  query/passage prefix templates per model; summarizer small enough for
  battery-tolerable index runs with context ≥ the map-reduce section size;
  embedding model small enough to stay resident.
- **Embedding context strategy for long documents.** Chunk-level embeddings
  decided; whether to also embed summaries (a doc-level vector for coarse
  retrieval) — decide during M4 when pyramid data exists.
- **FSEvents edge cases.** Volumes appearing/disappearing (external disks),
  packages/bundles, event-stream overflow handling. Handle in M3; may narrow
  supported paths. (iCloud dataless files already resolved: skipped, never
  materialized.)

## Closed issues

- **Language: Go over Python/TypeScript.** Python: best doc-processing
  ecosystem, but conversion moved to bscribe so that advantage evaporates;
  daemon deployment weaker. TypeScript: native liteparse, but weakest language
  and the liteparse path was superseded by bscribe. Go wins on daemon
  ergonomics, self-contained binary, low idle footprint.
- **Doc conversion: bscribe over lit CLI subprocess / docling in-process.**
  Subprocess = external Node dependency to manage, no memory isolation.
  In-process Python libs = wrong language. bscribe: already running, hardened,
  memory-cappable, stable API, anticipated bsearch as its first consumer. Cost
  accepted: runtime dependency, mitigated by queue-and-retry degradation.
- **Storage: SQLite + sqlite-vec + FTS5 over LanceDB / qdrant / separate
  vector lib.** One file, one engine, transactional consistency between
  catalog/queue/vectors/FTS; brute-force with quantize + rescore covers the
  target scale. Server DBs rejected: always-on container tax on a laptop. ANN
  rejected: immature in sqlite-vec, unneeded at this scale. Acknowledged bet:
  sqlite-vec is pre-1.0 — version pinned, format breaks covered by
  drop-and-reindex.
- **Vector search: brute-force + quantization over ANN.** Exact or near-exact,
  zero maintenance, delete-friendly; ANN buys nothing below many millions of
  vectors. Float32-only is the small-corpus configuration; quantize + rescore
  is the planned configuration at ~1M chunks (not break-glass — the
  extrapolated float32 scan at that scale exceeds the latency SLO).
- **Search response: single summary level per request over multi-level
  payload.** Levels beyond the requested one are redundant tokens once in
  context; drill-down happens via `get`. Level 4 exists for enumeration
  (`list`) where results aren't ranked and lists are long; search defaults to
  16.
- **doc_id: opaque surrogate over content/path hash.** Stable across edits
  and moves; agent references survive. Rename detection requires the old path
  to be gone and a unique hash match (duplicate-content false merges
  excluded); rename+edit in one window churns the id — accepted limitation.
- **Local endpoint enforcement: rejected.** Considered refusing non-loopback
  inference endpoints; rejected — remote inference on a private tailnet is
  legitimate. Privacy guarantee documented as conditional on endpoint choice.
- **Adversarial design review (2026-07-19) folded.** Multi-agent review
  against live bscribe/lore source; ~35 findings accepted (TCC constraint —
  the sole HIGH; queue-predicate and claim fixes; embed ∥ summarize decouple;
  inference health gates; query/passage prefix templates; quantization
  reframed as planned-at-scale; doc_id rename guards; dependency-accuracy
  corrections; scan deny-list; deletion/backup mechanics). Rejected from the
  review: a persistent `processing` claim state (single-process in-memory
  claim + idempotent redo is simpler); a default relevance floor (distance
  scores uncalibrated — would silently cost recall); unifying config/data
  paths (split is deliberate); a backed-up doc_id map (softened the
  derived-data claim instead).

## Alternatives considered

- **Extend lore.** Rejected: lost mental model (vibe-coded), query-time
  indexing flaw, wiki-scoped design. Lessons carried: sqlite-vec + FTS5 + RRF
  hybrid implemented and working (though unmeasured); breadcrumb-prefixed
  chunks; per-model-family query/passage prefix handling.
- **Spotlight / Apple's built-in search.** Keyword + metadata only, no
  semantic retrieval, no API for pyramid-style agent access. Non-extensible.
- **Existing OSS tools (Khoj, Reor, Recoll, AnythingLLM-class).** Each misses
  on at least one hard requirement: hexagonal/API-first design for agent
  integration, BYO-inference over a local socket, macOS battery citizenship,
  or maintainability-by-boring-Go. And: this is a for-fun project — building
  it is the point.
- **Do nothing.** Fails the agent-context goal; Spotlight roulette continues.
