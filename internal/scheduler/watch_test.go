package scheduler

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/bcrisp4/bsearch/internal/config"
	"github.com/bcrisp4/bsearch/internal/discovery"
	"github.com/bcrisp4/bsearch/internal/domain"
)

// fakeWatcher is a hand-driven event source: a test pushes batches through
// feed and the scheduler sees them as it would see FSEvents callbacks.
type fakeWatcher struct {
	mu      sync.Mutex
	batches chan domain.WatchBatch
	err     error
	roots   []string
	calls   int
}

func newFakeWatcher() *fakeWatcher {
	return &fakeWatcher{batches: make(chan domain.WatchBatch, 8)}
}

func (w *fakeWatcher) Watch(_ context.Context, roots []string) (<-chan domain.WatchBatch, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.calls++
	w.roots = slices.Clone(roots)
	if w.err != nil {
		return nil, w.err
	}
	return w.batches, nil
}

func (w *fakeWatcher) subscribed() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return slices.Clone(w.roots)
}

// watchedScanner is a scanner with roots to watch and a result to report,
// wired for the reconcile path.
func watchedScanner(roots ...string) *fakeScanner {
	return &fakeScanner{roots: roots}
}

// run starts the scheduler and returns a stop function. Every watch test
// wants the same shape: run, drive events, assert, stop.
func run(t *testing.T, s *Scheduler) func() {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = s.Run(ctx)
	}()
	synctest.Wait()
	return func() {
		cancel()
		<-done
	}
}

// seedPending stages a closed window the way the watcher would, minus the
// wake hint and minus the overflow reporting.
//
// mergePending rather than enqueue, so a test that reads s.notify is reading
// its own subject rather than the staging. Tests that care about the wake
// itself call enqueue directly — see
// TestTheHandoffWakesTheSchedulerAndTheReconcileDoesNot.
func seedPending(s *Scheduler, paths ...string) {
	set := make(map[string]struct{}, len(paths))
	for _, p := range paths {
		set[p] = struct{}{}
	}
	s.mergePending(set) //nolint:errcheck // staging a window; the cap is exercised by its own test
}

// A burst of events inside one debounce window is one reconcile, with the
// paths deduplicated — the whole point of the window.
func TestWatchCoalescesABurstIntoOneReconcile(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		sc := watchedScanner("/root")
		sc.pathResult = discovery.Result{Discovered: 1}
		w := newFakeWatcher()
		s := newScheduler(t, newFakeQueue(), newFakeIndexer(), sc, func(o *Options) {
			o.Watcher = w
		})
		stop := run(t, s)
		defer stop()

		w.batches <- domain.WatchBatch{Paths: []string{"/root/b.md", "/root/a.md"}}
		w.batches <- domain.WatchBatch{Paths: []string{"/root/a.md"}}
		synctest.Wait()

		if got := sc.reconciled(); len(got) != 0 {
			t.Fatalf("reconciled %v before the window closed", got)
		}
		time.Sleep(defaultDebounce)
		synctest.Wait()

		got := sc.reconciled()
		if len(got) != 1 {
			t.Fatalf("reconciles = %d, want 1 for the whole burst", len(got))
		}
		if want := []string{"/root/a.md", "/root/b.md"}; !slices.Equal(got[0], want) {
			t.Errorf("reconciled %v, want %v (deduplicated and sorted)", got[0], want)
		}
		if snap := s.Snapshot(); !snap.Watching || snap.WatchRoots != 1 || snap.WatchReconciled != 1 {
			t.Errorf("Snapshot = %+v, want watching 1 root with 1 document queued", snap)
		}
		if got := w.subscribed(); !slices.Equal(got, []string{"/root"}) {
			t.Errorf("subscribed to %v, want the scanner's roots", got)
		}
	})
}

// The wake belongs to the handoff, not to the reconcile.
//
// On main, reconcile ran on the watch goroutine and notified when it found
// work. It now runs on the scheduler goroutine, which is the only reader of
// that channel — so notifying there would be waking itself, and the buffered
// token would survive to fire an entirely redundant cycle. The wake that
// matters is enqueue's, which is what makes a closed window reconcile
// promptly instead of waiting out a fifteen-minute interval.
func TestTheHandoffWakesTheSchedulerAndTheReconcileDoesNot(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		sc := watchedScanner("/root")
		sc.pathResult = discovery.Result{Deleted: 1}
		s := newScheduler(t, newFakeQueue(), newFakeIndexer(), sc, func(o *Options) {
			o.Watcher = newFakeWatcher()
		})
		s.mark(func(snap *Snapshot) { snap.Watching = true })

		// enqueue is the watch goroutine's half: it must wake the scheduler,
		// or nothing reconciles until the next interval.
		s.enqueue(map[string]struct{}{"/root/a.md": {}})
		select {
		case <-s.notify:
		default:
			t.Fatal("a closed window did not wake the scheduler")
		}

		// The reconcile itself must not, however productive it is.
		s.reconcilePending(t.Context())
		select {
		case <-s.notify:
			t.Error("the reconcile notified the goroutine it was already running on")
		default:
		}
		if snap := s.Snapshot(); snap.WatchDeleted != 1 {
			t.Errorf("WatchDeleted = %d, want 1", snap.WatchDeleted)
		}
	})
}

// An unreachable endpoint is probed on the recheck interval, not on every
// wake. Since ADR 0014 a closed debounce window wakes a cycle, so ordinary
// churn under a watched root would otherwise set the probe rate — a connect
// attempt every ten seconds against DESIGN.md's near-zero-CPU-when-idle.
func TestAFailedEndpointProbeIsNotRepeatedOnEveryWake(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		sc := watchedScanner("/root")
		ix := newFakeIndexer()
		ix.prepareErr = errors.New("connection refused")
		s := newScheduler(t, newFakeQueue(discoveredDoc("d_1")), ix, sc, nil)

		for range 5 {
			s.cycle(t.Context())
		}
		if got := ix.prepareCount(); got != 1 {
			t.Errorf("probes = %d over five cycles, want 1 until the recheck interval", got)
		}

		time.Sleep(deferRecheckInterval)
		s.cycle(t.Context())
		if got := ix.prepareCount(); got != 2 {
			t.Errorf("probes = %d after the recheck interval, want a second attempt", got)
		}

		// And the deferral is forgotten once the endpoint answers, or a
		// server that came back would stay locked out for an interval.
		ix.prepareErr = nil
		time.Sleep(deferRecheckInterval)
		s.cycle(t.Context())
		s.cycle(t.Context())
		if got := ix.prepareCount(); got != 4 {
			t.Errorf("probes = %d, want the deferral cleared by a healthy probe", got)
		}
	})
}

// An incomplete event stream is not reconciled path-by-path — the paths have
// stopped being the whole story, so the answer is a full walk.
func TestWatchRescanForcesAFullScan(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		sc := watchedScanner("/root")
		w := newFakeWatcher()
		s := newScheduler(t, newFakeQueue(), newFakeIndexer(), sc, func(o *Options) {
			o.Watcher = w
			// Long enough that only the forced request can produce a
			// second walk inside this test.
			o.ScanInterval = time.Hour
		})
		stop := run(t, s)
		defer stop()

		if sc.count() != 1 {
			t.Fatalf("scans = %d, want the startup scan", sc.count())
		}

		w.batches <- domain.WatchBatch{Paths: []string{"/root/a.md"}, Rescan: true}
		time.Sleep(defaultDebounce)
		synctest.Wait()

		if got := sc.reconciled(); len(got) != 0 {
			t.Errorf("reconciled %v, want the paths discarded in favour of a walk", got)
		}
		if sc.count() != 2 {
			t.Errorf("scans = %d, want a second walk forced by the rescan", sc.count())
		}
		if snap := s.Snapshot(); snap.WatchRescans != 1 {
			t.Errorf("WatchRescans = %d, want 1", snap.WatchRescans)
		}
	})
}

// A watcher that cannot start is not a failure to propagate: the daemon
// keeps indexing from the walk, and says why in the one place the user will
// look.
func TestWatchStartFailureLeavesScanOnly(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		sc := watchedScanner("/root")
		w := newFakeWatcher()
		w.err = errors.New("filesystem watching is only supported on macOS")
		s := newScheduler(t, newFakeQueue(), newFakeIndexer(), sc, func(o *Options) {
			o.Watcher = w
		})
		stop := run(t, s)
		defer stop()

		snap := s.Snapshot()
		if snap.Watching {
			t.Error("Watching is true after the subscription failed")
		}
		if snap.WatchReason != w.err.Error() {
			t.Errorf("WatchReason = %q, want the start error", snap.WatchReason)
		}
		if got := s.currentScanInterval(); got != defaultScanInterval {
			t.Errorf("scan interval = %v, want the tighter scan-only %v", got, defaultScanInterval)
		}
	})
}

// No roots is a different diagnosis from a watcher that would not start:
// nothing is wrong with the watcher, there is nothing to watch.
func TestWatchNoRootsIsItsOwnReason(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		sc := &fakeScanner{rootErrs: []discovery.PathError{{Path: "/gone", Err: discovery.ErrRootExcluded}}}
		w := newFakeWatcher()
		s := newScheduler(t, newFakeQueue(), newFakeIndexer(), sc, func(o *Options) {
			o.Watcher = w
		})
		stop := run(t, s)
		defer stop()

		if snap := s.Snapshot(); snap.Watching || snap.WatchReason != WatchOffNoRoots {
			t.Errorf("Snapshot = %+v, want not watching with the no-roots reason", snap)
		}
		if w.subscribed() != nil {
			t.Error("subscribed despite having no roots")
		}
	})
}

// The walk means two different things depending on the watcher, and the
// cadence follows it live rather than being latched at startup.
func TestScanIntervalFollowsTheWatcher(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		sc := watchedScanner("/root")
		w := newFakeWatcher()
		s := newScheduler(t, newFakeQueue(), newFakeIndexer(), sc, func(o *Options) {
			o.Watcher = w
		})
		if got := s.currentScanInterval(); got != defaultScanInterval {
			t.Errorf("before starting: interval = %v, want %v", got, defaultScanInterval)
		}

		stop := run(t, s)
		if got := s.currentScanInterval(); got != watchedScanInterval {
			t.Errorf("while watching: interval = %v, want the backstop %v", got, watchedScanInterval)
		}

		// The watcher stopping restores the tighter cadence on its own.
		stop()
		synctest.Wait()
		if got := s.currentScanInterval(); got != defaultScanInterval {
			t.Errorf("after stopping: interval = %v, want %v", got, defaultScanInterval)
		}
		if snap := s.Snapshot(); snap.Watching {
			t.Error("Watching survived shutdown")
		}
	})
}

// An explicit ScanInterval wins over both, so a test or an operator can pin
// the cadence regardless of the watcher.
func TestConfiguredScanIntervalWins(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := newScheduler(t, newFakeQueue(), newFakeIndexer(), watchedScanner("/root"), func(o *Options) {
			o.Watcher = newFakeWatcher()
			o.ScanInterval = 42 * time.Second
		})
		stop := run(t, s)
		defer stop()

		if got := s.currentScanInterval(); got != 42*time.Second {
			t.Errorf("interval = %v, want the configured 42s", got)
		}
	})
}

// A reconcile that fails is recorded, not fatal: the next window tries
// again, and the walk is still there underneath.
func TestWatchReconcileFailureIsRecorded(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		sc := watchedScanner("/root")
		sc.pathErr = errors.New("disk on fire")
		w := newFakeWatcher()
		s := newScheduler(t, newFakeQueue(), newFakeIndexer(), sc, func(o *Options) {
			o.Watcher = w
		})
		stop := run(t, s)
		defer stop()

		w.batches <- domain.WatchBatch{Paths: []string{"/root/a.md"}}
		time.Sleep(defaultDebounce)
		synctest.Wait()

		if snap := s.Snapshot(); snap.LastError != "disk on fire" {
			t.Errorf("LastError = %q, want the reconcile failure", snap.LastError)
		}
		if !s.Snapshot().Watching {
			t.Error("a failed reconcile stopped the watcher")
		}
	})
}

// Shutdown is one story: cancelling the context accounts for both
// goroutines before Run returns.
func TestWatchStopsWithTheScheduler(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := newScheduler(t, newFakeQueue(), newFakeIndexer(), watchedScanner("/root"), func(o *Options) {
			o.Watcher = newFakeWatcher()
		})
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan struct{})
		go func() {
			defer close(done)
			_ = s.Run(ctx)
		}()
		synctest.Wait()

		cancel()
		select {
		case <-done:
		case <-time.After(time.Minute):
			t.Fatal("Run did not return after cancellation")
		}
	})
}

// The window still open when the daemon stops is reconciled anyway. Not
// symmetry for its own sake: an edit dropped here is picked up by the next
// walk, but a deletion dropped here is picked up by nothing — only ScanPaths
// purges — so the last few seconds before a restart would otherwise be the
// one window whose deletions stay in the index for good.
func TestWatchFlushesTheOpenWindowOnShutdown(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		sc := watchedScanner("/root")
		sc.pathResult = discovery.Result{Deleted: 1}
		w := newFakeWatcher()
		s := newScheduler(t, newFakeQueue(), newFakeIndexer(), sc, func(o *Options) {
			o.Watcher = w
		})
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan struct{})
		go func() {
			defer close(done)
			_ = s.Run(ctx)
		}()
		synctest.Wait()

		// Deleted, then stopped well inside the debounce window.
		w.batches <- domain.WatchBatch{Paths: []string{"/root/gone.md"}}
		synctest.Wait()
		if got := sc.reconciled(); len(got) != 0 {
			t.Fatalf("reconciled %v before the window closed", got)
		}
		cancel()
		<-done

		got := sc.reconciled()
		if len(got) != 1 || !slices.Equal(got[0], []string{"/root/gone.md"}) {
			t.Fatalf("reconciled %v on shutdown, want the pending deletion", got)
		}
		if snap := s.Snapshot(); snap.WatchDeleted != 1 {
			t.Errorf("WatchDeleted = %d, want the flushed deletion counted", snap.WatchDeleted)
		}
	})
}

// A watcher delivering events that all fall outside every root is the one
// failure that looks like success: subscribed, healthy, and indexing nothing.
// The count is what makes it visible.
func TestWatchCountsIgnoredPaths(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		sc := watchedScanner("/root")
		sc.pathResult = discovery.Result{Ignored: 1}
		s := newScheduler(t, newFakeQueue(), newFakeIndexer(), sc, func(o *Options) {
			o.Watcher = newFakeWatcher()
		})

		s.mark(func(snap *Snapshot) { snap.Watching = true })
		seedPending(s, "/elsewhere/a.md")
		s.reconcilePending(t.Context())

		select {
		case <-s.notify:
			t.Error("a wholly ignored reconcile woke the drain")
		default:
		}
		if snap := s.Snapshot(); snap.WatchReconciled != 0 {
			t.Errorf("WatchReconciled = %d, want 0", snap.WatchReconciled)
		}
	})
}

// Per-path problems found by the watcher are as first-class as the walk's:
// a permission error reaches `bsearch status`, not just the log.
func TestWatchPathErrorsReachTheSnapshot(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		sc := watchedScanner("/root")
		sc.pathResult = discovery.Result{PathErrors: []discovery.PathError{
			{Path: "/root/locked.md", Err: errors.New("permission denied")},
		}}
		s := newScheduler(t, newFakeQueue(), newFakeIndexer(), sc, func(o *Options) {
			o.Watcher = newFakeWatcher()
		})

		s.mark(func(snap *Snapshot) { snap.Watching = true })
		seedPending(s, "/root/locked.md")
		s.reconcilePending(t.Context())

		snap := s.Snapshot()
		if snap.ScanErrs != 1 || len(snap.PathErrors) != 1 {
			t.Fatalf("Snapshot = %+v, want the watcher's path error surfaced", snap)
		}
		if snap.PathErrors[0].Path != "/root/locked.md" {
			t.Errorf("PathErrors[0] = %+v", snap.PathErrors[0])
		}
	})
}

// A walk asked for out of band answers for events that were dropped, and
// those events exist nowhere else — so if the walk fails, the request has to
// survive it. Losing it would mean waiting out a whole interval (fifteen
// minutes while watching) for changes nothing else can find.
func TestForcedScanSurvivesAFailedWalk(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		sc := watchedScanner("/root")
		sc.err = errors.New("disk on fire")
		w := newFakeWatcher()
		s := newScheduler(t, newFakeQueue(), newFakeIndexer(), sc, func(o *Options) {
			o.Watcher = w
			// Long enough that only a surviving request can produce
			// another walk inside this test.
			o.ScanInterval = time.Hour
		})
		stop := run(t, s)
		defer stop()

		before := sc.count()
		w.batches <- domain.WatchBatch{Rescan: true}
		time.Sleep(defaultDebounce)
		synctest.Wait()
		if sc.count() != before+1 {
			t.Fatalf("scans = %d, want the forced walk", sc.count())
		}

		// The walk failed, so the request must still be pending: the next
		// cycle takes it rather than waiting out the hour.
		if due, forced := s.scanDue(); !due || !forced {
			t.Errorf("scanDue() = (%v, %v), want the forced request re-armed", due, forced)
		}
	})
}

// The reconcile lands between documents, not after the batch. That placement
// is what bounds how long a purged file stays searchable to one document
// rather than to a batch of thirty-two, and it is the axis on which ADR 0014
// rejected draining only between batches.
func TestAClosedWindowIsReconciledBetweenDocuments(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		sc := watchedScanner("/root")
		q := newFakeQueue(discoveredDoc("d_1"), discoveredDoc("d_2"), discoveredDoc("d_3"))
		ix := newFakeIndexer()
		s := newScheduler(t, q, ix, sc, nil)

		var events []string
		ix.markIndexed = func(docID string) {
			events = append(events, "processed:"+docID)
			if docID == "d_1" {
				// Stands in for the watcher closing a window mid-drain.
				seedPending(s, "/root/saved.md")
			}
		}
		sc.onScanPaths = func() { events = append(events, "reconciled") }

		s.cycle(t.Context())

		want := []string{"processed:d_1", "reconciled", "processed:d_2", "processed:d_3"}
		if !slices.Equal(events, want) {
			t.Errorf("events = %v, want %v — the window waited for the batch to finish", events, want)
		}
	})
}

// Discovery is a cheap stage and runs on battery (DESIGN.md), and reconciling
// named paths is the same stat-and-hash a walk is. Below the power gate this
// would be the one thing a deferred cycle never reached, and an unplugged
// laptop would collect a day of deletions and purge none of them.
func TestAClosedWindowIsReconciledWhileIndexingIsDeferred(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		sc := watchedScanner("/root")
		sc.pathResult = discovery.Result{Deleted: 1}
		s := newScheduler(t, newFakeQueue(), newFakeIndexer(), sc, func(o *Options) {
			o.Power = func() config.PowerPolicy {
				return config.PowerPolicy{IndexInterval: config.Interval{Defer: true}}
			}
		})

		seedPending(s, "/root/gone.md")
		s.cycle(t.Context())

		if got := sc.reconciled(); len(got) != 1 || !slices.Equal(got[0], []string{"/root/gone.md"}) {
			t.Errorf("reconciled = %v, want the window reconciled despite the battery gate", got)
		}
		if snap := s.Snapshot(); !snap.Deferring || snap.WatchDeleted != 1 {
			t.Errorf("Snapshot = %+v, want deferring with the deletion still applied", snap)
		}
	})
}

// Overflow keeps what it holds and refuses what arrives, the opposite of the
// adapter's rule. The adapter collapses a batch the kernel has already called
// incomplete; this set is exact, and what discarding it would cost is its
// deletions — which a walk cannot recover, because a walk sees what exists.
func TestPendingOverflowKeepsTheHeldSetAndAsksForAWalk(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		sc := watchedScanner("/root")
		s := newScheduler(t, newFakeQueue(), newFakeIndexer(), sc, nil)

		held := make(map[string]struct{}, maxPendingReconcile)
		for i := range maxPendingReconcile {
			held["/root/"+itoa(i)+".md"] = struct{}{}
		}
		if _, ok := s.mergePending(held); !ok {
			t.Fatal("the first window was refused; the cap is not where the test thinks")
		}

		s.enqueue(map[string]struct{}{"/root/refused.md": {}})

		if snap := s.Snapshot(); snap.WatchRescans != 1 {
			t.Errorf("WatchRescans = %d, want 1 — an overflow is a reason to walk", snap.WatchRescans)
		}
		if due, forced := s.scanDue(); !due || !forced {
			t.Errorf("scanDue = (%v, %v), want a walk armed out of band", due, forced)
		}

		s.reconcilePending(t.Context())
		got := sc.reconciled()
		if len(got) != 1 || len(got[0]) != maxPendingReconcile {
			t.Fatalf("reconciled %d paths, want the %d held", len(got[0]), maxPendingReconcile)
		}
		if slices.Contains(got[0], "/root/refused.md") {
			t.Error("the refused window was kept and the held set dropped — backwards")
		}
	})
}

// Cancellation is the one error branch where something is still going to run,
// so the paths go back rather than away: Run's shutdown flush reconciles on a
// context cancellation cannot defeat, and a deletion dropped here would be
// lost rather than deferred.
func TestAReconcileCancelledMidFlightHandsItsPathsBack(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		sc := watchedScanner("/root")
		sc.pathErr = context.Canceled
		s := newScheduler(t, newFakeQueue(), newFakeIndexer(), sc, nil)

		ctx, cancel := context.WithCancel(t.Context())
		sc.onScanPaths = func() { cancel() }

		seedPending(s, "/root/gone.md")
		s.reconcilePending(ctx)

		if !s.hasPending() {
			t.Fatal("the window was dropped on cancellation instead of handed back")
		}

		// What Run does next. The flush runs on a detached context, so the
		// same paths are reconciled rather than lost.
		sc.pathErr, sc.onScanPaths = nil, nil
		sc.pathResult = discovery.Result{Deleted: 1}
		s.flushPending(ctx)

		got := sc.reconciled()
		if len(got) != 2 || !slices.Equal(got[1], []string{"/root/gone.md"}) {
			t.Fatalf("reconciled = %v, want the cancelled window retried by the flush", got)
		}
		if s.hasPending() {
			t.Error("the flush left the window pending")
		}
	})
}

// Both producers of PathErrors now run on one goroutine, and cycle fixes their
// order: the walk replaces, the reconcile appends. Two goroutines made that
// safe but not repeatable — a reconcile could append errors the walk it
// interleaved with was about to overwrite, and only sometimes.
func TestAWalkAndAReconcileBothReachTheSnapshotInOneCycle(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		sc := watchedScanner("/root")
		sc.result = discovery.Result{PathErrors: []discovery.PathError{
			{Path: "/root/walked.md", Err: errors.New("permission denied")},
		}}
		sc.pathResult = discovery.Result{PathErrors: []discovery.PathError{
			{Path: "/root/watched.md", Err: errors.New("permission denied")},
		}}
		s := newScheduler(t, newFakeQueue(), newFakeIndexer(), sc, nil)

		seedPending(s, "/root/watched.md")
		s.cycle(t.Context())

		snap := s.Snapshot()
		if snap.ScanErrs != 2 {
			t.Errorf("ScanErrs = %d, want 2 — the walk's and the reconcile's", snap.ScanErrs)
		}
		var paths []string
		for _, pe := range snap.PathErrors {
			paths = append(paths, pe.Path)
		}
		want := []string{"/root/walked.md", "/root/watched.md"}
		if !slices.Equal(paths, want) {
			t.Errorf("PathErrors = %v, want %v (walk replaces, then reconcile appends)", paths, want)
		}
	})
}

// Issue #63's second half. ClaimBatch hands out copies read once; a rename
// reconciled between documents moves a row out from under the copy still
// waiting its turn. Working from that copy sends the pipeline to a path the
// rename has since given to a different file — and the write that follows
// displaces that file's row and reverts the rename.
func TestProcessWorksFromTheRowAsItIsNowNotAsItWasClaimed(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		sc := watchedScanner("/root")
		q := newFakeQueue(discoveredDoc("d_1"), discoveredDoc("d_2"))
		ix := newFakeIndexer()
		s := newScheduler(t, q, ix, sc, nil)

		ix.markIndexed = func(docID string) {
			if docID != "d_1" {
				return
			}
			// The write discovery makes for a rename, landing between
			// documents while d_2's claimed copy still says /tmp/d_2.md.
			q.mu.Lock()
			q.docs["d_2"].Path = "/tmp/renamed.md"
			q.mu.Unlock()
		}

		s.cycle(t.Context())

		var got string
		for _, doc := range ix.processedDocs {
			if doc.ID == "d_2" {
				got = doc.Path
			}
		}
		if got != "/tmp/renamed.md" {
			t.Errorf("d_2 processed at %q, want /tmp/renamed.md — the claimed copy is stale", got)
		}
	})
}

// The cap bounds the set's size, not merely admission to it. A window is
// merged whole, and a debounce window can carry far more than one adapter
// batch — the adapter's 8192 bound is per batch, and at a 1s coalescing
// latency a 10s window holds about ten of them. Testing the size we already
// have would let a merge accepted just under the cap land at multiples of it.
func TestPendingCapBoundsTheSetNotJustAdmission(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := newScheduler(t, newFakeQueue(), newFakeIndexer(), watchedScanner("/root"), nil)

		// Start just under the cap, the way a busy window leaves it.
		under := make(map[string]struct{}, maxPendingReconcile-1)
		for i := range maxPendingReconcile - 1 {
			under["/root/"+itoa(i)+".md"] = struct{}{}
		}
		if _, ok := s.mergePending(under); !ok {
			t.Fatal("the first window was refused below the cap")
		}

		// A window that would take it past the cap is refused whole.
		oversized := make(map[string]struct{}, 1000)
		for i := range 1000 {
			oversized["/root/next-"+itoa(i)+".md"] = struct{}{}
		}
		if _, ok := s.mergePending(oversized); ok {
			t.Error("a window that would overshoot the cap was accepted")
		}
		if got := len(s.takePending()); got != maxPendingReconcile-1 {
			t.Errorf("held %d paths, want the set left at %d", got, maxPendingReconcile-1)
		}
	})
}

// Repeats cost nothing to absorb and must not be refused for it — which is the
// common case, not an edge one: a file written continuously reopens a window
// naming the same path, and a directory event arrives alongside events for the
// files inside it.
func TestPendingCapCountsOnlyNewlySeenPaths(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := newScheduler(t, newFakeQueue(), newFakeIndexer(), watchedScanner("/root"), nil)

		full := make(map[string]struct{}, maxPendingReconcile)
		for i := range maxPendingReconcile {
			full["/root/"+itoa(i)+".md"] = struct{}{}
		}
		if _, ok := s.mergePending(full); !ok {
			t.Fatal("the first window was refused")
		}

		// Exactly at the cap, but every path is already held.
		if _, ok := s.mergePending(map[string]struct{}{"/root/1.md": {}}); !ok {
			t.Error("a window of paths already held was refused at the cap")
		}
		if _, ok := s.mergePending(map[string]struct{}{"/root/new.md": {}}); ok {
			t.Error("a genuinely new path was accepted past the cap")
		}
	})
}
