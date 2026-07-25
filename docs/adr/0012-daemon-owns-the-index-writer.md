# 0012 — The daemon is the only index writer

- **Status:** proposed
- **Date:** 2026-07-24
- **Confidence:** high

## Context

Until now the daemon was a reader. It opened the index with `OpenExisting`,
kept no warm writer connection, and re-stat'd the file on every request
because `bsearch index` — a separate process — could replace it underneath.
Indexing was that command's job.

Issue [#12](https://github.com/bcrisp4/bsearch/issues/12) gives the daemon a
scheduler, which makes it a writer. That forces a decision about `bsearch
index`, because two processes writing one SQLite file is a design, not an
accident: WAL and `busy_timeout` keep it from corrupting, but the vector
store's orphan-rowid guard fails loudly if one process deletes a document's
chunks while the other is writing vectors keyed on them.

DESIGN.md already anticipated the answer: "`bsearch index` is the exception
and the transitional one: it is a writer, and it disappears once the daemon
owns discovery and the queue, leaving `reindex` as the way to force the work."

## Decision

We will remove `bsearch index`, and make the daemon the only thing that writes
the index.

**The daemon creates and migrates the database.** The scheduler opens it with
`Open` rather than `OpenExisting`, so `bsearch serve` no longer needs anything
to have run first. The query path keeps its existing lazily-opened read handle
and its inode-swap check, unchanged: that machinery is still correct, and
still needed once `bsearch reindex` (#24) can replace the file.

**Two handles in one process, not one.** The scheduler holds its own
`*sqlite.DB` for the process lifetime; requests keep using the daemon's
reference-counted read handle. Sharing one handle would mean threading the
writer through the request path's swap-and-retire logic for no benefit; two
pools on one WAL database is exactly the case SQLite handles well.

**Failing to open the index does not stop the daemon.** Every way the indexing
side can fail to start — an unopenable database, a schema from a newer build,
a missing embedding model — is logged and survived, leaving `/v1/status`
answering. A LaunchAgent that exits non-zero is a crash-loop with nothing able
to explain itself, and "why is nothing being indexed" is the question status
exists to answer.

**The advice changes with the command.** Every message that told the user to
run `bsearch index` now says the daemon indexes in the background, or names
the restart that re-reads configuration. A message naming a command that does
not exist is worse than no message.

## Alternatives considered

- **Keep `bsearch index` and guard it with a lock.** The daemon already holds
  a `flock` for single-instance; exporting it would let `bsearch index` refuse
  while the daemon runs. It works, but it preserves a command that DESIGN.md
  says is transitional, adds a failure mode users would meet routinely
  ("indexing is disabled while the daemon runs"), and leaves two code paths to
  keep in step for the entire time it survives.
- **Keep it unguarded** and rely on idempotent upserts plus `busy_timeout`.
  Rejected: the orphan-rowid guard turns an interleaving into a loud failure,
  which is the correct behaviour for a guard and the wrong thing to design
  towards on purpose.
- **Make the daemon read-only and keep indexing in a separate process** driven
  by launchd on a schedule. It preserves the current reader/writer split, but
  a separate process cannot be woken by an FSEvents stream the daemon owns
  (#13), pays a cold start per run, and puts the queue's state in one process
  while the thing that knows about change lives in another.
- **Share a single `*sqlite.DB` between the scheduler and the query path.**
  Fewer connections, but it entangles the writer with the request path's
  handle retirement, and the query path's read-only opener exists precisely so
  a broken index is reported rather than repaired mid-request.

## Consequences

There is one writer and one place that decides what the index contains. The
daemon works standalone: install it, start it, and search — no separate
indexing step in the documentation, and nothing for a user to forget to run.

The cost is that indexing now requires the daemon. Someone who wants a
one-shot rebuild has no command until `bsearch reindex`
([#24](https://github.com/bcrisp4/bsearch/issues/24)) lands — which the stale
sweep in [ADR 0011](0011-indexing-queue-dispatch-and-retry.md) makes less
pressing, since a configuration change now repairs itself; `reindex` becomes
"force it now" rather than the only path to a rebuild.

This strengthens rather than supersedes
[ADR 0010](0010-cli-as-socket-only-client.md): its "`bsearch index` and
`bsearch eval` keep direct database access" clause now applies to `eval`
alone, which writes only its own per-corpus database in a work directory and
never touches the index.

`bsearch serve` now creates a database where before it would not, so a
mistyped `--db` leaves an empty file behind rather than reporting that nothing
is there. Accepted: the path is reported in `/v1/status`, and an empty index
is cheap.

Tests that used `bsearch index` as a fixture now start a daemon and wait for
it to index, which is slower and closer to what users do. One test needed a
stop-and-restart helper to exercise a configuration change against an existing
index — the daemon tests run in-process (#54), so only one daemon can be up at
a time.
