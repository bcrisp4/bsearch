# 0015 — Separate identity from location: path-keyed documents, hash-keyed content

- **Status:** accepted
- **Date:** 2026-07-26
- **Confidence:** high

## Context

`doc_id` is a surrogate that identifies a **file**, and everything hangs off it:
the path, the content hash, the queue state, the chunks, the vectors, the
summaries. But chunks, vectors and summaries are functions of **content**, and
the queue is processing **content**. Only the path is really about the file.

The schema does not know that, so the relationship is maintained by hand:

- Renaming a file throws away valid chunks, vectors and summaries and re-embeds
  them for no reason ([#32](https://github.com/bcrisp4/bsearch/issues/32)).
- Two identical files embed twice.
- `Scanner.resolveID` (`internal/discovery/discovery.go:462`) is ~50 lines of
  heuristic keeping the surrogate attached to the right file, with a documented
  failure case: rename + edit inside one scan window mints a fresh id and burns
  the work.
- `failed` has to be un-set by hand when a file changes (`UpsertDocument`
  clears the retry columns when the hash moves).

This record is **not** motivated by bug prevention. ADR 0014 closed that class
structurally, and [#69](https://github.com/bcrisp4/bsearch/issues/69) — asserting
the invariant rather than holding it — was closed unbuilt. This is a modelling
argument: put each key where its dependency already is.

The project is pre-0.1.0 with no index in real use and one user, who is the only
signatory to every contract involved. There is no migration or compatibility
cost to weigh.

## Decision

**We will split identity from location. `documents` keys on path and mirrors the
filesystem; `content` keys on the content hash and carries the queue and
everything derived from the bytes. `doc_id` is retired; path becomes the
agent-facing identity.**

| Table | Key | Holds |
|---|---|---|
| `documents` | `path` | `content_hash` (nullable), `unread_reason`, size, mtime |
| `content` | `content_hash` | state, stage versions, retry columns |
| `chunks` | `id` (autoincrement) | `content_hash`, ordinal, text, offsets |
| `summaries` | `content_hash` + level | text |
| `vec_chunks` | `rowid` = `chunks.id` | vectors |

Four things follow that are part of the decision rather than of its
implementation:

**Ownership is a module boundary, not a concurrency one.** Discovery writes
`documents` and inserts `content` rows; the pipeline mutates `content` and
writes `chunks`. ADR 0014 already puts every catalog write on one goroutine, so
these are two modules called by the same worker. Discovery inserting eagerly is
what keeps dispatch a plain predicate over `content` rather than an anti-join.

**The state machine changes subject and loses two members.** `state` describes
processing *content*, so it moves to `content`. `deleted` is removed — it is
already vestigial (nothing writes it; deletion is a hard `DELETE`; its only
reader is a `status` line that always prints 0), and a deleted file is now a
removed `documents` row. Paths whose bytes were never obtained leave the state
machine too: `documents.content_hash` is NULL with `unread_reason` recording
which of `denied` / `dataless` / `io_error` it was, because a TCC denial and a
deliberately-skipped iCloud placeholder are opposite situations that must not
report as one number.

**`failed` becomes permanent by construction.** Content is immutable, so a
failure against those bytes cannot expire, and a changed file is a *different*
`content` row that starts at `discovered`. Nothing has to reset it.

**Deduplication and renames stop being features and become consequences.** There
is one queue row per distinct content, so a second identical file schedules no
work at all. A rename is a `documents` row moving to a new path with the same
hash; nothing derived was addressed by path, so `resolveID` and its heuristic
delete entirely.

Schema, queue and API shape are recorded in DESIGN.md (*Identity: path locates,
content hash identifies*; *Queue*; *HTTP API*).

## Alternatives considered

- **Do nothing.** Renames keep costing a re-embed, duplicates embed twice,
  `resolveID` stays load-bearing. Rejected: the cost of changing is at its
  lifetime minimum right now and rises with the corpus.

- **Rekey derived data only** — `chunks`/`summaries` on the content hash, keeping
  `documents`, `doc_id` and the queue. Buys #32 and deduplication with a much
  smaller diff, and was this record's first draft. Rejected because it leaves
  every modelling problem in place: `resolveID` survives, `state` still describes
  content while living on a file row, `failed` still needs hand-resetting, and
  dedup needs an explicit "do chunks already exist?" branch in the pipeline
  instead of falling out. It would also have to be re-migrated into this shape
  later, when migration actually costs something.

- **Compare-and-set on a `revision` column.** Addresses a bug-prevention framing
  this record does not claim, and buys neither #32 nor deduplication. Content
  addressing and CAS are complements rather than alternatives — Git
  content-addresses objects and uses CAS on refs — so this does not foreclose
  guarding a mutable pointer later.

- **Returning one search hit per path** rather than per content. Rejected: it
  undoes deduplication exactly where it pays. The scores are identical, so N
  copies consume N slots of `limit` with no ranking signal to order them, and an
  agent pays N× the tokens for 1× the information.

## Consequences

**Easier.** Renames and moves are free. Duplicate content embeds once. The state
machine describes one subject. `failed` and the retry columns stop needing a
reset path. `resolveID` deletes. The index becomes wholly regenerable — the
`doc_id` continuity caveat in Data retention goes with the surrogate.

**Harder.** Orphan collection is new machinery, mandatory rather than
housekeeping, and its correctness constraint (one transaction) has a silent
failure mode. Result assembly gains a join that can fan out. `bsearch status`
must report three populations — files, contents by state, unread by reason — and
must not conflate them.

**Committed to.** A schema rewrite executed as drop-and-reindex, free today and
not for long. Every storage port signature taking a `docID` changes. Worth
landing before [#18](https://github.com/bcrisp4/bsearch/issues/18) populates
`summaries` and before M4's FTS5 triggers are written, purely to avoid writing
them twice.

**Risks.** The nullable `content_hash` is the sharp edge. The `CHECK` makes the
illegal states unrepresentable, so what remains is queries misreading a *legal*
state — most dangerously `NOT IN` against a nullable column, which silently
matches nothing and would make the orphan sweep a permanent no-op the first time
a real file is denied. Two mitigations, to be carried into the implementation
issues: prefer `NOT EXISTS` over `NOT IN` for every query over a nullable
`content_hash`, and put an unread row in the *shared* store-test seed so a query
with that flaw fails on arrival rather than waiting to be anticipated. The
general defence is asserting that counts reconcile, so any accounting that drops
a population fails loudly.

**Follow-up.** Discovery can then hash only paths it has not seen before, letting
the pipeline's hash serve the rest — taking a changed file from two full reads to
one. Its own issue. The `ConverterPort` signature (M6) must also preserve the
pipeline's access to source bytes, or hashing stops being free.
