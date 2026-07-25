# 0011 — Indexing queue: dispatch, retry, and health gates

- **Status:** proposed
- **Date:** 2026-07-24
- **Confidence:** medium

## Context

DESIGN.md specifies the indexing queue in outline — SQLite-backed states,
dispatch that skips terminal rows, exponential backoff, health gates, recency
priority with aging — but leaves the parts that decide how the daemon behaves
when things go wrong. Issue [#12](https://github.com/bcrisp4/bsearch/issues/12)
implements it, and the schema has carried `attempts`, `next_retry_at` and
`last_error` since migration v1 with nothing reading them.

The decisions worth recording are the ones a future reader would otherwise
have to reverse-engineer from a control loop: what happens when a cycle
overruns its interval, what a "limit" is when the queue is a catalog rather
than a submission queue, and who pays when an external service is down.
DESIGN.md's own queue paragraph is written for a design where dispatch claims
rows in a write transaction and tracks claims in memory; with one worker,
neither is needed, and the doc is updated in the same change.

The lenses here are borrowed rather than invented — schedule overrun
semantics, explicit limits, and fault models, via
[Job Queues Are Deceptively Tricky](https://typesanitizer.com/blog/job-queues.html);
partial claim indexing from Honker; full jitter from
[AWS's Exponential Backoff and Jitter](https://aws.amazon.com/blogs/architecture/exponential-backoff-and-jitter/).

## Decision

We will run the queue as follows.

**One worker, and therefore no claims.** A single goroutine does the indexing.
That removes the claimed-state machinery DESIGN.md anticipated: there is no
contention to arbitrate, so `ClaimBatch` is a plain read and nothing has to be
released after a crash. Recovery is redo — every stage is an idempotent
upsert, so a document interrupted mid-flight is simply processed again.

Concurrency above one buys overlapping network waits and costs a claim set.
`EmbedPassages` already batches 64 chunks per request, so a document of any
size keeps the endpoint busy across back-to-back calls; SQLite serialises
writes regardless (the writer pool is one connection); and concurrent embed
requests to one local GPU largely time-slice rather than add throughput. With
no measurements to the contrary and a battery to answer to, one is the honest
default. It is a named constant, and raising it is an addition rather than a
redesign.

**Prefer Old on overrun.** The interval timer is reset *after* a cycle
finishes, not on an absolute schedule, so a trigger arriving mid-cycle is shed
rather than queued or allowed to pre-empt. Of the four possible overrun
policies this is the one whose stated assumption matches the workload: a drain
that ran long did so because there is a lot of work, not because it is hung,
and a second concurrent drain would contend for the same rows and the same
inference server while making no more progress. Nothing is lost, because the
queue is durable and the next cycle re-reads it.

**Attempts are only spent while the service is healthy.** Before working a
batch the scheduler proves the embedding endpoint is up, using the same
`EmbedQuery` probe that discovers the vector dimensions — a probe that
exercises a different code path than the work is a probe that can pass while
the work fails. When a document then fails transiently, the endpoint is
*re-probed* before the failure is charged to it: a failed re-probe means the
service went down mid-batch, so the batch is deferred and nothing is charged;
a passing re-probe means this document is the odd one out, and only then does
it spend an attempt. Without the re-probe, an inference server switched off
overnight would quietly mark a chunk of the corpus permanently failed.

**Full jitter backoff**, `random(0, min(cap, base·2^(attempt-1)))`, base 30 s,
cap 15 minutes, 5 attempts. Of the published variants full jitter does the
least total work and puts the least load on the struggling dependency, which
is the right trade for a local inference server sharing a laptop. The
randomness is what stops a batch that failed together from returning together.
The cap can be short because backoff only ever applies to failures met while
the endpoint was verifiably healthy.

**A partial claim index.** Migration v2 replaces v1's
`(state, next_retry_at)` index — which led on a column the dispatch predicate
only negates, and carried every terminal row — with
`(updated_at) WHERE state NOT IN ('indexed','failed','deleted')`. Terminal
rows are absent from the B-tree entirely, so claim cost tracks the working set
rather than the corpus, and moving a document into a terminal state costs no
index maintenance on the hot path.

**Batch composition, not just ordering.** A claim takes three quarters of its
rows from the most recently changed documents and one quarter from the least.
That is "recency with aging" without a priority column or a decay score: a
file saved a moment ago is indexed in the current cycle, and a bulk backlog
still drains rather than starving behind it. `updated_at` has one-second
resolution, so `id` is the tiebreak — without it a bulk discovery pass could
return the same arbitrary subset forever.

**Limits, stated.** Batch size 32, so priority is re-evaluated often enough
that a new file never waits behind a whole backlog. Attempts 5. Scan interval
5 minutes, independent of the drain interval: until the watcher lands the scan
is the only way a new file is noticed, so it *is* the freshness SLO. A walk of
a ~500k-file `$HOME` with the deny-list applied measures about a second warm,
which makes that affordable; it should get slower once FSEvents is primary.
Drain interval from `[power].{ac,battery}.index_interval`.
Queue depth is **deliberately unbounded**: the queue *is* the catalog, one row
per file, bounded by the corpus, and there is no submission path that can
inflate it — a depth cap would bound nothing. That is recorded so its absence
reads as a decision rather than an oversight.

**A stale sweep, once per process.** Documents whose `stage_versions` predate
the current configuration are moved back to `discovered` before anything can
conclude the queue is empty, because a fully-indexed corpus built by a
superseded model looks exactly like no work at all. It runs in two passes: the
version keys need no network and run first; the dimension key is only knowable
after the probe. `updated_at` is deliberately not bumped — stamping the whole
corpus with one timestamp would flatten the ordering signal at the moment
every document is queued at once.

**A wake hint, with the poll as the fallback.** `Notify()` is a non-blocking
send on a one-deep channel, for #13's watcher. It carries no information, so
dropping one when a hint is already pending is correct: waking once and
finding fifty new rows beats waking fifty times. The periodic cycle is what
makes a lost hint a latency problem rather than a correctness one.

## Alternatives considered

- **A persistent `processing` state.** Already rejected in DESIGN.md's Closed
  issues; re-confirmed. It exists to let a second worker know a row is taken,
  and there is no second worker. It would add a write per document and a
  reconciliation path for rows left claimed by a crash.
- **A claim in an `IMMEDIATE` transaction**, as DESIGN.md's queue paragraph
  describes. Nothing is being reserved, so the write lock would protect
  nothing and would contend with the pipeline's own short writes.
- **Wait or Prefer New on overrun.** Both assume the overrun was a transient
  fault the next run will not hit. Here the opposite holds. Prefer New would
  additionally turn the scheduling interval into an unstated cap on cycle
  runtime; Wait would need a queue bound and a policy at that bound, for a
  trigger whose entire content is "look again".
- **Decorrelated or equal jitter.** Decorrelated completes marginally faster
  at the cost of more total work against the dependency — the wrong side of
  the trade for a local server. Equal jitter guarantees a minimum wait, which
  buys nothing once a document already worked this drain cannot be re-claimed
  until the next cycle.
- **Retrying immediately with a fixed delay.** Simpler, and wrong in the case
  that matters: a batch that fails together returns together and re-creates
  the failure.
- **Cross-document embed batching** — packing chunks from several documents
  into one request — as the throughput lever instead of concurrency. It is the
  better lever, but it muddies failure attribution (which document poisoned a
  200-chunk batch?) and needs an unpick path. Deferred until there is a
  measurement saying it is needed.
- **Bounding the queue by depth.** No mechanism can inflate it, so the bound
  would be decorative. The thing actually worth bounding — how long a new file
  waits behind a backlog — is bounded by batch size and claim composition
  instead.

## Consequences

The daemon indexes on its own, which is the point: `bsearch index` is removed
in the same change ([ADR 0012](0012-daemon-owns-the-index-writer.md)), and a
model change re-embeds the corpus with no command run.

An inference server that is off for a week costs nothing: no attempts burned,
no documents condemned, and indexing resumes by itself. The corresponding cost
is that a genuinely poisonous document takes five attempts and roughly half an
hour of wall-clock before it is called failed.

The daemon probes the embedding endpoint once per process even when there is
nothing to index, because the dimension sweep cannot be done without it. A
side effect is that a corpus with no documents reports `ready: true` with
everything at zero, where it previously reported not-ready. That is the more
accurate description — and without the probe, a dimension change on the
inference server under an unchanged model name would be a dead end that only a
manual file edit could clear.

`Snapshot()` exposes gate reasons, timestamps and counters but nothing renders
them yet; `bsearch status` is [#16](https://github.com/bcrisp4/bsearch/issues/16).
Until then a deferred queue is distinguishable from a stalled one only in the
log.

Known gaps, both deliberate: power state is a seam that always reports AC
until macOS detection lands in M7, so the battery policy is configurable but
unreachable; and the scan does not mark vanished files `deleted`, which
belongs with #13's delete events.

The concurrency-of-one decision is the one most likely to be revisited, and
the confidence on this record is medium because of it. Revisiting means adding
a claim set, not restructuring the loop.
