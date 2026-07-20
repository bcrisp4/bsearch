# Changelog

All notable changes to bsearch are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

How to maintain this file is documented in [docs/changelog.md](docs/changelog.md):
every behaviour-changing PR adds an entry under `[Unreleased]`; at release time
that section is renamed to the new version and becomes the GitHub Release notes.

## [Unreleased]

### Added

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
