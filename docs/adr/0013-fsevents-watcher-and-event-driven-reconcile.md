# 0013 — FSEvents watcher and event-driven reconcile

- **Status:** accepted; the concurrency decision is reversed by
  [0014](0014-single-writer-catalog.md)
- **Date:** 2026-07-25
- **Confidence:** medium

> **One decision here no longer holds.** "Reconciling runs on the watch
> goroutine, concurrently with a drain" was reversed by
> [ADR 0014](0014-single-writer-catalog.md): every catalog write is back on the
> scheduler goroutine, which is what closed the two races this record's own
> Consequences section pre-registered (#62, #63). Everything else below stands —
> the library choice, the stream flags, the debounce, the rescan-on-overflow
> rule, the two-pass reconcile order, and the deletion guards. Not marked
> superseded, because most of it is still the decision in force.

## Context

Until now the daemon noticed change one way: a full walk of the include roots
every five minutes ([ADR 0011](0011-indexing-queue-dispatch-and-retry.md)).
That met DESIGN.md's "searchable ≤ 5 min on AC" only by stat-walking `$HOME`
twelve times an hour, and it never noticed a deletion at all — a walk sees
what exists, so a deleted file stayed `indexed` and kept turning up in search
results pointing at nothing.

Issue [#13](https://github.com/bcrisp4/bsearch/issues/13) implements
DESIGN.md's Change-detection row: an FSEvents watch behind a `WatcherPort`,
with the walk demoted to a missed-event backstop. The decisions worth
recording are the ones a future reader would otherwise have to reconstruct
from a control loop and a cgo binding: where the FSEvents code comes from,
what happens when the event stream cannot be trusted, and how far an event is
allowed to be believed when it says a file is gone.

Deletion is the sharp end. Issue
[#57](https://github.com/bcrisp4/bsearch/issues/57) splits it deliberately:
the event-driven half here, the scan-side reconcile there, because the two
have completely different evidence available and only one of them can destroy
the corpus by being wrong.

## Decision

**We will depend on `github.com/fsnotify/fsevents` rather than hand-roll the
cgo.** It is a small, focused, zero-transitive-dependency BSD-3 binding from
the fsnotify organisation that exposes exactly what is needed: the create
flags, the overflow flags, the coalescing latency, and a dispatch-queue-based
stream. The alternative was ~200 lines of our own cgo doing unsafe pointer
arithmetic over the C flags and paths arrays and managing stream lifetime —
code the library has already got right.

**The stream is created with `FileEvents | NoDefer | WatchRoot` and a 1 s
latency.** `FileEvents` gives per-file paths; without it every event names a
directory and the caller must re-walk to find out what changed. `NoDefer`
delivers the first event of a burst at the start of the latency window rather
than the end. `WatchRoot` reports a root itself being moved or replaced.

**An untrustworthy event stream becomes a full walk, never a guess.**
`MustScanSubDirs`, `KernelDropped`, `UserDropped`, `RootChanged`, `Mount`,
`Unmount` and `EventIDsWrapped` all collapse the batch to a single "rescan"
flag, discarding the paths collected so far — keeping them would imply they
were sufficient. The same collapse happens if a batch held back by a slow
consumer exceeds 8192 paths. Losing events costs a walk, never correctness.

**Events are debounced for 10 s in a fixed window armed by the first event of
a burst**, not a quiet period that resets on activity. A resetting window
never closes on a file being written continuously — a large export, a log —
and that is the file most worth noticing. Closing early costs nothing: the
content hash makes the work idempotent and the events for the rest of the
write open another window that catches the final state.

**Reconciling a window runs in two passes, and the order is load-bearing.**
Paths that exist are reconciled first, then paths that are still missing are
considered for deletion. A rename arrives as the old path and the new path in
one batch, and rename detection works by finding a catalog row whose content
hash matches and whose path is gone from disk; reconciling what exists first
lets the new path claim the old row, so by the time the old path is examined
the row has moved out from under it and is not purged. In the other order
every rename would purge and re-mint the document ID.

**A vanished path is purged with its whole subtree, but only on positive
evidence.** One store call (`DeleteByPathPrefix`) covers both a deleted file
and a deleted directory, because an event does not say which it was. Three
guards stand in front of it: only `ENOENT` qualifies (EPERM and EIO are
recorded as path errors, never deletions); the path is re-stat'ed, since the
first pass may have moved a renamed row onto it; and **the parent must still
exist**. An unmounted volume or a revoked Full Disk Access grant makes a whole
subtree return `ENOENT` at once, and treating that as a thousand deletions
would destroy the corpus over a blip.

**The walk's cadence follows the watcher: 5 minutes scan-only, 15 minutes
while watching.** Read live rather than latched at startup, so a watcher that
stops restores the tighter cadence on its own. Two constants, not a config
key — DESIGN.md's sample config has no scan interval to grow and nobody has
asked to turn this knob.

**Reconciling runs on the watch goroutine, concurrently with a drain.** A
note saved during a multi-hour backlog drain must not wait for it.
Reconciling is stat, hash and upsert with no network in it, and the SQLite
writer is one connection with a busy timeout. For an *edit*, ADR 0011's
argument carries over unchanged: every pipeline stage is an idempotent
upsert, so a reconcile that resets a document the pipeline is halfway through
is the right answer rather than a race lost — the file changed underneath it.

**A delete is not idempotent that way, so it needs an invariant: only
discovery creates catalog rows.** Purging a row the pipeline is mid-flight on
would otherwise be undone by the pipeline's own next write, which was a plain
INSERT — the document would come back, be embedded, be finalized, and be
served, with a source file that no longer exists and nothing anywhere to
notice. The write that would have resurrected it now reports
`domain.ErrDocumentGone` instead, and the pipeline treats that as
`OutcomeSkipped`: the file is gone, so there is nothing to index and nothing
to record about it. `UpsertDocument` creates a row only for state
`discovered`, which is discovery announcing a file; every other state is a
write to a row expected to exist. The scheduler makes the same distinction
when recording a retry or a failure, because "the index is broken" and "the
file is gone" look identical from a failed write and are opposite situations
— gating the drain on the second would let one deleted file stall indexing
for everything behind it.

**A watcher that will not start is scan-only, not a failed daemon.** Every
failure path — an unsupported platform, no watchable roots, a rejected
subscription — logs a reason, records it in `/v1/status`, and leaves the walk
carrying freshness alone.

## Alternatives considered

- **Hand-rolled cgo binding.** No dependency to vet or track, and no
  pre-1.0 risk. Rejected: it is precisely the unsafe, lifetime-sensitive code
  worth not owning, and `WatcherPort` already makes replacing it cheap if the
  library goes stale.
- **`github.com/rjeczalik/notify`.** More popular, more recently pushed, and
  cross-platform — a free inotify adapter later. Rejected: a much larger
  surface for one platform, and it abstracts away the FSEvents-specific
  overflow flags that the rescan decision depends on.
- **`github.com/fsnotify/fsnotify`.** The obvious name. Rejected: on macOS it
  is kqueue, which needs an open file descriptor per watched file and cannot
  watch recursively — unusable for a `$HOME`-scale corpus.
- **Resume from a persisted `EventID`.** FSEvents can replay what happened
  while the daemon was down. Deferred to a follow-up issue: the periodic walk
  already covers the restart case for additions, so resume buys latency
  rather than correctness, and it needs the device UUID persisted alongside
  (event IDs are per-device, so a UUID change must force a cold start).
- **Soft-delete to the unused `deleted` state instead of purging.** Would
  keep a document ID recoverable if the file came back. Rejected: DESIGN.md's
  Data retention says purged, `DeleteDocument` already cascades correctly,
  and a purge path would still have to be written for the soft-deleted rows.
- **Reconciling on the scheduler goroutine.** One writer, no concurrency
  story to reason about, and the delete-versus-pipeline interaction would not
  arise. Rejected: it puts a saved note behind whatever drain is in flight,
  which is the exact latency this change exists to remove.
- **Letting the pipeline re-create a purged row and relying on #57 to clean
  up.** Rejected: #57 does not exist yet, and a resurrected document is
  indistinguishable from a real one — it looks indexed, it is served, and
  nothing degrades to signal it. A deleted file that stays findable is the
  failure DESIGN.md's Data retention section exists to rule out.
- **A quiet-period debounce.** The more common shape. Rejected for the
  continuously-written file, as above.
- **Escalating only the named subtree on `MustScanSubDirs`.** More precise
  than a full walk. Deferred to a follow-up: a walk is about a second warm,
  and overflow is rare enough that the precision is not yet worth the code.

## Consequences

A saved file is searchable in around fifteen seconds rather than up to five
minutes, and a deleted one stops being searchable in about the same time —
the first time deletion has followed the source at all. The walk drops to a
quarter of its former frequency, so the background cost of the daemon falls
even as freshness improves.

The daemon now carries a cgo dependency that is pre-1.0 and last released in
May 2024. Pinned, and small enough behind `WatcherPort` that replacing it is
an adapter swap; the risk is accepted rather than mitigated.

Deletion is now partially implemented, and the seam is visible: a file deleted
while the daemon is running is purged, and a file deleted while it was **not**
running stays in the index until #57 lands the scan-side reconcile. The
vanished-parent guard makes the same trade inside a single window — a subtree
that disappears without its own directory event is left alone. Both are
freshness costs paid to avoid a corpus-destroying failure mode, and both are
#57's to close.

`discovery.Scan` and `discovery.ScanPaths` now share `walkTree`, `roots` and
`processFile`, so there is one implementation of "has this file changed" and
one of "which directories are in scope" — the watcher cannot drift into
watching paths the walk does not index.

"Only discovery creates catalog rows" is a new invariant on `UpsertDocument`,
and it is load-bearing rather than defensive: without it the watcher's purge
and the pipeline's writes race to a wrong answer. It also means a test can no
longer conjure a chunked or failed document out of nothing, which is why the
storage tests grew a `seedUpsert` helper that goes through discovery's create
first — the same two steps production takes.

Roots are resolved and subscribed to once. A root created after the daemon
started is not watched, and a stream that ends is not re-subscribed; both
need a restart. Deliberate — re-resolving on a timer is a retry loop to tune,
and the failure degrades visibly (the walk tightens back to five minutes and
`bsearch status` says the watcher stopped) rather than silently. One caveat
found in review: `fsnotify/fsevents` never closes its event channel, so a
dead stream is indistinguishable from a quiet one and the daemon would keep
reporting a healthy watcher. There is no liveness signal today (issue #65).

**Known gaps, all the same shape.** Deleting is the only operation here that
destroys work, so where the daemon cannot get positive evidence it declines,
and the cost is a stale row rather than a lost document. Three cases decline:
a file deleted while the daemon was not running; a deletion in a burst that
overflowed the event stream, since the resulting walk cannot see absences
either; and a subtree that vanished without its own directory event. All
three are #57's to close from the catalog side, and until then re-deleting
with the daemon running clears them.

Two narrower races remain open, both from this ADR's own concurrency
decision, and both needing a compare-and-set write — that is, enforcing
`domain.ValidTransition` in the store, which `internal/domain/document.go`
explicitly defers until "there are enough writers to lose track of". This
change adds the second concurrent writer that makes that true, so the
deferral is now the thing to revisit (issue #63): a reconcile landing between
the vector write and the finalize can leave a document marked `indexed` with
no chunks, and a stale pipeline write can revert a rename. Separately, a file
new to both the walk and a reconcile at the same moment gets two document IDs
and the loser's row is displaced (#62) — the idempotency argument above
covers writes to existing rows, not the creation of new ones. And an include
root spelled in the wrong case resolves, subscribes, and matches no event
(#64), which the new `Result.Ignored` counter makes diagnosable but does not
fix.

`Scanner` (the scheduler's port) grew two methods, so any future scanner
implementation owes them. `bsearch status` grew a `watch` object, additive
like every other field, and a line that says plainly when the watcher is off
and why — including the case that reads as healthy from everywhere else: a
watcher subscribed and running but never told anything, which on macOS means
a missing Full Disk Access grant ([#14](https://github.com/bcrisp4/bsearch/issues/14)).

The FSEvents integration test waits on the kernel, so it is slower than the
rest of the suite and skipped under `-short`. It is the only thing that
proves events actually fire, and unit tests over `absorb` and `normalize`
cover the translation rules without it.
