package scheduler

import (
	"context"
	"maps"
	"slices"
	"time"
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
	// WatchOffStopped means the event stream ended on its own. It should not
	// happen, which is exactly why it is worth being able to see.
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
// re-resolving on a timer is a retry loop to tune, and the failure it would
// cover already degrades safely (the walk tightens back to
// defaultScanInterval and `bsearch status` says the watcher stopped) rather
// than silently. Worth revisiting if it turns out to happen.
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
			s.flush(ctx, paths, rescan)
			return
		case batch, ok := <-events:
			if !ok {
				s.flush(ctx, paths, rescan)
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
			s.reconcile(ctx, paths, rescan)
			paths, rescan = map[string]struct{}{}, false
		}
	}
}

// flush reconciles a debounce window that was still open when the watcher
// stopped, on a context of its own so cancellation does not defeat it.
//
// Worth the extra machinery because the window's contents are not equally
// recoverable. An edit dropped here is picked up by the next walk; a deletion
// dropped here is never picked up at all, since only ScanPaths purges and a
// walk sees what exists. So the last ten seconds before a daemon restart
// would otherwise be the one window whose deletions stay in the index for
// good.
func (s *Scheduler) flush(ctx context.Context, paths map[string]struct{}, rescan bool) {
	if len(paths) == 0 && !rescan {
		return
	}
	// Detached from ctx: it is almost certainly already cancelled — that is
	// why we are here — and a reconcile on a cancelled context does nothing.
	// Bounded so a slow store cannot hold shutdown open.
	flushCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownFlush)
	defer cancel()
	s.log.Debug("reconciling the last events before shutting down", "paths", len(paths))
	s.reconcile(flushCtx, paths, rescan)
}

// reconcile turns one debounce window's worth of events into catalog writes.
//
// It runs on the watch goroutine, concurrently with a drain, and that is
// deliberate: a note saved while a bulk backlog is being embedded must not
// wait hours for it. Reconciling is stat, hash and upsert with no network in
// it, and the SQLite writer is one connection with a busy timeout.
//
// For an edit that is the whole story — every pipeline stage is an
// idempotent upsert, the property ADR 0011 already relies on to make crash
// recovery a redo, so a reconcile that resets a document mid-pipeline is the
// right answer rather than a race lost: the file changed underneath it.
//
// A delete is not idempotent the same way, and rests on a storage invariant
// instead: only discovery creates catalog rows (domain.ErrDocumentGone).
// Without it the purge here would be undone by the pipeline's next write,
// putting a document whose file is gone back into the index permanently.
func (s *Scheduler) reconcile(ctx context.Context, paths map[string]struct{}, rescan bool) {
	if rescan {
		// The event stream lost events or a volume moved, so the path list
		// has stopped being the whole story and only a walk is honest.
		s.log.Info("filesystem event stream is incomplete; scheduling a full scan",
			"paths_superseded", len(paths))
		s.mark(func(snap *Snapshot) {
			snap.WatchRescans++
			snap.LastWatchEvent = s.now()
		})
		s.requestScan()
		s.Notify()
		return
	}
	if len(paths) == 0 {
		return
	}

	// Recorded on delivery rather than on a successful reconcile: the field
	// answers "is the watcher being told anything", and `bsearch status`
	// reads a watcher that has never seen a change as a missing Full Disk
	// Access grant. A run of failing reconciles must not be made to look
	// like that — it has LastError to explain itself with.
	s.mark(func(snap *Snapshot) { snap.LastWatchEvent = s.now() })

	// Sorted so a reconcile is reproducible from the log and tests are not
	// hostage to map iteration order.
	list := slices.Sorted(maps.Keys(paths))
	res, err := s.scanner.ScanPaths(ctx, list)
	if err != nil {
		if ctx.Err() != nil {
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
	})
	if res.Discovered > 0 || res.Deleted > 0 {
		s.Notify()
	}
}

// requestScan makes the next cycle walk the filesystem whatever the clock
// says. Consumed by scanDue, so a request that arrives while a scan is
// already running survives it and gets its own.
func (s *Scheduler) requestScan() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.forceScan = true
}
