# 0004 — SQLite driver: mattn/go-sqlite3 with sqlite-vec cgo bindings

- **Status:** proposed
- **Date:** 2026-07-19
- **Confidence:** high

## Context

DESIGN.md (Storage, SQLite driver rows) commits to SQLite with sqlite-vec
statically compiled in via cgo, but names the driver only as
"mattn/go-sqlite3-class". Issue #2 makes the dependency concrete, and a
driver choice is expensive to reverse once the adapter, build flags, and CI
are shaped around it.

Requirements: load the sqlite-vec C extension (rules out pure-Go
modernc.org/sqlite — no C extension loading), self-contained binary (no
runtime `.dylib` loading), FTS5 available for M4, boring and mature.

## Decision

We will use `mattn/go-sqlite3` (v1.14.x) as the SQL driver with
`github.com/asg017/sqlite-vec-go-bindings/cgo` (v0.1.6) providing sqlite-vec,
registered process-wide via `sqlite_vec.Auto()` (SQLite's auto-extension
hook). The bindings vendor the sqlite-vec C amalgamation, so pinning the Go
module pins the extension version — sqlite-vec is pre-1.0 with no on-disk
format guarantee, and the pin is the mitigation DESIGN.md requires.

FTS5 is compiled in from day one via the `sqlite_fts5` build tag (`GOFLAGS`
in the Makefile, `run.build-tags` in `.golangci.yml`): the driver is compiled
once, and M4's keyword search must not require a build-flag change.

## Alternatives considered

- **ncruces/go-sqlite3 (WASM)** — pure-Go, cross-compiles freely, and
  sqlite-vec ships officially documented bindings for it. Rejected for v1:
  a WASM runtime between the daemon and the KNN hot loop is unquantified
  risk against the p95 < 500 ms SLO, and the design already accepts
  native-only builds (ADR 0001 built CI around them). Remains the documented
  escape hatch if cgo becomes untenable (DESIGN.md Storage row).
- **modernc.org/sqlite (pure-Go transpile)** — cannot load C extensions;
  no sqlite-vec. Rejected outright.
- **Loadable extension at runtime (`.dylib` + `load_extension()`)** — breaks
  the self-contained single-binary goal; adds a file-distribution and
  version-skew problem. Rejected.

## Consequences

- cgo is mandatory everywhere: native-only builds, no cross-compilation
  (already accepted in ADR 0001; macOS CI runners build on-platform).
- `sqlite_vec.Auto()` registers the extension for **every** SQLite
  connection in the process — fine for a single-purpose daemon, would need
  care if another SQLite consumer ever lands in-process.
- macOS builds emit a harmless `sqlite3_auto_extension is deprecated`
  warning (sqlite-vec issue #169): the cgo prolog compiles against the SDK's
  sqlite3.h, but the symbol links against mattn's statically-compiled
  SQLite, not the system library. Verified working (`vec_version()` returns
  at open; adapter tests exercise KNN).
- The bindings module (v0.1.6) trails upstream sqlite-vec (0.1.10-alpha.x).
  Acceptable: the adapter uses only vec0 basics (float columns, KNN MATCH,
  rowid deletes), all stable since 0.1.0. Upgrading = bumping one Go module.
- Every `go` invocation needs `-tags=sqlite_fts5` to match CI; running bare
  `go test ./...` without the tag still compiles (the tag only adds FTS5),
  so drift shows up only when FTS5 features land — the Makefile is the
  single source of truth.
