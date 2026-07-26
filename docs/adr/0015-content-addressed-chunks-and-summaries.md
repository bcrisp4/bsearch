# 0015 — Key chunks and summaries by content hash

- **Status:** proposed
- **Date:** 2026-07-26
- **Confidence:** high

## Context

Chunks, vectors and summaries are functions of a file's **content**. They are
keyed on `doc_id`, which identifies the **file**. The schema therefore does not
know about the dependency that actually exists, and we maintain it by hand:

- Renaming a file throws away valid chunks, vectors and summaries and
  re-embeds them for no reason ([#32](https://github.com/bcrisp4/bsearch/issues/32)).
- Two identical files embed twice — copies, boilerplate, the same PDF filed in
  two folders.
- `Scanner.resolveID` (`internal/discovery/discovery.go:462`) is ~50 lines of
  heuristic whose whole job is keeping a surrogate id attached to the right
  file, with a documented failure case: rename + edit inside one scan window
  looks like delete + create and mints a fresh id.

Worth stating what this record is **not** motivated by, because two earlier
framings were wrong and are recorded in
[#67](https://github.com/bcrisp4/bsearch/issues/67):

- It is **not** a fix for [#63](https://github.com/bcrisp4/bsearch/issues/63)
  or a bug-prevention measure. ADR 0014 closed that class structurally by
  putting every catalog write on one goroutine, and
  [#69](https://github.com/bcrisp4/bsearch/issues/69) — asserting the invariant
  rather than holding it — was closed unbuilt.
- It is **not** urgent. There is no deadline; the project is pre-0.1.0 with no
  index in real use.

It is a feature argument: put the key where the dependency already is.

## Decision

**We will key chunks and summaries on the content hash. `documents`, `doc_id`,
the queue and the API contract stay as they are.**

```sql
documents  (id, path, content_hash, state, attempts, …)        -- UNCHANGED
chunks     (id INTEGER PRIMARY KEY AUTOINCREMENT,              -- id UNCHANGED
            content_hash NOT NULL, ordinal, text, heading_path,
            byte_start, byte_end, UNIQUE (content_hash, ordinal))
summaries  (content_hash, level, text, PRIMARY KEY (content_hash, level))
vec_chunks (rowid = chunks.id)                                 -- UNCHANGED
```

`chunks.id AUTOINCREMENT` survives deliberately: `vec_chunks` keys on it, an
external-content FTS5 table will key on it (`content='chunks',
content_rowid='id'`), and it is the schema's only immutable version token — the
liveness check at `vec.go:274-301` depends on a freed id never being reused.

Three consequences follow directly, and are part of the decision:

**1. The pipeline hashes the bytes it reads.** Today only discovery hashes
(`discovery.go:409`); the pipeline reads the file again (`pipeline.go:276`) and
stamps the resulting chunks with the hash discovery computed earlier. That is
already wrong — if the file changed in between, the chunks are of the new
content under the old hash — and once the hash is the key it is unusable. The
pipeline will hash what it read and write chunks under that hash, updating
`documents.content_hash` in the same transaction. It already writes that row
(`UpsertDocument`, `pipeline.go:307`), so there is no ownership question. This
adds no I/O: sha256 over bytes already in memory.

**2. Orphan chunks are collected by a single-statement sweep.** `ON DELETE
CASCADE` from `documents` no longer reaches chunks, because other files may
share them, so deletion needs:

```sql
DELETE FROM chunks WHERE content_hash NOT IN (SELECT content_hash FROM documents)
```

`idx_documents_content_hash` already exists, so the subquery is an index scan.
It runs on the indexing worker like every other catalog write (ADR 0014), after
deletions rather than every cycle.

It must be **one statement**, never find-then-delete. A file restored to prior
content between the two phases — editor undo, `git checkout`, a sync client —
would otherwise have its chunks deleted while its hash is referenced again,
leaving a row terminal with no chunks: #63's exact signature, produced by new
machinery.

**3. Search collapses per content, and reports every location.** One content
hash can be referenced by several documents. `/v1/search` returns **one hit per
content**, carrying one primary path plus the others as metadata:

```json
{
  "doc_id": "d_8f3a91",
  "path": "~/Documents/quotes/heatpump-vaillant-2025.pdf",
  "score": 0.83,
  "summary": "…",
  "also_at": [
    {"doc_id": "d_1c04b2", "path": "~/Archive/2025/heatpump-vaillant-2025.pdf"}
  ]
}
```

- `also_at` is omitted when empty, so the overwhelmingly common single-location
  case is byte-identical to today's response.
- **Primary path** is the referencing document with the most recent `mtime`,
  tie-broken by path ascending. Deterministic given the data; the tie-break
  exists so the same corpus always produces the same answer.
- When a query is **scoped** (a path prefix), the primary is chosen from
  documents *inside* the scope, and `also_at` lists only in-scope locations —
  a scoped search must never return a path outside its own scope.
- Search **inner-joins chunks to documents**, so a content hash with no
  referencing document contributes nothing. Orphaned chunks are invisible from
  the moment their last path goes, independent of when the sweep runs — GC lag
  can never surface a hit for a deleted file.
- `CollapseBestPerDoc` (`internal/domain/search.go`) becomes
  `CollapseBestPerContent`: same shape, dedup key moves from `Doc.ID` to the
  content hash.

`GET /v1/docs` is **unchanged** and still enumerates files: two copies really
are two files, and enumeration is about the filesystem. Deduplication is about
derived data, not about pretending files do not exist.

**`stage_versions` stays per document**, which is what makes deduplication fall
out of the existing queue rather than needing new machinery. A second file with
identical content is discovered, claimed and read as usual; the pipeline hashes
it, finds chunks for that hash already at current stage versions, skips the
embed, records its own `stage_versions` and finishes. One embed, two indexed
documents, no special case. A chunker or model bump resets both rows, and
whichever is claimed first does the single re-chunk the other then reuses.

## Alternatives considered

- **Do nothing (keep `doc_id` keying).** Rename keeps costing a full re-embed,
  duplicates embed twice, and `resolveID`'s heuristic stays load-bearing.
  Rejected: the cost of change is near zero right now (no index in real use),
  and it only rises with corpus size.

- **Full identity split** — `documents(path PK)` + `content(hash PK, state,
  attempts, …)`, retiring `doc_id` entirely. Additionally deletes `resolveID`
  and dissolves stale-identity bugs. Rejected **for now**, not on merit: it
  costs an ownership rule to reconcile (nobody can insert the `content` row for
  a newly discovered hash without either an insert-if-absent rule or an
  anti-join dispatch that breaks `idx_documents_queue`), a `doc_id` API
  contract change across `/v1/search`, `/v1/docs/{doc_id}` and the MCP tools,
  and a change to what `bsearch status` counts (contents, not files). Nothing
  forces that decision now and it can be adopted later on top of this record.

- **Return N hits, one per path.** Rejected: it undoes deduplication at exactly
  the moment it pays. The scores are identical — same chunk, same content — so
  N adjacent slots of `limit` are consumed with no ranking signal to order
  them, and `limit=10` could return ten copies of one document. For the primary
  scenario (an agent pulling context) that is N× the tokens for 1× the
  information.

- **Return one hit and hide the other locations.** Rejected: the index knows
  something the user wants — that this also lives in their archive — and the
  chosen path would appear to change for no visible reason. `also_at` costs one
  omitted-when-empty field.

- **Compare-and-set on a `revision` column** (optimistic locking) instead.
  Rejected: it addresses the bug-prevention framing this record explicitly does
  not claim, and buys neither #32 nor deduplication. Note that content
  addressing and CAS are complements rather than alternatives — Git
  content-addresses objects and uses CAS on refs (`--force-with-lease`) — so
  this does not foreclose adding CAS to a mutable pointer later.

## Consequences

**Easier.** A rename becomes a `documents.path` update with nothing derived to
touch (#32 closes). Duplicate content embeds once and shares summaries. The
pipeline's chunks are provably of the bytes it read. `resolveID`'s stakes
collapse: a mis-resolved id now costs a stable agent reference, not lost
derived data — its known rename+edit failure case stops destroying work.

**Harder.** Orphan GC is new machinery, mandatory rather than tidiness, with a
correctness constraint (single statement) that is easy to get wrong and whose
failure mode is silent. Deleting a document is no longer a cascade. Result
assembly gains a join (`chunks → content_hash → documents`) that can fan out.

**Committed to.** Migration is drop-and-reindex — free today, and this record
should be executed before that stops being true. `summaries` must be rekeyed
before [#18](https://github.com/bcrisp4/bsearch/issues/18) populates it, and
the FTS5 triggers landing in M4 should be written against the new shape;
neither is a hard deadline, both are avoidable rework.

**Not addressed.** This changes nothing about writer concurrency: ADR 0014
remains load-bearing, and the sweep is one more reason it is. It does not
retire `state`, the queue, or the `GetByID` re-read — the pipeline still needs
a *path* to read bytes, and paths stay mutable.

**Follow-up.** Discovery could then go stat-only and hash only paths it has not
seen before (which is exactly when rename detection applies), taking a changed
file from two full reads to one. Not required here; worth its own issue.
