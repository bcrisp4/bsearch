package scheduler

import (
	"context"
	"maps"
	"slices"
	"time"

	"github.com/bcrisp4/bsearch/internal/discovery"
)

const (
	// defaultDebounce is how long the watcher collects events before
	// reconciling them.
	//
	// It is a fixed window armed by the first event of a burst, not a quiet
	// period that resets on activity. A resetting window never closes on a
	// file being written continuously — a large export, a log — and that is
	// the file most worth noticing. The cost of closing early is reading a
	// half-written file, and that costs nothing: the content hash makes the
	// work idempotent, and the events for the rest of the write reopen a
	// window that catches the final state (DESIGN.md: ~10 s debounce).
	defaultDebounce = 10 * time.Second
	// watchedScanInterval is how often the filesystem is walked once the
	// watcher is running. The walk is then a backstop for events that were
	// missed — a stream overflow, a daemon that was not running — rather
	// than the mechanism that notices change, so it can afford to be three
	// times lazier and leave the battery alone.
	watchedScanInterval = 15 * time.Minute
	// maxLoggedWatchErrors caps per-reconcile path-error logging, for the
	// same reason maxLoggedPathErrors caps the scan's.
	maxLoggedWatchErrors = 5
	// shutdownFlush bounds the final reconcile run as the daemon stops. The
	// window's events are otherwise thrown away, and a deletion thrown away
	// is not deferred but lost: only ScanPaths purges, so no later walk can
	// notice the file is gone. Short, because a shutdown that hangs is worse
	// than a stale row — launchd's patience is finite.
	shutdownFlush = 5 * time.Second
	// maxPendingReconcile caps the paths the watcher may hand over faster
	// than the scheduler reconciles them. Eight times the adapter's
	// per-batch cap, because this one bounds a merged accumulation across
	// windows rather than a single held batch, and because merging dedups.
	//
	// Roughly 11 MB of paths at the cap, against a laptop where the
	// alternative is losing deletions. Reaching it at all takes a stall: the
	// scheduler drains between documents, between batches, and on every
	// wake, so the accumulation windows are one ProcessDocument, one walk, or
	// one endpoint probe — never an idle daemon, which Notify wakes.
	maxPendingReconcile = 65536
)

// Watch reasons. Why the daemon is falling back to the periodic scan, in
// the user's terms, rendered verbatim by `bsearch status`.
const (
	// WatchStarting means the watcher has not finished subscribing. The
	// window is normally imperceptible, but it covers a stat of every
	// include root — so on a cold or network-backed volume it is seconds
	// long, and that is exactly when someone runs `bsearch status`. Without
	// it that report reads "off, no reason given", which says the feature is
	// broken when it is merely starting.
	WatchStarting = "starting up"
	// WatchOffNoRoots means every configured root failed to resolve or was
	// excluded. Distinct from a watcher that could not start: there is
	// nothing wrong with the watcher, there is nothing to watch.
	WatchOffNoRoots = "no watchable paths — check paths.include and the exclude rules"
	// WatchOffStopped means the event stream ended on its own — a Watcher
	// closed its channel without being asked to. It should not happen, which
	// is exactly why it is worth being able to see. Currently unreachable
	// with the FSEvents adapter, which has no way to report a dead stream
	// (issue #65); a watcher that dies there stays reported as healthy.
	WatchOffStopped = "the filesystem watcher stopped"
)

// watch subscribes to filesystem events and reconciles them until ctx is
// done. It never fails the daemon: every way this can go wrong ends with
// scan-only indexing and a reason recorded where `bsearch status` will find
// it, because a periodic walk is a working mode and "nothing is being
// indexed" is the question status exists to answer.
//
// Roots are resolved once and subscribed to once. An include root created
// after the daemon started is not watched, and a stream that ends is not
// re-subscribed — both need a restart. Deliberate rather than overlooked:
// re-resolving on a timer is a retry loop to tune.
//
// The fallback below is conditional on something this package cannot
// enforce. Closing the batch channel is how a Watcher says its stream is
// gone, and only then does the loop exit, clear Watching, and let the walk
// tighten back to defaultScanInterval. The FSEvents adapter cannot send that
// signal today: fsnotify/fsevents never closes the channel it delivers on,
// so a stream that dies leaves this goroutine parked on a receive with
// `bsearch status` still reporting a healthy watcher on the lazy backstop
// cadence. Giving the watcher a liveness signal is issue #65; until it
// lands, treat the graceful-degradation path here as reachable only when a
// Watcher implementation closes the channel.
func (s *Scheduler) watch(ctx context.Context) {
	roots, rootErrs := s.scanner.Roots()
	for _, pe := range rootErrs {
		s.log.Warn("watch root unusable", "path", pe.Path, "error", pe.Err)
	}
	if len(roots) == 0 {
		s.log.Warn("filesystem watching is off", "reason", WatchOffNoRoots)
		s.mark(func(snap *Snapshot) { snap.WatchReason = WatchOffNoRoots })
		return
	}

	events, err := s.watcher.Watch(ctx, roots)
	if err != nil {
		s.log.Warn("filesystem watching is off; falling back to the periodic scan", "error", err)
		s.mark(func(snap *Snapshot) { snap.WatchReason = err.Error() })
		return
	}

	s.mark(func(snap *Snapshot) {
		snap.Watching = true
		snap.WatchRoots = len(roots)
		snap.WatchReason = ""
	})
	// Logged here rather than at startup: the walk's cadence depends on
	// whether this subscription succeeded, so this is the first moment the
	// number is true.
	s.log.Info("watching for filesystem changes",
		"roots", len(roots), "debounce", s.debounce, "scan_interval", s.currentScanInterval())
	defer s.mark(func(snap *Snapshot) {
		snap.Watching = false
		if snap.WatchReason == "" && ctx.Err() == nil {
			// Only when the stream ended under us. A cancelled context is the
			// daemon shutting down, and reporting that as an anomaly would
			// make the one reason that means "something is wrong" the reason
			// every clean stop produces.
			snap.WatchReason = WatchOffStopped
		}
	})

	var (
		timer  *time.Timer
		window <-chan time.Time
		paths  = map[string]struct{}{}
		rescan bool
	)
	defer func() {
		if timer != nil {
			timer.Stop()
		}
	}()

	for {
		select {
		case <-ctx.Done():
			s.closeWindow(paths, rescan)
			return
		case batch, ok := <-events:
			if !ok {
				s.closeWindow(paths, rescan)
				return
			}
			for _, path := range batch.Paths {
				paths[path] = struct{}{}
			}
			rescan = rescan || batch.Rescan
			if window == nil {
				timer = time.NewTimer(s.debounce)
				window = timer.C
			}
		case <-window:
			timer, window = nil, nil
			s.closeWindow(paths, rescan)
			paths, rescan = map[string]struct{}{}, false
		}
	}
}

// closeWindow hands one debounce window over to the scheduler goroutine.
//
// It runs on the watch goroutine and writes no catalog row (ADR 0014). Its
// whole job is to record that events were delivered and to make the paths
// visible to the one goroutine allowed to act on them. The shutdown arms of
// the loop above call it too, with no context and no timeout: merging into a
// map cannot be defeated by cancellation, so the machinery the old flush
// needed now lives in Run, next to the write.
func (s *Scheduler) closeWindow(paths map[string]struct{}, rescan bool) {
	if len(paths) == 0 && !rescan {
		return
	}

	// Recorded on delivery rather than on a successful reconcile: the field
	// answers "is the watcher being told anything", and `bsearch status`
	// reads a watcher that has never seen a change as a missing Full Disk
	// Access grant. A run of failing reconciles must not be made to look like
	// that — it has LastError to explain itself with. Since ADR 0014 that is
	// no longer only about failures: the reconcile is a whole document away
	// on another goroutine, so recording it there would make a merely busy
	// scheduler look like a missing grant too.
	s.mark(func(snap *Snapshot) { snap.LastWatchEvent = s.now() })

	if rescan {
		// The event stream lost events or a volume moved, so the path list
		// has stopped being the whole story and only a walk is honest. No
		// catalog write in this branch before ADR 0014 or after it, so it
		// stays here whole.
		s.log.Info("filesystem event stream is incomplete; scheduling a full scan",
			"paths_superseded", len(paths))
		s.mark(func(snap *Snapshot) { snap.WatchRescans++ })
		s.requestScan()
		s.Notify()
		return
	}
	s.enqueue(paths)
}

// enqueue merges a closed window into the set the scheduler reconciles, and
// hints that there is work. It never blocks on the scheduler: the watch
// goroutine's next duty is to be ready for the next FSEvents callback, and a
// reader that stalls there stalls the stream.
func (s *Scheduler) enqueue(paths map[string]struct{}) {
	if held, ok := s.mergePending(paths); !ok {
		s.refusePending(held, len(paths))
	}
	s.Notify()
}

// refusePending reports a set of paths that could not be held.
//
// Deliberately not the adapter's overflow rule, which collapses a batch and
// discards its paths: that batch is one the kernel has already declared
// incomplete, whereas this set is exact. What a refused set costs is its
// deletions — a walk buys back the creates and the edits, and nothing buys
// back the rest, because a walk sees what exists (#57). So the loss is logged,
// it counts, and it asks for the walk that recovers everything recoverable.
//
// held is the real size of the set that was kept, not the cap: an operator
// diagnosing an overshoot needs the number the process actually holds.
func (s *Scheduler) refusePending(held, refused int) {
	s.log.Warn("unreconciled paths are piling up; falling back to a full walk",
		"held", held, "refused", refused, "cap", maxPendingReconcile)
	s.mark(func(snap *Snapshot) { snap.WatchRescans++ })
	s.requestScan()
}

// mergePending folds paths into the pending set, reporting the resulting size
// and whether they were taken. Nothing is logged or written under the lock:
// the watch goroutine's latency must not become the status handler's problem.
func (s *Scheduler) mergePending(paths map[string]struct{}) (held int, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Counted before merging rather than after, or the cap would gate
	// admission instead of bounding size: one window can carry the set far
	// past it, since a debounce window merges every batch the adapter delivers
	// inside it and the adapter's own 8192 bound is per batch, not per window.
	// At a 1 s coalescing latency that is ~10 batches, so a merge accepted at
	// one under the cap could land at more than twice it.
	//
	// Newly-seen paths only. A window that repeats paths already held costs
	// nothing to absorb and must not be refused for it — which is the common
	// case, since a file being written continuously reopens a window naming
	// the same path.
	fresh := 0
	for p := range paths {
		if _, dup := s.pendingPaths[p]; !dup {
			fresh++
		}
	}
	if len(s.pendingPaths)+fresh > maxPendingReconcile {
		return len(s.pendingPaths), false
	}

	if s.pendingPaths == nil {
		s.pendingPaths = make(map[string]struct{}, len(paths))
	}
	maps.Copy(s.pendingPaths, paths)
	return len(s.pendingPaths), true
}

// takePending removes the pending set and returns it sorted, so a reconcile is
// reproducible from the log and tests are not hostage to map iteration order.
func (s *Scheduler) takePending() []string {
	s.mu.Lock()
	pending := s.pendingPaths
	s.pendingPaths = nil
	s.mu.Unlock()
	if len(pending) == 0 {
		return nil
	}
	return slices.Sorted(maps.Keys(pending))
}

// restorePending hands a taken set back, for the one caller that took it and
// could not use it.
//
// The refusal is reported rather than swallowed, and that matters more here
// than at the other call site. The watcher keeps closing windows while a long
// ScanPaths runs, and it merges them freely because takePending has just
// emptied the set — so by the time this restores an interrupted list, the set
// can be back at the cap. Dropping the list silently there would lose exactly
// the deletions the caller's own comment promises to defer rather than lose.
func (s *Scheduler) restorePending(list []string) {
	paths := make(map[string]struct{}, len(list))
	for _, p := range list {
		paths[p] = struct{}{}
	}
	if held, ok := s.mergePending(paths); !ok {
		s.refusePending(held, len(paths))
	}
}

// hasPending reports whether a reconcile is owed.
func (s *Scheduler) hasPending() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.pendingPaths) > 0
}

// reconcilePending turns the debounce windows the watcher has handed over
// into catalog writes.
//
// It runs on the scheduler goroutine and on no other (ADR 0014). That is what
// makes the per-document re-read in process exactly sufficient rather than
// merely narrowing, and what makes issues #62 and #63 unrepresentable rather
// than guarded against.
//
// For an edit the story is unchanged — every pipeline stage is an idempotent
// upsert, the property ADR 0011 relies on to make crash recovery a redo, so a
// reconcile that resets a document the pipeline has not reached yet is the
// right answer: the file changed underneath it.
//
// A delete is not idempotent the same way. "Only discovery creates catalog
// rows" (domain.ErrDocumentGone) is still worth keeping as a guard, but it is
// no longer the only thing standing between a purge and a resurrected
// document: nothing can write between this call and the end of the drain step
// that made it.
//
// The set is taken before the write rather than cleared after it. A reconcile
// that failed must not leave its paths behind for the next drain to retry —
// the failure path notifies, so that would turn a wedged store into a hot
// loop. The window is gone either way, and requestScan is the answer to it.
func (s *Scheduler) reconcilePending(ctx context.Context) {
	if ctx.Err() != nil {
		return
	}
	list := s.takePending()
	if len(list) == 0 {
		return
	}

	res, err := s.scanner.ScanPaths(ctx, list)

	// Recorded before the error is considered, because ScanPaths commits as it
	// goes rather than at the end: purge deletes rows and *then* adds to
	// res.Deleted, and every per-path problem met on the way is already in
	// res.PathErrors. Returning on the error without reading res would drop
	// both — a TCC-denied subtree found mid-reconcile would reach neither the
	// log nor `bsearch status`, against CLAUDE.md's rule that EPERM is never a
	// silent skip, and WatchDeleted would undercount permanently, since a
	// re-run deletes nothing for rows already purged.
	s.recordReconcile(list, res)

	if err != nil {
		if ctx.Err() != nil {
			// Handed back rather than dropped. The daemon is stopping and
			// Run's shutdown flush is about to reconcile on a context
			// cancellation cannot defeat, so this is the one error branch
			// where something is still going to run. A deletion dropped here
			// would be lost rather than deferred.
			//
			// The whole list goes back, not the unprocessed remainder, because
			// ScanPaths does not report where it stopped. Re-reconciling a
			// path already done is free — the change detection is idempotent —
			// but it does mean the flush repeats pass one from the start, and
			// deletions happen only in pass two. A flush that runs out of
			// budget mid-repeat therefore purges nothing, which is the
			// shutdownFlush bound's job to make unlikely rather than
			// impossible.
			s.restorePending(list)
			return
		}
		s.log.Error("reconciling changed paths failed", "error", err)
		s.mark(func(snap *Snapshot) { snap.LastError = err.Error() })
		// The window is gone either way, so fall back to the mechanism that
		// can rebuild most of it. A walk recovers the creates and edits; the
		// deletions in this window are lost until #57, which is the same
		// gap a rescan leaves and is worth no worse an answer.
		s.requestScan()
		s.Notify()
		return
	}
}

// recordReconcile logs and counts one ScanPaths result. Separate from
// reconcilePending so that both the success and the failure path go through
// it: the result is a live accumulator of work already committed, not a
// summary produced at the end.
func (s *Scheduler) recordReconcile(list []string, res discovery.Result) {
	sample := s.logPathErrors(res.PathErrors, "could not reconcile a changed path",
		"further watch path errors suppressed", maxLoggedWatchErrors)

	if res.Ignored > 0 && res.Ignored == len(list) {
		// Every path the watcher named was out of scope. Once is unremarkable
		// (a save inside an excluded tree); every time means the roots the
		// watcher subscribed to and the paths it reports are not spelled the
		// same way, and the daemon is watching attentively while indexing
		// nothing. Loud, because nothing else about it looks wrong.
		s.log.Warn("every watched path was out of scope — check that paths.include matches the on-disk spelling, including letter case",
			"paths", len(list))
	}

	s.log.Debug("changed paths reconciled",
		"paths", len(list),
		"discovered", res.Discovered,
		"renamed", res.Renamed,
		"deleted", res.Deleted,
		"unchanged", res.Unchanged,
		"ignored", res.Ignored,
		"path_errors", len(res.PathErrors))

	s.mark(func(snap *Snapshot) {
		snap.WatchReconciled += res.Discovered
		snap.WatchDeleted += res.Deleted
		// Merged into the scan's list rather than replacing it: both describe
		// paths the daemon could not read, and a permission error found by
		// the watcher is exactly as first-class as one found by the walk
		// (CLAUDE.md: EPERM is never a silent skip). Without this the
		// watcher-only errors — a declined deletion, a mid-window lstat
		// failure — would reach the log and nothing else.
		snap.ScanErrs += len(res.PathErrors)
		snap.PathErrors = appendCapped(snap.PathErrors, sample, maxLoggedPathErrors)
		// Cumulative, and reported, because the wholly-ignored warning above
		// stopped being a reliable signal once a reconcile could span several
		// windows: one in-scope path anywhere in the set silences it for the
		// whole set. A running total does not care how the windows were
		// grouped — a watcher subscribed to a mis-spelled root shows up as
		// "reconciled 0, ignored 12431", which is unmissable however the
		// scheduler happened to batch its wakes.
		snap.WatchIgnored += res.Ignored
	})
	// No Notify here. This runs on the scheduler goroutine, which is the only
	// reader of that channel, so it would be waking itself: the drain that
	// follows in the same cycle already sees the rows, and the buffered token
	// would survive to fire one entirely redundant cycle immediately after.
	// On battery that is worse than redundant — the reconcile sits above the
	// defer gate, so the woken cycle wakes only to hit the same gate again.
}

// requestScan makes the next cycle walk the filesystem whatever the clock
// says. Consumed by scanDue, so a request that arrives while a scan is
// already running survives it and gets its own.
func (s *Scheduler) requestScan() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.forceScan = true
}
