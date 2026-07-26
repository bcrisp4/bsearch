# Changelog

All notable changes to bsearch are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

How to maintain this file is documented in [docs/changelog.md](docs/changelog.md):
every behaviour-changing PR adds an entry under `[Unreleased]`; at release time
that section is renamed to the new version and becomes the GitHub Release notes.

## [Unreleased]

### Changed

- **Files are now identified by path, and their content by hash — `doc_id` is
  gone.** ([ADR 0015](docs/adr/0015-content-addressed-chunks-and-summaries.md),
  [#77](https://github.com/bcrisp4/bsearch/issues/77)) What this buys:

  - **Renaming or moving a file no longer costs a re-embed**
    ([#32](https://github.com/bcrisp4/bsearch/issues/32)): everything derived
    from the file's bytes is keyed by their hash, and a rename doesn't change
    the bytes.
  - **Identical files are embedded once**, however many copies exist. A search
    hit names one primary path (the most recently modified copy) and lists the
    rest in `also_at`, rather than spending result slots on copies that would
    all rank identically.
  - Search results (`POST /v1/search`, `bsearch search --json`) carry `path`
    (the identity), `content_hash`, and `also_at` — the `doc_id` field is
    removed. A path is what an agent can act on; nothing needs to survive a
    rebuild.

  **The index database is rebuilt from scratch by this change.** There is no
  in-place migration — the index is derived data. Delete the old database
  (`~/Library/Application Support/bsearch/bsearch.db*`) and the daemon
  reindexes on next start; an old database left in place is refused with an
  error saying exactly that.

- **Files bsearch couldn't read are now remembered, not just logged.** A file
  that can be seen but not opened (a per-file permission denial), an iCloud
  placeholder skipped so it never triggers a download, and a file that failed
  mid-read are each recorded with their reason and stay visible in
  `bsearch status` in steady state — previously a denial was only reported if
  the *most recent* scan happened to touch it. A directory-level denial — the
  usual missing Full Disk Access shape — still surfaces through scan errors
  only: the files inside were never even seen, so there is nothing to record.
  A file that was indexed before becoming unreadable keeps serving its
  indexed content rather than disappearing from search.

- **Discovery writes are batched.** A first scan over a large corpus commits
  catalog rows hundreds at a time instead of one transaction per file
  ([#34](https://github.com/bcrisp4/bsearch/issues/34)), so the initial index
  of a big folder spends its time hashing and embedding, not committing.

### Added

- **Save a file, and it is searchable in seconds.** The daemon now watches
  your configured paths instead of waiting for its next walk, so a note you
  just wrote turns up in search about fifteen seconds later rather than up to
  five minutes. Deleting a file now works too, for the first time: it stops
  appearing in search just as quickly, instead of lingering in the index
  pointing at a path that is not there.

  The periodic walk has not gone away — it is the backstop for changes the
  event stream missed, and it now runs every fifteen minutes rather than
  every five, so the daemon costs less in the background while being fresher.

  `bsearch status` gains a `watching` line saying whether changes are being
  noticed as they happen, and if not, why. Worth a look when everything else
  reads healthy: a watcher that has never seen a change is usually a missing
  Full Disk Access grant. On a machine without FSEvents the daemon falls back
  to the walk and says so, rather than failing to start.

  Deletion has deliberate gaps, all with the same shape: when the daemon
  cannot be sure a file is really gone, it leaves the index alone rather than
  risk destroying it. A file deleted while the daemon was *not* running stays
  indexed; so does one deleted in a burst large enough to overflow the event
  stream, or one whose whole folder vanished at once (which is what an
  unmounting disk looks like from here). In every case, deleting the file
  again with the daemon running clears it. Closing the gaps properly is
  [#57](https://github.com/bcrisp4/bsearch/issues/57).

- **New `bsearch status` command: what the daemon is doing, and why it isn't.**
  It reports the index — ready or not and why, the embedding model, per-state
  document counts, and what the index costs on disk — alongside the
  background indexing loop: what it is waiting on ("embedding endpoint
  unreachable", "deferred: on battery", "files could not be read — check Full
  Disk Access"), when it last scanned and last made progress, and what it has
  got through since it started.

  The two are reported separately on purpose, because they break separately:
  "nothing is indexed" and "nothing is indexing" have different fixes, and a
  daemon whose indexing never started now says so instead of looking idle.
  Documents that were given up on are listed by reason, largest group first,
  with a path to look at; directories that could not be read are listed too,
  which on macOS usually means the binary needs Full Disk Access.

  `bsearch status --json` emits the daemon's `GET /v1/status` document
  verbatim for scripts. The command exits 0 for any answer the daemon gives,
  however unhappy — a non-zero exit means the daemon could not be reached.

- **The daemon now indexes on its own.** `bsearch serve` walks your configured
  paths, indexes what it finds, and keeps up with new and changed files — no
  command to run and nothing to remember. The daemon also creates the index
  database itself, so a fresh install is `bsearch serve` and nothing else.

  Two things it now handles for you. **A stopped inference server costs
  nothing**: the daemon checks the embedding endpoint before working, and
  checks again before blaming a document for a failure, so leaving LM Studio
  closed for a week marks nothing as failed and indexing resumes by itself.
  And **changing `inference.embedding_model` re-embeds your corpus**: restart
  the daemon and it notices the index was built by a different model and works
  through it again, so searches are thin rather than wrong while it catches
  up. Documents that fail for their own reasons are retried five times with
  backoff before being given up on.

  Battery-aware scheduling is configurable but not yet active. See
  [docs/daemon.md](docs/daemon.md).

- New `bsearch serve` command: runs the bsearch daemon, serving search over
  an HTTP+JSON API on a unix socket at
  `~/Library/Application Support/bsearch/bsearch.sock` (mode 0600, so only
  your account can reach it — there is no authentication in v1 and none is
  needed). Endpoints are `POST /v1/search` and `GET /v1/status`, and
  `bsearch status` is the readable view of the latter. See
  [docs/daemon.md](docs/daemon.md).

  The daemon starts whether or not you have an index: with none it reports
  `index.ready: false` and why, and picks up an index created afterwards
  without a restart. It also starts when `inference.embedding_model` is
  unset, so a half-configured install can still be diagnosed rather than
  crash-looping. Only one daemon runs at a time; a socket left behind by one
  that was killed is reclaimed automatically. Configuration is read once at
  startup, so restart the daemon after editing `config.toml`.


- New `bsearch eval run` command: scores an embedding model's retrieval
  quality against a golden query set (`--corpus <dir>`, a corpusgen-built
  corpus plus `golden.yaml`). It indexes the corpus into a scratch database
  under `--work-dir` (default `~/bsearch-eval/work`, reused across reruns
  against the same corpus and model — only a changed embedding fingerprint
  re-triggers indexing), then embeds and searches every golden query,
  recording recall@k, MRR, success@1, and per-stage latency. Results are
  written as JSON (`--out`, default under `~/bsearch-eval/results/`) for
  later comparison; a headline summary prints to the terminal. Never prints
  query text or document content.

- New `bsearch eval summarize` command: benches a summarizer LLM's
  throughput over a sample of the golden corpus (`--corpus <dir>`,
  `--model <name>`, `--docs` sample size, default 10). Streams a chat
  completion per sampled document, writes each summary to `--out-dir`
  (default under `~/bsearch-eval/summaries/`), and records per-doc and
  aggregate tokens/second in `metrics.json`. Never prints document content
  or summary text — only paths and throughput numbers.

- New `bsearch eval compare <a.json> <b.json>` command: compares two
  `bsearch eval run` results files scored against the same corpus version
  and query set, reporting per-query win/loss/tie counts and a paired
  t-test (mean delta, 95% CI, p-value) on recall@10 and MRR@10, overall and
  per query-tag slice. `--json` emits the comparison as JSON instead of the
  human-readable table. Refuses to compare runs from different corpus
  versions or mismatched query sets. Never prints query text — aggregate
  model names, tags, and numbers only.

- New `bsearch search` command: semantic search over your indexed files from
  the terminal — `bsearch search "heat pump quote"`. The query is embedded
  with the same model-specific prefix used at indexing time, matched against
  every chunk, and results collapse to the best chunk per document: file
  path, distance (lower = better; raw model distance, uncalibrated — judge
  relevance by the preview, not the number), the matching section's heading
  path, and a short excerpt showing why it matched. `--limit` caps the
  number of documents (default 10); `--json` emits a machine-readable
  response including `took_ms`. Searching before anything is indexed, or
  after changing the embedding model or its prefix templates without
  re-indexing, fails with a clear message instead of returning empty or
  wrong-model results — and never creates or modifies the database.

- New `bsearch index` command: one-shot indexing of the folders in your
  config. It scans for new and changed markdown/text files, chunks them,
  embeds them through your inference server, and stores everything in the
  local index — with per-file progress output and a final summary. Re-runs
  are fast and idempotent: unchanged files are skipped without touching the
  network, an interrupted run resumes where it left off, and changing the
  embedding model, its prefix templates, or even just the dimensions the
  server returns re-embeds automatically. If the inference server is down —
  including dying mid-response — the run stops cleanly and nothing is marked
  failed; genuinely broken files (e.g. undecodable encodings) are recorded
  with a reason, reported, and retried automatically after a config change.
  Files that can't be read right now (vanished, permissions) are skipped and
  retried next run, never written off. The command exits non-zero when any
  document failed or when no configured folder could be read at all (e.g.
  missing Full Disk Access), so scheduled runs can't fail silently. Requires
  `inference.embedding_model` to be set in config.

- bsearch can now turn text into search vectors through any OpenAI-compatible
  embeddings endpoint (LM Studio, Ollama, vLLM, …). Chunks are embedded many
  per request, and the model-specific query/passage prefixes that asymmetric
  embedding models need are applied automatically — identically at indexing
  and at search time — from a built-in per-model registry
  (EmbeddingGemma so far), overridable in config (`[inference]`
  `query_template`, `passage_template`, `input_ceiling_tokens`) for models
  bsearch doesn't know. Oversized inputs fail loudly rather than being
  silently truncated, and switching models — or even just changing a prefix
  template — starts a fresh vector generation so incompatible vectors are
  never mixed.

- bsearch can now discover the files to index: it walks the configured
  include paths (honouring the privacy deny-list — exclusions always win),
  picks up new and changed markdown/text files, and skips unchanged ones
  cheaply so repeat scans are fast. Renamed or moved files keep their
  document identity. Include roots that are symlinks are followed. iCloud
  "Optimize Storage" placeholders are never downloaded, and unreadable
  paths (e.g. missing Full Disk Access) — as well as an include root
  swallowed by the exclude rules — are reported per path instead of being
  silently skipped.

- Markdown files are now split into search-sized chunks by a
  markdown-aware chunker: sections follow the document's heading
  structure, every chunk carries its heading path (e.g.
  `Quotes > Vaillant`) for context, and tables and code blocks are never
  split down the middle. Obsidian-style YAML frontmatter is excluded from
  chunks, and UTF-16/BOM-marked files are transcoded automatically —
  undecodable files are reported as failures instead of being indexed
  garbled.

- The index now lives in one SQLite database at
  `~/Library/Application Support/bsearch/bsearch.db` (created 0600, directory
  0700): document catalog, chunks, pyramid-summary slots, and semantic-search
  vectors (sqlite-vec), with production pragmas (WAL, foreign keys, busy
  timeout) applied on every connection. The schema is versioned, so future
  upgrades migrate in place instead of forcing a re-index.

- bsearch reads its configuration from `~/.config/bsearch/config.toml`
  (or `$XDG_CONFIG_HOME/bsearch/config.toml`): indexed paths, inference and
  converter endpoints, and power-aware indexing intervals, with sensible
  defaults when no file exists. Unknown or invalid keys fail loudly instead
  of silently falling back to defaults. A built-in privacy deny-list
  (`~/.ssh`, `~/Library`, VCS internals, key/secret file patterns, …) is
  always active; `[paths].exclude` extends it.


### Changed

- **`bsearch search` now requires a running daemon.** It talks to
  `bsearch serve` over the unix socket instead of opening the index itself,
  which is what the architecture always described — one query path means one
  place that agrees about prefix templates and index identity. Start the
  daemon in another terminal (`bsearch serve`) before searching; a
  LaunchAgent that starts it at login is coming.

  Its flags change with it: `--socket` replaces `--config` and `--db`, which
  move to `bsearch serve`. `--limit` and `--json` are unchanged, and the
  `--json` output is byte-identical to what the daemon returns. Errors read
  the same as before — the daemon's message, on stderr, with exit status 1.

- Semantic search now ranks by cosine distance instead of Euclidean (L2).
  For the normalized embedding models most people run, rankings are
  identical — but models that emit non-normalized vectors (or truncated
  ones) no longer silently skew results toward larger-magnitude embeddings.
  The `distance` in `bsearch search` output is now bounded [0, 2] (still
  lower = better, still uncalibrated). Existing indexes migrate automatically:
  the daemon notices the change and re-embeds everything in the background,
  and search keeps the old ranking behaviour until it catches up. (ADR 0007)


### Removed

- **`bsearch index` is gone.** The daemon indexes continuously now, so there
  is nothing left for a one-shot command to do — and having two things that
  write the index meant two things that could disagree about it. Run
  `bsearch serve` and it takes care of itself; if you had `bsearch index` in a
  cron job or a shell alias, delete it. A command to force a rebuild on demand
  is coming separately, and matters less than it used to now that a
  configuration change re-indexes by itself. (ADR 0012)


### Fixed

- **Saving or renaming a file while it was being indexed could quietly lose
  it.** Two ways, both rare and both permanent until you touched the file
  again. A file saved during its own indexing run could end up recorded as
  indexed with nothing actually stored against it — present in the catalog,
  on disk, and findable by no search. And a file renamed while the old
  version was mid-flight could have the rename undone, leaving the new name
  unindexed and the old name serving a different file's contents.

  A brand new file could also be picked up twice at once and given two
  identities, one of which was then discarded — so an agent holding onto the
  discarded `doc_id` got nothing back.

  All three came from the same place: the daemon had two things writing to the
  index at once. It now has one.

  What that costs is small and worth stating. A deleted file leaves search up
  to one document later than before — in practice still seconds, and still
  well inside the fifteen-second freshness target. Shutting the daemon down
  now takes its steps in sequence rather than in parallel, so the launchd
  configuration in `docs/daemon.md` allows more time for it; if you installed
  the agent earlier, raising `ExitTimeOut` to 60 is worth doing.

  `bsearch status` gains two numbers that should always read zero. One counts
  watched paths that were out of scope — a large number against nothing queued
  means an include root whose spelling does not match what macOS reports, which
  otherwise looks like a perfectly healthy daemon indexing nothing. The other
  counts documents whose index entry changed underneath the indexer, which
  after this change should be impossible; if it isn't zero, something is wrong
  and the line says so in plain words.
