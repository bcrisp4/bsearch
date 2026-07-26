# 0015 — Separate identity from location: path-keyed documents, hash-keyed content

- **Status:** proposed
- **Date:** 2026-07-26
- **Confidence:** high

## Context

`doc_id` is a surrogate that identifies a **file**, and everything hangs off it:
the path, the content hash, the queue state, the chunks, the vectors, the
summaries. But chunks, vectors and summaries are functions of **content**, and
the queue is processing **content**. Only the path is really about the file.

The schema does not know that, so we maintain it by hand:

- Renaming a file throws away valid chunks, vectors and summaries and re-embeds
  them for no reason ([#32](https://github.com/bcrisp4/bsearch/issues/32)).
- Two identical files embed twice — copies, boilerplate, the same PDF filed in
  two folders.
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

The project is pre-0.1.0 with no index in real use and one user, who is the
only signatory to every contract involved. There is no migration or
compatibility cost to weigh.

## Decision

**We will split identity from location. `documents` keys on path and mirrors
the filesystem; `content` keys on the content hash and is the work queue.
`doc_id` is retired; path becomes the agent-facing identity.**

```sql
documents (
  path         TEXT PRIMARY KEY,
  content_hash TEXT REFERENCES content(content_hash),  -- NULL = seen but unreadable
  size         INTEGER NOT NULL,
  mtime        INTEGER NOT NULL   -- unix nanoseconds
) STRICT;

content (
  content_hash   TEXT PRIMARY KEY,
  state          TEXT NOT NULL CHECK (state IN
    ('discovered','converted','chunked','embedded','indexed','failed')),
  stage_versions TEXT NOT NULL DEFAULT '{}',
  attempts       INTEGER NOT NULL DEFAULT 0,
  next_retry_at  INTEGER,
  last_error     TEXT,
  created_at     INTEGER NOT NULL,
  updated_at     INTEGER NOT NULL
) STRICT;

chunks (
  id           INTEGER PRIMARY KEY AUTOINCREMENT,   -- load-bearing, see below
  content_hash TEXT NOT NULL REFERENCES content(content_hash) ON DELETE CASCADE,
  ordinal, text, heading_path, byte_start, byte_end,
  UNIQUE (content_hash, ordinal)
) STRICT;

summaries (
  content_hash TEXT NOT NULL REFERENCES content(content_hash) ON DELETE CASCADE,
  level, text, PRIMARY KEY (content_hash, level)
) STRICT;

vec_chunks (rowid = chunks.id)

CREATE INDEX idx_content_queue ON content (updated_at)
  WHERE state NOT IN ('indexed', 'failed');
CREATE INDEX idx_documents_content ON documents (content_hash);
```

`chunks.id AUTOINCREMENT` survives: `vec_chunks` keys on it, the FTS5
external-content table will (`content='chunks', content_rowid='id'`), and a
freed id must never be reused or a stale vector attaches to a new chunk.

### Ownership is code organisation, not concurrency

Discovery writes `documents` and inserts `content` rows; the pipeline mutates
`content` and writes `chunks`/`summaries`. That is a module boundary, **not** a
concurrency boundary — ADR 0014 puts every catalog write on one goroutine, so
discovery and the pipeline are two modules called by the same worker.

Discovery inserting eagerly (`INSERT INTO content(content_hash, state, …)
VALUES (?, 'discovered', …) ON CONFLICT DO NOTHING`) is what keeps dispatch a
plain predicate over `content`, exactly as today:

```sql
SELECT … FROM content
 WHERE state NOT IN ('indexed','failed')
   AND (next_retry_at IS NULL OR next_retry_at <= ?)
 ORDER BY updated_at DESC, content_hash DESC LIMIT ?
```

served by `idx_content_queue` — the same partial index as migration v2, on the
same terms, so claim cost still tracks the working set rather than the corpus.
No anti-join, no lazily-appearing queue rows.

### The state machine changes subject, and shrinks

`state` describes **processing content**, so it moves to `content` and loses
the two members that were never about content:

- **`deleted` is removed.** It is already vestigial — nothing writes it, and
  deletion is a hard `DELETE` (`DeleteDocument`, `DeleteByPathPrefix`). Its
  only reader is a `bsearch status` line that always prints 0. Under this
  record a deleted file is a removed `documents` row, and the content it
  referenced is collected by the sweep.
- **Unreadable paths stop being a state.** A file we cannot read (TCC, EPERM,
  a dataless iCloud placeholder) has no content to have a state, so
  `documents.content_hash` is **NULL**: "seen this path, could not read it."
  That is a distinct representable fact rather than a value competing with the
  processing states, and it is what DESIGN.md asks for when it calls TCC
  first-class state that is never a silent skip.

**`failed` becomes permanent by construction.** Content is immutable, so a
failure against those bytes is a fact that cannot expire, and the "a file
change resets `failed`" special case in `UpsertDocument` disappears: a changed
file has a different hash, so it is a *different* `content` row, already
`discovered`. Nothing to reset.

### Deduplication becomes structural

There is exactly one queue row per unique content. A second file with identical
bytes inserts a `documents` row pointing at content that is already `indexed`
— and there is no work to do, because the queue never had a second entry. No
"check whether chunks already exist" branch in the pipeline; the schema does
it.

### Renames stop being detected

A rename is: the `documents` row for the old path goes, a row for the new path
arrives with the same `content_hash`. Content, chunks, vectors and summaries
are untouched because nothing addressed them by path. There is no id to
preserve, so `resolveID` and its heuristic **delete entirely** — along with its
rename+edit failure case, which was only a failure because it destroyed work.

### The pipeline hashes what it reads

Today only discovery hashes (`discovery.go:409`); the pipeline reads the file
again (`pipeline.go:276`) and stamps the chunks with the earlier hash. Once the
hash is the key that is unusable, and it is already wrong. The pipeline hashes
the bytes it read and writes under that hash. Costs no I/O — sha256 over bytes
already in memory.

If the hash differs from the one it was dispatched for, the file changed under
us: the pipeline abandons the claim (that content row stays where it is) and
discovery's next pass reconciles the path to the new hash.

### Orphan collection

`content` no longer referenced by any `documents` row is swept, in one
transaction on the indexing worker:

```sql
DELETE FROM vec_chunks WHERE rowid IN (
  SELECT c.id FROM chunks c LEFT JOIN documents d USING (content_hash)
   WHERE d.path IS NULL);
DELETE FROM content WHERE content_hash NOT IN (
  SELECT content_hash FROM documents WHERE content_hash IS NOT NULL);
```

Vectors first, while the chunk ids still resolve; `chunks` and `summaries`
follow by `ON DELETE CASCADE`. It must be **one transaction**, never
find-then-delete across two: a file restored to prior content in the gap
(editor undo, `git checkout`, a sync client) would have its derived data
deleted while its hash was referenced again.

**Deletion still takes effect immediately, independent of the sweep**, because
search inner-joins chunks to documents — content no path references contributes
nothing from the moment the `documents` row goes. The sweep reclaims space; it
is not what makes a delete visible.

### API: path is the identity

`doc_id` disappears from `/v1/search`, `/v1/docs`, `/v1/docs/{doc_id}` (becomes
path-addressed) and the MCP `get_document`. A path is more actionable to an
agent than an opaque surrogate, and the "doc_id continuity" caveat in Data
retention — the one thing the index promised that was not regenerable from
source — goes with it. The index becomes wholly derived data.

One content can be referenced by several paths, so `/v1/search` returns **one
hit per content**, naming a primary path with the rest in `also_at` (omitted
when empty):

- **Primary** is the referencing document with the most recent `mtime`,
  tie-broken by path ascending, so the same corpus always yields the same
  answer.
- A **scoped** query draws both the primary and `also_at` only from inside the
  scope; a scoped search never returns a path outside it.
- Returning one hit per path instead would undo deduplication exactly where it
  pays: the scores are identical, so N copies consume N slots of `limit` with
  no ranking signal to order them, and an agent pays N× the tokens for 1× the
  information.

`GET /v1/docs` still enumerates **files** — two copies really are two files.

## Alternatives considered

- **Do nothing.** Renames keep costing a re-embed, duplicates embed twice,
  `resolveID` stays load-bearing. Rejected: the cost of changing is at its
  lifetime minimum right now.

- **Rekey derived data only** — `chunks`/`summaries` on the content hash,
  keeping `documents`, `doc_id` and the queue as they are. Buys #32 and
  deduplication with a much smaller diff, and was the earlier draft of this
  record. Rejected because it leaves every modelling problem in place:
  `resolveID` survives, `state` still describes content while living on a file
  row, `failed` still needs hand-resetting, and dedup needs an explicit
  "chunks already exist" branch in the pipeline instead of falling out. It
  would also have to be re-migrated to reach this shape later, at a moment when
  migration actually costs something.

- **Compare-and-set on a `revision` column.** Addresses a bug-prevention
  framing this record does not claim, and buys neither #32 nor deduplication.
  Content addressing and CAS are complements rather than alternatives — Git
  content-addresses objects and uses CAS on refs — so this does not foreclose
  guarding a mutable pointer later.

## Consequences

**Easier.** Renames and moves are free. Duplicate content embeds once. The
state machine describes one subject and loses two members. `failed` and the
retry columns stop needing a reset path. `resolveID` deletes. The index becomes
entirely regenerable, with no continuity caveat. Chunks are provably of the
bytes that were read.

**Harder.** Orphan collection is new machinery, mandatory rather than
housekeeping, with a correctness constraint (one transaction) whose failure
mode is silent. Result assembly gains a join that can fan out. `bsearch status`
now reports two populations — files and distinct contents — and must not
conflate them.

**Committed to.** A schema rewrite executed as drop-and-reindex, which is free
today and will not stay free. Every storage port signature that currently takes
a `docID` changes. This should land before
[#18](https://github.com/bcrisp4/bsearch/issues/18) populates `summaries` and
before M4's FTS5 triggers are written, purely to avoid writing them twice.

**Risks.** The `documents.content_hash IS NULL` state is new and every read
path has to handle it — a missed case shows up as an unreadable file silently
vanishing from `status` rather than being reported, which is the exact failure
DESIGN.md's TCC handling exists to prevent. Worth a test that a permission
error survives a full scan cycle and appears in `status`.

**Follow-up.** Discovery can then hash only paths whose size/mtime changed
*and* which it has not seen before, letting the pipeline's hash serve the rest
— taking a changed file from two full reads to one. Its own issue.
