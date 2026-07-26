// Package scheduler is the daemon's indexing loop: it keeps the catalog in
// step with the filesystem and works the queue down, in the background, on a
// laptop that is often on battery and often talking to services that are not
// running (DESIGN.md: Indexing pipeline and queue; ADR 0011).
//
// Three properties are load-bearing, and each is a decision rather than an
// implementation detail:
//
// One worker, no claims. A single goroutine does the indexing, so there is no
// contention to arbitrate and no claimed state to reconcile after a crash.
// Every pipeline stage is an idempotent upsert, which is what makes "redo the
// in-flight document on restart" a complete recovery story.
//
// Prefer Old on overrun. When a cycle takes longer than the interval, the
// next cycle is simply late: the timer is reset after the drain returns, not
// on an absolute schedule, so triggers are shed rather than queued. The
// alternative policies both assume the overrun was a transient fault that the
// next run will not hit; here the opposite is true — a drain that ran long
// did so because there is a lot of work, and a second concurrent drain would
// contend for the same rows and the same inference server while making the
// same amount of progress.
//
// Outages are not the document's fault. Before working a batch the scheduler
// proves the embedding endpoint is up, and it re-proves it before charging any
// document for a failure. An inference server that is switched off for a week
// costs zero attempts, so nothing is quietly marked failed while the user was
// not looking.
//
// Logging is operational: counts, timings, gate reasons, and the paths of
// documents that failed or were skipped, because a failure you cannot locate
// is not actionable. Never document content, never summaries, never queries
// (DESIGN.md: Privacy).
package scheduler

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"slices"
	"sync"
	"time"

	"github.com/bcrisp4/bsearch/internal/config"
	"github.com/bcrisp4/bsearch/internal/discovery"
	"github.com/bcrisp4/bsearch/internal/domain"
	"github.com/bcrisp4/bsearch/internal/pipeline"
)

const (
	// defaultBatchSize is how many documents one claim returns. It bounds
	// how long a file saved a moment ago waits behind a bulk backlog: the
	// queue is re-read, and priority re-evaluated, every batch.
	defaultBatchSize = 32
	// defaultScanInterval is how often the filesystem is walked when the
	// watcher is not running. Without events the scan is the only way a new
	// file is noticed, so anything slower would miss DESIGN.md's "new or
	// changed file searchable ≤ 5 min on AC". With the watcher running the
	// scan is a backstop instead, and watchedScanInterval applies.
	//
	// Affordable because the deny-list does the work: a walk of a ~500k-file
	// $HOME with VCS, dependency and Library trees pruned measures around a
	// second warm, so this is well under a percent of one core.
	//
	// Independent of the drain interval on purpose — one is a stat walk, the
	// other talks to an inference server, and tying them together would mean
	// choosing which of the two cadences to get wrong.
	defaultScanInterval = 5 * time.Minute
	// deferRecheckInterval is how often a deferred scheduler wakes to see
	// whether the power policy still says defer. Short enough that plugging
	// the laptop in has a visible effect, cheap enough to be free.
	deferRecheckInterval = 5 * time.Minute
	// maxLoggedPathErrors bounds per-scan path-error logging. A revoked Full
	// Disk Access grant produces one error per directory; logging all of them
	// turns a diagnosis into a haystack.
	maxLoggedPathErrors = 5
)

// Gate reasons. These are the user-facing explanations for a cycle that did
// no work, and `bsearch status` renders them verbatim. A stalled queue
// must always be distinguishable from a deferred one.
const (
	GateNone      = ""
	GateIdle      = "idle — nothing to index"
	GateBattery   = "deferred: on battery"
	GateEmbedder  = "embedding endpoint unreachable"
	GateStoreFail = "index write failed"
	// GateUnreadable means the drain ran out of documents it could read, not
	// of documents to do — files are being skipped every cycle (permissions,
	// or files that vanished). Distinct from GateIdle because reporting an
	// index as complete while it is quietly passing over files is the failure
	// this whole gate vocabulary exists to prevent.
	GateUnreadable = "files could not be read — check Full Disk Access"
)

// Queue is the catalog access the scheduler needs. Composed from the domain
// port plus the one ContentStore method that belongs to the same story:
// exhausting a content's attempts is a queue decision, not a pipeline one.
type Queue interface {
	domain.Queue
	// MarkFailed records a content as permanently failed with a reason.
	MarkFailed(ctx context.Context, hash, reason string) error
	// SweepOrphans collects orphaned content — vectors, chunks and
	// summaries with it — in short batched transactions, returning how many
	// contents went. The queue is where it belongs because orphaned
	// non-terminal rows are otherwise claimed and Gone-skipped every drain,
	// forever; scope says whether terminal orphans go too
	// (domain.SweepScope).
	SweepOrphans(ctx context.Context, scope domain.SweepScope) (int, error)
}

// Indexer is the per-content work, as implemented by *pipeline.Indexer.
type Indexer interface {
	// Prepare proves the embedding endpoint is up and returns its dimension
	// count, creating the vector table if needed. It is the health gate.
	Prepare(ctx context.Context) (int, error)
	// StageVersions is what content processed now is stamped with.
	StageVersions(dims int) map[string]string
	// ProcessContent runs one claimed work item through the pipeline. Its
	// error return is fatal; everything about the content arrives as a
	// Result.
	ProcessContent(ctx context.Context, item domain.WorkItem, sv map[string]string) (pipeline.Result, error)
}

// Scanner reconciles the catalog with the filesystem, as implemented by
// *discovery.Scanner.
type Scanner interface {
	// Scan walks every root — the periodic backstop.
	Scan(ctx context.Context) (discovery.Result, error)
	// ScanPaths reconciles a named set of paths — what the watcher drives.
	ScanPaths(ctx context.Context, paths []string) (discovery.Result, error)
	// Roots reports the directories a scan would walk, so the watcher
	// subscribes to exactly those.
	Roots() ([]string, []discovery.PathError)
}

// Options wires a Scheduler. Queue, Indexer and Scanner are required;
// everything else has a working default.
type Options struct {
	Queue   Queue
	Indexer Indexer
	Scanner Scanner
	Logger  *slog.Logger

	// Watcher delivers filesystem changes as they happen. Nil means
	// scan-only, which is a working mode rather than a broken one — it is
	// what a platform without FSEvents gets, and what a failed subscription
	// falls back to.
	Watcher domain.Watcher
	// Debounce is how long watched events are collected before being
	// reconciled; zero means the default.
	Debounce time.Duration

	// Power reports the policy for the current power state. Nil means always
	// on AC — the honest default until macOS power detection lands (M7),
	// because guessing "battery" would silently stop indexing on a desktop.
	Power func() config.PowerPolicy
	// ScanInterval pins how often the filesystem is walked. Zero means
	// automatic: defaultScanInterval while scan-only, watchedScanInterval
	// once the watcher is running.
	ScanInterval time.Duration
	// BatchSize is how many documents one claim returns; zero means the
	// default.
	BatchSize int
	// MaxAttempts is how many transient failures a document absorbs before
	// it is called failed; zero means the default.
	MaxAttempts int

	// Clock is the time source, for tests. Nil means time.Now.
	Clock func() time.Time
	// Rand returns a value in [0, n), for deterministic backoff in tests.
	// Nil means math/rand/v2.
	Rand func(n int64) int64
}

// PathError is one path the last scan could not read, as reported to
// `bsearch status`. The error is flattened to a string so a Snapshot is an
// immutable value that a status handler can hold onto while the next scan
// runs.
type PathError struct {
	Path string
	Err  string
}

// Snapshot is what the scheduler is doing, as `bsearch status` reports it.
// Counters are cumulative since start; timestamps are zero until they happen.
type Snapshot struct {
	// Gate says why the last cycle did no work, in the user's terms. Empty
	// means the last cycle worked normally.
	Gate string
	// LastError is the most recent failure that stopped a cycle.
	LastError string
	// LastScan is when the filesystem was last walked to completion.
	LastScan time.Time
	// LastCycle is when the last cycle finished, worked or gated.
	LastCycle time.Time
	// LastProgress is when a document was last indexed — the timestamp that
	// distinguishes a queue that is slow from one that is stuck.
	LastProgress time.Time

	Indexed int // documents indexed since start
	Failed  int // documents given up on since start
	Skipped int // documents unreadable at the time since start
	Retried int // transient failures rescheduled since start
	Swept   int // documents re-queued by a stale sweep since start
	// Collected counts orphaned contents the sweep removed since start —
	// the visibility that keeps a silently no-op sweep (the class ADR 0015
	// singles out) diagnosable from status.
	Collected int
	// Changed counts claims abandoned because no copy of the file still
	// held the claimed bytes. Counted because the pathological shape — a
	// file rewritten continuously, or an edit that preserves size and
	// mtime so discovery never re-points it — would otherwise re-read and
	// re-hash the file on a schedule with nothing in status to show for
	// it.
	Changed int
	// Superseded counts documents whose row changed under the pipeline
	// mid-document. Since ADR 0014 that can only happen if something other
	// than the scheduler goroutine wrote the catalog, so any non-zero value
	// is a broken invariant rather than a busy daemon.
	Superseded int
	// ScanErrs counts per-path problems (TCC, unreadable) the daemon has met
	// since the last walk: the walk replaces the count, and each reconcile in
	// between adds to it. Both sources on purpose — a permission error the
	// watcher hit is exactly as first-class as one the walk hit, and with the
	// walk down to a quarter-hour cadence, "wait for the next scan" is not an
	// acceptable answer to "why is nothing being indexed".
	ScanErrs int
	// PathErrors is a sample of those problems — the first few, capped at
	// maxLoggedPathErrors. A count alone says something is unreadable without
	// saying what, and "~/Documents: operation not permitted" is the whole
	// diagnosis (issue #14 turns it into remediation).
	PathErrors []PathError
	// ScanReachedNothing means the last scan hit errors and reached no files
	// at all — the signature of a missing Full Disk Access grant, and the one
	// scan outcome that leaves the daemon looking healthy while indexing
	// nothing (DESIGN.md: TCC is first-class state).
	ScanReachedNothing bool
	Deferring          bool

	// Watching means the filesystem watcher is subscribed and delivering.
	// When it is false, WatchReason says why in the user's terms and the
	// scan is carrying freshness on its own.
	Watching bool
	// WatchReason is why the watcher is not running. Empty while it is.
	WatchReason string
	// WatchRoots is how many directories are subscribed.
	WatchRoots int
	// LastWatchEvent is when the watcher last delivered something. Against
	// LastScan it is what distinguishes "nothing has changed" from "events
	// have stopped arriving" — the signature of a watcher that is nominally
	// running but is being starved of events, which on macOS means the
	// daemon is missing Full Disk Access.
	LastWatchEvent time.Time

	WatchReconciled int // documents queued from watched events since start
	WatchDeleted    int // documents purged because their file went away
	WatchRescans    int // full scans forced by an incomplete event stream
	// WatchIgnored counts watched paths that were out of scope — outside
	// every include root, or deny-listed. A running total rather than a
	// per-reconcile log line, because the pathology it exists to expose (a
	// root whose on-disk spelling does not match the paths FSEvents reports,
	// so every event is discarded) is invisible in any single reconcile once
	// one reconcile can span several debounce windows. Reconciled 0 against a
	// large Ignored is the signature.
	WatchIgnored int
}

// Scheduler runs the indexing loop. Construct with New, drive with Run.
type Scheduler struct {
	queue   Queue
	indexer Indexer
	scanner Scanner
	watcher domain.Watcher
	log     *slog.Logger

	power        func() config.PowerPolicy
	scanInterval time.Duration
	debounce     time.Duration
	batchSize    int
	maxAttempts  int
	now          func() time.Time
	rnd          func(int64) int64

	// notify is the wake hint. Buffered by one and written non-blockingly:
	// the table is the source of truth, so a dropped hint costs latency, not
	// correctness, and the timer is the fallback that makes that true.
	notify chan struct{}

	// sweptVersions and sweptDims track the once-per-process stale sweeps.
	// Once per process, not once per cycle: configuration is read at startup
	// and cannot change under a running daemon, so a second sweep could only
	// ever move zero rows.
	sweptVersions bool
	sweptDims     bool

	mu   sync.Mutex
	snap Snapshot
	// needQueueSweep asks the next cycle to collect non-terminal orphaned
	// content (ADR 0015). Set by the two orphan producers — a deletion
	// removing rows, an edit re-pointing a path away from its old hash.
	// Never every cycle: a quiet machine's cycles pay nothing. Terminal
	// orphans are deliberately not collected eagerly — they cost nothing
	// in the queue and an undo or a split-window rename re-references them
	// for free — so needFullSweep runs once at startup and covers them,
	// plus whatever a crash or a previous build left behind.
	//
	// Under mu only because scan results are recorded under mu; the sweep
	// itself runs on the scheduler goroutine like every other write.
	needQueueSweep bool
	needFullSweep  bool
	// forceScan is a walk requested out of band — by an incomplete event
	// stream. Consumed by scanDue, so it survives a scan already in flight.
	forceScan bool
	// pendingPaths is the debounce windows the watcher has closed and this
	// goroutine has not reconciled yet. It is the whole of the handoff ADR
	// 0014 asks for: the watcher merges into it, the scheduler swaps it out,
	// and no catalog row is written anywhere but here.
	//
	// A set rather than a queue of windows, for two reasons. A file saved
	// three times during one long embed is one path to stat, so merging is
	// free deduplication; and the bound that matters — the one the overflow
	// rule is written against — is distinct paths, which a queue of windows
	// could not express.
	pendingPaths map[string]struct{}
	// nextProbe is when the embedding endpoint may be probed again after a
	// failure. Zero means "now". See prepare for why the wake rate must not
	// decide the probe rate.
	nextProbe time.Time
}

// New builds a Scheduler.
func New(opts Options) (*Scheduler, error) {
	if opts.Queue == nil || opts.Indexer == nil || opts.Scanner == nil {
		return nil, errors.New("scheduler: Queue, Indexer, and Scanner are all required")
	}
	s := &Scheduler{
		queue:        opts.Queue,
		indexer:      opts.Indexer,
		scanner:      opts.Scanner,
		watcher:      opts.Watcher,
		log:          opts.Logger,
		power:        opts.Power,
		scanInterval: opts.ScanInterval,
		debounce:     opts.Debounce,
		batchSize:    opts.BatchSize,
		maxAttempts:  opts.MaxAttempts,
		now:          opts.Clock,
		rnd:          opts.Rand,
		notify:       make(chan struct{}, 1),
		// The first cycle sweeps everything: one indexed anti-join probe,
		// buying a catalog with no leftovers from before a restart — and
		// the collection point for terminal orphans the eager sweep
		// deliberately spares.
		needFullSweep: true,
	}
	if s.log == nil {
		s.log = slog.Default()
	}
	if s.power == nil {
		s.power = func() config.PowerPolicy {
			return config.PowerPolicy{IndexInterval: config.Interval{Duration: 5 * time.Minute}}
		}
	}
	if s.debounce <= 0 {
		s.debounce = defaultDebounce
	}
	if s.batchSize <= 0 {
		s.batchSize = defaultBatchSize
	}
	if s.maxAttempts <= 0 {
		s.maxAttempts = defaultMaxAttempts
	}
	if s.now == nil {
		s.now = time.Now
	}
	if s.rnd == nil {
		//nolint:gosec // G404: jitter spreads retries, it is not a secret
		s.rnd = rand.Int64N
	}
	return s, nil
}

// Notify hints that there may be new work. It never blocks and never fails:
// a hint carries no information beyond "look again", so dropping one when a
// hint is already pending is exactly right — waking once and finding fifty
// new rows beats waking fifty times.
//
// #13's watcher is the caller. Nothing depends on delivery: the periodic
// cycle is the fallback that makes a lost hint a latency problem rather than
// a correctness one.
func (s *Scheduler) Notify() {
	select {
	case s.notify <- struct{}{}:
	default:
	}
}

// Snapshot reports what the scheduler is doing. Safe for concurrent use —
// the HTTP status handler calls it while a cycle is running.
//
// PathErrors is copied rather than shared. Returning the struct by value
// copies the slice header, not the backing array, so the caller would
// otherwise be reading the same memory the next scan overwrites — a data race
// on the one field a user reads when something is already wrong.
func (s *Scheduler) Snapshot() Snapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	snap := s.snap
	snap.PathErrors = slices.Clone(s.snap.PathErrors)
	return snap
}

// Run drives the loop until ctx is cancelled, which is the only way it ends;
// the returned error is always nil, and exists so Run reads like every other
// long-running call in the codebase.
//
// The first cycle runs immediately. A daemon that starts after the machine
// has been off for a week should not sit idle waiting for a tick before
// noticing that the corpus moved on without it.
func (s *Scheduler) Run(ctx context.Context) error {
	// No scan_interval here: the cadence follows the watcher, and the watcher
	// has not subscribed yet — the number would always be the scan-only one,
	// contradicting the "watching" field beside it. watch logs the cadence it
	// settles on once it knows.
	s.log.Info("indexing scheduler started",
		"batch_size", s.batchSize,
		"max_attempts", s.maxAttempts,
		"watching", s.watcher != nil)

	// The watcher runs alongside the cycle rather than inside it: FSEvents
	// intake and the debounce window must not wait behind a drain that may be
	// hours long. It writes nothing, though — it hands its closed windows to
	// this goroutine (ADR 0014). Run owns it so shutdown stays one story.
	var watching sync.WaitGroup
	if s.watcher != nil {
		// Recorded before the goroutine starts, not inside it: resolving
		// roots stats every include path, which on a cold or network-backed
		// volume is exactly the moment someone runs `bsearch status`. Without
		// a reason here, that report reads "off — no reason given", which
		// says the freshness feature is broken when it is merely starting.
		s.mark(func(snap *Snapshot) { snap.WatchReason = WatchStarting })
		watching.Add(1)
		go func() {
			defer watching.Done()
			s.watch(ctx)
		}()
	}
	// Ordered, not a bare `defer watching.Wait()`. The watcher hands over its
	// still-open debounce window as it exits, and only this goroutine may
	// write it — so the wait is a precondition of the final reconcile rather
	// than goroutine hygiene, whichever goroutine notices the cancellation
	// first.
	//
	// Worth the machinery because a window's contents are not equally
	// recoverable. An edit dropped here is picked up by the next walk; a
	// deletion dropped here is picked up by nothing, since only ScanPaths
	// purges and a walk sees what exists. The last ten seconds before a
	// restart would otherwise be the one window whose deletions stay in the
	// index for good.
	defer func() {
		watching.Wait()
		s.flushPending(ctx)
		s.log.Info("indexing scheduler stopped")
	}()

	timer := time.NewTimer(0)
	defer timer.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-timer.C:
		case <-s.notify:
		}

		next := s.cycle(ctx)
		if ctx.Err() != nil {
			return nil
		}
		if next <= 0 {
			// A policy with no interval — a zero-value PowerPolicy from a
			// future power adapter — would otherwise reset the timer to fire
			// immediately, spinning this loop on a full core. Config validates
			// intervals as positive, so this only ever catches a caller bug.
			next = deferRecheckInterval
		}
		// Reset after the cycle rather than on a fixed schedule: that is what
		// makes the overrun policy Prefer Old (see the package comment) and
		// what lets a power-state change take effect on the next interval
		// rather than the next restart.
		//
		// No drain of timer.C between Stop and Reset. That idiom is for Go
		// 1.22 and earlier, where the channel was buffered and a tick that
		// landed while the cycle was running would survive the Reset and fire
		// the next select immediately — which would defeat exactly the policy
		// above. Since 1.23 the channel is unbuffered and Reset guarantees no
		// stale value is received; go.mod requires 1.26, so the drain would
		// now block forever on the common path where the timer never fired.
		timer.Stop()
		timer.Reset(next)
	}
}

// flushPending reconciles what the watcher handed over and the loop did not
// reach, as the daemon stops.
func (s *Scheduler) flushPending(ctx context.Context) {
	if !s.hasPending() {
		// Checked before the context is built, so a daemon with no watcher —
		// or nothing owed — does not arm a timer on its way out.
		return
	}
	// Detached from ctx: it is almost certainly already cancelled — that is
	// why we are here — and ScanPaths on a cancelled context does nothing.
	// Bounded so a slow store cannot hold shutdown open; a shutdown that hangs
	// is worse than a stale row, and launchd's patience is finite.
	flushCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownFlush)
	defer cancel()
	s.log.Debug("reconciling the last events before shutting down")
	s.reconcilePending(flushCtx)
}

// cycle runs one scan-if-due plus one drain, and returns how long to wait
// before the next one.
func (s *Scheduler) cycle(ctx context.Context) time.Duration {
	defer s.mark(func(snap *Snapshot) { snap.LastCycle = s.now() })

	policy := s.power()

	// Discovery runs whether or not indexing does. It is a stat walk, not
	// inference: cheap enough to keep the catalog honest on battery, and
	// leaving it out would mean a laptop that spent the day unplugged came
	// back with no idea what had changed (DESIGN.md: cheap stages always run).
	if due, forced := s.scanDue(); due {
		s.scan(ctx, forced)
	}
	if ctx.Err() != nil {
		return deferRecheckInterval
	}

	// Above the battery gate, and below the walk, both deliberately.
	//
	// Above the gate because reconciling named paths is the same stat-and-hash
	// the walk is, and discovery already runs on battery for that reason
	// (DESIGN.md: cheap stages always run). Below the gate this would be the
	// one thing a deferred cycle never reached, and an unplugged laptop would
	// accumulate a day of deletions and purge none of them.
	//
	// Below the walk because scan *replaces* ScanErrs and PathErrors while
	// this appends to them. One order or the other had to be chosen; this one
	// makes the sample deterministic, where two goroutines made it a race the
	// mutex kept safe but not repeatable.
	s.reconcilePending(ctx)
	if ctx.Err() != nil {
		return deferRecheckInterval
	}

	// The orphan sweep sits with its producers, above the battery gate: the
	// walk and the reconcile above both run on battery and both orphan
	// content (deletions, edits), so a gated sweep would let an unplugged
	// day accumulate garbage its producers kept making. It is one indexed
	// anti-join probe when there is nothing to do — no network, no
	// inference (DESIGN.md: cheap stages always run). Running here, at most
	// once per cycle, is also what bounds it: reconciles inside the drain
	// re-arm the flag, and those arms are consumed next cycle, never
	// mid-drain (a churn-heavy drain must not pay one sweep per batch).
	//
	// Before the drain, so a row the reconcile above just orphaned is
	// collected before anything is claimed. (process reconciles again
	// between contents, so a mid-batch deletion can still surface as
	// ErrContentGone downstream — contentGone handles it; the sweep
	// placement narrows the window, it does not close it.)
	s.sweepOrphans(ctx)
	if ctx.Err() != nil {
		return deferRecheckInterval
	}

	if policy.IndexInterval.Defer {
		s.gate(GateBattery, true)
		s.log.Debug("indexing deferred", "reason", GateBattery)
		return deferRecheckInterval
	}
	s.mark(func(snap *Snapshot) { snap.Deferring = false })

	s.drain(ctx)
	return policy.IndexInterval.Duration
}

// scanDue reports whether this cycle should walk the filesystem, and whether
// the walk was asked for out of band. It consumes any such request as it
// goes; a walk that then fails re-arms it (see scan).
func (s *Scheduler) scanDue() (due, forced bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.forceScan {
		// Cleared on consumption rather than after the walk: a request that
		// arrives while this scan is running is about changes this scan may
		// already have passed, and deserves its own.
		s.forceScan = false
		return true, true
	}
	return s.snap.LastScan.IsZero() || s.now().Sub(s.snap.LastScan) >= s.scanIntervalLocked(), false
}

// currentScanInterval is how long between walks right now.
func (s *Scheduler) currentScanInterval() time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.scanIntervalLocked()
}

// scanIntervalLocked requires s.mu.
//
// A configured interval wins; otherwise the cadence follows the watcher,
// because the walk means two different things depending on it. Scan-only, it
// is the freshness mechanism and has an SLO to meet. Watched, it is the
// backstop for events that went missing, and walking $HOME twelve times an
// hour to catch them is a battery cost with nothing behind it. Reading the
// live flag rather than latching it at startup means a watcher that stops
// restores the tighter cadence on its own.
func (s *Scheduler) scanIntervalLocked() time.Duration {
	switch {
	case s.scanInterval > 0:
		return s.scanInterval
	case s.snap.Watching:
		return watchedScanInterval
	default:
		return defaultScanInterval
	}
}

// scan walks the filesystem and reconciles the catalog. Per-path problems are
// reported, never fatal: a single unreadable directory must not stop the rest
// of the corpus being indexed, and a permission error is a first-class state
// the user needs to see (DESIGN.md: TCC).
//
// forced says the walk was asked for out of band rather than falling due,
// which changes only what a failure means: the request is re-armed, because
// nothing else knows what it was for.
func (s *Scheduler) scan(ctx context.Context, forced bool) {
	start := s.now()
	res, err := s.scanner.Scan(ctx)

	// Recorded before the error is considered — the walk flushes as it
	// goes, so a fatal error still leaves prior batches (and their
	// deletions and re-points) committed, exactly like the watcher's
	// reconcile. The path errors met before the failure are equally real;
	// discarding them with the result was the silent-skip shape CLAUDE.md
	// forbids. LastScan and the reached-nothing signature stay
	// success-only below: a partial walk proves nothing about the corpus.
	sample := s.logPathErrors(res.PathErrors, "scan could not read a path",
		"further scan path errors suppressed", maxLoggedPathErrors)
	s.mark(func(snap *Snapshot) {
		snap.ScanErrs = len(res.PathErrors)
		snap.PathErrors = sample
	})
	s.noteOrphanProducers(res)

	if err != nil {
		if ctx.Err() != nil {
			return
		}
		s.log.Error("scan failed", "error", err)
		s.mark(func(snap *Snapshot) { snap.LastError = err.Error() })
		if forced {
			// A walk asked for out of band answers for events that were
			// dropped, and those events exist nowhere else. LastScan is not
			// advanced on this path either, so without re-arming, the retry
			// would wait out a full interval — up to fifteen minutes while
			// watching — for changes nothing else can find.
			s.requestScan()
		}
		return
	}
	// Reached excludes Unread on purpose: a denied file was not reached,
	// and counting it would disarm this alarm for a listable root whose
	// every file fails open with EPERM.
	reached := res.Reached()
	if len(res.PathErrors) > 0 && reached == 0 {
		// Every root failed and nothing was reachable. That is a permissions
		// problem — almost always a missing or revoked Full Disk Access grant
		// — not a successful scan of an empty machine, and it is the one scan
		// outcome that leaves the daemon looking healthy while indexing
		// nothing at all. The removed one-shot command exited non-zero here;
		// a daemon cannot, so it says so at Error and keeps the reason where
		// `bsearch status` will find it (CLAUDE.md: EPERM on scan is
		// first-class state, never a silent skip).
		s.log.Error("scan reached no files at all — check Full Disk Access for bsearch in System Settings",
			"path_errors", len(res.PathErrors))
	}

	s.log.Info("scan complete",
		"discovered", res.Discovered,
		"unchanged", res.Unchanged,
		"changed", res.Changed,
		"dataless", res.Dataless,
		"unread", res.Unread,
		"path_errors", len(res.PathErrors),
		"duration_ms", s.now().Sub(start).Milliseconds())

	s.mark(func(snap *Snapshot) {
		snap.LastScan = s.now()
		snap.ScanReachedNothing = len(res.PathErrors) > 0 && reached == 0
	})
}

// noteOrphanProducers arms the orphan sweep when a discovery result did
// either of the two things that orphan content: deleted documents rows, or
// re-pointed a path away from its old hash (an edit). New files and
// unchanged files produce no orphans, so a bulk initial index never pays
// for sweeping (ADR 0015; the issue's "after deletions, not every cycle",
// widened to edits because they orphan identically).
func (s *Scheduler) noteOrphanProducers(res discovery.Result) {
	if res.Deleted > 0 || res.Changed > 0 {
		s.requestOrphanSweep(domain.SweepScopeQueue)
	}
}

// logPathErrors reports per-path problems at Warn up to limit, says how many
// it held back, and returns the same prefix in the Snapshot's shape. Shared
// by the walk and the watcher's reconcile so the two cannot drift into
// reporting the same class of problem differently.
func (s *Scheduler) logPathErrors(errs []discovery.PathError, msg, suppressed string, limit int) []PathError {
	var sample []PathError
	for i, pe := range errs {
		if i == limit {
			s.log.Warn(suppressed, "suppressed", len(errs)-limit)
			break
		}
		s.log.Warn(msg, "path", pe.Path, "error", pe.Err)
		sample = append(sample, PathError{Path: pe.Path, Err: pe.Err.Error()})
	}
	return sample
}

// appendCapped adds src to dst, stopping at limit. Status shows a sample, not
// a log: past a handful more paths stop adding information.
func appendCapped(dst, src []PathError, limit int) []PathError {
	for _, pe := range src {
		if len(dst) >= limit {
			break
		}
		dst = append(dst, pe)
	}
	return dst
}

// drain works the queue until it is empty, gated, or cancelled.
//
// It re-claims rather than iterating one list, so a file saved during a long
// backlog drain is picked up within a batch instead of at the end. The cost
// is that a document which stays claimable — one skipped because it is
// unreadable right now, and therefore left untouched by design — would be
// claimed forever; seen is what bounds the drain to one pass per document.
func (s *Scheduler) drain(ctx context.Context) {
	seen := make(map[string]struct{})
	dims, prepared := 0, false
	skipped := 0

	for {
		if ctx.Err() != nil {
			return
		}
		// Between batches, so the row a reconcile just wrote is at the head of
		// the very next claim rather than one batch late. It also covers the
		// two `continue` arms below, which never reach process.
		s.reconcilePending(ctx)
		// The version sweep needs no network, so it runs before anything can
		// decide the queue is empty: a corpus that is entirely indexed under
		// a superseded model looks like no work at all until it is swept.
		if !s.sweptVersions {
			sv := s.indexer.StageVersions(0)
			delete(sv, domain.StageEmbeddingDims) // unknown until Prepare
			if !s.sweep(ctx, sv) {
				return
			}
			s.sweptVersions = true
			continue
		}

		contents, err := s.queue.ClaimBatch(ctx, s.now(), s.batchSize)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			s.log.Error("claim batch failed", "error", err)
			s.gate(GateStoreFail, false)
			s.mark(func(snap *Snapshot) { snap.LastError = err.Error() })
			return
		}
		// Filtered here, recorded in process: a batch can be claimed and then
		// set aside (the dims sweep re-claims from scratch), and marking a
		// content seen before it has actually been through the pipeline
		// would drop it for the rest of the drain.
		fresh := contents[:0]
		for _, c := range contents {
			if _, dup := seen[c.Hash]; !dup {
				fresh = append(fresh, c)
			}
		}

		// Nothing to do and nothing left to check: stop before Prepare, so an
		// up-to-date corpus costs zero network calls per cycle. The daemon is
		// idle for weeks at a time; probing an endpoint every five minutes to
		// confirm there is still nothing to embed would keep a model resident
		// and a laptop awake for no reason.
		if len(fresh) == 0 && s.sweptDims {
			s.gate(idleGate(skipped), false)
			return
		}

		if !prepared {
			d, ok := s.prepare(ctx)
			if !ok {
				return
			}
			dims, prepared = d, true
			if !s.sweptDims {
				// Dims are part of the vector table's identity but outside
				// the embedding fingerprint, so a server that changed them
				// under an unchanged model name strands documents that look
				// current. Only knowable after the probe, hence the second
				// sweep.
				if !s.sweep(ctx, map[string]string{
					domain.StageEmbeddingDims: s.indexer.StageVersions(d)[domain.StageEmbeddingDims],
				}) {
					return
				}
				s.sweptDims = true
				continue
			}
		}
		if len(fresh) == 0 {
			s.gate(idleGate(skipped), false)
			return
		}

		sv := s.indexer.StageVersions(dims)
		n, ok := s.process(ctx, fresh, sv, seen)
		skipped += n
		if !ok {
			return
		}
		s.gate(GateNone, false)

		// Power is re-read between batches, not once per cycle: unplugging
		// the laptop part-way through a backlog should stop the drain, not
		// be noticed an hour later.
		if s.power().IndexInterval.Defer {
			s.gate(GateBattery, true)
			s.log.Debug("drain paused", "reason", GateBattery)
			return
		}
	}
}

// prepare runs the health gate. A failure defers the whole batch and charges
// nothing to any document: an endpoint that is switched off is not evidence
// about any file.
func (s *Scheduler) prepare(ctx context.Context) (int, bool) {
	// A probe that failed a moment ago is not worth repeating. Cycles used to
	// arrive on a timer, so "once per cycle" was already a rate limit; since
	// ADR 0014 a closed debounce window wakes one too, and ordinary background
	// churn under a watched root — an editor's save, a build writing into a
	// deny-listed directory — would otherwise probe a switched-off inference
	// server every debounce window rather than every interval.
	//
	// Not backoff: there is nothing to back off from, no attempt is charged to
	// any document, and the interval is the same one a deferred cycle already
	// rechecks on. It only stops the wake *rate* deciding the probe rate
	// (DESIGN.md: near-zero CPU when idle).
	if until, ok := s.probeDeferredUntil(); ok {
		s.log.Debug("skipping the embedding endpoint probe", "retry_at", until)
		return 0, false
	}

	dims, err := s.indexer.Prepare(ctx)
	if err != nil {
		if ctx.Err() != nil {
			return 0, false
		}
		s.log.Warn("indexing deferred", "reason", GateEmbedder, "error", err)
		s.gate(GateEmbedder, false)
		s.mark(func(snap *Snapshot) { snap.LastError = err.Error() })
		s.deferProbe()
		return 0, false
	}
	s.clearProbeDeferral()
	return dims, true
}

// probeDeferredUntil reports whether a failed probe is still inside its
// recheck interval, and when that interval ends.
func (s *Scheduler) probeDeferredUntil() (time.Time, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.nextProbe, !s.nextProbe.IsZero() && s.now().Before(s.nextProbe)
}

// deferProbe holds the next endpoint probe off until the recheck interval.
func (s *Scheduler) deferProbe() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.nextProbe = s.now().Add(deferRecheckInterval)
}

// clearProbeDeferral forgets a past failure once the endpoint answers.
func (s *Scheduler) clearProbeDeferral() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.nextProbe = time.Time{}
}

// sweep re-queues documents whose derived data predates current, reporting
// whether the drain should continue.
func (s *Scheduler) sweep(ctx context.Context, current map[string]string) bool {
	moved, err := s.queue.ResetStale(ctx, current)
	if err != nil {
		if ctx.Err() != nil {
			return false
		}
		s.log.Error("stale sweep failed", "error", err)
		s.gate(GateStoreFail, false)
		s.mark(func(snap *Snapshot) { snap.LastError = err.Error() })
		return false
	}
	if moved > 0 {
		// Worth Info: this is the daemon explaining why it is about to spend
		// an hour of inference on a corpus the user thought was indexed.
		s.log.Info("re-queued documents built by a superseded pipeline stage", "documents", moved)
		s.mark(func(snap *Snapshot) { snap.Swept += moved })
	}
	return true
}

// process works one batch, reporting how many contents were unreadable and
// whether the drain should continue. Each content is recorded in seen as it
// is handled, which is what bounds the drain to one pass per content however
// the outcome leaves the row.
func (s *Scheduler) process(ctx context.Context, contents []domain.Content, sv map[string]string, seen map[string]struct{}) (skipped int, ok bool) {
	for _, c := range contents {
		if ctx.Err() != nil {
			return skipped, false
		}
		// Between contents, never inside one. Both halves of this matter and
		// they are the same constraint (ADR 0014): a saved file waits at most
		// one content to be noticed, and — because nothing else writes the
		// catalog — the re-read below cannot go stale between here and the
		// end of ProcessContent.
		s.reconcilePending(ctx)
		if ctx.Err() != nil {
			return skipped, false
		}

		// The copy in contents was read when the batch was claimed, which may
		// be many contents and several reconciles ago. GetWork re-reads the
		// row and picks the path to read bytes from *now* — working from the
		// claimed copy would use a stale attempt count, and there was no path
		// in it to begin with (issue #63's lesson, rebuilt for ADR 0015).
		item, err := s.queue.GetWork(ctx, c.Hash)
		if err != nil {
			if ctx.Err() != nil {
				return skipped, false
			}
			if s.contentGone(c.Hash, err) {
				seen[c.Hash] = struct{}{}
				continue
			}
			return skipped, s.storeFailure(ctx, "re-read content", err)
		}

		seen[c.Hash] = struct{}{}
		res, err := s.indexer.ProcessContent(ctx, item, sv)
		if err != nil {
			if ctx.Err() != nil {
				return skipped, false
			}
			// The machinery failed, not the content: a store write that did
			// not land says nothing about this file, so nothing is charged.
			s.log.Error("indexing failed", "path", item.Path, "error", err)
			s.gate(GateStoreFail, false)
			s.mark(func(snap *Snapshot) { snap.LastError = err.Error() })
			return skipped, false
		}

		switch res.Outcome {
		case pipeline.OutcomeIndexed:
			s.log.Debug("indexed", "path", item.Path)
			s.mark(func(snap *Snapshot) {
				snap.Indexed++
				snap.LastProgress = s.now()
			})
		case pipeline.OutcomeFailed:
			s.log.Warn("content cannot be indexed", "path", item.Path, "error", res.Err)
			s.mark(func(snap *Snapshot) { snap.Failed++ })
		case pipeline.OutcomeSkipped:
			// Unreadable right now — vanished, or TCC. The row is untouched
			// on purpose: restoring the file or granting access must take
			// effect without the file having to change.
			s.log.Debug("content unreadable, leaving it alone", "path", item.Path, "error", res.Err)
			skipped++
			s.mark(func(snap *Snapshot) { snap.Skipped++ })
		case pipeline.OutcomeChanged:
			// No copy of the file still holds the claimed bytes — the
			// content changed under the claim (the pipeline already tried
			// every referencing path). Discovery's next pass re-points the
			// path and the sweep collects this row; until then it is pushed
			// onto the retry schedule at the backoff cap WITHOUT charging
			// an attempt (the file is not at fault, but the row must not
			// stay hot). The defer is what bounds the pathological shapes —
			// a continuously-rewritten file, or an edit that preserves size
			// and mtime so discovery never notices — to one full read and
			// hash per cap interval instead of per drain, and the counter
			// is what makes that loop visible in status.
			s.log.Debug("content changed while queued, abandoned", "path", item.Path)
			s.mark(func(snap *Snapshot) { snap.Changed++ })
			if err := s.queue.Reschedule(ctx, c.Hash, item.Content.Attempts,
				s.now().Add(defaultBackoffCap), "content changed on disk while queued; awaiting rediscovery"); err != nil {
				if !s.contentGone(item.Path, err) && !s.storeFailure(ctx, "defer changed content", err) {
					return skipped, false
				}
			}
		case pipeline.OutcomeSuperseded:
			// Two arms share this outcome and mean opposite things —
			// pipeline.movedOn says the caller tells them apart, and this
			// is that split. Gone is routine: the row was swept (every
			// referencing path deleted mid-flight, collected between the
			// re-read and the pipeline's write) — same handling as
			// contentGone at the re-read, counted nowhere.
			if errors.Is(res.Err, domain.ErrContentGone) {
				s.log.Debug("content was deleted while it was being indexed", "path", item.Path)
				continue
			}
			// Superseded proper: since ADR 0014 this can only mean the
			// single-writer invariant broke. The content was re-read a
			// moment ago, so its row was there; for its chunk rows to be
			// rewritten by the time the pipeline wrote, something other
			// than this goroutine must have written the catalog while this
			// goroutine was inside ProcessContent — and there is nothing
			// else.
			//
			// It used to be routine, which is why it was logged at Debug and
			// counted nowhere. Left that way it is now the quietest failure in
			// the daemon: the row keeps its state and its unset next_retry_at,
			// so it is re-claimed and superseded every cycle forever, while
			// `bsearch status` reports no failures, no skips, and an empty
			// gate over permanently unsearchable content. That is precisely
			// the shape ADR 0014 exists to rule out, so it reports, it counts,
			// and it goes on the retry schedule rather than staying hot.
			//
			// reschedule rather than retry: the embedding endpoint is not
			// implicated, so its health probe would be answering a question
			// nobody asked.
			s.log.Warn("content was superseded mid-flight; only this goroutine should be writing the catalog (ADR 0014)",
				"path", item.Path, "error", res.Err)
			s.mark(func(snap *Snapshot) { snap.Superseded++ })
			if !s.reschedule(ctx, item, res.Err) {
				return skipped, false
			}
		case pipeline.OutcomeTransient:
			if !s.retry(ctx, item, res.Err) {
				return skipped, false
			}
		}
	}
	return skipped, true
}

// idleGate names why a drain stopped with nothing left to claim.
//
// "Idle" and "everything left is unreadable" look identical from the queue's
// side — a skipped document is deliberately left untouched, so it stays
// claimable and simply falls out of this drain's seen set — but they are
// opposite situations for the user. One means the corpus is indexed; the
// other means files are being passed over every cycle, which is what a
// revoked Full Disk Access grant looks like from here.
func idleGate(skipped int) string {
	if skipped > 0 {
		return GateUnreadable
	}
	return GateIdle
}

// retry decides what a transient failure means, and reports whether the drain
// should continue.
//
// The decision rests on one question: was the service healthy when this
// document failed? Re-probing answers it. A failed probe means the endpoint
// went down mid-batch, so the failure is the outage's and the batch is
// deferred with nothing charged. A passing probe means the endpoint is fine
// and this document is the odd one out, which is the only case where an
// attempt is spent.
//
// That is the whole of "attempts are only counted while the service is
// healthy": without the re-probe, an inference server switched off overnight
// would quietly mark a chunk of the corpus permanently failed.
func (s *Scheduler) retry(ctx context.Context, item domain.WorkItem, cause error) bool {
	if _, err := s.indexer.Prepare(ctx); err != nil {
		if ctx.Err() != nil {
			return false
		}
		s.log.Warn("indexing deferred mid-batch", "reason", GateEmbedder, "error", err)
		s.gate(GateEmbedder, false)
		s.mark(func(snap *Snapshot) { snap.LastError = err.Error() })
		return false
	}
	return s.reschedule(ctx, item, cause)
}

// reschedule charges an attempt and sets when the content is next due,
// giving up on it at the cap. Split from retry because not every caller needs
// retry's health probe: that probe exists to decide whether a failure belongs
// to the content or to the embedding endpoint, and a caller that already
// knows the endpoint is not involved would only be paying for it.
func (s *Scheduler) reschedule(ctx context.Context, item domain.WorkItem, cause error) bool {
	reason := "unknown error"
	if cause != nil {
		reason = cause.Error()
	}
	hash := item.Content.Hash
	attempts := item.Content.Attempts + 1
	if attempts >= s.maxAttempts {
		s.log.Warn("giving up on a content after repeated failures",
			"path", item.Path, "attempts", attempts, "error", cause)
		// Record the final count before flipping the state, so the row does
		// not end up claiming "after 5 attempts" in last_error while the
		// attempts column still reads 4 — MarkFailed writes state and reason
		// only. The next_retry_at this also sets is inert once the second
		// write lands: the row is terminal, so dispatch stops looking at it,
		// and anything that revives it (a stale sweep) clears it. A crash
		// between the two writes leaves the row claimable with attempts
		// already at the cap, so it is re-claimed and goes terminal one
		// attempt later — self-correcting, and the reason collapsing this
		// into one transaction is not worth a new store method.
		if err := s.queue.Reschedule(ctx, hash, attempts, s.now(), reason); err != nil {
			return s.contentGone(item.Path, err) || s.storeFailure(ctx, "record final attempt", err)
		}
		if err := s.queue.MarkFailed(ctx, hash,
			fmt.Sprintf("%s (after %d attempts)", reason, attempts)); err != nil {
			return s.contentGone(item.Path, err) || s.storeFailure(ctx, "mark failed", err)
		}
		s.mark(func(snap *Snapshot) { snap.Failed++ })
		return true
	}

	wait := fullJitter(attempts, defaultBackoffBase, defaultBackoffCap, s.rnd)
	if err := s.queue.Reschedule(ctx, hash, attempts, s.now().Add(wait), reason); err != nil {
		return s.contentGone(item.Path, err) || s.storeFailure(ctx, "reschedule", err)
	}
	s.log.Debug("retrying a content later",
		"path", item.Path, "attempts", attempts, "retry_in_s", int(wait.Seconds()))
	s.mark(func(snap *Snapshot) { snap.Retried++ })
	return true
}

// takeOrphanSweep consumes the pending sweep request, widest scope first.
func (s *Scheduler) takeOrphanSweep() (domain.SweepScope, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch {
	case s.needFullSweep:
		s.needFullSweep, s.needQueueSweep = false, false
		return domain.SweepScopeAll, true
	case s.needQueueSweep:
		s.needQueueSweep = false
		return domain.SweepScopeQueue, true
	}
	return 0, false
}

// requestOrphanSweep re-arms a sweep at the given scope.
func (s *Scheduler) requestOrphanSweep(scope domain.SweepScope) {
	s.mu.Lock()
	if scope == domain.SweepScopeAll {
		s.needFullSweep = true
	} else {
		s.needQueueSweep = true
	}
	s.mu.Unlock()
}

// sweepOrphans runs one armed orphan sweep, at most once per cycle. A
// failure re-arms the same scope and lets the cycle continue — the drain
// tolerates orphans (GetWork answers ErrContentGone and process skips), so
// a persistently failing sweep degrades to Gone-skips instead of wedging
// indexing behind garbage collection with no backoff.
func (s *Scheduler) sweepOrphans(ctx context.Context) {
	scope, due := s.takeOrphanSweep()
	if !due {
		return
	}
	n, err := s.queue.SweepOrphans(ctx, scope)
	if err != nil {
		if ctx.Err() != nil {
			s.requestOrphanSweep(scope)
			return
		}
		// Re-armed, or the deletions that set the flag would never be
		// answered for: nothing else re-requests them.
		s.requestOrphanSweep(scope)
		s.log.Error("sweep orphaned content failed", "error", err)
		s.mark(func(snap *Snapshot) { snap.LastError = err.Error() })
		return
	}
	if n > 0 {
		s.log.Info("swept orphaned content", "contents", n)
		s.mark(func(snap *Snapshot) { snap.Collected += n })
	} else {
		s.log.Debug("orphan sweep found nothing to collect")
	}
}

// contentGone reports whether a store error means the content was swept (or
// every path referencing it deleted) while the drain was working on it, in
// which case the drain carries on. ref is for the log line — a path where
// one was resolved, the content hash at the GetWork call site, which fails
// precisely because there is no path.
//
// It is the difference between "the index is broken" and "the file is gone",
// which look identical from a failed write and are opposite situations:
// gating the drain on the second would let one deleted file stop indexing
// for everything behind it, and would report the deletion as an index write
// failure in `bsearch status`.
func (s *Scheduler) contentGone(ref string, err error) bool {
	if !errors.Is(err, domain.ErrContentGone) {
		return false
	}
	// Counted nowhere, for the same reason OutcomeSuperseded is: Skipped
	// means "could not be read", which `bsearch status` reports as a
	// permissions problem. A deleted file is not one.
	s.log.Debug("content was deleted while it was being indexed", "ref", ref)
	return true
}

// storeFailure records a write that did not land and stops the drain.
func (s *Scheduler) storeFailure(ctx context.Context, op string, err error) bool {
	if ctx.Err() != nil {
		return false
	}
	s.log.Error(op+" failed", "error", err)
	s.gate(GateStoreFail, false)
	s.mark(func(snap *Snapshot) { snap.LastError = err.Error() })
	return false
}

// gate records why the scheduler is not making progress.
func (s *Scheduler) gate(reason string, deferring bool) {
	s.mark(func(snap *Snapshot) {
		snap.Gate = reason
		snap.Deferring = deferring
	})
}

// mark applies an update to the snapshot under the lock.
func (s *Scheduler) mark(update func(*Snapshot)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	update(&s.snap)
}
