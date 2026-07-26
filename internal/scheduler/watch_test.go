package scheduler

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"testing/synctest"
	"time"

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

// A burst of events inside one debounce window is one reconcile, with the
// paths deduplicated — the whole point of the window.
// seedPending stages a closed window the way the watcher would, minus the
// wake hint.
//
// Deliberately mergePending rather than enqueue: enqueue notifies
// unconditionally, and the tests below read s.notify as the observable of what
// the *reconcile* decided. Going through enqueue would poison exactly the
// assertion being made.
func seedPending(s *Scheduler, paths ...string) {
	set := make(map[string]struct{}, len(paths))
	for _, p := range paths {
		set[p] = struct{}{}
	}
	s.mergePending(set)
}

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

// A reconcile that changed nothing must not wake the drain: the daemon is
// idle for weeks at a time, and an editor touching a file it did not change
// is not a reason to probe an inference endpoint.
func TestWatchNotifiesOnlyWhenSomethingChanged(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		sc := watchedScanner("/root")
		sc.pathResult = discovery.Result{Unchanged: 1}
		w := newFakeWatcher()
		s := newScheduler(t, newFakeQueue(), newFakeIndexer(), sc, func(o *Options) {
			o.Watcher = w
		})

		// Not started: Notify's buffer is the observable, so the loop is
		// left out of it and the reconcile is driven directly.
		s.mark(func(snap *Snapshot) { snap.Watching = true })
		seedPending(s, "/root/a.md")
		s.reconcilePending(t.Context())
		select {
		case <-s.notify:
			t.Error("an unchanged reconcile woke the drain")
		default:
		}

		sc.pathResult = discovery.Result{Deleted: 1}
		seedPending(s, "/root/a.md")
		s.reconcilePending(t.Context())
		select {
		case <-s.notify:
		default:
			t.Error("a deletion did not wake the drain")
		}
		if snap := s.Snapshot(); snap.WatchDeleted != 1 {
			t.Errorf("WatchDeleted = %d, want 1", snap.WatchDeleted)
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
