# Changelog

All notable changes to bsearch are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

How to maintain this file is documented in [docs/changelog.md](docs/changelog.md):
every behaviour-changing PR adds an entry under `[Unreleased]`; at release time
that section is renamed to the new version and becomes the GitHub Release notes.

## [Unreleased]

### Added

- bsearch can now discover the files to index: it walks the configured
  include paths (honouring the privacy deny-list — exclusions always win),
  picks up new and changed markdown/text files, and skips unchanged ones
  cheaply so repeat scans are fast. Renamed or moved files keep their
  document identity. Include roots that are symlinks are followed. iCloud
  "Optimize Storage" placeholders are never downloaded, and unreadable
  paths (e.g. missing Full Disk Access) — as well as an include root
  swallowed by the exclude rules — are reported per path instead of being
  silently skipped.

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
