package scheduler

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/bcrisp4/bsearch/internal/config"
	"github.com/bcrisp4/bsearch/internal/discovery"
	"github.com/bcrisp4/bsearch/internal/domain"
	"github.com/bcrisp4/bsearch/internal/pipeline"
)

// The doubles below are hand-written rather than generated, matching the rest
// of the repo: each records what it was asked to do and exposes a knob per
// failure mode, so a test reads as a scenario rather than as setup.

// fakeQueue is an in-memory catalog. Content rows are keyed by hash; the claim
// applies the same dispatch predicate as the SQLite implementation, whose own
// behaviour is covered by the adapter's tests.
type fakeQueue struct {
	mu       sync.Mutex
	contents map[string]*domain.Content
	// order fixes claim order so assertions are stable.
	order []string
	// paths is what GetWork resolves each hash to. Absent means the default
	// /tmp/<hash>.md scheme, so most tests never think about paths at all.
	paths map[string]string
	// pathGone marks a hash whose row exists but which no live document
	// references any more — the file was deleted while its claim was in
	// flight. GetWork reports ErrContentGone for it, as the store does.
	pathGone map[string]bool

	claimErr     error
	rescheduleEr error
	resetErr     error

	reschedules []rescheduled
	failures    []failure
	sweeps      []map[string]string
	// sweepMoves is what ResetStale reports moved, per call.
	sweepMoves []int
	claims     int

	// orphanSweeps counts SweepOrphans calls, sweepScopes records each
	// call's scope, and orphanSweepErr fails them. The sweep itself is
	// modelled, not stubbed: it removes pathGone rows the way the store
	// does, so tests can observe orphans actually leaving the queue.
	orphanSweeps   int
	sweepScopes    []domain.SweepScope
	orphanSweepErr error

	// getWorks records every GetWork call, so a test can assert an orphan
	// was never handed toward the pipeline at all.
	getWorks []string
}

type rescheduled struct {
	hash     string
	attempts int
	at       time.Time
	reason   string
}

type failure struct {
	hash   string
	reason string
}

func newFakeQueue(contents ...domain.Content) *fakeQueue {
	q := &fakeQueue{
		contents: map[string]*domain.Content{},
		paths:    map[string]string{},
		pathGone: map[string]bool{},
	}
	for _, c := range contents {
		content := c
		q.contents[content.Hash] = &content
		q.order = append(q.order, content.Hash)
	}
	return q
}

// add stages a new content row mid-test, the way discovery would.
func (q *fakeQueue) add(c domain.Content) {
	q.mu.Lock()
	defer q.mu.Unlock()
	content := c
	q.contents[content.Hash] = &content
	q.order = append(q.order, content.Hash)
}

// setState mutates a row the way the real pipeline's own writes would.
func (q *fakeQueue) setState(hash string, state domain.ContentState) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.contents[hash].State = state
}

// setPath re-points a hash at a new path, standing in for the documents-table
// write a rename produces.
func (q *fakeQueue) setPath(hash, path string) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.paths[hash] = path
}

// GetWork is the scheduler's re-read. It returns the *current* row and the
// path resolved *now*, so a test that mutates q between the claim and the
// process is standing in for a reconcile landing between contents.
func (q *fakeQueue) GetWork(_ context.Context, hash string) (domain.WorkItem, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.getWorks = append(q.getWorks, hash)
	c, ok := q.contents[hash]
	if !ok || q.pathGone[hash] {
		return domain.WorkItem{}, fmt.Errorf("get work %s: %w", hash, domain.ErrContentGone)
	}
	path, ok := q.paths[hash]
	if !ok {
		path = "/tmp/" + hash + ".md"
	}
	return domain.WorkItem{Content: *c, Path: path}, nil
}

func (q *fakeQueue) ClaimBatch(_ context.Context, now time.Time, limit int) ([]domain.Content, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.claims++
	if q.claimErr != nil {
		return nil, q.claimErr
	}
	var out []domain.Content
	for _, hash := range q.order {
		if len(out) == limit {
			break
		}
		c := q.contents[hash]
		if c.State.Terminal() {
			continue
		}
		if !c.NextRetryAt.IsZero() && c.NextRetryAt.After(now) {
			continue
		}
		out = append(out, *c)
	}
	return out, nil
}

func (q *fakeQueue) Reschedule(_ context.Context, hash string, attempts int, at time.Time, reason string) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.rescheduleEr != nil {
		return q.rescheduleEr
	}
	q.reschedules = append(q.reschedules, rescheduled{hash, attempts, at, reason})
	c, ok := q.contents[hash]
	if !ok {
		return errors.New("no such content")
	}
	c.Attempts = attempts
	c.NextRetryAt = at
	c.LastError = reason
	return nil
}

func (q *fakeQueue) ResetStale(_ context.Context, current map[string]string) (int, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.resetErr != nil {
		return 0, q.resetErr
	}
	q.sweeps = append(q.sweeps, current)
	moved := 0
	if len(q.sweepMoves) > 0 {
		moved = q.sweepMoves[0]
		q.sweepMoves = q.sweepMoves[1:]
	}
	return moved, nil
}

func (q *fakeQueue) MarkFailed(_ context.Context, hash, reason string) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.failures = append(q.failures, failure{hash, reason})
	if c, ok := q.contents[hash]; ok {
		c.State = domain.ContentStateFailed
	}
	return nil
}

// SweepOrphans models the store's sweep (review 3653204158): every pathGone
// row is an orphan, and the scope decides whether terminal orphans go too.
// Removing the rows is the point — it is what lets a test observe that a
// swept orphan is never claimed again.
func (q *fakeQueue) SweepOrphans(_ context.Context, scope domain.SweepScope) (int, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.orphanSweeps++
	q.sweepScopes = append(q.sweepScopes, scope)
	if q.orphanSweepErr != nil {
		return 0, q.orphanSweepErr
	}
	removed := 0
	kept := q.order[:0]
	for _, hash := range q.order {
		c := q.contents[hash]
		if q.pathGone[hash] && (scope == domain.SweepScopeAll || !c.State.Terminal()) {
			delete(q.contents, hash)
			delete(q.pathGone, hash)
			removed++
			continue
		}
		kept = append(kept, hash)
	}
	q.order = kept
	return removed, nil
}

func (q *fakeQueue) orphanSweepCount() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.orphanSweeps
}

func (q *fakeQueue) scopes() []domain.SweepScope {
	q.mu.Lock()
	defer q.mu.Unlock()
	return slices.Clone(q.sweepScopes)
}

// orphan marks a hash as referenced by no live document — the state a
// deletion or an edit leaves the old content in.
func (q *fakeQueue) orphan(hash string) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.pathGone[hash] = true
}

// has reports whether the content row is still in the catalog.
func (q *fakeQueue) has(hash string) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	_, ok := q.contents[hash]
	return ok
}

func (q *fakeQueue) getWorkHashes() []string {
	q.mu.Lock()
	defer q.mu.Unlock()
	return slices.Clone(q.getWorks)
}

func (q *fakeQueue) snapshotContent(hash string) domain.Content {
	q.mu.Lock()
	defer q.mu.Unlock()
	return *q.contents[hash]
}

func (q *fakeQueue) counts() (reschedules, failures, sweeps, claims int) {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.reschedules), len(q.failures), len(q.sweeps), q.claims
}

// fakeIndexer stands in for *pipeline.Indexer. prepareErr simulates the
// inference endpoint being down; outcomes maps a content hash to what
// processing it produces.
type fakeIndexer struct {
	mu sync.Mutex

	dims       int
	prepareErr error
	// prepareErrAfter makes the endpoint fail from the Nth Prepare onwards,
	// for "the server went down mid-batch" scenarios.
	prepareErrAfter int
	prepares        int

	outcomes map[string]pipeline.Result
	// processErr is a fatal machinery failure.
	processErr error
	processed  []string
	// processedItems is what ProcessContent was actually handed, which since
	// ADR 0014 is the re-read row and path rather than the copy the claim
	// produced.
	processedItems []domain.WorkItem
	// markIndexed lets a test mutate the queue as the real pipeline would.
	markIndexed func(hash string)
}

func newFakeIndexer() *fakeIndexer {
	return &fakeIndexer{dims: 768, outcomes: map[string]pipeline.Result{}, prepareErrAfter: -1}
}

func (f *fakeIndexer) Prepare(context.Context) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.prepares++
	if f.prepareErr != nil {
		return 0, f.prepareErr
	}
	if f.prepareErrAfter >= 0 && f.prepares > f.prepareErrAfter {
		return 0, errors.New("connection refused")
	}
	return f.dims, nil
}

func (f *fakeIndexer) StageVersions(dims int) map[string]string {
	return map[string]string{
		domain.StageChunker:       "c1",
		domain.StageEmbedding:     "fp1",
		domain.StageEmbeddingDims: itoa(dims),
		domain.StageVecMetric:     domain.VectorMetric,
	}
}

func (f *fakeIndexer) ProcessContent(_ context.Context, item domain.WorkItem, _ map[string]string) (pipeline.Result, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.processed = append(f.processed, item.Content.Hash)
	f.processedItems = append(f.processedItems, item)
	if f.processErr != nil {
		return pipeline.Result{}, f.processErr
	}
	res, ok := f.outcomes[item.Content.Hash]
	if !ok {
		res = pipeline.Result{Outcome: pipeline.OutcomeIndexed}
	}
	if res.Outcome == pipeline.OutcomeIndexed && f.markIndexed != nil {
		f.markIndexed(item.Content.Hash)
	}
	return res, nil
}

func (f *fakeIndexer) prepareCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.prepares
}

func (f *fakeIndexer) processedHashes() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.processed...)
}

// fakeScanner records scans and can block, so a test can hold a cycle open.
type fakeScanner struct {
	mu      sync.Mutex
	scans   int
	result  discovery.Result
	err     error
	entered chan struct{}
	release chan struct{}
	// pinAt is which scan (1-based) the entered/release pair holds open; zero
	// means the first. Only one scan is ever pinned, so the cycles either side
	// of it run to completion and can be counted.
	pinAt  int
	pinned bool

	// The watcher-driven half: what Roots reports, and what each ScanPaths
	// call was given and returns.
	roots        []string
	rootErrs     []discovery.PathError
	pathResult   discovery.Result
	pathErr      error
	scannedPaths [][]string
	// onScanPaths runs inside ScanPaths, so a test can order a reconcile
	// against content processing, or cancel mid-reconcile.
	onScanPaths func()
}

func (s *fakeScanner) ScanPaths(_ context.Context, paths []string) (discovery.Result, error) {
	s.mu.Lock()
	s.scannedPaths = append(s.scannedPaths, slices.Clone(paths))
	hook, res, err := s.onScanPaths, s.pathResult, s.pathErr
	s.mu.Unlock()
	// Called outside the lock so a hook may reach back into the scheduler.
	if hook != nil {
		hook()
	}
	return res, err
}

func (s *fakeScanner) Roots() ([]string, []discovery.PathError) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.roots, s.rootErrs
}

// reconciled returns the path lists ScanPaths has been given.
func (s *fakeScanner) reconciled() [][]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.scannedPaths)
}

func (s *fakeScanner) Scan(ctx context.Context) (discovery.Result, error) {
	s.mu.Lock()
	s.scans++
	var entered, release chan struct{}
	pinAt := s.pinAt
	if pinAt == 0 {
		pinAt = 1
	}
	// The channels stay addressable so a test can still close release.
	if !s.pinned && s.scans == pinAt {
		s.pinned = true
		entered, release = s.entered, s.release
	}
	s.mu.Unlock()
	if entered != nil {
		entered <- struct{}{}
	}
	if release != nil {
		select {
		case <-release:
		case <-ctx.Done():
			return discovery.Result{}, ctx.Err()
		}
	}
	return s.result, s.err
}

func (s *fakeScanner) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.scans
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

func discoveredContent(hash string) domain.Content {
	return domain.Content{Hash: hash, State: domain.ContentStateDiscovered}
}

// newScheduler builds a Scheduler over the given doubles with test-friendly
// defaults: no jitter randomness, a silent logger, and AC power.
func newScheduler(t *testing.T, q *fakeQueue, ix *fakeIndexer, sc *fakeScanner, tweak func(*Options)) *Scheduler {
	t.Helper()
	opts := Options{
		Queue:   q,
		Indexer: ix,
		Scanner: sc,
		Logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
		// Deterministic backoff: the maximum of the window, so assertions can
		// name an exact due time. fullJitter's own randomness is tested in
		// backoff_test.go.
		Rand: func(n int64) int64 { return n - 1 },
	}
	if tweak != nil {
		tweak(&opts)
	}
	s, err := New(opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s
}

func TestNewRequiresCollaborators(t *testing.T) {
	_, err := New(Options{})
	if err == nil {
		t.Error("New with no collaborators succeeded, want an error")
	}
}

// The first cycle must run immediately. A daemon started after the machine
// was off for a week should not sit idle waiting for a tick.
func TestFirstCycleRunsImmediately(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		q := newFakeQueue(discoveredContent("c_1"))
		ix := newFakeIndexer()
		ix.markIndexed = func(hash string) { q.setState(hash, domain.ContentStateIndexed) }
		sc := &fakeScanner{}
		s := newScheduler(t, q, ix, sc, nil)

		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		go func() { _ = s.Run(ctx) }()
		synctest.Wait()

		if got := ix.processedHashes(); len(got) != 1 || got[0] != "c_1" {
			t.Errorf("processed %v, want [c_1] before any interval elapsed", got)
		}
		if sc.count() != 1 {
			t.Errorf("scans = %d, want 1 on the first cycle", sc.count())
		}
	})
}

// Content discovered between cycles is indexed on the next one, with no
// command run — the whole point of the daemon.
func TestNewWorkIsIndexedOnTheNextCycle(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		q := newFakeQueue()
		ix := newFakeIndexer()
		ix.markIndexed = func(hash string) { q.setState(hash, domain.ContentStateIndexed) }
		s := newScheduler(t, q, ix, &fakeScanner{}, func(o *Options) {
			o.Power = func() config.PowerPolicy {
				return config.PowerPolicy{IndexInterval: config.Interval{Duration: 5 * time.Minute}}
			}
		})

		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		go func() { _ = s.Run(ctx) }()
		synctest.Wait()

		if got := ix.processedHashes(); len(got) != 0 {
			t.Fatalf("processed %v on an empty queue", got)
		}

		q.add(discoveredContent("c_new"))

		time.Sleep(5 * time.Minute)
		synctest.Wait()

		if got := ix.processedHashes(); len(got) != 1 || got[0] != "c_new" {
			t.Errorf("processed %v after one interval, want [c_new]", got)
		}
	})
}

// A hint must wake the loop well before the interval would. Losing one costs
// latency, not correctness, which is exactly why it is allowed to be a
// non-blocking send.
func TestNotifyWakesTheLoopBeforeTheInterval(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		q := newFakeQueue()
		ix := newFakeIndexer()
		ix.markIndexed = func(hash string) { q.setState(hash, domain.ContentStateIndexed) }
		s := newScheduler(t, q, ix, &fakeScanner{}, func(o *Options) {
			o.Power = func() config.PowerPolicy {
				return config.PowerPolicy{IndexInterval: config.Interval{Duration: time.Hour}}
			}
		})

		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		go func() { _ = s.Run(ctx) }()
		synctest.Wait()

		q.add(discoveredContent("c_new"))

		s.Notify()
		synctest.Wait()

		if got := ix.processedHashes(); len(got) != 1 || got[0] != "c_new" {
			t.Errorf("processed %v after Notify, want [c_new] without waiting an hour", got)
		}
	})
}

// Notify must never block, whether or not the loop is listening — a watcher
// callback that stalls on a full channel would back up the event stream.
func TestNotifyNeverBlocks(t *testing.T) {
	s := newScheduler(t, newFakeQueue(), newFakeIndexer(), &fakeScanner{}, nil)
	for range 100 {
		s.Notify() // no reader at all; the second onwards find the buffer full
	}
}

// Prefer Old: a trigger that arrives while a cycle is in flight is shed, not
// queued. The alternative would start a second drain contending for the same
// rows and the same inference server.
func TestTriggersDuringACycleAreShed(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		sc := &fakeScanner{entered: make(chan struct{}), release: make(chan struct{})}
		q := newFakeQueue()
		ix := newFakeIndexer()
		s := newScheduler(t, q, ix, sc, func(o *Options) {
			o.Power = func() config.PowerPolicy {
				return config.PowerPolicy{IndexInterval: config.Interval{Duration: time.Minute}}
			}
		})

		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		go func() { _ = s.Run(ctx) }()

		<-sc.entered // the first cycle is pinned inside Scan

		// Ten intervals' worth of triggers arrive while the cycle is stuck.
		for range 10 {
			s.Notify()
		}
		time.Sleep(10 * time.Minute)
		synctest.Wait()

		if n := sc.count(); n != 1 {
			t.Errorf("scans = %d while one cycle was in flight, want 1", n)
		}

		close(sc.release)
		synctest.Wait()

		// Exactly one deferred trigger survives — the buffer holds one, and
		// the rest coalesce into it.
		if n := sc.count(); n > 2 {
			t.Errorf("scans = %d after release, want the shed triggers not to have queued up", n)
		}
	})
}

// A cycle that outlives its interval must not be followed immediately by
// another. The timer necessarily fires while such a cycle is running, and if
// that tick survived the Stop/Reset the next select would return at once —
// Prefer Old would collapse into back-to-back cycles, which on a large
// backlog is a hot loop against the inference server.
//
// (Go 1.23 made timer channels unbuffered, which is why Run does not drain
// timer.C between Stop and Reset. This is what holds that reasoning to
// account rather than trusting the release notes.)
func TestACycleThatOutlivesItsIntervalStillWaitsAFullInterval(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const interval = time.Minute
		// The second cycle is the one held open. The first must complete so
		// that the timer is armed for a full interval; a Notify then wakes the
		// loop early, leaving that armed timer to expire mid-cycle. Pinning the
		// first cycle would prove nothing — the initial timer has already
		// fired by then and is not re-armed until the cycle returns.
		sc := &fakeScanner{pinAt: 2, entered: make(chan struct{}), release: make(chan struct{})}
		q := newFakeQueue()
		ix := newFakeIndexer()
		s := newScheduler(t, q, ix, sc, func(o *Options) {
			o.ScanInterval = time.Nanosecond // every cycle scans
			o.Power = func() config.PowerPolicy {
				return config.PowerPolicy{IndexInterval: config.Interval{Duration: interval}}
			}
		})

		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		go func() { _ = s.Run(ctx) }()
		synctest.Wait() // first cycle done; timer armed for a full interval

		s.Notify()   // wake early, with the timer still armed
		<-sc.entered // second cycle pinned inside Scan

		// Five intervals elapse while the cycle is stuck, so the armed timer
		// has certainly expired mid-cycle.
		time.Sleep(5 * interval)
		close(sc.release)
		synctest.Wait()

		if got := sc.count(); got != 2 {
			t.Fatalf("scans = %d immediately after the long cycle finished, want 2 — a tick that expired mid-cycle fired an extra one", got)
		}

		// Just short of the interval: still nothing.
		time.Sleep(interval - time.Second)
		synctest.Wait()
		if got := sc.count(); got != 2 {
			t.Errorf("scans = %d before the interval elapsed, want 2", got)
		}

		time.Sleep(2 * time.Second)
		synctest.Wait()
		if got := sc.count(); got != 3 {
			t.Errorf("scans = %d after the interval elapsed, want 3", got)
		}
	})
}

// An endpoint that is switched off must cost no content anything. This is
// the difference between a daemon you can leave running and one that quietly
// condemns your corpus while the inference server is down.
func TestEmbedderOutageBurnsNoAttempts(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		q := newFakeQueue(discoveredContent("c_1"), discoveredContent("c_2"))
		ix := newFakeIndexer()
		ix.prepareErr = errors.New("connection refused")
		s := newScheduler(t, q, ix, &fakeScanner{}, nil)

		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		go func() { _ = s.Run(ctx) }()
		synctest.Wait()

		if got := ix.processedHashes(); len(got) != 0 {
			t.Errorf("processed %v with the endpoint down, want nothing attempted", got)
		}
		reschedules, failures, _, _ := q.counts()
		if reschedules != 0 || failures != 0 {
			t.Errorf("reschedules = %d, failures = %d during an outage, want 0 and 0",
				reschedules, failures)
		}
		for _, hash := range []string{"c_1", "c_2"} {
			if c := q.snapshotContent(hash); c.Attempts != 0 || c.State != domain.ContentStateDiscovered {
				t.Errorf("%s = state %q attempts %d, want it untouched", hash, c.State, c.Attempts)
			}
		}
		if got := s.Snapshot().Gate; got != GateEmbedder {
			t.Errorf("gate = %q, want %q", got, GateEmbedder)
		}
	})
}

// Indexing must resume by itself once the endpoint comes back. Requiring a
// restart would make every inference-server hiccup a manual chore.
func TestIndexingResumesAfterAnOutage(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		q := newFakeQueue(discoveredContent("c_1"))
		ix := newFakeIndexer()
		ix.prepareErr = errors.New("connection refused")
		ix.markIndexed = func(hash string) { q.setState(hash, domain.ContentStateIndexed) }
		s := newScheduler(t, q, ix, &fakeScanner{}, func(o *Options) {
			o.Power = func() config.PowerPolicy {
				return config.PowerPolicy{IndexInterval: config.Interval{Duration: 5 * time.Minute}}
			}
		})

		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		go func() { _ = s.Run(ctx) }()
		synctest.Wait()

		if got := ix.processedHashes(); len(got) != 0 {
			t.Fatalf("processed %v while down", got)
		}

		ix.mu.Lock()
		ix.prepareErr = nil
		ix.mu.Unlock()

		time.Sleep(5 * time.Minute)
		synctest.Wait()

		if got := ix.processedHashes(); len(got) != 1 || got[0] != "c_1" {
			t.Errorf("processed %v after recovery, want [c_1]", got)
		}
		if got := s.Snapshot().Gate; got != GateIdle {
			t.Errorf("gate = %q after recovery, want %q", got, GateIdle)
		}
	})
}

// A transient failure on a healthy endpoint is the content's problem, so it
// costs an attempt and comes back later rather than immediately.
func TestTransientFailureOnAHealthyEndpointBacksOff(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		q := newFakeQueue(discoveredContent("c_1"))
		ix := newFakeIndexer()
		ix.outcomes["c_1"] = pipeline.Result{
			Outcome: pipeline.OutcomeTransient,
			Err:     errors.New("embed /tmp/c_1.md: unexpected EOF"),
		}
		s := newScheduler(t, q, ix, &fakeScanner{}, nil)

		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		start := time.Now()
		go func() { _ = s.Run(ctx) }()
		synctest.Wait()

		q.mu.Lock()
		got := append([]rescheduled(nil), q.reschedules...)
		q.mu.Unlock()

		if len(got) != 1 {
			t.Fatalf("reschedules = %d, want 1", len(got))
		}
		if got[0].attempts != 1 {
			t.Errorf("attempts = %d, want 1", got[0].attempts)
		}
		// Rand is pinned to the top of the window, so the first retry is due
		// one base interval out.
		if want := start.Add(defaultBackoffBase - 1); !got[0].at.Equal(want) {
			t.Errorf("next retry at %v, want %v", got[0].at, want)
		}
		if got[0].reason == "" {
			t.Error("reschedule recorded no reason; status would have nothing to show")
		}
		if c := q.snapshotContent("c_1"); c.State == domain.ContentStateFailed {
			t.Error("a single transient failure marked the content failed")
		}
	})
}

// Content in backoff must not be retried before it is due — otherwise the
// backoff is decorative and a flaky file is hammered.
func TestContentInBackoffIsNotRetriedEarly(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		q := newFakeQueue(discoveredContent("c_1"))
		ix := newFakeIndexer()
		ix.outcomes["c_1"] = pipeline.Result{
			Outcome: pipeline.OutcomeTransient,
			Err:     errors.New("embed: unexpected EOF"),
		}
		s := newScheduler(t, q, ix, &fakeScanner{}, func(o *Options) {
			o.Power = func() config.PowerPolicy {
				return config.PowerPolicy{IndexInterval: config.Interval{Duration: time.Second}}
			}
		})

		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		go func() { _ = s.Run(ctx) }()
		synctest.Wait()

		// Many cycles pass, all well inside the first backoff window.
		time.Sleep(defaultBackoffBase / 2)
		synctest.Wait()

		if got := ix.processedHashes(); len(got) != 1 {
			t.Errorf("processed %v; the content was retried before its backoff expired", got)
		}
	})
}

// After the attempt cap the content is called failed, with the reason and
// the attempt count preserved so status can explain it.
func TestRepeatedTransientFailuresEventuallyGiveUp(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		q := newFakeQueue(discoveredContent("c_1"))
		ix := newFakeIndexer()
		ix.outcomes["c_1"] = pipeline.Result{
			Outcome: pipeline.OutcomeTransient,
			Err:     errors.New("embed: unexpected EOF"),
		}
		s := newScheduler(t, q, ix, &fakeScanner{}, func(o *Options) {
			o.Power = func() config.PowerPolicy {
				return config.PowerPolicy{IndexInterval: config.Interval{Duration: time.Minute}}
			}
			o.MaxAttempts = 3
		})

		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		go func() { _ = s.Run(ctx) }()

		// Well past the capped window, so every retry has come due.
		time.Sleep(2 * time.Hour)
		synctest.Wait()

		q.mu.Lock()
		failures := append([]failure(nil), q.failures...)
		reschedules := append([]rescheduled(nil), q.reschedules...)
		q.mu.Unlock()

		// Two retries, then a third write recording the final count before
		// the row goes terminal.
		if len(reschedules) != 3 {
			t.Fatalf("reschedules = %d, want 3 (two retries plus the final count)", len(reschedules))
		}
		for i, got := range reschedules {
			if want := i + 1; got.attempts != want {
				t.Errorf("reschedule[%d] attempts = %d, want %d", i, got.attempts, want)
			}
		}
		if len(failures) != 1 {
			t.Fatalf("failures = %d, want 1", len(failures))
		}
		if failures[0].hash != "c_1" {
			t.Errorf("failed %q, want c_1", failures[0].hash)
		}
		// The reason names the attempt count, and the column has to agree
		// with it — otherwise a failed row reads as barely tried.
		if !strings.Contains(failures[0].reason, "after 3 attempts") {
			t.Errorf("failure reason = %q, want it to name the attempt count", failures[0].reason)
		}
		if got := q.snapshotContent("c_1").Attempts; got != 3 {
			t.Errorf("recorded attempts = %d, want 3 to match the reason", got)
		}
	})
}

// If the endpoint dies part-way through a batch, the content that happened
// to be in flight must not be charged for it.
func TestAnOutageMidBatchChargesNothing(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		q := newFakeQueue(discoveredContent("c_1"))
		ix := newFakeIndexer()
		// The gate probe passes; the re-probe after the failure does not.
		ix.prepareErrAfter = 1
		ix.outcomes["c_1"] = pipeline.Result{
			Outcome: pipeline.OutcomeTransient,
			Err:     errors.New("embed: connection reset"),
		}
		s := newScheduler(t, q, ix, &fakeScanner{}, nil)

		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		go func() { _ = s.Run(ctx) }()
		synctest.Wait()

		reschedules, failures, _, _ := q.counts()
		if reschedules != 0 || failures != 0 {
			t.Errorf("reschedules = %d, failures = %d, want 0 and 0 — the outage is not the content's fault",
				reschedules, failures)
		}
		if c := q.snapshotContent("c_1"); c.Attempts != 0 {
			t.Errorf("attempts = %d, want 0", c.Attempts)
		}
		if got := s.Snapshot().Gate; got != GateEmbedder {
			t.Errorf("gate = %q, want %q", got, GateEmbedder)
		}
	})
}

// Content the pipeline judges permanently unindexable is already recorded
// as failed; the scheduler must not also charge it an attempt or retry it.
func TestPermanentFailureIsNotRetried(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		q := newFakeQueue(discoveredContent("c_1"))
		ix := newFakeIndexer()
		ix.outcomes["c_1"] = pipeline.Result{
			Outcome: pipeline.OutcomeFailed,
			Err:     errors.New("normalize: undecodable"),
		}
		s := newScheduler(t, q, ix, &fakeScanner{}, nil)

		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		go func() { _ = s.Run(ctx) }()
		synctest.Wait()

		reschedules, failures, _, _ := q.counts()
		if reschedules != 0 {
			t.Errorf("reschedules = %d for a permanent failure, want 0", reschedules)
		}
		// The pipeline already wrote the failed row; the scheduler must not
		// write a second one over the top with a different reason.
		if failures != 0 {
			t.Errorf("scheduler recorded %d failures over the pipeline's, want 0", failures)
		}
		if got := s.Snapshot().Failed; got != 1 {
			t.Errorf("snapshot Failed = %d, want 1", got)
		}
	})
}

// A file that is unreadable right now is the environment's fault. Its row
// must be left alone so that restoring the file or granting Full Disk Access
// takes effect without the file having to change — and the drain must still
// terminate, even though the content stays claimable.
func TestSkippedContentDoesNotSpinTheDrain(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		q := newFakeQueue(discoveredContent("c_1"))
		ix := newFakeIndexer()
		ix.outcomes["c_1"] = pipeline.Result{
			Outcome: pipeline.OutcomeSkipped,
			Err:     errors.New("permission denied"),
		}
		s := newScheduler(t, q, ix, &fakeScanner{}, nil)

		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		go func() { _ = s.Run(ctx) }()
		synctest.Wait()

		if got := ix.processedHashes(); len(got) != 1 {
			t.Errorf("processed %v in one cycle; skipped content was re-claimed in the same drain", got)
		}
		if c := q.snapshotContent("c_1"); c.State != domain.ContentStateDiscovered || c.Attempts != 0 {
			t.Errorf("c_1 = state %q attempts %d, want it untouched", c.State, c.Attempts)
		}
		if got := s.Snapshot().Skipped; got != 1 {
			t.Errorf("snapshot Skipped = %d, want 1", got)
		}
		// A drain that ran out of *readable* content is not an indexed
		// corpus. Reporting it as idle would describe a daemon quietly
		// passing over files every cycle as finished.
		if got := s.Snapshot().Gate; got != GateUnreadable {
			t.Errorf("gate = %q, want %q", got, GateUnreadable)
		}
	})
}

// A scan that hits errors and reaches nothing at all is the signature of a
// missing Full Disk Access grant. It is the one outcome where the daemon
// looks perfectly healthy while indexing nothing, so it must be recorded —
// the removed one-shot command exited non-zero here, and a daemon cannot.
func TestScanReachingNoFilesIsRecorded(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		q := newFakeQueue()
		ix := newFakeIndexer()
		sc := &fakeScanner{result: discovery.Result{
			// WalkErrors as well as the slice: the alarm asks whether the
			// *walk* hit trouble, and the deletion reconcile appends to the
			// same PathErrors. These two came from the walk.
			WalkErrors: 2,
			PathErrors: []discovery.PathError{
				{Path: "/Users/ben/Documents", Err: errors.New("operation not permitted")},
				{Path: "/Users/ben/Desktop", Err: errors.New("operation not permitted")},
			},
		}}
		s := newScheduler(t, q, ix, sc, nil)

		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		go func() { _ = s.Run(ctx) }()
		synctest.Wait()

		snap := s.Snapshot()
		if !snap.ScanReachedNothing {
			t.Error("ScanReachedNothing = false; status would report a healthy daemon that indexes nothing")
		}
		if snap.ScanErrs != 2 {
			t.Errorf("ScanErrs = %d, want 2", snap.ScanErrs)
		}
		want := []PathError{
			{Path: "/Users/ben/Documents", Err: "operation not permitted"},
			{Path: "/Users/ben/Desktop", Err: "operation not permitted"},
		}
		if !slices.Equal(snap.PathErrors, want) {
			t.Errorf("PathErrors = %+v, want %+v — a count alone does not say which directory", snap.PathErrors, want)
		}
	})
}

// The sample is capped so status stays readable when a revoked grant produces
// one error per directory, but the count must still report the whole truth.
func TestScanPathErrorSampleIsCapped(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var errs []discovery.PathError
		for i := range maxLoggedPathErrors + 3 {
			errs = append(errs, discovery.PathError{
				Path: "/x/" + itoa(i), Err: errors.New("operation not permitted"),
			})
		}
		s := newScheduler(t, newFakeQueue(), newFakeIndexer(),
			&fakeScanner{result: discovery.Result{PathErrors: errs}}, nil)

		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		go func() { _ = s.Run(ctx) }()
		synctest.Wait()

		snap := s.Snapshot()
		if len(snap.PathErrors) != maxLoggedPathErrors {
			t.Errorf("sampled %d path errors, want the cap of %d", len(snap.PathErrors), maxLoggedPathErrors)
		}
		if snap.ScanErrs != len(errs) {
			t.Errorf("ScanErrs = %d, want the full %d — the cap is on the sample, not the count", snap.ScanErrs, len(errs))
		}
	})
}

// A snapshot is a value the status handler may hold while the next scan runs.
// Returning the struct copies the slice header, not the backing array, so
// without an explicit clone the caller reads memory the scheduler overwrites.
func TestSnapshotDoesNotAliasPathErrors(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		sc := &fakeScanner{result: discovery.Result{
			PathErrors: []discovery.PathError{{Path: "/x", Err: errors.New("operation not permitted")}},
		}}
		s := newScheduler(t, newFakeQueue(), newFakeIndexer(), sc, nil)

		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		go func() { _ = s.Run(ctx) }()
		synctest.Wait()

		snap := s.Snapshot()
		if len(snap.PathErrors) != 1 {
			t.Fatalf("PathErrors = %+v, want one", snap.PathErrors)
		}
		snap.PathErrors[0].Path = "/mutated"
		if again := s.Snapshot(); again.PathErrors[0].Path != "/x" {
			t.Errorf("scheduler state = %q after a caller wrote to its snapshot; the slice is shared", again.PathErrors[0].Path)
		}
	})
}

// A scan that reaches files is not a permissions failure, even when some
// paths errored — one unreadable directory must not look like a revoked
// grant.
func TestPartialScanErrorsAreNotAPermissionsFailure(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		q := newFakeQueue()
		ix := newFakeIndexer()
		sc := &fakeScanner{result: discovery.Result{
			Unchanged:  40,
			PathErrors: []discovery.PathError{{Path: "/x", Err: errors.New("operation not permitted")}},
		}}
		s := newScheduler(t, q, ix, sc, nil)

		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		go func() { _ = s.Run(ctx) }()
		synctest.Wait()

		if s.Snapshot().ScanReachedNothing {
			t.Error("ScanReachedNothing = true despite the scan reaching 40 files")
		}
	})
}

// The version sweep is what makes a model change re-embed the corpus with no
// command run. It must happen before anything can conclude the queue is
// empty, because a fully-indexed stale corpus looks exactly like no work.
func TestStaleSweepRunsBeforeConcludingThereIsNoWork(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		q := newFakeQueue() // nothing claimable to begin with
		q.sweepMoves = []int{7, 0}
		ix := newFakeIndexer()
		s := newScheduler(t, q, ix, &fakeScanner{}, nil)

		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		go func() { _ = s.Run(ctx) }()
		synctest.Wait()

		q.mu.Lock()
		sweeps := append([]map[string]string(nil), q.sweeps...)
		q.mu.Unlock()

		if len(sweeps) != 2 {
			t.Fatalf("sweeps = %d, want 2 (versions without dims, then dims)", len(sweeps))
		}
		// The first sweep must not mention dims: they are unknown until the
		// endpoint has been probed, and a wrong value here would re-queue the
		// whole corpus on every start.
		if _, ok := sweeps[0][domain.StageEmbeddingDims]; ok {
			t.Error("the first sweep compared embedding dims, which are not known before Prepare")
		}
		for _, key := range []string{domain.StageChunker, domain.StageEmbedding, domain.StageVecMetric} {
			if _, ok := sweeps[0][key]; !ok {
				t.Errorf("the first sweep did not compare %q", key)
			}
		}
		if got := sweeps[1][domain.StageEmbeddingDims]; got != "768" {
			t.Errorf("the dims sweep compared %q, want 768", got)
		}
		if got := s.Snapshot().Swept; got != 7 {
			t.Errorf("snapshot Swept = %d, want 7", got)
		}
	})
}

// Configuration is read once at startup, so a second sweep could only ever
// move zero rows. Repeating it every cycle would be a full-table UPDATE on a
// 100k-row catalog, forever, for nothing.
func TestStaleSweepRunsOncePerProcess(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		q := newFakeQueue()
		ix := newFakeIndexer()
		s := newScheduler(t, q, ix, &fakeScanner{}, func(o *Options) {
			o.Power = func() config.PowerPolicy {
				return config.PowerPolicy{IndexInterval: config.Interval{Duration: time.Minute}}
			}
		})

		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		go func() { _ = s.Run(ctx) }()
		synctest.Wait()

		time.Sleep(10 * time.Minute)
		synctest.Wait()

		_, _, sweeps, _ := q.counts()
		if sweeps != 2 {
			t.Errorf("sweeps = %d after ten cycles, want 2 (both from the first cycle)", sweeps)
		}
	})
}

// An idle corpus must cost nothing. The daemon runs for weeks; probing the
// endpoint every interval to confirm there is still nothing to embed would
// keep a model resident and the machine awake for no reason.
func TestAnIdleCorpusMakesNoNetworkCalls(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		q := newFakeQueue()
		ix := newFakeIndexer()
		s := newScheduler(t, q, ix, &fakeScanner{}, func(o *Options) {
			o.Power = func() config.PowerPolicy {
				return config.PowerPolicy{IndexInterval: config.Interval{Duration: time.Minute}}
			}
		})

		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		go func() { _ = s.Run(ctx) }()
		synctest.Wait()

		after := ix.prepareCount()
		time.Sleep(30 * time.Minute)
		synctest.Wait()

		if got := ix.prepareCount(); got != after {
			t.Errorf("probes went from %d to %d over thirty idle cycles, want no further calls", after, got)
		}
	})
}

// On battery the heavy stages defer, but discovery still runs: a laptop that
// spent the day unplugged must not come back with no idea what changed.
func TestOnBatteryIndexingDefersButDiscoveryContinues(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		q := newFakeQueue(discoveredContent("c_1"))
		ix := newFakeIndexer()
		sc := &fakeScanner{}
		s := newScheduler(t, q, ix, sc, func(o *Options) {
			o.Power = func() config.PowerPolicy {
				return config.PowerPolicy{IndexInterval: config.Interval{Defer: true}}
			}
		})

		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		go func() { _ = s.Run(ctx) }()
		synctest.Wait()

		if got := ix.processedHashes(); len(got) != 0 {
			t.Errorf("processed %v on battery, want nothing", got)
		}
		if sc.count() != 1 {
			t.Errorf("scans = %d on battery, want discovery to still run", sc.count())
		}
		snap := s.Snapshot()
		if snap.Gate != GateBattery || !snap.Deferring {
			t.Errorf("gate = %q deferring = %v, want %q and true", snap.Gate, snap.Deferring, GateBattery)
		}
		if c := q.snapshotContent("c_1"); c.Attempts != 0 {
			t.Errorf("attempts = %d, want deferral to charge nothing", c.Attempts)
		}
	})
}

// Plugging the laptop in must start indexing without a restart.
func TestIndexingStartsWhenPowerReturns(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		q := newFakeQueue(discoveredContent("c_1"))
		ix := newFakeIndexer()
		ix.markIndexed = func(hash string) { q.setState(hash, domain.ContentStateIndexed) }

		var mu sync.Mutex
		onBattery := true
		s := newScheduler(t, q, ix, &fakeScanner{}, func(o *Options) {
			o.Power = func() config.PowerPolicy {
				mu.Lock()
				defer mu.Unlock()
				if onBattery {
					return config.PowerPolicy{IndexInterval: config.Interval{Defer: true}}
				}
				return config.PowerPolicy{IndexInterval: config.Interval{Duration: time.Minute}}
			}
		})

		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		go func() { _ = s.Run(ctx) }()
		synctest.Wait()

		if got := ix.processedHashes(); len(got) != 0 {
			t.Fatalf("processed %v on battery", got)
		}

		mu.Lock()
		onBattery = false
		mu.Unlock()

		time.Sleep(deferRecheckInterval)
		synctest.Wait()

		if got := ix.processedHashes(); len(got) != 1 || got[0] != "c_1" {
			t.Errorf("processed %v after plugging in, want [c_1]", got)
		}
	})
}

// The scan walks the filesystem; the drain talks to an inference server.
// Tying them to one interval would mean either walking $HOME every five
// minutes or indexing every fifteen.
func TestScanAndDrainRunOnIndependentIntervals(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		q := newFakeQueue()
		ix := newFakeIndexer()
		sc := &fakeScanner{}
		s := newScheduler(t, q, ix, sc, func(o *Options) {
			o.ScanInterval = 30 * time.Minute
			o.Power = func() config.PowerPolicy {
				return config.PowerPolicy{IndexInterval: config.Interval{Duration: 5 * time.Minute}}
			}
		})

		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		go func() { _ = s.Run(ctx) }()
		synctest.Wait()

		time.Sleep(29 * time.Minute)
		synctest.Wait()
		if got := sc.count(); got != 1 {
			t.Errorf("scans = %d after 29 minutes on a 30-minute scan interval, want 1", got)
		}
		_, _, _, claims := q.counts()
		if claims < 5 {
			t.Errorf("claims = %d over six drain intervals, want the drain to run independently", claims)
		}

		time.Sleep(2 * time.Minute)
		synctest.Wait()
		if got := sc.count(); got != 2 {
			t.Errorf("scans = %d after the scan interval elapsed, want 2", got)
		}
	})
}

// Per-path scan problems (a revoked Full Disk Access grant, an unreadable
// directory) must never stop indexing — they are reported, and the rest of
// the corpus carries on.
func TestScanPathErrorsDoNotStopIndexing(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		q := newFakeQueue(discoveredContent("c_1"))
		ix := newFakeIndexer()
		ix.markIndexed = func(hash string) { q.setState(hash, domain.ContentStateIndexed) }
		sc := &fakeScanner{result: discovery.Result{
			Discovered: 1,
			PathErrors: []discovery.PathError{
				{Path: "/Users/ben/Documents", Err: errors.New("operation not permitted")},
			},
		}}
		s := newScheduler(t, q, ix, sc, nil)

		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		go func() { _ = s.Run(ctx) }()
		synctest.Wait()

		if got := ix.processedHashes(); len(got) != 1 {
			t.Errorf("processed %v; a path error stopped indexing", got)
		}
		if got := s.Snapshot().ScanErrs; got != 1 {
			t.Errorf("snapshot ScanErrs = %d, want 1 so status can surface the TCC problem", got)
		}
	})
}

// A store write that did not land says nothing about any content, so it
// stops the cycle without charging anyone — and the next cycle tries again.
func TestAStoreFailureStopsTheCycleWithoutBlamingContent(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		q := newFakeQueue(discoveredContent("c_1"))
		q.claimErr = errors.New("database is locked")
		ix := newFakeIndexer()
		s := newScheduler(t, q, ix, &fakeScanner{}, nil)

		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		go func() { _ = s.Run(ctx) }()
		synctest.Wait()

		if got := ix.processedHashes(); len(got) != 0 {
			t.Errorf("processed %v despite a failing claim", got)
		}
		snap := s.Snapshot()
		if snap.Gate != GateStoreFail {
			t.Errorf("gate = %q, want %q", snap.Gate, GateStoreFail)
		}
		if snap.LastError == "" {
			t.Error("no LastError recorded; status would say the queue is stuck without saying why")
		}
	})
}

// Shutdown must stop the loop promptly and leave everything durable — the
// daemon is killed by launchd on logout, mid-drain, routinely.
func TestRunStopsOnContextCancellation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		q := newFakeQueue()
		ix := newFakeIndexer()
		s := newScheduler(t, q, ix, &fakeScanner{}, nil)

		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan error, 1)
		go func() { done <- s.Run(ctx) }()
		synctest.Wait()

		cancel()
		synctest.Wait()

		select {
		case err := <-done:
			if err != nil {
				t.Errorf("Run returned %v, want nil", err)
			}
		default:
			t.Error("Run did not return after cancellation")
		}
	})
}

// Cancelling mid-scan must unwind rather than finish the cycle.
func TestCancellationInterruptsACycleInFlight(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		sc := &fakeScanner{entered: make(chan struct{}), release: make(chan struct{})}
		q := newFakeQueue(discoveredContent("c_1"))
		ix := newFakeIndexer()
		s := newScheduler(t, q, ix, sc, nil)

		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan error, 1)
		go func() { done <- s.Run(ctx) }()

		<-sc.entered
		cancel()
		synctest.Wait()

		select {
		case <-done:
		default:
			t.Fatal("Run did not return after cancellation during a scan")
		}
		if got := ix.processedHashes(); len(got) != 0 {
			t.Errorf("processed %v after cancellation, want the cycle abandoned", got)
		}
	})
}

// Snapshot is read by the HTTP status handler while a cycle is running, so it
// must not race the loop that writes it.
func TestSnapshotIsSafeConcurrently(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		q := newFakeQueue(discoveredContent("c_1"), discoveredContent("c_2"))
		ix := newFakeIndexer()
		ix.markIndexed = func(hash string) { q.setState(hash, domain.ContentStateIndexed) }
		s := newScheduler(t, q, ix, &fakeScanner{}, func(o *Options) {
			o.Power = func() config.PowerPolicy {
				return config.PowerPolicy{IndexInterval: config.Interval{Duration: time.Second}}
			}
		})

		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		go func() { _ = s.Run(ctx) }()

		var wg sync.WaitGroup
		for range 4 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for range 50 {
					_ = s.Snapshot()
				}
			}()
		}
		wg.Wait()
		synctest.Wait()
	})
}

// OutcomeSuperseded stopped being routine when the catalog gained a single
// writer (ADR 0014): the content is re-read immediately before processing, so
// for its row to change while the pipeline holds it, something other than this
// goroutine must have written the catalog — and there is nothing else.
//
// Left as it was — Debug, counted nowhere, row untouched — it would be the
// quietest failure in the daemon: re-claimed and superseded every cycle
// forever, with `bsearch status` reporting no failures, no skips and an empty
// gate over permanently unsearchable content.
func TestSupersededContentIsReportedAndRescheduled(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		q := newFakeQueue(discoveredContent("c_1"))
		ix := newFakeIndexer()
		ix.outcomes["c_1"] = pipeline.Result{
			Outcome: pipeline.OutcomeSuperseded,
			Err:     domain.ErrContentSuperseded,
		}
		s := newScheduler(t, q, ix, watchedScanner("/root"), nil)

		s.cycle(t.Context())

		if snap := s.Snapshot(); snap.Superseded != 1 {
			t.Errorf("Superseded = %d, want 1 — a broken invariant must be counted", snap.Superseded)
		}
		if len(q.reschedules) != 1 {
			t.Fatalf("reschedules = %+v, want the row put on the retry schedule rather than left hot", q.reschedules)
		}
		if got := q.reschedules[0]; got.hash != "c_1" || got.attempts != 1 {
			t.Errorf("reschedule = %+v, want c_1 charged one attempt", got)
		}

		// No health probe: the embedding endpoint is not implicated, so
		// retry's fault-attribution probe would answer a question nobody
		// asked. One probe for the drain itself, none for this.
		if got := ix.prepareCount(); got != 1 {
			t.Errorf("probes = %d, want only the drain's own", got)
		}
	})
}

// OutcomeChanged means the file changed between the claim and the pipeline's
// read: the bytes no longer hash to this content, nothing was written, and
// discovery's next pass re-points the path. It is nobody's failure, so it
// charges no attempt and moves no counter — but seen must still bound a
// hot-edited file to one abandoned read per drain, because the row stays
// claimable the whole time.
func TestChangedContentIsDeferredWithoutChargeAndCounted(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		q := newFakeQueue(discoveredContent("c_1"))
		ix := newFakeIndexer()
		ix.outcomes["c_1"] = pipeline.Result{Outcome: pipeline.OutcomeChanged}
		s := newScheduler(t, q, ix, watchedScanner("/root"), nil)

		s.cycle(t.Context())

		snap := s.Snapshot()
		if snap.Indexed != 0 || snap.Failed != 0 || snap.Skipped != 0 ||
			snap.Retried != 0 || snap.Superseded != 0 {
			t.Errorf("counters = indexed %d failed %d skipped %d retried %d superseded %d, want all zero — a changed file is nobody's failure",
				snap.Indexed, snap.Failed, snap.Skipped, snap.Retried, snap.Superseded)
		}
		if snap.Changed != 1 {
			t.Errorf("Changed = %d, want 1 — the abandoned claim must be visible in status", snap.Changed)
		}
		// Deferred at the backoff cap without charging: attempts unchanged,
		// next_retry_at in the future, so the pathological shapes (a
		// continuously-rewritten file, a size-and-mtime-preserving edit
		// discovery can never notice) cost one read per cap interval, not
		// one per drain.
		reschedules, failures, _, _ := q.counts()
		if reschedules != 1 || failures != 0 {
			t.Errorf("reschedules = %d, failures = %d, want 1 and 0", reschedules, failures)
		}
		if got := q.reschedules[0]; got.hash != "c_1" || got.attempts != 0 {
			t.Errorf("reschedule = %+v, want c_1 deferred with attempts unchanged (0)", got)
		}
		if got := ix.processedHashes(); len(got) != 1 || got[0] != "c_1" {
			t.Errorf("processed %v, want exactly one abandoned read of c_1 in this drain", got)
		}
		if c := q.snapshotContent("c_1"); c.State != domain.ContentStateDiscovered || c.Attempts != 0 {
			t.Errorf("c_1 = state %q attempts %d, want state and budget untouched", c.State, c.Attempts)
		}
	})
}

// pipeline.movedOn folds two sentinels into OutcomeSuperseded and documents
// that the caller tells them apart. ErrContentGone is routine — the row was
// swept mid-flight once the orphan sweep exists — and must not trip the
// ADR 0014 invariant alarm or charge backoff to a row that no longer
// exists.
func TestSupersededWithContentGoneIsRoutineNotAnAlarm(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		q := newFakeQueue(discoveredContent("c_1"))
		ix := newFakeIndexer()
		ix.outcomes["c_1"] = pipeline.Result{
			Outcome: pipeline.OutcomeSuperseded,
			Err:     fmt.Errorf("store chunks: %w", domain.ErrContentGone),
		}
		s := newScheduler(t, q, ix, watchedScanner("/root"), nil)

		s.cycle(t.Context())

		snap := s.Snapshot()
		if snap.Superseded != 0 {
			t.Errorf("Superseded = %d, want 0 — a mid-flight sweep is not a broken invariant", snap.Superseded)
		}
		if reschedules, failures, _, _ := q.counts(); reschedules != 0 || failures != 0 {
			t.Errorf("reschedules = %d, failures = %d, want 0 and 0 — nothing is owed to a swept row", reschedules, failures)
		}
	})
}

// Content whose every referencing path vanished between the claim and the
// re-read is gone, not broken: GetWork reports ErrContentGone, the drain
// marks it seen and carries on with the rest of the batch. Gating on it
// would let one deleted file stop indexing for everything behind it, and
// counting it as skipped would report a deletion as a permissions problem.
//
// The deletion lands mid-drain — after the startup sweep, while the batch is
// in flight — because that is the only window this path serves: an orphan
// already present when the cycle starts is collected by the sweep and never
// re-read at all (its own test below).
func TestContentGoneAtTheReReadIsSkippedWithoutStoppingTheDrain(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		q := newFakeQueue(discoveredContent("c_live"), discoveredContent("c_gone"))
		ix := newFakeIndexer()
		ix.markIndexed = func(hash string) {
			q.setState(hash, domain.ContentStateIndexed)
			// The file backing c_gone is deleted while c_live is indexing —
			// between c_gone's claim and its re-read.
			q.orphan("c_gone")
		}
		s := newScheduler(t, q, ix, watchedScanner("/root"), nil)

		s.cycle(t.Context())

		if got := ix.processedHashes(); !slices.Equal(got, []string{"c_live"}) {
			t.Errorf("processed %v, want [c_live] — the gone content skipped, the rest worked", got)
		}
		snap := s.Snapshot()
		if snap.Gate != GateIdle {
			t.Errorf("gate = %q, want %q — a deleted file is not an unreadable one", snap.Gate, GateIdle)
		}
		if snap.Skipped != 0 || snap.Failed != 0 {
			t.Errorf("Skipped = %d, Failed = %d, want 0 and 0 — gone is counted nowhere", snap.Skipped, snap.Failed)
		}
		reschedules, failures, _, _ := q.counts()
		if reschedules != 0 || failures != 0 {
			t.Errorf("reschedules = %d, failures = %d, want 0 and 0", reschedules, failures)
		}
	})
}

// The orphan sweep is armed at construction — the first cycle collects
// whatever a crash or a previous build left behind, terminal orphans
// included — and a quiet cycle afterwards must not pay for it again.
func TestOrphanSweepRunsOnceAtStartThenNotOnQuietCycles(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		q := newFakeQueue()
		s := newScheduler(t, q, newFakeIndexer(), watchedScanner("/root"), nil)

		s.cycle(t.Context())
		if got := q.orphanSweepCount(); got != 1 {
			t.Fatalf("orphan sweeps after first cycle = %d, want 1", got)
		}
		if scopes := q.scopes(); scopes[0] != domain.SweepScopeAll {
			t.Errorf("startup sweep scope = %v, want SweepScopeAll — a previous process's terminal orphans have no other collector", scopes[0])
		}

		s.cycle(t.Context())
		if got := q.orphanSweepCount(); got != 1 {
			t.Errorf("orphan sweeps after a quiet cycle = %d, want still 1 — quiet cycles must not sweep", got)
		}
	})
}

// Removals and edits are the orphan producers, so a scan reporting any of
// them arms a sweep — at queue scope, so a terminal orphan keeps its derived
// data for the free-restore window (an undo must not cost a re-embed).
//
// Scope prunes count as removals: a row dropped because the file left the
// configured paths orphans its content exactly as a deleted file does, and
// leaving it out would let a config edit strand chunks and vectors with
// nothing to collect them.
func TestDeletionsAndEditsArmTheOrphanSweep(t *testing.T) {
	for name, tc := range map[string]struct {
		res   discovery.Result
		scope domain.SweepScope
	}{
		"deletions": {discovery.Result{Deleted: 1}, domain.SweepScopeQueue},
		"edits":     {discovery.Result{Changed: 1}, domain.SweepScopeQueue},
		// Queue scope for a prune too. Escalating on it was tried and
		// reverted: the scope is a property of the pass, so one pruned row in
		// a cycle that also saw deletions collected those as well, destroying
		// the free-restore window this branch exists to hold open.
		"scope prunes": {discovery.Result{Pruned: 1}, domain.SweepScopeQueue},
	} {
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				q := newFakeQueue()
				sc := watchedScanner("/root")
				s := newScheduler(t, q, newFakeIndexer(), sc, nil)

				s.cycle(t.Context()) // consumes the construction arm
				if got := q.orphanSweepCount(); got != 1 {
					t.Fatalf("orphan sweeps = %d, want 1", got)
				}

				sc.mu.Lock()
				sc.result = tc.res
				sc.mu.Unlock()
				s.requestScan()
				s.cycle(t.Context())
				if got := q.orphanSweepCount(); got != 2 {
					t.Errorf("orphan sweeps = %d, want 2 — a scan with orphan producers must arm the sweep", got)
				}
				if scopes := q.scopes(); len(scopes) == 2 && scopes[1] != tc.scope {
					t.Errorf("armed sweep scope = %v, want %v", scopes[1], tc.scope)
				}

				s.cycle(t.Context())
				if got := q.orphanSweepCount(); got != 2 {
					t.Errorf("orphan sweeps = %d, want still 2 — the arm is consumed, not standing", got)
				}
			})
		})
	}
}

// The reconcile's numbers reach status: two cumulative totals for what was
// removed, split by why, and two gauges for what was declined.
//
// The gauges are gauges on purpose. Unverified describes a subtree that is
// blocked right now — a directory that cannot be read, a volume that is not
// mounted — so it must clear when the next scan finds the obstruction gone.
// Accumulated instead, a long-running daemon would report a permanent alarm
// for a drive plugged back in an hour ago.
func TestScanReconcileCountsReachTheSnapshot(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		sc := watchedScanner("/root")
		s := newScheduler(t, newFakeQueue(), newFakeIndexer(), sc, nil)

		sc.mu.Lock()
		sc.result = discovery.Result{
			Deleted: 3, Pruned: 2, Unverified: 7, Reconciled: true,
			Unmounted: []string{"/Volumes/Archive"},
		}
		sc.mu.Unlock()
		s.requestScan()
		s.cycle(t.Context())

		snap := s.Snapshot()
		if snap.ScanDeleted != 3 || snap.ScanPruned != 2 {
			t.Errorf("ScanDeleted/ScanPruned = %d/%d, want 3/2 kept apart — one is the corpus changing, the other the config",
				snap.ScanDeleted, snap.ScanPruned)
		}
		if snap.ScanUnverified != 7 || !slices.Equal(snap.ScanUnmounted, []string{"/Volumes/Archive"}) {
			t.Errorf("ScanUnverified/ScanUnmounted = %d/%v, want 7 and the volume named",
				snap.ScanUnverified, snap.ScanUnmounted)
		}

		// A clean scan: the totals keep climbing, the gauges clear.
		sc.mu.Lock()
		sc.result = discovery.Result{Deleted: 1, Reconciled: true}
		sc.mu.Unlock()
		s.requestScan()
		s.cycle(t.Context())

		snap = s.Snapshot()
		if snap.ScanDeleted != 4 || snap.ScanPruned != 2 {
			t.Errorf("ScanDeleted/ScanPruned = %d/%d, want 4/2 — totals accumulate", snap.ScanDeleted, snap.ScanPruned)
		}
		if snap.ScanUnverified != 0 || len(snap.ScanUnmounted) != 0 {
			t.Errorf("ScanUnverified/ScanUnmounted = %d/%v, want cleared — the obstruction is gone",
				snap.ScanUnverified, snap.ScanUnmounted)
		}
	})
}

// A scan that only found new and unchanged files arms nothing: neither
// produces an orphan, and a bulk initial index must never pay for sweeping.
// This is the negative half of noteOrphanProducers' predicate — the
// mutation `|| res.Discovered > 0` must fail here (review 3653204192).
func TestAScanWithoutOrphanProducersDoesNotArmTheSweep(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		q := newFakeQueue()
		sc := watchedScanner("/root")
		s := newScheduler(t, q, newFakeIndexer(), sc, nil)

		s.cycle(t.Context()) // consumes the construction arm
		if got := q.orphanSweepCount(); got != 1 {
			t.Fatalf("orphan sweeps = %d, want the startup sweep only", got)
		}

		sc.mu.Lock()
		sc.result = discovery.Result{Discovered: 5, Unchanged: 3}
		sc.mu.Unlock()
		s.requestScan()
		s.cycle(t.Context())

		if got := q.orphanSweepCount(); got != 1 {
			t.Errorf("orphan sweeps = %d after a discover-only scan, want still 1 — new files produce no orphans", got)
		}
	})
}

// A failed sweep re-arms itself and the cycle carries on to the drain: the
// deletions that armed it exist nowhere else, but the drain tolerates
// orphans (GetWork answers ErrContentGone), so wedging indexing behind
// garbage collection would trade a benign Gone-skip for a stalled queue.
func TestOrphanSweepFailureRearmsAndTheCycleContinues(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		q := newFakeQueue(discoveredContent("c_1"))
		q.orphanSweepErr = errors.New("disk full")
		ix := newFakeIndexer()
		ix.markIndexed = func(hash string) { q.setState(hash, domain.ContentStateIndexed) }
		s := newScheduler(t, q, ix, watchedScanner("/root"), nil)

		s.cycle(t.Context())

		// The drain ran despite the failed sweep, and worked the queue down.
		if _, _, _, claims := q.counts(); claims == 0 {
			t.Error("claims = 0 — a failed sweep stopped the drain, wedging indexing behind garbage collection")
		}
		if got := ix.processedHashes(); !slices.Equal(got, []string{"c_1"}) {
			t.Errorf("processed %v, want [c_1] indexed despite the failed sweep", got)
		}
		snap := s.Snapshot()
		if snap.Gate == GateStoreFail {
			t.Errorf("gate = %q — the sweep alone must not report the store as failed while indexing works", snap.Gate)
		}
		if snap.LastError == "" {
			t.Error("no LastError recorded; status would show nothing about the failing sweep")
		}

		// The failure re-armed the same scope, and the next cycle retries it.
		q.mu.Lock()
		q.orphanSweepErr = nil
		q.mu.Unlock()
		s.cycle(t.Context())
		if got := q.orphanSweepCount(); got != 2 {
			t.Fatalf("orphan sweeps = %d, want 2 — a failed sweep must re-arm", got)
		}
		if scopes := q.scopes(); scopes[1] != scopes[0] {
			t.Errorf("retry scope = %v, want the failed sweep's own %v", scopes[1], scopes[0])
		}
	})
}

// A queue-scope sweep spares terminal orphans (review 3653203993): the
// indexed content an edit just orphaned is exactly what an undo or a `git
// checkout` re-references, and collecting it eagerly would turn each of
// those into a full re-embed. It waits for the next process's startup sweep.
func TestQueueScopeSweepSparesTerminalOrphansUntilRestart(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		q := newFakeQueue(domain.Content{Hash: "c_kept", State: domain.ContentStateIndexed})
		sc := watchedScanner("/root")
		s := newScheduler(t, q, newFakeIndexer(), sc, nil)

		s.cycle(t.Context()) // startup full sweep: c_kept is referenced, survives
		if !q.has("c_kept") {
			t.Fatal("the startup sweep collected referenced content")
		}

		// An edit re-points the path and orphans the indexed content; the
		// scan reports it and arms a queue-scope sweep.
		q.orphan("c_kept")
		sc.mu.Lock()
		sc.result = discovery.Result{Changed: 1}
		sc.mu.Unlock()
		s.requestScan()
		s.cycle(t.Context())

		if scopes := q.scopes(); len(scopes) != 2 || scopes[1] != domain.SweepScopeQueue {
			t.Fatalf("sweep scopes = %v, want the edit to arm a queue-scope sweep", scopes)
		}
		if !q.has("c_kept") {
			t.Fatal("the queue-scope sweep collected a terminal orphan — an undo would now cost a re-embed")
		}

		// The free-restore window is the life of the process: a fresh
		// scheduler's startup sweep is full-scope and collects it.
		s2 := newScheduler(t, q, newFakeIndexer(), watchedScanner("/root"), nil)
		s2.cycle(t.Context())
		if q.has("c_kept") {
			t.Error("the next process's startup sweep left the terminal orphan behind")
		}
		if got := s2.Snapshot().Collected; got != 1 {
			t.Errorf("Collected = %d, want 1", got)
		}
	})
}

// An orphan already in the queue when the daemon starts is collected before
// anything is claimed (review 3653204158): the sweep runs above the drain,
// so the row is never claimed, never re-read, never Gone-skipped — the
// "claimed and skipped forever" loop the sweep exists to end.
func TestAnOrphanPresentAtStartupIsNeverClaimed(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		q := newFakeQueue(discoveredContent("c_orphan"))
		q.orphan("c_orphan")
		ix := newFakeIndexer()
		s := newScheduler(t, q, ix, watchedScanner("/root"), nil)

		s.cycle(t.Context())

		if got := ix.processedHashes(); len(got) != 0 {
			t.Errorf("processed %v, want nothing — the orphan was handed to the pipeline", got)
		}
		if got := q.getWorkHashes(); len(got) != 0 {
			t.Errorf("GetWork called for %v, want never — the sweep runs before anything is claimed", got)
		}
		if q.has("c_orphan") {
			t.Error("the orphan is still in the queue after the startup sweep")
		}
		if got := s.Snapshot().Gate; got != GateIdle {
			t.Errorf("gate = %q, want %q", got, GateIdle)
		}
	})
}

// The sweep runs above the battery gate, with its producers: the walk and
// the reconcile both run on battery and both orphan content, so a gated
// sweep would let an unplugged day accumulate garbage its producers kept
// making (review 3653204249).
func TestTheOrphanSweepRunsWhileIndexingIsDeferred(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		q := newFakeQueue(discoveredContent("c_orphan"))
		q.orphan("c_orphan")
		s := newScheduler(t, q, newFakeIndexer(), watchedScanner("/root"), func(o *Options) {
			o.Power = func() config.PowerPolicy {
				return config.PowerPolicy{IndexInterval: config.Interval{Defer: true}}
			}
		})

		s.cycle(t.Context())

		if got := q.orphanSweepCount(); got != 1 {
			t.Errorf("orphan sweeps = %d on battery, want 1 — the sweep is a cheap stage and cheap stages always run", got)
		}
		if q.has("c_orphan") {
			t.Error("the orphan survived a deferred cycle; garbage would accumulate all day on battery")
		}
		if _, _, _, claims := q.counts(); claims != 0 {
			t.Errorf("claims = %d on battery, want 0 — sweeping must not drag the drain above the gate with it", claims)
		}
	})
}

// A walk that fails part-way still reports what it committed before dying
// (review 3653204007): ScanPaths and the walk both flush as they go, so the
// deletions and path errors met before the failure are real. Discarding
// them with the error would be the silent-skip shape CLAUDE.md forbids —
// and would leave real orphans with nothing armed to collect them.
func TestAFailedScanStillReportsPartialResultsAndArmsTheSweep(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		q := newFakeQueue()
		sc := watchedScanner("/root")
		s := newScheduler(t, q, newFakeIndexer(), sc, nil)

		s.cycle(t.Context()) // consumes the construction arm; records LastScan
		lastScan := s.Snapshot().LastScan

		time.Sleep(time.Minute) // separate the two cycles on the fake clock
		sc.mu.Lock()
		sc.err = errors.New("disk on fire")
		sc.result = discovery.Result{
			Changed: 1,
			PathErrors: []discovery.PathError{
				{Path: "/root/locked", Err: errors.New("operation not permitted")},
			},
		}
		sc.mu.Unlock()
		s.requestScan()
		s.cycle(t.Context())

		snap := s.Snapshot()
		if snap.ScanErrs != 1 || len(snap.PathErrors) != 1 || snap.PathErrors[0].Path != "/root/locked" {
			t.Errorf("ScanErrs = %d, PathErrors = %+v — the errors met before the failure were dropped", snap.ScanErrs, snap.PathErrors)
		}
		if snap.LastError == "" {
			t.Error("no LastError recorded for the failed walk")
		}
		// The committed edit orphaned content, so the sweep must be armed
		// despite the error — count 2: startup, then this one.
		if got := q.orphanSweepCount(); got != 2 {
			t.Errorf("orphan sweeps = %d, want 2 — a partial walk's committed edits still produce orphans", got)
		}
		// A partial walk proves nothing about the corpus: LastScan stays put.
		if !snap.LastScan.Equal(lastScan) {
			t.Errorf("LastScan = %v, want %v — a failed walk must not count as a completed one", snap.LastScan, lastScan)
		}
	})
}

// What the sweep collects reaches status: Collected is the visibility that
// keeps a silently no-op sweep — the class ADR 0015 singles out —
// diagnosable from `bsearch status` (review 3653204235).
func TestCollectedOrphansReachTheSnapshot(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		q := newFakeQueue(discoveredContent("c_o1"), discoveredContent("c_o2"))
		q.orphan("c_o1")
		q.orphan("c_o2")
		s := newScheduler(t, q, newFakeIndexer(), watchedScanner("/root"), nil)

		s.cycle(t.Context())

		if got := s.Snapshot().Collected; got != 2 {
			t.Errorf("Collected = %d, want 2 — every content the sweep removed", got)
		}
	})
}

// The Full Disk Access alarm keys on the walk's own errors, not on the whole
// PathErrors slice. The deletion reconcile appends to that slice too, so a
// scan that legitimately reached no files while the reconcile declined a few
// rows would otherwise be diagnosed as a permissions problem on a machine
// that has none.
func TestReconcileErrorsDoNotTriggerTheFullDiskAccessAlarm(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		sc := watchedScanner("/root")
		sc.result = discovery.Result{
			// The walk was clean and the corpus is genuinely empty; the
			// reconcile could not stat three rows (a symlink loop).
			WalkErrors: 0,
			Unverified: 3,
			Reconciled: true,
			PathErrors: []discovery.PathError{{Path: "/root/loop", Err: errors.New("too many levels of symbolic links")}},
		}
		s := newScheduler(t, newFakeQueue(), newFakeIndexer(), sc, nil)
		s.requestScan()
		s.cycle(t.Context())

		if snap := s.Snapshot(); snap.ScanReachedNothing {
			t.Error("ScanReachedNothing = true — a reconcile stat failure was read as a missing Full Disk Access grant")
		}
	})
}

// Removals are committed batch by batch, so a reconcile that fails part-way
// still leaves rows genuinely gone. Those counts must be recorded before the
// error return, or status reports nothing removed while the catalog shrank.
func TestScanRecordsCommittedRemovalsWhenTheReconcileFails(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		sc := watchedScanner("/root")
		sc.result = discovery.Result{Deleted: 256, Pruned: 4}
		sc.err = errors.New("database is locked")
		s := newScheduler(t, newFakeQueue(), newFakeIndexer(), sc, nil)
		s.requestScan()
		s.cycle(t.Context())

		snap := s.Snapshot()
		if snap.ScanDeleted != 256 || snap.ScanPruned != 4 {
			t.Errorf("ScanDeleted/ScanPruned = %d/%d, want 256/4 — the rows are gone whether or not the pass finished",
				snap.ScanDeleted, snap.ScanPruned)
		}
	})
}

// Snapshot copies every slice it hands out. ScanUnmounted is overwritten
// wholesale by the next scan, so sharing the backing array would race with a
// caller still reading it.
func TestSnapshotCopiesTheUnmountedVolumeList(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		sc := watchedScanner("/root")
		sc.result = discovery.Result{Unverified: 1, Reconciled: true, Unmounted: []string{"/Volumes/Archive"}}
		s := newScheduler(t, newFakeQueue(), newFakeIndexer(), sc, nil)
		s.requestScan()
		s.cycle(t.Context())

		snap := s.Snapshot()
		snap.ScanUnmounted[0] = "mutated"
		if again := s.Snapshot(); again.ScanUnmounted[0] != "/Volumes/Archive" {
			t.Errorf("ScanUnmounted = %v, want the scheduler's copy untouched", again.ScanUnmounted)
		}
	})
}

// The gauges describe an obstruction that exists right now, so they must be
// assigned on every scan — including one that failed part-way. Left in the
// success-only mark, a reattached drive kept being reported as unmounted for
// as long as scans kept failing.
func TestScanGaugesClearAfterAFailedScan(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		sc := watchedScanner("/root")
		sc.result = discovery.Result{Unverified: 412000, Reconciled: true, Unmounted: []string{"/Volumes/Archive"}}
		s := newScheduler(t, newFakeQueue(), newFakeIndexer(), sc, nil)
		s.requestScan()
		s.cycle(t.Context())
		if snap := s.Snapshot(); snap.ScanUnverified != 412000 {
			t.Fatalf("ScanUnverified = %d, want the obstruction reported", snap.ScanUnverified)
		}

		// Drive reattached; this scan fails on something else entirely.
		sc.mu.Lock()
		sc.result = discovery.Result{Reconciled: true}
		sc.err = errors.New("database is locked")
		sc.mu.Unlock()
		s.requestScan()
		s.cycle(t.Context())

		snap := s.Snapshot()
		if snap.ScanUnverified != 0 || len(snap.ScanUnmounted) != 0 {
			t.Errorf("ScanUnverified/ScanUnmounted = %d/%v, want cleared — the volume is back",
				snap.ScanUnverified, snap.ScanUnmounted)
		}
	})
}
