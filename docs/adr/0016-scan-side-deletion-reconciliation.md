# 0016 — Scan-side deletion reconciliation: absence needs positive evidence

- **Status:** accepted
- **Date:** 2026-07-26
- **Confidence:** medium

## Context

A walk sees what exists. `discovery.Scan` enumerates the filesystem and
upserts what it finds, so nothing has ever looked at the catalog and asked
which rows the filesystem no longer has. [ADR
0013](0013-fsevents-watcher-and-event-driven-reconcile.md) shipped the
event-driven half of deletion and named three cases it deliberately declines,
all left to [#57](https://github.com/bcrisp4/bsearch/issues/57): a file deleted
while the daemon was not running, a deletion lost to an event stream that
overflowed, and a subtree that vanished without its own directory event. Until
now all three stay `indexed` forever and keep returning search hits pointing at
nothing.

The reason it was split off rather than done alongside the event path is that
the two halves have different evidence available, and this is the half that can
destroy the corpus. The asymmetry is the whole design constraint: removing a row
is milliseconds; rebuilding what it referenced is battery-gated local inference
over the corpus, which DESIGN.md puts at potentially days. A partial scan must
therefore never read as mass deletion — not a revoked Full Disk Access grant,
not an unmounted volume, not a root that failed to resolve.

## Decision

**We will reconcile absences as a second phase of `Scan`, enumerating the
catalog and stat-ing each row, and we will delete only on positive evidence —
declining, visibly, wherever the walk cannot vouch for what it found.**

**Phase 2 is private to `Scan`, not a fourth `Scanner` method.** It takes the
walk's own coverage, so "only a completed walk licenses a deletion" is
structural rather than a convention someone must remember. `scheduler.Scanner`
is unchanged, and `noteOrphanProducers` already arms the orphan sweep off
`Result.Deleted`.

**Rows are deleted individually (`DeleteByPaths`), never by prefix.**
`DeleteByPathPrefix` exists because an event names one path and can neither say
whether it was a file or a directory nor enumerate what was under it. Here the
catalog *is* that enumeration: `rm -rf dir` arrives as N rows that each
independently returned `ENOENT`, so per-row deletes close the subtree gap with
per-row evidence and keep the blast radius of a wrong answer at one row.

**Coverage is built by the walk, never reverse-engineered afterwards.** Only a
directory the walk was *refused* is a prefix it cannot vouch for. Inferring
that set from the accumulated path errors instead conflates three unlike
things — a refused directory, a file that stat'ed fine but would not open, and
a root deliberately excluded by config — and the last two are not opaque
subtrees at all. Root resolution likewise reports explicitly whether it
succeeded, because its failures are spelled in the *configured* path while the
rows beneath a symlinked root are spelled in the *resolved* one.

**Five guards, and the order is load-bearing.** Per row, first to answer wins:

1. outside every root, or deny-listed → `Pruned`, unless a root failed to
   resolve this pass, when it is `Ignored`. Scope is asked first because it is
   a question about the configuration, not the filesystem: an unreadable
   directory or an absent volume has no bearing on it, and asked later a
   de-scoped path under an unplugged drive could never be pruned at all;
2. beneath a prefix the walk cannot vouch for → `Unverified`, **without a
   stat**, so a denied subtree costs no syscalls and reports once rather than
   once per row;
3. stat succeeds **and what is there is still a regular indexable file** →
   kept. A path replaced in place by a directory or a symlink still stats, but
   the walk will never index it again, so its chunks and vectors would answer
   searches for content that is not there;
4. stat fails with anything but `ENOENT` → `Unverified`, never deleted.
   `EACCES`, `EIO`, `ESTALE` and `ELOOP` all mean "cannot tell", and "cannot
   tell" is never spent on a delete (ADR 0013's rule, unchanged). Counted, not
   merely skipped, and reported once per directory so a large subtree cannot
   flood the status sample;
5. `ENOENT` → deleted.

**Every decline is counted and reported.** `Unverified`, `Ignored` and the
named unmounted volumes all reach `bsearch status`, including the whole-corpus
decline when no root resolves at all — an early return there was invisible in
every number the daemon reports. A decline no counter records is the silent
skip this codebase keeps legislating against.

**A symlink the walk skipped is a prefix it did not look through.** `os.Lstat`
declines to follow only the *final* component, so a directory replaced by a
link leaves rows beneath it that stat perfectly well through it — and vanish
the moment its target does, which for a link to an external volume is every
unplug. Skipped links therefore join the coverage set. They decline rows
*beneath* them and not the row *at* them, which is what lets a **file**
replaced by a link still be deleted: the walk can never refresh that row, so
its chunks would answer searches for content that is not there.

**The orphan sweep stays at queue scope for prunes as well as deletions.**
Escalating a prune to full scope was tried and reverted: the sweep's scope is a
property of the *pass*, not of the content, so one pruned row in a cycle that
also saw four hundred deletions collected those four hundred too — destroying
the free-restore window the queue scope exists to hold open. The cost of not
escalating is that pruned-but-indexed content keeps its vectors until the next
daemon start, where they still occupy KNN result slots (search reads *k*
neighbours and then drops the ones no document references), so a query whose
nearest hits were all pruned comes back short. That is pre-existing for
deletions and widened by pruning; the real fix is over-fetching in the vector
search or a content-scoped sweep, and it has its own issue.

**A path out of bsearch's configured scope has vanished as far as the index is
concerned, so it is pruned** — counted apart from a deleted file, because one
is the corpus changing and the other is the config changing, and a user
watching a number drop needs to know which. Scope deletion is licensed by
configuration rather than by the filesystem, so it is **suspended wholesale
for any pass in which a configured root failed to resolve**. `canonicalRoot`
records the failure against the *configured* spelling while rows under a
symlinked root carry the *resolved* one, so guard 1 cannot cover them and a
transient `EvalSymlinks` error would otherwise prune everything beneath.

**Include roots are canonicalised for case, not just for symlinks.** On a
case-insensitive volume `~/Notes` and `~/notes` open the same directory but are
different strings, and this design compares path strings in four places that
must agree: the roots, the catalog rows a walk writes from those roots, the
mount table, and the paths FSEvents delivers. A root typed in the wrong case
indexes perfectly well while matching no mount at all, which disables the
unplugged-drive protection outright and silently. `pathutil.CanonicalCase`
resolves it once, where the root is resolved, so every comparison downstream
stays byte-wise — making `Within` case-insensitive instead would be wrong on a
case-sensitive volume, where two spellings really are two directories. It also
closes [#64](https://github.com/bcrisp4/bsearch/issues/64), the watch root that
subscribes and matches nothing.

**Unmounted volumes are recognised from the mount table, not inferred.**
`syscall.Getfsstat` (MNT_NOWAIT — the waiting form blocks on an unresponsive
network mount, which is one of the situations this exists to notice) behind a
`mounts_darwin.go` / `mounts_other.go` seam, matching the `dataless` seam. The
mount points seen inside the include roots are persisted through a new narrow
`domain.ScanState` port, and any remembered mount absent from the live table
becomes a decline-prefix. The set **accumulates** and is pruned only when no
catalog row remains beneath an entry: a snapshot of the current table would be
useless for the case that matters, a drive detached before the daemon started.

**The memory is learned before it is acted on, and an absent memory declines
deletions.** Writing the observed mounts after the deletions they were meant to
inform protects nothing on the pass that needed it, so the union happens first
and the forget-what-is-empty step last — while the "have we ever observed
anything" flag is read *before* the learning, or the very first pass would both
learn the mounted volumes and act on them, which is precisely the pre-existing
catalog with an unplugged drive.

A set that has never been written is not an empty set; it is ignorance, the one
state where deleting would be deleting blind. What stands down is the
destructive step alone — `ENOENT` is counted rather than acted on — not the
whole corpus: on a fresh install every row was written by the same walk, so
nothing would be deleted anyway, and declining wholesale there fires the
product's loudest alarm with no reason to attach to it. What is recorded is the
**observation**, not the value, so a mount table that is legitimately empty
(every non-macOS build) still flips the flag rather than declining for ever.

Both halves of the memory are advisory, re-derivable cache: a read or write
failure is recorded and the pass declines, exactly as an unreadable mount table
does, and never fails the Scan — a fatal error there would stop `LastScan`
advancing, leaving every cycle due for a full walk. A failed *read* also
suppresses the write, because `known` is then empty from the failure rather
than from the world, and overwriting the record of an unplugged volume with a
record that it does not exist is how a transient database error becomes a
corpus wipe on the following pass.

**A cleanly-walked root that reaches zero files purges.** Mount evidence
covers the dangerous reading of an empty root — a volume unmounted whose mount
point survived as an empty directory — so what is left is a folder genuinely
emptied, where purging is correct.

**Declines are reported.** `Unverified` and the unmounted volume names reach
`bsearch status` and a Warn log. This is the outcome that reads as health
everywhere else: the scan succeeded, no errors need be present, nothing was
deleted — and deletions are quietly not being noticed.

## Alternatives considered

- **A per-scan generation column** — stamp every row the walk visited, sweep
  the unstamped. Rejected: the walk's cheap path is a size/mtime check with no
  write behind it, so stamping means a row write per file per scan — ~100k WAL
  writes every fifteen minutes on a battery, to learn what `lstat` reports for
  free. Enumeration plus stat is strictly cheaper than the walk that precedes
  it.
- **Diffing an in-memory set of walked paths against the catalog.** Rejected:
  needs corpus-sized live strings *and* the same catalog enumeration anyway,
  and its evidence is weaker — "the walk didn't visit it" cannot tell a
  deletion from a directory the walk never descended into.
- **A separate exported `ReconcileDeleted` on the `Scanner` port**, scheduled
  independently. More seams, and a walk without deletions becomes possible.
  Rejected: the pass would know only what `lstat` tells it, so it could not see
  that the walk hit `EPERM` on three of four hundred subdirectories, and the
  safety gate would degrade to a corpus-wide "did the scan reach anything" —
  exactly what lets a partial failure read as mass deletion.
- **Climbing to the highest missing ancestor and prefix-deleting from there.**
  This record's first draft. Rejected: it multiplies every wrong answer by the
  size of the subtree above the row, and the ancestor chain is licensed by
  `ENOENT`s on directories the catalog never had a row for — an ejected volume
  under `/Volumes` climbs to a live `/Volumes` and takes the whole corpus.
- **A grace period / tombstone** — `missing_since` on `documents`, purge after
  N hours of continuous absence, hidden from search meanwhile. General, no
  platform code, and it would absorb TCC blips too. Rejected as the primary
  mechanism: it does not solve the case that motivated the work, since a drive
  detached for longer than the window is still purged and fully re-indexed; and
  it reintroduces a soft-delete state ADR 0013 rejected plus a predicate on the
  search join. Not foreclosed — it composes with mount evidence if a general
  net is later wanted.
- **Declining whenever a root reaches zero files.** Cheap insurance, and this
  record's position before mount evidence existed. Rejected as redundant: it
  cannot tell an unmounted volume from an emptied folder, which is precisely
  what the mount table answers, and it would permanently strand rows under a
  root the user deliberately emptied.
- **Purging out-of-scope rows unconditionally**, with no root-resolution
  suspension. Simpler to state. Rejected: it makes a transient `EvalSymlinks`
  failure on one symlinked root indistinguishable from that root being removed
  from the config.

## Consequences

Deletion follows the source in both directions for the first time. A file
removed while the daemon was down leaves the index at the next walk rather than
never; `rm -rf` on a subtree no longer needs a second deletion with the daemon
running to take effect; and a corpus that has drifted from `paths.include` can
be brought back into line by editing the config, which previously stranded rows
permanently.

Every decline the reconcile makes carries a reason in the user's terms, and
those reasons are deliberately not `PathError`s: status renders those under
"Unreadable paths", whose documented meaning is a missing Full Disk Access
grant, and filing a corrupt SQLite value there would send the reader to System
Settings for a database problem.

The cost is one `lstat` and one deny-list match per catalog row per walk. The
match is the more expensive of the two at around 3 µs, and memoising it on the
parent directory does not help — the port is one opaque predicate over a whole
path, so a miss still runs it in full and a miss is the common case. At the
~100k-document scale DESIGN.md targets that is a few hundred milliseconds per
walk, against a walk that is already doing more. Catalog rows are indexable
text files, a small subset of what the walk already stats, and the names are in
the kernel cache the walk just warmed — a few hundred milliseconds warm at the
100k-row target, one to two seconds cold, against a walk that is doing strictly
more work. Memory is bounded by the page size rather than the corpus, which is
what makes the cursor approach preferable to the set-diff.

Three declines are deliberate and permanent until something else clears them: a
subtree the walk could not read, a volume that is not mounted, and rows outside
every root during a pass where a root would not resolve. All three are counted
and named in `bsearch status`. A volume retired for good therefore protects its
rows forever; clearing that is expected to fall out of automatic scheduled
pruning rather than needing a command, and is not urgent — the rows cost index
space and stale hits, not correctness.

The third of those is a **pass-wide** switch with no distinction between a
transient and a permanent resolution failure, so one include root with a
persistently unreadable ancestor suspends scope pruning for every other root
too, indefinitely. Deliberate — the failure is spelled in a path that cannot be
matched against the rows at risk, so a narrower rule would be a guess — but it
means the "editing `paths.include` takes effect" behaviour can be switched off
by an unrelated misconfiguration. `ScanIgnored` is what makes that visible
rather than mysterious.

**Residual risks, accepted and unmitigated.** All three share a mitigation:
external volumes belong in `paths.include` as their own root, where a root that
will not resolve declines without needing the mount table at all.

- **A volume never observed while mounted.** The remembered set only protects
  what it has seen, so a drive already detached the first time this mechanism
  ran — an upgrade from a version without it — is indistinguishable from a
  deleted subtree. The first-pass decline gives one scan interval of grace, and
  no more.
- **A missing include root is treated as ordinary.** Only a *symlinked* root
  that will not resolve suspends scope pruning, because only its rows are
  spelled somewhere this pass cannot name. That is deliberate — the remedy
  above tells users to give an unplugged drive its own root, which then ENOENTs
  on every scan, and suspending pruning for that would switch off "editing
  paths.include takes effect" permanently — but it does mean a root removed by
  a typo prunes rather than declines.
- **A different filesystem mounted at a remembered path.** Entries are matched
  by mount point alone, so swapping a backup drive for another with the same
  volume name reads as "the volume is back" and its rows are then judged
  against the new disk's contents. The identifiers `statfs` offers cheaply —
  `f_fsid`, `f_mntfromname` — are derived from the device number and change
  across ordinary remounts, so keying on them would decline every legitimate
  reconnection instead, which is worse. A stable answer needs the volume UUID
  (`getattrlist`/`ATTR_VOL_UUID`): [#88](https://github.com/bcrisp4/bsearch/issues/88).
- **A different filesystem mounted at a remembered path** — see above; tracked
  as [#88](https://github.com/bcrisp4/bsearch/issues/88).

**Load-bearing assumption, only partly verified:** on macOS a TCC denial
surfaces as `EPERM`, not `ENOENT`. Guard 4 rests on it, and if a release ever
returned `ENOENT` for a path under a protected directory the per-row rule would
lose that row. What has been checked is the ordinary-permissions analogue — a
directory at mode 0 yields `EACCES` on both the walk and the stat, covered by
tests — not a live Full Disk Access revocation, which is a manual System
Settings action. Guard 1 limits the damage either way: the walk's own failure
on the directory declines the whole subtree before any row is stat-ed. Worth
confirming against a real revocation when [#14](https://github.com/bcrisp4/bsearch/issues/14)
takes up TCC onboarding.

**New contract on `Options.Excluded`:** it must answer for descendants, not
just for the directory a rule names. The walk prunes at the directory and never
asks about what is inside; the deletion pass asks about catalog rows, which are
files. `config.ExcludeSet.Match` is prefix-based and satisfies this, and the
requirement is now documented on the field.

`domain.DocumentStore` grows `ListPaths` and `DeleteByPaths`, and
`domain.ScanState` is new. `ListPaths` must enumerate unread (NULL
`content_hash`) rows: a filter there would make denied and dataless rows
unpurgeable forever with nothing to signal it, so a store test asserts the
shared seed's NULL-hash row comes back. Storing mount points in `meta` stretches
that table's stated purpose (pipeline metadata) a little; it is a small
key/value fact and did not seem worth a table.

The mount table is read once per walk, and a failure to read it declines the
whole pass rather than proceeding blind. Off macOS `mountPoints` returns
nothing, so an unmounted volume under an include root reads as a deletion
there; every other guard still applies, and a Linux port owes a
`/proc/self/mountinfo` reader.
