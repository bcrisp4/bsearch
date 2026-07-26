# 0014 — Single-writer catalog

- **Status:** proposed
- **Date:** 2026-07-25
- **Confidence:** high

## Context

[ADR 0013](0013-fsevents-watcher-and-event-driven-reconcile.md) decided that
reconciling a debounce window runs on the watch goroutine, concurrently with a
drain, so "a note saved during a multi-hour backlog drain must not wait for
it." That made the catalog a two-writer structure: the scheduler goroutine
(periodic walk, then the indexing drain) and the watch goroutine (reconcile).

The bill arrived immediately. Holding it together needed a new load-bearing
invariant ("only discovery creates catalog rows"), two sentinel errors
(`ErrDocumentGone`, `ErrDocumentSuperseded`), and two hand-placed liveness
guards (`store.go`'s row check, `vec.go`'s chunk-id check). ADR 0013's own
Consequences section pre-registered two bugs it could not close —
[#62](https://github.com/bcrisp4/bsearch/issues/62) (walk and reconcile each
mint a document id for the same new path; the loser's row is silently
displaced) and [#63](https://github.com/bcrisp4/bsearch/issues/63) (a reconcile
landing between the vector write and the finalize strands a document as
`indexed` with no chunks, permanently unsearchable). Both issues propose
compare-and-set: enforcing `domain.ValidTransition` in every writer, or a
version column.

Re-examining the premise rather than the fix, the concurrency turns out to buy
nothing measurable.

- **No indexing latency.** `Scheduler.drain` is a loop — claim 32, process,
  re-claim, repeat until the queue is empty — and the claimed slice is fixed at
  claim time. A row written mid-drain is not in the in-flight batch either way.
  `ClaimBatch` orders recency-first (`updated_at DESC`, with an aging quarter),
  so the freshly written row is at the head of the *next* claim — the same next
  claim in both designs. The mechanism that actually rescues a saved note from
  a backlog is the recency ordering, which already exists and does not care
  which goroutine wrote the row.
- **No write throughput.** The writer pool is `openPool(dsn, 1, 1)` — one
  connection, max. Both goroutines already serialise on it.
- **What it does buy** is that a deletion leaves search up to one document
  sooner, and that `bsearch status` counters move sooner. Both sit behind a
  deliberate 10 s debounce.

Set against that: DESIGN.md's Storage row still claims a single-writer model,
and its fourth goal is "Stay understandable for years … explicitly the
anti-`lore` goal." The concurrency trades that goal for a latency improvement
that measures zero, and the tax compounds — `embed ∥ summarize` (#18) puts a
third writer in the design.

Single-writer is also achievable for real rather than by convention: the flock
in `socket.Listen` gives `ErrAlreadyRunning` to a second daemon, so it is one
writer per machine, not merely one per process.

## Decision

**We will put every catalog write back on the scheduler goroutine**, reversing
ADR 0013's concurrent-reconcile decision.

The watch goroutine stays. It keeps the FSEvents subscription and the 10 s
debounce window — that window is armed by the first event of a burst and must
not be coupled to drain progress — but it stops writing. It merges each closed
window into a pending set the scheduler drains, calling `ScanPaths` itself.
This is a move of the write, not the deletion of a goroutine.

**The handoff is a mutex-guarded set, not a channel**, and the difference is
not incidental. A set merges, so a file saved three times during one long embed
is one path to stat; a channel of windows cannot dedup, so its bound would be
total events rather than distinct paths. It is also non-blocking in the
direction that matters: the watch goroutine's next duty is to be ready for the
next FSEvents callback, and a send that blocked on the scheduler would stall
the stream.

**The set is capped, and overflow keeps what it holds rather than what
arrives.** This is deliberately the opposite of the adapter's own overflow rule
and is worth recording because it trades away deletions. The adapter collapses
a batch the kernel has already declared incomplete — its paths have stopped
being sufficient, so discarding them costs nothing. The scheduler's set is
exact, so refusing the *incoming* window and keeping the held one is what
minimises the loss: a walk buys back the refused window's creates and edits,
and nothing buys back its deletions, because a walk sees what exists (#57).
The refusal is logged, counted in `WatchRescans`, and asks for that walk.

Drains happen at four points, and the placements are load-bearing: above the
battery gate in `cycle` (reconciling named paths is a cheap stage, and below
the gate an unplugged laptop would collect a day of deletions and purge none),
below the walk in `cycle` (the walk *replaces* the path-error sample and a
reconcile *appends* to it, so one order had to be chosen), at the top of
`drain`'s loop, and between documents in `process`.

One cost the "cheap stage" framing understates: `ScanPaths` descends into a
directory path, because FSEvents reports a directory once rather than once per
file inside it. So a directory event is a recursive walk with content hashing,
not a stat — bounded only by the subtree. Ordinary windows are a handful of
file paths and this does not arise; a bulk copy into a watched root is where it
does, and bounding it is issue #70.

**We will re-read each document's row immediately before processing it**, via a
new `GetByID` on `DocumentStore`, and process the fresh copy. `ClaimBatch`
returns 32 copies read at claim time; a reconcile running between documents can
rename a document still sitting unprocessed in that slice, and the pipeline
would otherwise read `doc.Path` from a stale copy — the old path, now holding a
different file. This re-read is *exactly* sufficient only because there is one
writer: nothing can write between the re-read and the end of that document's
processing. The two changes travel together, and neither is complete alone.

That sufficiency is a constraint on where the handoff is drained, not a
property that falls out of single-writer on its own. **The pending-path set is
drained strictly between `process` iterations — before the re-read, and never
inside `ProcessDocument`.** A drain anywhere within a document re-opens #63
with the re-read still in place and no test to catch it, because the stale copy
would once again outlive a write. It is recorded here rather than left to a
comment because it is the one ordering the whole argument rests on.

The re-read belongs to the scheduler, not to `pipeline.ProcessDocument`. The
pipeline has a second caller — `bsearch eval` (`cmd/bsearch/eval.go`) drives
`Indexer.Run` over its own per-corpus database — which has no concurrent writer
and should not pay for a race it cannot have. Putting it in the pipeline would
also silently invalidate `Run`'s own up-to-date/stale classification, which is
computed from the documents it was handed.

**We will add a state clause to the pipeline's finalize** —
`UPDATE documents SET state = 'indexed' … WHERE id = ? AND state = 'chunked'` —
and treat zero rows affected as an error. This is an **assertion that the
single-writer invariant holds, not concurrency control**, and the ADR says so
deliberately: a future reader must not reconstruct a concurrency story that is
not there. It earns its place because #18's third writer is in the design
document rather than hypothetical, and without the clause its arrival would
re-open #63 silently — documents reaching `indexed` with no chunks and nothing
failing.

We will **not** add a version/generation column, and will **not** enforce
`domain.ValidTransition` across every writer. The deferral recorded in
`internal/domain/document.go` stands: there are not enough writers to lose
track of, and after this change there is one.

## Alternatives considered

- **Compare-and-set, as #62 and #63 propose** (mint the id inside the insert
  transaction; version column or `ValidTransition` enforced on every write).
  Fixes both bugs and keeps the concurrency. Rejected: it is coordination
  machinery bought to protect a benefit that measures zero, and it leaves the
  doc_id minting policy and `resolveID`'s rename heuristic intact. More
  concepts, more code, same outcome.
- **A mutex around `Scan` and `ScanPaths`** (#62's second option). Simplest
  possible patch. Rejected: it serialises a reconcile behind a *whole* walk,
  and it fixes #62 without touching #63 at all — the pipeline's stale snapshot
  is untouched by it.
- **Content-addressed derived data**
  ([#67](https://github.com/bcrisp4/bsearch/issues/67)) — key chunks and
  vectors by content hash, so identity is immutable and both bugs become
  unrepresentable rather than guarded. Genuinely the deeper answer, and it
  also delivers rename-preserves-derived-data (#32) and duplicate-file
  deduplication. Deferred, not rejected: it is a schema rewrite plus a change
  to the documented `doc_id` API contract, and it needs its own ADR. It
  composes with this decision rather than competing — it would stop the
  single-writer invariant being load-bearing for correctness.
- **Draining the path channel between batches rather than between
  documents.** Fewer interruption points. Rejected on the one axis where the
  choice matters: deletion visibility. Both are identical for indexing
  latency, but between-batches leaves a purged file searchable for up to 32
  documents instead of one.
- **Keeping the concurrency and accepting the two bugs.** Rejected: #63 makes
  a document permanently unsearchable with nothing to signal it, which is the
  failure mode the observability work in `bsearch status` exists to rule out.

## Consequences

#62 closes outright — the walk and the reconcile are no longer concurrent, so
two writers cannot mint two ids for one path. #63 closes in two parts: the
finalize clobber becomes impossible, and the reverted-rename half is closed by
the per-document re-read.

`ErrDocumentSuperseded` goes: it exists to tell the pipeline to stand down from
a race that can no longer happen. **The chunk-id liveness check in `vec.go`
stays**, and the distinction is worth stating because the first draft of this
record had it removing both.

Unreachable does not mean removable, because these two guards fail in opposite
directions. The finalize clause *reports* — it fires after its document's
writes have committed, and everything it protects is repairable by re-queuing
the row. The liveness check *prevents*: `chunks.id` is `AUTOINCREMENT`
specifically so a freed id is never reused, and every delete path in the store
reaches vec rows by joining through `chunks`, so a vector row whose chunk is
gone is unreachable by every delete we have. `SearchVectors` takes `k` from the
vector table in a subquery and only then inner-joins `chunks`, so such a row
displaces a real hit before being dropped — a `k=10` search silently returns
nine results, permanently, with nothing anywhere to say why. An assertion that
reports after an unrepairable write is not an assertion. The cost of keeping it
is one `count(*)` over an integer primary key inside a transaction that is
already open, which is the same cost argument this record already accepts for
the re-read.

`ErrDocumentGone` **stays** too, for a different reason: a purge landing
between documents can still remove a row that is sitting in the claimed slice.
It stops being a race and becomes a deterministic sequential case, which is
easier to test and to reason about. Its producer moves — with the re-read in
place, `GetByID` is where a purged row is normally noticed, and the write-side
guards behind it become the second line rather than the first.

DESIGN.md's Storage row becomes true again ("single-writer model fits"), and
ADR 0013's "only discovery creates catalog rows" invariant stops being
load-bearing for correctness — worth keeping as a guard, but no longer the only
thing standing between a purge and a resurrected document.

The costs are real and small. A deletion leaves search up to one document later
than today. A document that hangs — a bscribe conversion of a large PDF once
#21/#22 land, or a slow embedder — now stalls reconciles behind it, so
per-request timeouts on the converter and embedder become load-bearing rather
than merely good practice.

**Shutdown gets longer, and serially so.** The final reconcile used to run on
the watch goroutine the instant the context fired, in parallel with the drain
unwinding — `max(unwind, flush)`. Only the scheduler may write now, and it
cannot flush until the watcher has handed its last window over, so the wait
becomes a precondition: `unwind + flush`, after an HTTP drain of up to 10 s
that precedes both. Against launchd's patience that is the whole safety margin,
and what is at stake if it runs out is the one thing a later pass cannot
recover — the last window's deletions.

Accepted rather than engineered around, because every step of the unwind is
fast for the corpus this indexes: the embedder cancels on context, and hashing
and chunking a markdown file is milliseconds. `ExitTimeOut` in the documented
plist is raised to 60 s to cover it with room. The step that will not be fast is
the converter, which is why #21 now carries the constraint. A shutdown that
still runs out of budget is bounded by `shutdownFlush` rather than by
correctness, and the loss is the same one #57 owns.

`DocumentStore` grows `GetByID`, so any future store implementation owes it.
The re-read costs one primary-key `SELECT` per document, against an embed
measured in hundreds of milliseconds.

Known risk: the finalize assertion is a branch that cannot fire under the
design as written, and unfirable branches rot. Mitigated by unit-testing it
directly, and by the fact that the condition it detects (#18's parallel
summarizer) is scheduled rather than speculative.

**Landing order is not free choice, and the deferred half is tracked as
[#69](https://github.com/bcrisp4/bsearch/issues/69).** The assertions must ship *after* the
writer move, never before or alongside it in a separate release. A save landing
during its own document's embed is ordinary use, and while the watcher still
writes concurrently that event legitimately resets the row and clears its
chunks — so a finalize that demanded `chunked`, or a vector write whose
staleness had stopped being a stand-down and become fatal, would turn a routine
save into a stalled drain. Adding a guard for an invariant to a codebase that
does not yet hold it reports the guard's own prematurity as a bug in the
system. `GetByID` and the re-read carry no such constraint: they are additions,
and they belong with the writer move because that is where their caller is.
